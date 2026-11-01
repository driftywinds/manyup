package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/driftywinds/manyup/internal/config"
	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	// InterimServiceName is the plugin name of the default interim instance.
	InterimServiceName = "interim"

	// DefaultInterimBaseURL is the public origin of the main interim instance.
	DefaultInterimBaseURL = "https://interim.drifty.win"

	// interimInstancePrefix marks a configured additional instance. An instance
	// named "friends" is registered as the service "interim@friends" and stores
	// its origin in the BASE_URL option.
	interimInstancePrefix = InterimServiceName + "@"

	// interimAPIPath is appended to an instance origin to reach its API.
	interimAPIPath = "/api/v1"

	// interimInstanceNameLimit bounds an instance label so it stays readable in
	// the service list and the progress display.
	interimInstanceNameLimit = 48

	// interimPartBatch is the largest number of part URLs the API signs in one
	// request.
	interimPartBatch = 100

	// interimNameLimit is the maximum length, in characters, of the file name
	// an upload intent accepts.
	interimNameLimit = 180

	// interimCompleteAttempts bounds how often completion is retried for
	// transient states such as a not-yet-visible object or a busy intent.
	interimCompleteAttempts = 5

	// interimAbortTimeout bounds the best-effort cleanup of a failed upload.
	interimAbortTimeout = 10 * time.Second
)

const (
	interimModeSingle    = "single"
	interimModeMultipart = "multipart"
)

// interimCompleteRetryDelay is the base pause between completion attempts. It
// is a variable so tests can shrink the wait.
var interimCompleteRetryDelay = 500 * time.Millisecond

// Interim uploads files to an interim instance, a temporary file host built
// around direct-to-S3 uploads. interim is open source, so any number of
// instances can be configured; each one is registered under its own service
// name and uploads independently.
//
// An upload intent reserves quota and either returns a presigned PUT URL for a
// single request or, for larger files, opens an S3 multipart session whose parts
// are uploaded and reported individually. The bytes go straight from this
// machine to storage, and a completion call commits the file record.
//
// A user hash is optional per instance: instances with public uploads enabled
// accept anonymous intents, while restricted instances require an
// admin-approved hash.
//
// See api-refs/interim.md for the full contract.
type Interim struct {
	// name is the registered service name: "interim" for the default instance,
	// or "interim@<label>" for an additional one.
	name string
	// baseURL is the instance origin, without the /api/v1 suffix.
	baseURL string
}

// NewInterim builds a plugin for one instance. Empty values select the default
// instance. baseURL may be an origin or an origin with the /api/v1 suffix.
func NewInterim(name, baseURL string) *Interim {
	if strings.TrimSpace(name) == "" {
		name = InterimServiceName
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultInterimBaseURL
	}
	return &Interim{name: strings.TrimSpace(name), baseURL: normalizeInterimBaseURL(baseURL)}
}

// RegisterInterim registers the default interim instance.
func RegisterInterim(r *plugin.Registry) {
	r.Register(NewInterim(InterimServiceName, DefaultInterimBaseURL))
}

// RegisterInterimConfigured registers the default instance plus every additional
// instance stored in the config. Additional instances are service entries named
// "interim@<label>" with their origin in the BASE_URL option.
func RegisterInterimConfigured(r *plugin.Registry, cfg *config.AppConfig) {
	baseURL := DefaultInterimBaseURL
	if cfg != nil {
		if sc, ok := cfg.Service(InterimServiceName); ok && sc.Options != nil {
			if value := strings.TrimSpace(sc.Options["BASE_URL"]); value != "" {
				if origin, valid := validInterimOrigin(value); valid {
					baseURL = origin
				}
			}
		}
	}
	r.Register(NewInterim(InterimServiceName, baseURL))

	if cfg == nil {
		return
	}

	services := make([]string, 0, len(cfg.Services))
	for service := range cfg.Services {
		if _, ok := InterimInstanceName(service); ok {
			services = append(services, service)
		}
	}
	sort.Strings(services)

	for _, service := range services {
		sc, ok := cfg.Service(service)
		if !ok {
			continue
		}
		instanceBaseURL, ok := interimInstanceBaseURL(service, sc)
		if !ok {
			// A hand-edited entry without a usable BASE_URL is skipped rather
			// than pointed at a guessed host.
			continue
		}
		r.Register(NewInterim(service, instanceBaseURL))
	}
}

// Name returns the unique service slug used in config, selection, and progress.
func (i *Interim) Name() string {
	if i == nil || strings.TrimSpace(i.name) == "" {
		return InterimServiceName
	}
	return i.name
}

// BaseURL returns the instance origin (no /api/v1 suffix).
func (i *Interim) BaseURL() string {
	if i == nil || strings.TrimSpace(i.baseURL) == "" {
		return DefaultInterimBaseURL
	}
	return normalizeInterimBaseURL(i.baseURL)
}

// apiBase returns the root of the instance's REST API.
func (i *Interim) apiBase() string {
	base := strings.TrimSuffix(i.BaseURL(), "/")
	if strings.HasSuffix(base, interimAPIPath) {
		return base
	}
	return base + interimAPIPath
}

func (i *Interim) DisplayName() string {
	if name, ok := InterimInstanceName(i.Name()); ok {
		return "interim (" + name + ")"
	}
	return InterimServiceName
}

func (i *Interim) Description() string {
	if _, ok := InterimInstanceName(i.Name()); ok {
		return "interim instance at " + i.BaseURL() + " (optional user hash, single or multipart S3 upload)"
	}
	return "interim temporary file hosting (optional user hash, single or multipart S3 upload)"
}

// RequiredCredentials is empty: a user hash is optional because an instance
// with public uploads enabled accepts anonymous intents. A restricted instance
// rejects an anonymous create with 401 uploads_restricted, which the plugin
// reports with a hint to configure API_TOKEN.
func (i *Interim) RequiredCredentials() []string { return nil }

func (i *Interim) SupportsLargeUpload() bool { return true }

// ── Response types ──────────────────────────────────────────────────

// interimIntent is the response of POST /uploads. Only the fields the plugin
// acts on are decoded.
type interimIntent struct {
	UploadID    string            `json:"upload_id"`
	Mode        string            `json:"mode"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	PartSize    int64             `json:"part_size"`
	PartCount   int               `json:"part_count"`
	PartsURL    string            `json:"parts_url"`
	StatusURL   string            `json:"status_url"`
	Concurrency int               `json:"concurrency"`
	UploadToken string            `json:"upload_token"`
	CompleteURL string            `json:"complete_url"`
	AbortURL    string            `json:"abort_url"`
	DeleteToken string            `json:"delete_token"`
}

// interimPartURL is one presigned URL from POST /uploads/{id}/parts.
type interimPartURL struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
}

// interimPartsResponse is the response of POST /uploads/{id}/parts.
type interimPartsResponse struct {
	Parts []interimPartURL `json:"parts"`
}

// interimFile is the file record returned by completion.
type interimFile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	PageURL string `json:"page_url"`
}

// interimCompleted is the response of the completion endpoint. It is also
// returned (with a 200) when the upload was already committed.
type interimCompleted struct {
	UploadID string       `json:"upload_id"`
	File     *interimFile `json:"file"`
}

// interimKeyInfo is the response of GET /keys/me.
type interimKeyInfo struct {
	KeyPrefix string `json:"key_prefix"`
	Approved  bool   `json:"approved"`
}

// interimStatus is the subset of GET /status the credential check needs.
// AcceptingUploads is a pointer so an older instance that omits it is treated as
// accepting rather than as permanently unavailable.
type interimStatus struct {
	PublicUploads    bool  `json:"public_uploads"`
	AcceptingUploads *bool `json:"accepting_uploads"`
	MaxFileSizeBytes int64 `json:"max_file_size_bytes"`
	RetentionSeconds int64 `json:"retention_seconds"`
}

// accepting reports whether the instance currently takes new uploads.
func (s interimStatus) accepting() bool {
	return s.AcceptingUploads == nil || *s.AcceptingUploads
}

// interimError is the API's single error envelope.
type interimError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ── Upload ──────────────────────────────────────────────────────────

func (i *Interim) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	userHash := strings.TrimSpace(creds["API_TOKEN"])
	if size <= 0 {
		// The API accepts only objects with at least one byte, so an empty
		// file can never be completed.
		return nil, fmt.Errorf("%s: cannot upload an empty file", i.Name())
	}

	name := interimFileName(filename)
	contentType := interimContentType(name)

	intent, err := i.createIntent(ctx, userHash, name, size, contentType)
	if err != nil {
		return nil, err
	}

	// Until completion succeeds, a failure leaves a reserved intent and
	// possibly a temporary object behind. Abort it so a failed attempt does not
	// hold quota until the janitor runs.
	committed := false
	defer func() {
		if !committed {
			i.abort(intent)
		}
	}()

	// The server chooses the transport: one presigned PUT for small files, or
	// an S3 multipart session above its configured threshold.
	switch intent.Mode {
	case interimModeMultipart:
		if err := i.uploadMultipart(ctx, intent, reader, size); err != nil {
			return nil, fmt.Errorf("%s: multipart upload: %w", i.Name(), err)
		}
	default:
		if err := i.putObject(ctx, intent, reader, size, contentType); err != nil {
			return nil, fmt.Errorf("%s: upload: %w", i.Name(), err)
		}
	}

	file, err := i.complete(ctx, intent)
	if err != nil {
		return nil, fmt.Errorf("%s: complete: %w", i.Name(), err)
	}
	committed = true

	uploadedSize := file.Size
	if uploadedSize <= 0 {
		uploadedSize = size
	}

	return &plugin.UploadResult{
		Service:  i.Name(),
		Filename: filename,
		Size:     uploadedSize,
		URL:      file.PageURL,
	}, nil
}

// createIntent reserves quota and asks the API for a presigned PUT URL.
func (i *Interim) createIntent(
	ctx context.Context,
	userHash, name string,
	size int64,
	contentType string,
) (*interimIntent, error) {
	payload := map[string]any{
		"name":         name,
		"size":         size,
		"content_type": contentType,
	}

	status, body, err := interimRequest(ctx, http.MethodPost, i.apiBase()+"/uploads", userHash, "", payload)
	if err != nil {
		return nil, fmt.Errorf("%s: create intent: %w", i.Name(), err)
	}
	if status < 200 || status >= 300 {
		err := statusError(i.Name(), status, body)
		if status == http.StatusUnauthorized && userHash == "" {
			return nil, fmt.Errorf("%w (this instance requires an approved user hash; set API_TOKEN with `manyup config set %s <hash>`)",
				err, i.Name())
		}
		return nil, err
	}

	var intent interimIntent
	if err := json.Unmarshal(body, &intent); err != nil {
		return nil, fmt.Errorf("%s: parsing intent: %w (body: %s)", i.Name(), err, describeBody(body))
	}
	if intent.UploadToken == "" {
		return nil, fmt.Errorf("%s: intent has no upload token: %s", i.Name(), describeBody(body))
	}
	if intent.Mode == "" {
		// Older responses predate the mode field and always carried a single
		// PUT URL.
		intent.Mode = interimModeSingle
	}
	if intent.CompleteURL == "" && intent.UploadID != "" {
		intent.CompleteURL = i.apiBase() + "/uploads/" + url.PathEscape(intent.UploadID) + "/complete"
	}

	switch intent.Mode {
	case interimModeSingle:
		if intent.URL == "" {
			return nil, fmt.Errorf("%s: single-mode intent has no PUT URL: %s", i.Name(), describeBody(body))
		}
		if intent.Method == "" {
			intent.Method = http.MethodPut
		}
	case interimModeMultipart:
		if intent.PartsURL == "" && intent.UploadID != "" {
			intent.PartsURL = i.apiBase() + "/uploads/" + url.PathEscape(intent.UploadID) + "/parts"
		}
		if intent.PartSize <= 0 || intent.PartCount <= 0 || intent.PartsURL == "" {
			return nil, fmt.Errorf(
				"%s: incomplete multipart intent (part_size=%d, part_count=%d, parts_url=%q): %s",
				i.Name(), intent.PartSize, intent.PartCount, intent.PartsURL, describeBody(body))
		}
	default:
		return nil, fmt.Errorf("%s: unsupported upload mode %q", i.Name(), intent.Mode)
	}

	return &intent, nil
}

// putObject streams the file to the presigned URL.
func (i *Interim) putObject(
	ctx context.Context,
	intent *interimIntent,
	reader io.Reader,
	size int64,
	contentType string,
) error {
	req, err := http.NewRequestWithContext(ctx, intent.Method, intent.URL, reader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	// The signature covers the returned headers, so they must be sent verbatim.
	for key, value := range intent.Headers {
		req.Header.Set(key, value)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = size

	// The shared client caps a request at five minutes, which a multi-gigabyte
	// single PUT can exceed. Keep its transport but drop the overall timeout;
	// context cancellation still aborts the transfer.
	resp, err := interimStreamingClient().Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusError(i.Name()+": storage", resp.StatusCode, body)
	}
	return nil
}

// uploadMultipart streams a file as provider-sized parts. Part URLs are signed
// in batches (the API signs at most 100 at a time), each part is PUT straight to
// storage, and its ETag and exact size are reported before the next part.
//
// Parts are uploaded sequentially because the uploader hands the plugin one
// forward-only reader; the API's suggested browser parallelism is not used.
func (i *Interim) uploadMultipart(
	ctx context.Context,
	intent *interimIntent,
	reader io.Reader,
	size int64,
) error {
	expectedParts := (size + intent.PartSize - 1) / intent.PartSize
	if expectedParts != int64(intent.PartCount) {
		return fmt.Errorf("server declared %d parts of %d bytes for a %d-byte file (expected %d)",
			intent.PartCount, intent.PartSize, size, expectedParts)
	}

	for first := 1; first <= intent.PartCount; first += interimPartBatch {
		last := first + interimPartBatch - 1
		if last > intent.PartCount {
			last = intent.PartCount
		}

		numbers := make([]int, 0, last-first+1)
		for number := first; number <= last; number++ {
			numbers = append(numbers, number)
		}

		urls, err := i.requestPartURLs(ctx, intent, numbers)
		if err != nil {
			return err
		}

		byNumber := make(map[int]string, len(urls))
		for _, part := range urls {
			if part.PartNumber >= 1 && part.URL != "" {
				byNumber[part.PartNumber] = part.URL
			}
		}

		for number := first; number <= last; number++ {
			partURL, ok := byNumber[number]
			if !ok {
				return fmt.Errorf("no URL returned for part %d", number)
			}

			length := interimPartLength(size, intent.PartSize, number, intent.PartCount)
			if length <= 0 || length > intent.PartSize {
				return fmt.Errorf("part %d has invalid length %d (part_size %d)",
					number, length, intent.PartSize)
			}

			etag, err := i.putPart(ctx, partURL, reader, length, number)
			if err != nil {
				return err
			}
			if err := i.reportPart(ctx, intent, number, etag, length); err != nil {
				return err
			}
		}
	}

	return nil
}

// requestPartURLs asks the API for presigned URLs for one batch of parts.
func (i *Interim) requestPartURLs(
	ctx context.Context,
	intent *interimIntent,
	numbers []int,
) ([]interimPartURL, error) {
	status, body, err := interimRequest(ctx, http.MethodPost, intent.PartsURL, "", intent.UploadToken,
		map[string]any{"part_numbers": numbers})
	if err != nil {
		return nil, fmt.Errorf("requesting part URLs: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, statusError(i.Name()+": part URLs", status, body)
	}

	var response interimPartsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing part URLs: %w (body: %s)", err, describeBody(body))
	}
	if len(response.Parts) == 0 {
		return nil, fmt.Errorf("no part URLs in response: %s", describeBody(body))
	}
	return response.Parts, nil
}

// putPart streams one part to its presigned URL and returns the ETag that must
// be reported back to the API.
func (i *Interim) putPart(
	ctx context.Context,
	partURL string,
	reader io.Reader,
	length int64,
	partNumber int,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, io.LimitReader(reader, length))
	if err != nil {
		return "", fmt.Errorf("creating part %d request: %w", partNumber, err)
	}
	req.ContentLength = length

	// The part contract documents no request headers: the URL and the raw bytes
	// are all the storage provider needs.
	resp, err := interimStreamingClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("part %d upload failed: %w", partNumber, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", statusError(fmt.Sprintf("%s: part %d", i.Name(), partNumber), resp.StatusCode, body)
	}

	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" {
		return "", fmt.Errorf("part %d: no ETag in response", partNumber)
	}
	return etag, nil
}

// reportPart records a part's ETag and byte size with the API.
func (i *Interim) reportPart(
	ctx context.Context,
	intent *interimIntent,
	partNumber int,
	etag string,
	size int64,
) error {
	reportURL := fmt.Sprintf("%s/%d", strings.TrimSuffix(intent.PartsURL, "/"), partNumber)

	status, body, err := interimRequest(ctx, http.MethodPut, reportURL, "", intent.UploadToken,
		map[string]any{"etag": etag, "size": size})
	if err != nil {
		return fmt.Errorf("reporting part %d: %w", partNumber, err)
	}
	if status < 200 || status >= 300 {
		return statusError(fmt.Sprintf("%s: report part %d", i.Name(), partNumber), status, body)
	}
	return nil
}

// interimPartLength returns the byte length of one part. Only the final part may
// be shorter than part_size.
func interimPartLength(size, partSize int64, partNumber, partCount int) int64 {
	if partNumber < partCount {
		return partSize
	}
	return size - int64(partCount-1)*partSize
}

// interimStreamingClient returns the shared client without its overall
// five-minute timeout: a single PUT or an individual part can legitimately take
// longer. Context cancellation still aborts the transfer.
func interimStreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}

// complete commits the uploaded object. Completion is idempotent, so states
// that clear on their own are retried with a short backoff:
//
//   - 409 upload_missing: the single PUT has not become visible to the API yet.
//   - 409 upload_parts_incomplete: multipart part reports have not propagated.
//   - 409 upload_busy: another completion or abort holds the intent right now.
//   - 429 completion_busy: the instance's concurrent-completion limit was hit.
//
// Every other status is reported immediately.
func (i *Interim) complete(ctx context.Context, intent *interimIntent) (*interimFile, error) {
	completeURL := intent.CompleteURL
	if completeURL == "" {
		return nil, fmt.Errorf("no completion URL in the intent response")
	}

	var lastErr error
	for attempt := 1; attempt <= interimCompleteAttempts; attempt++ {
		status, body, err := interimRequest(ctx, http.MethodPost, completeURL, "", intent.UploadToken, nil)
		if err != nil {
			return nil, err
		}

		retryable := (status == http.StatusConflict && interimRetryableConflict(body)) ||
			(status == http.StatusTooManyRequests && interimRetryableRateLimit(body))
		if retryable {
			lastErr = statusError(i.Name(), status, body)
			if attempt == interimCompleteAttempts {
				break
			}
			if !interimSleep(ctx, interimCompleteRetryDelay<<(attempt-1)) {
				return nil, ctx.Err()
			}
			continue
		}

		if status < 200 || status >= 300 {
			return nil, statusError(i.Name(), status, body)
		}

		var completed interimCompleted
		if err := json.Unmarshal(body, &completed); err != nil {
			return nil, fmt.Errorf("parsing completion response: %w (body: %s)", err, describeBody(body))
		}
		if completed.File == nil {
			return nil, fmt.Errorf("no file in completion response: %s", describeBody(body))
		}
		if completed.File.PageURL == "" {
			return nil, fmt.Errorf("no page_url in completion response: %s", describeBody(body))
		}
		return completed.File, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("%s: completion did not succeed", i.Name())
	}
	return nil, lastErr
}

// abort discards a pending intent. Cleanup is best effort: the original upload
// failure is what the caller reports, so nothing here is surfaced. It runs on
// its own timeout because the parent context may already be cancelled.
func (i *Interim) abort(intent *interimIntent) {
	if intent.AbortURL == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), interimAbortTimeout)
	defer cancel()

	// A committed upload answers 409 here; both outcomes are ignored.
	_, _, _ = interimRequest(ctx, http.MethodDelete, intent.AbortURL, "", intent.UploadToken, nil)
}

// VerifyCredentials accepts either a user hash or, on an instance with public
// uploads enabled, no credential at all.
//
// With a hash it calls GET /keys/me and accepts an unapproved hash only when the
// instance allows public uploads. Without one it consults GET /status, because
// a restricted instance could never create an anonymous upload intent.
func (i *Interim) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	userHash := strings.TrimSpace(creds["API_TOKEN"])
	if userHash == "" {
		instance, err := i.fetchStatus(ctx)
		if err != nil {
			return fmt.Errorf("%s: could not check instance: %w", i.Name(), err)
		}
		if !instance.PublicUploads {
			return fmt.Errorf("%s: %w: this instance requires an approved user hash (anonymous uploads are disabled)",
				i.Name(), plugin.ErrCredentialRejected)
		}
		if !instance.accepting() {
			return fmt.Errorf("%s: instance is not accepting uploads right now", i.Name())
		}
		return nil
	}

	status, body, err := interimRequest(ctx, http.MethodGet, i.apiBase()+"/keys/me", userHash, "", nil)
	if err != nil {
		return fmt.Errorf("%s: could not verify: %w", i.Name(), err)
	}
	if outcome := authCheckOutcome(i.Name(), status, body); outcome != nil {
		return outcome
	}

	var info interimKeyInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("%s: could not verify: parsing response: %w", i.Name(), err)
	}
	if info.Approved {
		return nil
	}

	instance, err := i.fetchStatus(ctx)
	if err != nil {
		return fmt.Errorf("%s: could not verify approval: %w", i.Name(), err)
	}
	if !instance.PublicUploads {
		return fmt.Errorf("%s: %w: user hash is not approved for uploads (ask the admin to approve it)",
			i.Name(), plugin.ErrCredentialRejected)
	}
	if !instance.accepting() {
		return fmt.Errorf("%s: instance is not accepting uploads right now", i.Name())
	}
	return nil
}

// fetchStatus reads GET /status, which is public on every interim instance.
func (i *Interim) fetchStatus(ctx context.Context) (interimStatus, error) {
	status, body, err := interimRequest(ctx, http.MethodGet, i.apiBase()+"/status", "", "", nil)
	if err != nil {
		return interimStatus{}, err
	}
	if status < 200 || status >= 300 {
		return interimStatus{}, fmt.Errorf("status %d: %s", status, describeBody(body))
	}
	var instance interimStatus
	if err := json.Unmarshal(body, &instance); err != nil {
		return interimStatus{}, fmt.Errorf("parsing status: %w", err)
	}
	return instance, nil
}

// ── Helpers ─────────────────────────────────────────────────────────

// interimRequest performs one API call. Authentication is optional so the same
// helper serves the credential check, intent creation, completion, and abort.
func interimRequest(
	ctx context.Context,
	method, endpoint, userHash, uploadToken string,
	payload any,
) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if userHash != "" {
		req.Header.Set("Authorization", "Bearer "+userHash)
	}
	if uploadToken != "" {
		req.Header.Set("X-Upload-Token", uploadToken)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// interimErrorCode extracts the API's error code from an error envelope. It
// returns "" for a body that is not the expected envelope, which callers treat
// as "unknown, retry if the status is transient".
func interimErrorCode(body []byte) string {
	var envelope interimError
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Error.Code
}

// interimRetryableConflict reports whether a 409 completion body describes a
// state that can clear on its own: the object not being visible yet, part
// reports not having propagated, or another completion/abort briefly holding the
// intent. An unrecognised body is retried too, because 409 has historically been
// used only for eventual consistency here.
func interimRetryableConflict(body []byte) bool {
	switch interimErrorCode(body) {
	case "", "upload_missing", "upload_parts_incomplete", "upload_busy":
		return true
	}
	return false
}

// interimRetryableRateLimit reports whether a 429 should be retried in place.
// Completion is idempotent and the same upload token stays valid, so retrying
// the finalization step is much cheaper than restarting the whole upload after
// either the per-intent rate limit or the concurrent-completion limiter.
func interimRetryableRateLimit(body []byte) bool {
	switch interimErrorCode(body) {
	case "", "completion_busy", "rate_limited":
		return true
	}
	return false
}

// interimSleep waits for d, returning false if the context is cancelled first.
func interimSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ── Instances ───────────────────────────────────────────────────────

// InterimInstanceServiceName maps an instance label to its service name, for
// example "friends" to "interim@friends".
func InterimInstanceServiceName(name string) string {
	return interimInstancePrefix + name
}

// IsInterimService reports whether a service name belongs to an interim
// instance, either the default "interim" or an "interim@<label>" entry.
func IsInterimService(service string) bool {
	if service == InterimServiceName {
		return true
	}
	_, ok := InterimInstanceName(service)
	return ok
}

// InterimInstanceName maps a service name back to an instance label. The
// default instance ("interim") is not an additional instance and reports false.
func InterimInstanceName(service string) (string, bool) {
	if !strings.HasPrefix(service, interimInstancePrefix) {
		return "", false
	}
	name := strings.TrimPrefix(service, interimInstancePrefix)
	if name == "" {
		return "", false
	}
	return name, true
}

// NormalizeInterimURL validates an instance origin and returns both the stored
// origin (no trailing slash or /api/v1 suffix) and its API root. interim serves
// every instance from an origin, so a path prefix is rejected.
func NormalizeInterimURL(raw string) (origin, apiBase string, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", fmt.Errorf("the instance URL is empty")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", fmt.Errorf("invalid instance URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", fmt.Errorf("instance URL must use http or https, not %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("instance URL has no host")
	}
	if parsed.User != nil {
		return "", "", fmt.Errorf("instance URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("instance URL must not contain a query or fragment")
	}

	path := strings.TrimSuffix(parsed.Path, "/")
	if strings.HasSuffix(path, interimAPIPath) {
		path = strings.TrimSuffix(path, interimAPIPath)
	}
	path = strings.TrimSuffix(path, "/")
	if path != "" {
		return "", "", fmt.Errorf("instance URL must be an origin such as https://files.example.com (got path %q)", parsed.Path)
	}

	origin = scheme + "://" + strings.ToLower(parsed.Host)
	return origin, origin + interimAPIPath, nil
}

// SuggestInterimInstanceName derives a stable, readable label from an origin,
// using the hostname: https://files.example.com -> "files.example.com".
func SuggestInterimInstanceName(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return "instance"
	}

	name := strings.ToLower(parsed.Hostname())
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	label := strings.Trim(b.String(), ".-_")
	if label == "" {
		label = "instance"
	}
	if len(label) > interimInstanceNameLimit {
		label = strings.TrimRight(label[:interimInstanceNameLimit], ".-_")
	}
	if label == "" {
		label = "instance"
	}
	return label
}

// ValidateInterimInstanceName checks a label before it becomes the service name
// interim@<label>. Labels are lowercase so service names stay predictable in
// config files, shells, and completion.
func ValidateInterimInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("the instance name is empty")
	}
	if len(name) > interimInstanceNameLimit {
		return fmt.Errorf("the instance name is too long (%d characters, maximum %d)", len(name), interimInstanceNameLimit)
	}
	for i, r := range name {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !allowed {
			return fmt.Errorf("the instance name %q may only contain lowercase letters, digits, '-', '_' and '.'", name)
		}
		if (i == 0 || i == len(name)-1) && !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return fmt.Errorf("the instance name %q must start and end with a letter or digit", name)
		}
	}
	return nil
}

// normalizeInterimBaseURL trims an origin and removes a trailing slash or
// /api/v1 suffix. It is deliberately tolerant because it also runs on values
// loaded from a hand-edited config file.
func normalizeInterimBaseURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return strings.TrimSuffix(value, "/")
	}

	path := strings.TrimSuffix(parsed.Path, "/")
	if strings.HasSuffix(path, interimAPIPath) {
		path = strings.TrimSuffix(path, interimAPIPath)
	}
	path = strings.TrimSuffix(path, "/")
	return strings.TrimSuffix(strings.ToLower(parsed.Scheme)+"://"+strings.ToLower(parsed.Host)+path, "/")
}

// ConfiguredInterimBaseURL returns the origin stored for an interim service, or
// the instance default when it has no override. It returns "" for an additional
// instance that has neither a BASE_URL nor a hostname-shaped label.
func ConfiguredInterimBaseURL(cfg *config.AppConfig, service string) string {
	if cfg != nil {
		if sc, ok := cfg.Service(service); ok && sc.Options != nil {
			if value := strings.TrimSpace(sc.Options["BASE_URL"]); value != "" {
				if origin, valid := validInterimOrigin(value); valid {
					return origin
				}
				if service != InterimServiceName {
					return ""
				}
			}
		}
	}
	if service == InterimServiceName {
		return DefaultInterimBaseURL
	}
	name, ok := InterimInstanceName(service)
	if !ok {
		return ""
	}
	if strings.Contains(name, ".") || name == "localhost" {
		return "https://" + name
	}
	return ""
}

// validInterimOrigin normalizes and validates a stored origin. Unlike
// normalizeInterimBaseURL it rejects malformed values instead of returning them
// unchanged, so a hand-edited config cannot point a plugin at garbage.
func validInterimOrigin(raw string) (string, bool) {
	normalized := normalizeInterimBaseURL(raw)
	if normalized == "" {
		return "", false
	}
	origin, _, err := NormalizeInterimURL(normalized)
	if err != nil {
		return "", false
	}
	return origin, true
}

// interimInstanceBaseURL returns the origin configured for an additional
// instance. A hand-edited entry without BASE_URL is only usable when its label
// looks like a hostname, so a guessed host is never contacted accidentally.
func interimInstanceBaseURL(service string, sc *config.ServiceConfig) (string, bool) {
	if sc != nil && sc.Options != nil {
		if value := strings.TrimSpace(sc.Options["BASE_URL"]); value != "" {
			if origin, valid := validInterimOrigin(value); valid {
				return origin, true
			}
			return "", false
		}
	}

	name, ok := InterimInstanceName(service)
	if !ok {
		return "", false
	}
	if strings.Contains(name, ".") || name == "localhost" {
		return "https://" + name, true
	}
	return "", false
}

// interimContentType guesses a content type from the file extension. The
// server records it on the file record and signs it into the PUT request.
func interimContentType(name string) string {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

// interimFileName trims a name to the API's 180-character limit, keeping the
// extension so the hosted file stays recognisable.
func interimFileName(filename string) string {
	name := filepath.Base(filename)
	runes := []rune(name)
	if len(runes) <= interimNameLimit {
		return name
	}

	ext := []rune(filepath.Ext(name))
	if len(ext) >= interimNameLimit {
		return string(runes[:interimNameLimit])
	}
	return string(runes[:interimNameLimit-len(ext)]) + string(ext)
}
