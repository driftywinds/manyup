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
	"strconv"
	"strings"
	"time"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// mochaAPIBase and mochaShareBase are variables rather than constants so tests
// can point the plugin at a local httptest server.
var (
	mochaAPIBase   = "https://api.mocha.my"
	mochaShareBase = "https://mocha.my/share/"
)

const (
	// mochaPresignBatch is how many part URLs are signed per request. The
	// presigned URLs are valid for hours, so a window is plenty and it keeps
	// the request body small for very large files (up to 10,000 parts).
	mochaPresignBatch = 200

	// mochaDefaultPartSize is used only if the server omits both part sizes.
	mochaDefaultPartSize = 128 << 20

	// mochaRelayPartSize is the fallback chunk size for the deprecated relay.
	mochaRelayPartSize = 50 << 20

	// abortTimeout bounds the best-effort cleanup of a failed upload.
	abortTimeout = 10 * time.Second
)

// Mocha uploads files to mocha.my, a hosted file service with an API-key
// authenticated JSON API.
//
// Uploads are always done through the multipart endpoints. The single-request
// endpoint (POST /api/files) is rejected by many storage nodes and returned
// 503 on the account this plugin was written against, so it is not used.
//
// A fresh upload is private, so a share is created afterwards and its
// https://mocha.my/share/{token} link is what the user gets back.
//
// See api-refs/mocha.md for the full contract.
type Mocha struct{}

func RegisterMocha(r *plugin.Registry) {
	r.Register(&Mocha{})
}

func (m *Mocha) Name() string        { return "mocha" }
func (m *Mocha) DisplayName() string { return "Mocha" }
func (m *Mocha) Description() string {
	return "Mocha secure file hosting (API key, multipart upload)"
}

// RequiredCredentials reports the API key as mandatory: every Mocha endpoint is
// authenticated, so an upload without a key can only fail.
func (m *Mocha) RequiredCredentials() []string { return []string{"API_TOKEN"} }

func (m *Mocha) SupportsLargeUpload() bool { return true }

// mochaInitResponse is returned by POST /api/files/multipart/init.
type mochaInitResponse struct {
	UploadID              string `json:"uploadId"`
	Key                   string `json:"key"`
	NodeID                string `json:"nodeId"`
	OriginalName          string `json:"originalName"`
	Purpose               string `json:"purpose"`
	PartSizeBytes         int64  `json:"partSizeBytes"`
	DirectPartSizeBytes   int64  `json:"directPartSizeBytes"`
	PartUploadConcurrency int    `json:"partUploadConcurrency"`
	DirectPartUpload      bool   `json:"directPartUpload"`
}

// mochaPresignResponse is returned by POST /api/files/multipart/presigned.
type mochaPresignResponse struct {
	URLs []struct {
		PartNumber int    `json:"partNumber"`
		URL        string `json:"url"`
	} `json:"urls"`
}

// mochaCompletedPart is one entry of the "parts" array sent on completion.
type mochaCompletedPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}

// mochaCompleteResponse is returned by POST /api/files/multipart/complete.
type mochaCompleteResponse struct {
	File struct {
		ID           string `json:"id"`
		FileName     string `json:"file_name"`
		OriginalName string `json:"original_name"`
		Size         int64  `json:"size"`
		Status       string `json:"status"`
	} `json:"file"`
}

// mochaShareRequest is the body of POST /api/shares. ExpiresInHours is a
// pointer so an unset value is serialised as JSON null (share never expires),
// which is what we want: the file's own expiry then governs the link.
type mochaShareRequest struct {
	FileID         string `json:"fileId"`
	ExpiresInHours *int   `json:"expiresInHours"`
	MaxDownloads   *int   `json:"maxDownloads,omitempty"`
}

// mochaShareResponse is returned by POST /api/shares.
type mochaShareResponse struct {
	Share struct {
		Token string `json:"token"`
	} `json:"share"`
}

// mochaErrorResponse is the error envelope used across the API.
type mochaErrorResponse struct {
	Error string `json:"error"`
}

// Upload sends one file to Mocha and returns a public share link.
func (m *Mocha) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return nil, fmt.Errorf("mocha: API_TOKEN required")
	}
	if size <= 0 {
		// The multipart API needs the exact size up front to size the parts,
		// and manyup always knows it from the file stat.
		return nil, fmt.Errorf("mocha: file size must be known for multipart upload")
	}

	name := filepath.Base(filename)
	dest := mochaPath(cfg)

	init, err := m.initUpload(ctx, name, size, dest, apiKey)
	if err != nil {
		return nil, err
	}

	var parts []mochaCompletedPart
	if init.DirectPartUpload {
		parts, err = m.uploadPresigned(ctx, init, reader, size, dest, apiKey)
	} else {
		// Some nodes cannot hand out presigned part URLs and only support the
		// legacy relay endpoint.
		parts, err = m.uploadRelay(ctx, init, reader, size, dest, apiKey)
	}
	if err != nil {
		m.abortUpload(init, dest, apiKey)
		return nil, err
	}

	fileID, storedSize, err := m.completeUpload(ctx, init, name, size, parts, dest, apiKey)
	if err != nil {
		m.abortUpload(init, dest, apiKey)
		return nil, err
	}

	shareURL, err := m.createShare(ctx, fileID, apiKey, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w (file %s was uploaded but not shared)", err, fileID)
	}

	if storedSize <= 0 {
		storedSize = size
	}

	return &plugin.UploadResult{
		Service:  m.Name(),
		Filename: filename,
		Size:     storedSize,
		URL:      shareURL,
	}, nil
}

// VerifyCredentials checks an API key against the account endpoint.
func (m *Mocha) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return fmt.Errorf("mocha: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	status, body, err := authGet(ctx, mochaAPIBase+"/api/profile/summary", "Authorization", "Bearer "+apiKey)
	if err != nil {
		return fmt.Errorf("mocha: could not verify: %w", err)
	}
	return authCheckOutcome("mocha", status, body)
}

// initUpload opens a multipart session and reports how to split the file.
func (m *Mocha) initUpload(
	ctx context.Context,
	name string,
	size int64,
	dest string,
	apiKey string,
) (*mochaInitResponse, error) {
	payload := map[string]any{
		"originalName": name,
		"size":         size,
		"mimeType":     mochaMimeType(name),
		"path":         dest,
	}

	status, body, err := mochaJSON(ctx, http.MethodPost, mochaAPIBase+"/api/files/multipart/init", apiKey, payload)
	if err != nil {
		return nil, fmt.Errorf("mocha: init: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("mocha: init: status %d: %s", status, mochaDescribe(body))
	}

	var init mochaInitResponse
	if err := json.Unmarshal(body, &init); err != nil {
		return nil, fmt.Errorf("mocha: init: parsing response: %w (body: %s)", err, mochaDescribe(body))
	}
	if init.UploadID == "" || init.Key == "" || init.NodeID == "" {
		return nil, fmt.Errorf("mocha: init: incomplete session: %s", mochaDescribe(body))
	}
	return &init, nil
}

// uploadPresigned uploads the file in parts straight to the storage node.
// Parts are sent one at a time so the reader is never buffered.
func (m *Mocha) uploadPresigned(
	ctx context.Context,
	init *mochaInitResponse,
	reader io.Reader,
	size int64,
	dest string,
	apiKey string,
) ([]mochaCompletedPart, error) {
	partSize := init.DirectPartSizeBytes
	if partSize <= 0 {
		partSize = init.PartSizeBytes
	}
	if partSize <= 0 {
		partSize = mochaDefaultPartSize
	}

	total := int((size + partSize - 1) / partSize)
	parts := make([]mochaCompletedPart, 0, total)
	signed := make(map[int]string, mochaPresignBatch)

	for partNumber := 1; partNumber <= total; partNumber++ {
		if _, ok := signed[partNumber]; !ok {
			last := partNumber + mochaPresignBatch - 1
			if last > total {
				last = total
			}
			numbers := make([]int, 0, last-partNumber+1)
			for n := partNumber; n <= last; n++ {
				numbers = append(numbers, n)
			}

			batch, err := m.presignParts(ctx, init, numbers, dest, apiKey)
			if err != nil {
				return nil, err
			}
			for number, partURL := range batch {
				signed[number] = partURL
			}
		}

		partURL, ok := signed[partNumber]
		if !ok {
			return nil, fmt.Errorf("mocha: part %d: no upload URL returned", partNumber)
		}

		chunk := chunkSize(size, partSize, partNumber)
		etag, err := m.putPart(ctx, partURL, reader, chunk, partNumber)
		if err != nil {
			return nil, err
		}

		parts = append(parts, mochaCompletedPart{PartNumber: partNumber, ETag: etag})
		delete(signed, partNumber)
	}

	return parts, nil
}

// presignParts asks the API to sign a set of part numbers.
func (m *Mocha) presignParts(
	ctx context.Context,
	init *mochaInitResponse,
	numbers []int,
	dest string,
	apiKey string,
) (map[int]string, error) {
	payload := map[string]any{
		"uploadId":     init.UploadID,
		"key":          init.Key,
		"nodeId":       init.NodeID,
		"originalName": init.OriginalName,
		"path":         dest,
		"partNumbers":  numbers,
	}

	status, body, err := mochaJSON(ctx, http.MethodPost, mochaAPIBase+"/api/files/multipart/presigned", apiKey, payload)
	if err != nil {
		return nil, fmt.Errorf("mocha: presign: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("mocha: presign: status %d: %s", status, mochaDescribe(body))
	}

	var resp mochaPresignResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("mocha: presign: parsing response: %w (body: %s)", err, mochaDescribe(body))
	}

	out := make(map[int]string, len(resp.URLs))
	for _, u := range resp.URLs {
		if u.PartNumber > 0 && u.URL != "" {
			out[u.PartNumber] = u.URL
		}
	}
	return out, nil
}

// putPart streams one part to its presigned URL and returns the ETag verbatim,
// including the surrounding quotes that the completion request expects.
func (m *Mocha) putPart(
	ctx context.Context,
	partURL string,
	reader io.Reader,
	size int64,
	partNumber int,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, io.LimitReader(reader, size))
	if err != nil {
		return "", fmt.Errorf("mocha: part %d: creating request: %w", partNumber, err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return "", fmt.Errorf("mocha: part %d: upload failed: %w", partNumber, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("mocha: part %d: status %d: %s", partNumber, resp.StatusCode, mochaDescribe(body))
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("mocha: part %d: no ETag in response", partNumber)
	}
	return etag, nil
}

// uploadRelay is the fallback for nodes that cannot presign parts. It streams
// each chunk through the API, which relays it to storage. Marked deprecated
// upstream, so it is only used when presigning is unavailable.
func (m *Mocha) uploadRelay(
	ctx context.Context,
	init *mochaInitResponse,
	reader io.Reader,
	size int64,
	dest string,
	apiKey string,
) ([]mochaCompletedPart, error) {
	partSize := init.PartSizeBytes
	if partSize <= 0 {
		partSize = mochaRelayPartSize
	}

	total := int((size + partSize - 1) / partSize)
	parts := make([]mochaCompletedPart, 0, total)

	for partNumber := 1; partNumber <= total; partNumber++ {
		chunk := chunkSize(size, partSize, partNumber)

		query := url.Values{}
		query.Set("uploadId", init.UploadID)
		query.Set("key", init.Key)
		query.Set("nodeId", init.NodeID)
		query.Set("originalName", init.OriginalName)
		query.Set("path", dest)
		query.Set("partNumber", strconv.Itoa(partNumber))

		req, err := http.NewRequestWithContext(
			ctx, http.MethodPut,
			mochaAPIBase+"/api/files/multipart/part?"+query.Encode(),
			io.LimitReader(reader, chunk),
		)
		if err != nil {
			return nil, fmt.Errorf("mocha: relay part %d: creating request: %w", partNumber, err)
		}
		req.ContentLength = chunk
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := httpclient.Get().Do(req)
		if err != nil {
			return nil, fmt.Errorf("mocha: relay part %d: upload failed: %w", partNumber, err)
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("mocha: relay part %d: reading response: %w", partNumber, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("mocha: relay part %d: status %d: %s", partNumber, resp.StatusCode, mochaDescribe(body))
		}

		var part struct {
			ETag string `json:"etag"`
		}
		if err := json.Unmarshal(body, &part); err != nil {
			return nil, fmt.Errorf("mocha: relay part %d: parsing response: %w (body: %s)", partNumber, err, mochaDescribe(body))
		}
		if part.ETag == "" {
			return nil, fmt.Errorf("mocha: relay part %d: no etag in response", partNumber)
		}

		parts = append(parts, mochaCompletedPart{PartNumber: partNumber, ETag: part.ETag})
	}

	return parts, nil
}

// completeUpload finalises the session and returns the new file's id and size.
func (m *Mocha) completeUpload(
	ctx context.Context,
	init *mochaInitResponse,
	name string,
	size int64,
	parts []mochaCompletedPart,
	dest string,
	apiKey string,
) (string, int64, error) {
	payload := map[string]any{
		"uploadId":     init.UploadID,
		"key":          init.Key,
		"nodeId":       init.NodeID,
		"originalName": init.OriginalName,
		"path":         dest,
		"size":         size,
		"mimeType":     mochaMimeType(name),
		"parts":        parts,
	}

	status, body, err := mochaJSON(ctx, http.MethodPost, mochaAPIBase+"/api/files/multipart/complete", apiKey, payload)
	if err != nil {
		return "", 0, fmt.Errorf("mocha: complete: %w", err)
	}
	// Completion answers 201 Created.
	if status < 200 || status >= 300 {
		return "", 0, fmt.Errorf("mocha: complete: status %d: %s", status, mochaDescribe(body))
	}

	var resp mochaCompleteResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", 0, fmt.Errorf("mocha: complete: parsing response: %w (body: %s)", err, mochaDescribe(body))
	}
	if resp.File.ID == "" {
		return "", 0, fmt.Errorf("mocha: complete: no file id in response: %s", mochaDescribe(body))
	}
	return resp.File.ID, resp.File.Size, nil
}

// createShare publishes the uploaded file and returns its public link.
func (m *Mocha) createShare(
	ctx context.Context,
	fileID string,
	apiKey string,
	cfg plugin.Config,
) (string, error) {
	payload := mochaShareRequest{FileID: fileID}

	if raw, ok := cfg["shareExpiresInHours"]; ok && strings.TrimSpace(raw) != "" {
		hours, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || hours < 0 {
			return "", fmt.Errorf("mocha: invalid shareExpiresInHours %q", raw)
		}
		payload.ExpiresInHours = &hours
	}
	if raw, ok := cfg["maxDownloads"]; ok && strings.TrimSpace(raw) != "" {
		max, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || max < 0 {
			return "", fmt.Errorf("mocha: invalid maxDownloads %q", raw)
		}
		payload.MaxDownloads = &max
	}

	status, body, err := mochaJSON(ctx, http.MethodPost, mochaAPIBase+"/api/shares", apiKey, payload)
	if err != nil {
		return "", fmt.Errorf("mocha: share: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("mocha: share: status %d: %s", status, mochaDescribe(body))
	}

	var resp mochaShareResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("mocha: share: parsing response: %w (body: %s)", err, mochaDescribe(body))
	}
	if resp.Share.Token == "" {
		return "", fmt.Errorf("mocha: share: no token in response: %s", mochaDescribe(body))
	}

	return mochaShareBase + resp.Share.Token, nil
}

// abortUpload discards a half-finished session. Cleanup is best effort: the
// original failure is what the caller reports, so nothing here is surfaced.
// It runs on its own timeout because the parent context is usually already
// cancelled by the time this is reached.
func (m *Mocha) abortUpload(init *mochaInitResponse, dest, apiKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()

	payload := map[string]any{
		"uploadId":     init.UploadID,
		"key":          init.Key,
		"nodeId":       init.NodeID,
		"originalName": init.OriginalName,
		"path":         dest,
		"partNumbers":  []int{},
	}

	_, _, _ = mochaJSON(ctx, http.MethodPost, mochaAPIBase+"/api/files/multipart/abort", apiKey, payload)
}

// ── helpers ─────────────────────────────────────────────────────────

// mochaJSON performs an authenticated JSON request and returns the status and
// (size-capped) body. Transport failures are returned as errors; HTTP error
// statuses are returned to the caller so it can include the code in its own
// message, which is what the retry classifier looks for.
func mochaJSON(
	ctx context.Context,
	method, endpoint, apiKey string,
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
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
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

// mochaDescribe extracts the API's error message when there is one, falling
// back to the raw body so nothing is silently swallowed.
func mochaDescribe(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "no response body"
	}
	var envelope mochaErrorResponse
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != "" {
		return envelope.Error
	}
	if len(trimmed) > 300 {
		return trimmed[:300] + "..."
	}
	return trimmed
}

// chunkSize returns the byte length of one part, truncating the last one.
func chunkSize(size, partSize int64, partNumber int) int64 {
	remaining := size - int64(partNumber-1)*partSize
	if remaining < partSize {
		return remaining
	}
	return partSize
}

// mochaPath normalises the destination folder to a rooted path with a trailing
// slash, which is the form the API returns and expects.
func mochaPath(cfg plugin.Config) string {
	dest := "/"
	if cfg != nil {
		if raw := strings.TrimSpace(cfg["path"]); raw != "" {
			dest = raw
		}
	}
	if !strings.HasPrefix(dest, "/") {
		dest = "/" + dest
	}
	if !strings.HasSuffix(dest, "/") {
		dest += "/"
	}
	return dest
}

// mochaMimeType guesses the content type from the file extension.
func mochaMimeType(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}
