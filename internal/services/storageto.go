package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// stBaseURL is a variable so tests can point the client at a local server.
var stBaseURL = "https://storage.to/api"

// StorageTo uploads files to storage.to, a direct-to-storage host.
//
// The flow is always init → PUT → confirm. For files above the service's
// multipart threshold the init response asks for a chunked upload instead, and
// the bytes go up as parts before completion.
//
// Optional account token: `API_TOKEN`, sent as a bearer token. Anonymous
// uploads work but expire after about three days.
type StorageTo struct{}

func RegisterStorageTo(r *plugin.Registry) {
	r.Register(&StorageTo{})
}

func (s *StorageTo) Name() string        { return "storageto" }
func (s *StorageTo) DisplayName() string { return "storage.to" }
func (s *StorageTo) Description() string {
	return "storage.to direct upload (anonymous or account, multipart)"
}

// RequiredCredentials returns nil: anonymous uploads are supported.
func (s *StorageTo) RequiredCredentials() []string { return nil }
func (s *StorageTo) SupportsLargeUpload() bool     { return true }

// ── Wire types ──────────────────────────────────────────────────────

// stInitResponse covers both init shapes; Type is "single" or "multipart".
type stInitResponse struct {
	Success   bool   `json:"success"`
	Type      string `json:"type"`
	UploadURL string `json:"upload_url"`
	// Headers are header values the signature expects on the PUT. Values arrive
	// as either a string or an array of strings.
	Headers     map[string]any    `json:"headers"`
	R2Key       string            `json:"r2_key"`
	UploadID    string            `json:"upload_id"`
	PartSize    int64             `json:"part_size"`
	TotalParts  int               `json:"total_parts"`
	InitialURLs map[string]string `json:"initial_urls"`
	OwnerToken  string            `json:"owner_token"`
}

// stPartsResponse is /upload/parts. The live service returns `urls` as an
// object keyed by part number; the docs describe a `part_urls` array, so both
// are accepted.
type stPartsResponse struct {
	Success  bool              `json:"success"`
	URLs     map[string]string `json:"urls"`
	PartURLs []struct {
		PartNumber int    `json:"partNumber"`
		URL        string `json:"url"`
	} `json:"part_urls"`
}

// stFile is the file record returned by /upload/confirm.
type stFile struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

type stConfirmResponse struct {
	Success    bool   `json:"success"`
	File       stFile `json:"file"`
	OwnerToken string `json:"owner_token"`
}

type stPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}

// ── Upload ──────────────────────────────────────────────────────────

func (s *StorageTo) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	if size < 1 {
		return nil, fmt.Errorf("storageto: file size must be known")
	}

	token := strings.TrimSpace(creds["API_TOKEN"])
	contentType := stContentType(filename)

	init, err := s.initUpload(ctx, filename, contentType, size, token)
	if err != nil {
		return nil, fmt.Errorf("storageto: init: %w", err)
	}

	// The service decides the shape; follow whatever it asked for.
	if init.Type == "multipart" {
		if err := s.uploadMultipart(ctx, init, reader, size, token); err != nil {
			return nil, err
		}
	} else if err := s.uploadSingle(ctx, init, reader, size); err != nil {
		return nil, err
	}

	file, err := s.confirm(ctx, filename, contentType, size, init.R2Key, token, cfg)
	if err != nil {
		return nil, fmt.Errorf("storageto: confirm: %w", err)
	}

	return &plugin.UploadResult{
		Service:  s.Name(),
		Filename: filename,
		Size:     file.Size,
		URL:      file.URL,
	}, nil
}

func (s *StorageTo) initUpload(
	ctx context.Context,
	filename, contentType string,
	size int64,
	token string,
) (*stInitResponse, error) {
	payload := map[string]any{
		"filename":     filepath.Base(filename),
		"content_type": contentType,
		"size":         size,
	}

	status, body, err := stJSON(ctx, http.MethodPost, "/upload/init", payload, token, "")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError("storageto", status, body)
	}

	var init stInitResponse
	if err := json.Unmarshal(body, &init); err != nil {
		return nil, fmt.Errorf("parsing response: %w (body: %s)", err, describeBody(body))
	}

	if init.Type == "multipart" {
		if init.UploadID == "" || init.PartSize <= 0 {
			return nil, fmt.Errorf("incomplete multipart response: %s", describeBody(body))
		}
		return &init, nil
	}

	if init.UploadURL == "" || init.R2Key == "" {
		return nil, fmt.Errorf("incomplete upload response: %s", describeBody(body))
	}
	return &init, nil
}

// uploadSingle streams the whole file to the presigned URL.
func (s *StorageTo) uploadSingle(ctx context.Context, init *stInitResponse, reader io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, init.UploadURL, reader)
	if err != nil {
		return fmt.Errorf("storageto: creating upload request: %w", err)
	}
	if size > 0 {
		req.ContentLength = size
	}
	applySTHeaders(req, init.Headers)

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return fmt.Errorf("storageto: upload failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusError("storageto: storage", resp.StatusCode, body)
	}
	return nil
}

// uploadMultipart sends each part in order, then completes the upload.
func (s *StorageTo) uploadMultipart(
	ctx context.Context,
	init *stInitResponse,
	reader io.Reader,
	size int64,
	token string,
) error {
	owner := init.OwnerToken

	urls := make(map[int]string, len(init.InitialURLs))
	for key, partURL := range init.InitialURLs {
		n, err := strconv.Atoi(key)
		if err == nil && n > 0 && partURL != "" {
			urls[n] = partURL
		}
	}

	total := init.TotalParts
	if total <= 0 {
		total = int((size + init.PartSize - 1) / init.PartSize)
	}

	// Ask for URLs the init call did not include.
	missing := make([]int, 0, total)
	for n := 1; n <= total; n++ {
		if _, ok := urls[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		extra, err := s.partURLs(ctx, init.UploadID, missing, token, owner)
		if err != nil {
			s.abortUpload(init.UploadID, token, owner)
			return fmt.Errorf("storageto: part URLs: %w", err)
		}
		for n, partURL := range extra {
			urls[n] = partURL
		}
	}

	parts := make([]stPart, 0, total)
	for n := 1; n <= total; n++ {
		partURL, ok := urls[n]
		if !ok {
			s.abortUpload(init.UploadID, token, owner)
			return fmt.Errorf("storageto: no URL for part %d", n)
		}

		length := chunkSize(size, init.PartSize, n)
		etag, err := s.putPart(ctx, partURL, reader, length, n)
		if err != nil {
			s.abortUpload(init.UploadID, token, owner)
			return err
		}
		parts = append(parts, stPart{PartNumber: n, ETag: etag})
	}

	if err := s.completeMultipart(ctx, init.UploadID, parts, token, owner); err != nil {
		s.abortUpload(init.UploadID, token, owner)
		return fmt.Errorf("storageto: complete multipart: %w", err)
	}
	return nil
}

// partURLs signs part numbers that init did not already cover.
func (s *StorageTo) partURLs(
	ctx context.Context,
	uploadID string,
	partNumbers []int,
	token, owner string,
) (map[int]string, error) {
	payload := map[string]any{
		"upload_id":    uploadID,
		"part_numbers": partNumbers,
	}

	status, body, err := stJSON(ctx, http.MethodPost, "/upload/parts", payload, token, owner)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError("storageto", status, body)
	}

	var resp stPartsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing part URLs: %w (body: %s)", err, describeBody(body))
	}

	out := make(map[int]string, len(resp.URLs)+len(resp.PartURLs))
	for key, partURL := range resp.URLs {
		if n, convErr := strconv.Atoi(key); convErr == nil && partURL != "" {
			out[n] = partURL
		}
	}
	for _, entry := range resp.PartURLs {
		if entry.PartNumber > 0 && entry.URL != "" {
			out[entry.PartNumber] = entry.URL
		}
	}
	return out, nil
}

func (s *StorageTo) putPart(
	ctx context.Context,
	partURL string,
	reader io.Reader,
	size int64,
	partNumber int,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, io.LimitReader(reader, size))
	if err != nil {
		return "", fmt.Errorf("storageto: part %d: creating request: %w", partNumber, err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return "", fmt.Errorf("storageto: part %d: upload failed: %w", partNumber, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", statusError("storageto: part "+strconv.Itoa(partNumber), resp.StatusCode, body)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("storageto: part %d: no ETag in response", partNumber)
	}
	return etag, nil
}

func (s *StorageTo) completeMultipart(
	ctx context.Context,
	uploadID string,
	parts []stPart,
	token, owner string,
) error {
	payload := map[string]any{
		"upload_id": uploadID,
		"parts":     parts,
	}

	status, body, err := stJSON(ctx, http.MethodPost, "/upload/complete-multipart", payload, token, owner)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return statusError("storageto", status, body)
	}
	return nil
}

// abortUpload discards a partially uploaded multipart session. Best effort: the
// caller is already reporting a failure.
func (s *StorageTo) abortUpload(uploadID, token, owner string) {
	if uploadID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()

	payload := map[string]any{"upload_id": uploadID}
	_, _, _ = stJSON(ctx, http.MethodPost, "/upload/abort", payload, token, owner)
}

// confirm turns a finished upload into a file record and returns it.
func (s *StorageTo) confirm(
	ctx context.Context,
	filename, contentType string,
	size int64,
	r2Key, token string,
	cfg plugin.Config,
) (*stFile, error) {
	payload := map[string]any{
		"filename":     filepath.Base(filename),
		"size":         size,
		"content_type": contentType,
		"r2_key":       r2Key,
	}
	if collectionID := optionValue(cfg, "collectionId"); collectionID != "" {
		payload["collection_id"] = collectionID
	}

	status, body, err := stJSON(ctx, http.MethodPost, "/upload/confirm", payload, token, "")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError("storageto", status, body)
	}

	var resp stConfirmResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w (body: %s)", err, describeBody(body))
	}
	if resp.File.URL == "" {
		return nil, fmt.Errorf("no file URL in response: %s", describeBody(body))
	}
	return &resp.File, nil
}

// ── Helpers ─────────────────────────────────────────────────────────

// stJSON sends a JSON request to the storage.to API. token is an optional
// bearer credential; owner is an optional X-Owner-Token proof.
func stJSON(
	ctx context.Context,
	method, path string,
	payload any,
	token, owner string,
) (int, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, stBaseURL+path, strings.NewReader(string(encoded)))
	if err != nil {
		return 0, nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if owner != "" {
		req.Header.Set("X-Owner-Token", owner)
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, body, nil
}

// applySTHeaders copies the signed headers from an init response onto the PUT.
// Values may be strings or arrays; Host is set via req.Host so Go does not
// override it.
func applySTHeaders(req *http.Request, headers map[string]any) {
	for key, raw := range headers {
		values := stHeaderValues(raw)
		if len(values) == 0 {
			continue
		}
		if strings.EqualFold(key, "Host") {
			req.Host = values[0]
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
}

func stHeaderValues(raw any) []string {
	switch value := raw.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return value
	}
	return nil
}

// stContentType guesses the MIME type from the extension.
func stContentType(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}

// VerifyCredentials resolves the bearer token to a user. storage.to answers
// 401 {"message":"Unauthenticated."} for a missing or invalid token.
func (s *StorageTo) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	token := strings.TrimSpace(creds["API_TOKEN"])
	if token == "" {
		return fmt.Errorf("storageto: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	status, body, err := authGet(ctx, stBaseURL+"/user", "Authorization", "Bearer "+token)
	if err != nil {
		return fmt.Errorf("storageto: could not verify: %w", err)
	}
	return authCheckOutcome("storageto", status, body)
}
