// Package uploader orchestrates uploads across multiple services in either
// parallel or sequential mode. It handles credential validation, context
// cancellation, progress reporting, and result aggregation.
package uploader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/multiuploader/manyup/internal/config"
	"github.com/multiuploader/manyup/internal/plugin"
)

// Progress is emitted on the Progress channel during uploads.
type Progress struct {
	Service  string
	State    string // "starting", "uploading", "done", "error"
	Percent  float64
	Bytes    int64
	Total    int64
	Speed    float64 // bytes per second (smoothed)
}

// MultiResult aggregates results from all services for a single file.
type MultiResult struct {
	Filename string
	Results  []*plugin.UploadResult
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

// doUpload performs a single upload to one service.
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

	// Re-open the file for each upload (parallel needs independent readers).
	f, err := os.Open(filePath)
	if err != nil {
		return &plugin.UploadResult{
			Service:  svc.Name(),
			Filename: filename,
			Error:    fmt.Errorf("re-opening file: %w", err),
		}
	}
	defer f.Close()

	start := time.Now()
	creds := m.getCredentials(svc.Name())
	cfg := m.getServiceConfig(svc.Name())

	// Wrap the reader so progress events flow to the display.
	pr := newProgressReader(f, fileSize, svc.Name(), progressCh)

	res, err := svc.Upload(ctx, filename, pr, fileSize, creds, cfg)

	// Stop progress tracking; waits for final emit so the terminal
	// event below is guaranteed to arrive after the last progress tick.
	pr.Close()
	if err != nil {
		if res == nil {
			res = &plugin.UploadResult{}
		}
		res.Error = err
		res.Service = svc.Name()
		res.Filename = filename
	}

	if res != nil {
		res.Duration = time.Since(start).Seconds()
	}

	state := "done"
	if res != nil && res.Error != nil {
		state = "error"
	}

	progressCh <- Progress{
		Service: svc.Name(),
		State:   state,
	}

	return res
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
	for _, key := range []string{"API_KEY", "TOKEN", "USERNAME", "PASSWORD"} {
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
