// Package uploader orchestrates uploads across multiple services in either
// parallel or sequential mode. It handles credential validation, context
// cancellation, progress reporting, transient-failure retries, and result
// aggregation.
package uploader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/driftywinds/manyup/internal/config"
	"github.com/driftywinds/manyup/internal/plugin"
)

// Progress is emitted on the Progress channel during uploads.
type Progress struct {
	Service     string
	State       string // "starting", "uploading", "retry", "done", "error"
	Percent     float64
	Bytes       int64
	Total       int64
	Speed       float64 // bytes per second (smoothed)
	Attempt     int     // attempt number that just failed (State "retry")
	MaxAttempts int     // total attempts that will be made (State "retry")
	Message     string  // compact failure reason (State "retry")
}

// MultiResult aggregates results from all services for a single file.
type MultiResult struct {
	Filename  string
	Results   []*plugin.UploadResult
	TotalTime float64
}

// UploadManager coordinates uploads to multiple services.
type UploadManager struct {
	registry *plugin.Registry
	cfg      *config.AppConfig
}

// New creates an UploadManager.
func New(registry *plugin.Registry, cfg *config.AppConfig) *UploadManager {
	return &UploadManager{
		registry: registry,
		cfg:      cfg,
	}
}

// UploadFile uploads a single file to all selected services.
// progressCh receives progress updates; close it when done.
func (m *UploadManager) UploadFile(
	ctx context.Context,
	filePath string,
	progressCh chan<- Progress,
) (*MultiResult, error) {

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	fileSize := stat.Size()
	filename := filepath.Base(filePath)

	// Resolve which services to upload to.
	services := m.resolveServices()
	if len(services) == 0 {
		return nil, fmt.Errorf("no services selected for upload")
	}

	// Validate credentials for each service, skipping those with missing creds.
	var validServices []plugin.Uploader
	for _, svc := range services {
		if err := m.validateCredentials(svc); err != nil {
			progressCh <- Progress{
				Service: svc.Name(),
				State:   "error",
			}
			fmt.Printf("  ⚠  %s: skipping (%v)\n", svc.DisplayName(), err)
			continue
		}
		validServices = append(validServices, svc)
	}
	services = validServices

	start := time.Now()
	result := &MultiResult{
		Filename: filename,
		Results:  make([]*plugin.UploadResult, 0, len(services)),
	}

	if m.cfg.UploadMode == config.ModeParallel {
		m.uploadParallel(ctx, services, filePath, fileSize, progressCh, result)
	} else {
		m.uploadSequential(ctx, services, filePath, fileSize, progressCh, result)
	}

	result.TotalTime = time.Since(start).Seconds()
	return result, nil
}

// uploadParallel fans out uploads to all services concurrently.
func (m *UploadManager) uploadParallel(
	ctx context.Context,
	services []plugin.Uploader,
	filePath string,
	fileSize int64,
	progressCh chan<- Progress,
	result *MultiResult,
) {
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, svc := range services {
		wg.Add(1)
		go func(s plugin.Uploader) {
			defer wg.Done()
			r := m.doUpload(ctx, s, filePath, fileSize, progressCh)
			mu.Lock()
			result.Results = append(result.Results, r)
			mu.Unlock()
		}(svc)
	}
	wg.Wait()
}

// uploadSequential uploads to services one at a time.
func (m *UploadManager) uploadSequential(
	ctx context.Context,
	services []plugin.Uploader,
	filePath string,
	fileSize int64,
	progressCh chan<- Progress,
	result *MultiResult,
) {
	for _, svc := range services {
		if ctx.Err() != nil {
			break
		}
		r := m.doUpload(ctx, svc, filePath, fileSize, progressCh)
		result.Results = append(result.Results, r)
	}
}

// doUpload uploads to one service, retrying transient failures with
// exponential backoff. It emits exactly one terminal progress event.
func (m *UploadManager) doUpload(
	ctx context.Context,
	svc plugin.Uploader,
	filePath string,
	fileSize int64,
	progressCh chan<- Progress,
) *plugin.UploadResult {
	progressCh <- Progress{
		Service: svc.Name(),
		State:   "starting",
	}

	filename := filepath.Base(filePath)
	maxRetries, baseDelay := m.retrySettings()
	totalAttempts := maxRetries + 1

	start := time.Now()
	var res *plugin.UploadResult

	for attempt := 1; attempt <= totalAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}

		var err error
		res, err = m.attemptUpload(ctx, svc, filename, filePath, fileSize, progressCh)
		if err == nil {
			break
		}

		// Merge the failure into the result.
		if res == nil {
			res = &plugin.UploadResult{}
		}
		res.Service = svc.Name()
		res.Filename = filename
		res.Error = err

		// Retry transient failures; give up on permanent ones.
		if attempt >= totalAttempts || ctx.Err() != nil || !isRetryableError(err) {
			if attempt > 1 {
				res.Error = fmt.Errorf("%w (after %d attempts)", err, attempt)
			}
			break
		}

		wait := retryBackoff(attempt, baseDelay)
		progressCh <- Progress{
			Service:     svc.Name(),
			State:       "retry",
			Attempt:     attempt,
			MaxAttempts: totalAttempts,
			Message:     shortError(err),
		}
		if !sleepContext(ctx, wait) {
			break
		}
	}

	if res == nil {
		// Cancelled before the first attempt or during a backoff wait.
		res = &plugin.UploadResult{
			Service:  svc.Name(),
			Filename: filename,
			Error:    fmt.Errorf("%s: aborted: %w", svc.Name(), ctx.Err()),
		}
	}
	res.Duration = time.Since(start).Seconds()

	state := "done"
	if res.Error != nil {
		state = "error"
	}

	progressCh <- Progress{
		Service: svc.Name(),
		State:   state,
	}

	return res
}

// attemptUpload performs exactly one upload attempt for a service.
// It opens a fresh file handle so every attempt (and every parallel
// service) streams from offset 0.
func (m *UploadManager) attemptUpload(
	ctx context.Context,
	svc plugin.Uploader,
	filename string,
	filePath string,
	fileSize int64,
	progressCh chan<- Progress,
) (*plugin.UploadResult, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("re-opening file: %w", err)
	}
	defer f.Close()

	creds := m.getCredentials(svc.Name())
	cfg := m.getServiceConfig(svc.Name())

	// Wrap the reader so progress events flow to the display.
	pr := newProgressReader(f, fileSize, svc.Name(), progressCh)

	res, err := svc.Upload(ctx, filename, pr, fileSize, creds, cfg)

	// Stop progress tracking; waits for final emit so the terminal
	// event below is guaranteed to arrive after the last progress tick.
	pr.Close()

	return res, err
}

// retrySettings returns the configured retry behaviour.
func (m *UploadManager) retrySettings() (maxRetries int, baseDelay time.Duration) {
	retries, delaySecs := m.cfg.RetrySettings()
	return retries, time.Duration(delaySecs) * time.Second
}

// resolveServices returns the list of Uploader instances for the selected services.
func (m *UploadManager) resolveServices() []plugin.Uploader {
	var out []plugin.Uploader
	for _, name := range m.cfg.SelectedServices {
		if u, ok := m.registry.Get(name); ok {
			out = append(out, u)
		}
	}
	return out
}

// validateCredentials checks that all required credentials are present.
func (m *UploadManager) validateCredentials(svc plugin.Uploader) error {
	creds := m.getCredentials(svc.Name())
	for _, key := range svc.RequiredCredentials() {
		if _, ok := creds[key]; !ok {
			return fmt.Errorf("missing credential %q (set via config or env %s_%s)", key, "MANYUP", key)
		}
	}
	return nil
}

// getCredentials returns merged credentials: config file overrides env vars.
func (m *UploadManager) getCredentials(service string) plugin.Credentials {
	out := make(plugin.Credentials)

	// Env vars as fallback.
	for _, key := range []string{"API_TOKEN", "API_KEY", "TOKEN", "USERNAME", "PASSWORD"} {
		envKey := fmt.Sprintf("MANYUP_%s_%s", uppercase(service), key)
		if v := os.Getenv(envKey); v != "" {
			out[key] = v
		}
	}

	// Config file overrides.
	if sc, ok := m.cfg.Services[service]; ok && sc.Credentials != nil {
		for k, v := range sc.Credentials {
			out[k] = v
		}
	}

	return out
}

// getServiceConfig returns extra options for a service.
func (m *UploadManager) getServiceConfig(service string) plugin.Config {
	if sc, ok := m.cfg.Services[service]; ok && sc.Options != nil {
		return sc.Options
	}
	return nil
}

func uppercase(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		if s[i] >= 'a' && s[i] <= 'z' {
			b[i] = s[i] - 32
		} else {
			b[i] = s[i]
		}
	}
	return string(b)
}
