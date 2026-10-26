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
	"strings"
	"time"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// interimBaseURL is a variable so tests can point the plugin at a local server.
var interimBaseURL = "https://interim.drifty.win/api/v1"

const (
	// interimSinglePutLimit is the largest object an interim instance accepts
	// in one PUT. The API caps a single-request upload at 5 GiB.
	interimSinglePutLimit int64 = 5 << 30

	// interimNameLimit is the maximum length, in characters, of the file name
	// an upload intent accepts.
	interimNameLimit = 180

	// interimCompleteAttempts bounds how often completion is retried while the
	// uploaded object has not become visible to the API yet.
	interimCompleteAttempts = 5

	// interimAbortTimeout bounds the best-effort cleanup of a failed upload.
	interimAbortTimeout = 10 * time.Second
)

// interimCompleteRetryDelay is the base pause between completion attempts. It
// is a variable so tests can shrink the wait.
var interimCompleteRetryDelay = 500 * time.Millisecond

// Interim uploads files to interim.drifty.win, a temporary file host built
// around direct-to-S3 uploads.
//
// An upload intent reserves quota and returns a presigned PUT URL; the bytes go
// straight from this machine to storage, and a completion call commits the file
// record. The plugin always sends an admin-approved user hash, which an instance
// with public uploads disabled requires.
//
// See api-refs/interim.md for the full contract.
type Interim struct{}

func RegisterInterim(r *plugin.Registry) {
	r.Register(&Interim{})
}

func (i *Interim) Name() string        { return "interim" }
func (i *Interim) DisplayName() string { return "interim" }
func (i *Interim) Description() string {
	return "interim temporary file hosting (approved user hash, presigned S3 PUT)"
}

// RequiredCredentials reports the user hash as mandatory: the plugin always
// authenticates, and an instance with public uploads disabled refuses anonymous
// intents with 401 uploads_restricted.
func (i *Interim) RequiredCredentials() []string { return []string{"API_TOKEN"} }

func (i *Interim) SupportsLargeUpload() bool { return true }

// ── Response types ──────────────────────────────────────────────────

// interimIntent is the response of POST /uploads. Only the fields the plugin
// acts on are decoded.
type interimIntent struct {
	UploadID    string            `json:"upload_id"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	UploadToken string            `json:"upload_token"`
	CompleteURL string            `json:"complete_url"`
	AbortURL    string            `json:"abort_url"`
	DeleteToken string            `json:"delete_token"`
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
	if userHash == "" {
		return nil, fmt.Errorf("interim: API_TOKEN required (an admin-approved user hash)")
	}
	if size <= 0 {
		// The API accepts only objects with at least one byte, so an empty
		// file can never be completed.
		return nil, fmt.Errorf("interim: cannot upload an empty file")
	}
	if size > interimSinglePutLimit {
		return nil, fmt.Errorf("interim: file is %d bytes, above the %d-byte single-PUT limit",
			size, interimSinglePutLimit)
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

	if err := i.putObject(ctx, intent, reader, size, contentType); err != nil {
		return nil, fmt.Errorf("interim: upload: %w", err)
	}

	file, err := i.complete(ctx, intent)
	if err != nil {
		return nil, fmt.Errorf("interim: complete: %w", err)
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

	status, body, err := interimRequest(ctx, http.MethodPost, interimBaseURL+"/uploads", userHash, "", payload)
	if err != nil {
		return nil, fmt.Errorf("interim: create intent: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, statusError("interim", status, body)
	}

	var intent interimIntent
	if err := json.Unmarshal(body, &intent); err != nil {
		return nil, fmt.Errorf("interim: parsing intent: %w (body: %s)", err, describeBody(body))
	}
	if intent.URL == "" || intent.UploadToken == "" {
		return nil, fmt.Errorf("interim: incomplete intent response: %s", describeBody(body))
	}
	if intent.Method == "" {
		intent.Method = http.MethodPut
	}
	if intent.CompleteURL == "" && intent.UploadID != "" {
		intent.CompleteURL = interimBaseURL + "/uploads/" + url.PathEscape(intent.UploadID) + "/complete"
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
	client := *httpclient.Get()
	client.Timeout = 0

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusError("interim: storage", resp.StatusCode, body)
	}
	return nil
}

// complete commits the uploaded object. The API answers 409 upload_missing
// while storage has not caught up with the PUT, so that case is retried with a
// short backoff; every other status is reported immediately.
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

		if status == http.StatusConflict && interimUploadMissing(body) {
			lastErr = statusError("interim", status, body)
			if attempt == interimCompleteAttempts {
				break
			}
			if !interimSleep(ctx, interimCompleteRetryDelay<<(attempt-1)) {
				return nil, ctx.Err()
			}
			continue
		}

		if status < 200 || status >= 300 {
			return nil, statusError("interim", status, body)
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
		lastErr = fmt.Errorf("interim: completion did not succeed")
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

// VerifyCredentials checks the user hash with GET /keys/me. A valid but
// unapproved hash is rejected too: it could never create an upload intent while
// public uploads are disabled.
func (i *Interim) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	userHash := strings.TrimSpace(creds["API_TOKEN"])
	if userHash == "" {
		return fmt.Errorf("interim: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	status, body, err := interimRequest(ctx, http.MethodGet, interimBaseURL+"/keys/me", userHash, "", nil)
	if err != nil {
		return fmt.Errorf("interim: could not verify: %w", err)
	}
	if outcome := authCheckOutcome("interim", status, body); outcome != nil {
		return outcome
	}

	var info interimKeyInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("interim: could not verify: parsing response: %w", err)
	}
	if !info.Approved {
		return fmt.Errorf("interim: %w: user hash is not approved for uploads (ask the admin to approve it)",
			plugin.ErrCredentialRejected)
	}
	return nil
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

// interimUploadMissing reports whether a 409 completion body means the object
// has not reached storage yet. An unrecognised body is retried too: 409 is
// documented on this endpoint only for that case.
func interimUploadMissing(body []byte) bool {
	var envelope interimError
	if err := json.Unmarshal(body, &envelope); err != nil {
		return true
	}
	return envelope.Error.Code == "" || envelope.Error.Code == "upload_missing"
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
