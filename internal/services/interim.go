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
	// interimPartBatch is the largest number of part URLs the API signs in one
	// request.
	interimPartBatch = 100

	// interimNameLimit is the maximum length, in characters, of the file name
	// an upload intent accepts.
	interimNameLimit = 180

	// interimCompleteAttempts bounds how often completion is retried while the
	// uploaded object has not become visible to the API yet.
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

// Interim uploads files to interim.drifty.win, a temporary file host built
// around direct-to-S3 uploads.
//
// An upload intent reserves quota and either returns a presigned PUT URL for a
// single request or, for larger files, opens an S3 multipart session whose parts
// are uploaded and reported individually. The bytes go straight from this
// machine to storage, and a completion call commits the file record. The plugin
// always sends a user hash, which an instance with public uploads disabled
// requires to be approved.
//
// See api-refs/interim.md for the full contract.
type Interim struct{}

func RegisterInterim(r *plugin.Registry) {
	r.Register(&Interim{})
}

func (i *Interim) Name() string        { return "interim" }
func (i *Interim) DisplayName() string { return "interim" }
func (i *Interim) Description() string {
	return "interim temporary file hosting (user hash, single or multipart S3 upload)"
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
type interimStatus struct {
	PublicUploads bool `json:"public_uploads"`
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
		return nil, fmt.Errorf("interim: API_TOKEN required (a user hash)")
	}
	if size <= 0 {
		// The API accepts only objects with at least one byte, so an empty
		// file can never be completed.
		return nil, fmt.Errorf("interim: cannot upload an empty file")
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
			return nil, fmt.Errorf("interim: multipart upload: %w", err)
		}
	default:
		if err := i.putObject(ctx, intent, reader, size, contentType); err != nil {
			return nil, fmt.Errorf("interim: upload: %w", err)
		}
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
	if intent.UploadToken == "" {
		return nil, fmt.Errorf("interim: intent has no upload token: %s", describeBody(body))
	}
	if intent.Mode == "" {
		// Older responses predate the mode field and always carried a single
		// PUT URL.
		intent.Mode = interimModeSingle
	}
	if intent.CompleteURL == "" && intent.UploadID != "" {
		intent.CompleteURL = interimBaseURL + "/uploads/" + url.PathEscape(intent.UploadID) + "/complete"
	}

	switch intent.Mode {
	case interimModeSingle:
		if intent.URL == "" {
			return nil, fmt.Errorf("interim: single-mode intent has no PUT URL: %s", describeBody(body))
		}
		if intent.Method == "" {
			intent.Method = http.MethodPut
		}
	case interimModeMultipart:
		if intent.PartsURL == "" && intent.UploadID != "" {
			intent.PartsURL = interimBaseURL + "/uploads/" + url.PathEscape(intent.UploadID) + "/parts"
		}
		if intent.PartSize <= 0 || intent.PartCount <= 0 || intent.PartsURL == "" {
			return nil, fmt.Errorf(
				"interim: incomplete multipart intent (part_size=%d, part_count=%d, parts_url=%q): %s",
				intent.PartSize, intent.PartCount, intent.PartsURL, describeBody(body))
		}
	default:
		return nil, fmt.Errorf("interim: unsupported upload mode %q", intent.Mode)
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
		return statusError("interim: storage", resp.StatusCode, body)
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
		return nil, statusError("interim: part URLs", status, body)
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
		return "", statusError(fmt.Sprintf("interim: part %d", partNumber), resp.StatusCode, body)
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
		return statusError(fmt.Sprintf("interim: report part %d", partNumber), status, body)
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

// complete commits the uploaded object. The API answers 409 upload_missing
// while storage has not caught up with the upload, and 409 upload_parts_incomplete
// while multipart part reports have not propagated; both clear on their own, so
// they are retried with a short backoff. Every other status is reported
// immediately.
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

		if status == http.StatusConflict && interimRetryableConflict(body) {
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

// VerifyCredentials checks the user hash with GET /keys/me. An unapproved hash
// is accepted only when the instance has public uploads enabled; on a restricted
// instance it could never create an upload intent.
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
	if info.Approved {
		return nil
	}

	status, body, err = interimRequest(ctx, http.MethodGet, interimBaseURL+"/status", "", "", nil)
	if err != nil {
		return fmt.Errorf("interim: could not verify approval: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("interim: could not verify approval: %s", describeBody(body))
	}
	var instance interimStatus
	if err := json.Unmarshal(body, &instance); err != nil {
		return fmt.Errorf("interim: could not verify approval: parsing status: %w", err)
	}
	if !instance.PublicUploads {
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

// interimRetryableConflict reports whether a 409 completion body describes a
// state that can clear on its own: the object not being visible yet, or part
// reports not having propagated. An unrecognised body is retried too, because
// 409 has historically been used only for eventual consistency here.
func interimRetryableConflict(body []byte) bool {
	var envelope interimError
	if err := json.Unmarshal(body, &envelope); err != nil {
		return true
	}
	switch envelope.Error.Code {
	case "", "upload_missing", "upload_parts_incomplete":
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
