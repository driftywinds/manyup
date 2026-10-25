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

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// zdBaseURL is a variable so tests can point the client at a local server.
var zdBaseURL = "https://zincdrive.com/api/v1"

const (
	// zdSinglePartCeiling is ZincDrive's documented ceiling for one presigned
	// PUT; larger files must use the multipart endpoints.
	zdSinglePartCeiling = 4 << 30 // 4 GiB
)

// zdSinglePartMax and zdMultipartPartSize are variables so tests can shrink
// them. Production values are 4 GiB and 64 MiB (S3 requires at least 5 MiB per
// part, and 64 MiB keeps a 10 GiB file to roughly 160 parts).
var (
	zdSinglePartMax     int64 = zdSinglePartCeiling
	zdMultipartPartSize int64 = 64 << 20
)

// ZincDrive uploads files through presigned S3 PUTs.
//
// Files up to 4 GiB take the single-part /s3/sign path. Larger files — the
// service allows 10 GiB — use the multipart endpoints: initiate, sign and PUT
// each part, complete with the ETags, then confirm to create the file record.
type ZincDrive struct{}

func RegisterZincDrive(r *plugin.Registry) {
	r.Register(&ZincDrive{})
}

func (z *ZincDrive) Name() string        { return "zincdrive" }
func (z *ZincDrive) DisplayName() string { return "ZincDrive" }
func (z *ZincDrive) Description() string {
	return "ZincDrive S3 direct upload (single-part to 4 GB, multipart to 10 GB)"
}
func (z *ZincDrive) RequiredCredentials() []string { return []string{"API_TOKEN"} }
func (z *ZincDrive) SupportsLargeUpload() bool     { return true }

// ── Response types ──────────────────────────────────────────────────

// zdSignResponse is returned by POST /s3/sign.
type zdSignResponse struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Key     string            `json:"key"`
}

// zdConfirmResponse is returned by POST /s3/confirm.
type zdConfirmResponse struct {
	Type         string `json:"type"`
	SharedID     string `json:"shared_id"`
	DownloadLink string `json:"download_link"`
}

// zdPart is one entry of the completion list. The field names follow the
// AWS S3 convention that multipart implementations generally mirror.
type zdPart struct {
	PartNumber int    `json:"PartNumber"`
	ETag       string `json:"ETag"`
}

// ── Upload ──────────────────────────────────────────────────────────

func (z *ZincDrive) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return nil, fmt.Errorf("zincdrive: API_TOKEN required")
	}

	if size > zdSinglePartMax {
		return z.uploadMultipart(ctx, filename, reader, size, apiKey, cfg)
	}
	return z.uploadSinglePart(ctx, filename, reader, size, apiKey, cfg)
}

// uploadSinglePart signs one PUT, streams the whole file to it, then confirms.
func (z *ZincDrive) uploadSinglePart(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	apiKey string,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	sign, err := z.sign(ctx, filename, apiKey)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: sign: %w", err)
	}

	if err := z.putObject(ctx, sign, reader, size); err != nil {
		return nil, fmt.Errorf("zincdrive: upload: %w", err)
	}

	link, err := z.confirm(ctx, sign.Key, filename, size, apiKey, cfg)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: confirm: %w", err)
	}

	return &plugin.UploadResult{
		Service:  z.Name(),
		Filename: filename,
		Size:     size,
		URL:      link,
	}, nil
}

// uploadMultipart initiates a multipart upload, sends each part, completes it,
// then confirms the finished object.
func (z *ZincDrive) uploadMultipart(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	apiKey string,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	uploadID, key, err := z.initMultipart(ctx, filename, apiKey)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: multipart init: %w", err)
	}

	parts, err := z.uploadParts(ctx, uploadID, reader, size, apiKey)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: multipart upload: %w", err)
	}

	if err := z.completeMultipart(ctx, uploadID, parts, apiKey); err != nil {
		return nil, fmt.Errorf("zincdrive: multipart complete: %w", err)
	}

	link, err := z.confirm(ctx, key, filename, size, apiKey, cfg)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: confirm: %w", err)
	}

	return &plugin.UploadResult{
		Service:  z.Name(),
		Filename: filename,
		Size:     size,
		URL:      link,
	}, nil
}

// ── Single-part helpers ─────────────────────────────────────────────

func (z *ZincDrive) sign(ctx context.Context, filename, apiKey string) (*zdSignResponse, error) {
	form := url.Values{}
	form.Set("filename", filepath.Base(filename))
	if mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); mimeType != "" {
		form.Set("type", mimeType)
	}

	status, body, err := zdDo(ctx, http.MethodPost, "/s3/sign", apiKey, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError("zincdrive", status, body)
	}

	var sign zdSignResponse
	if err := json.Unmarshal(body, &sign); err != nil {
		return nil, fmt.Errorf("parsing sign response: %w (body: %s)", err, describeBody(body))
	}
	if sign.URL == "" || sign.Key == "" {
		return nil, fmt.Errorf("incomplete sign response: %s", describeBody(body))
	}
	return &sign, nil
}

func (z *ZincDrive) putObject(ctx context.Context, sign *zdSignResponse, reader io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sign.URL, reader)
	if err != nil {
		return err
	}

	// Apply the headers from the sign response: the signature covers them.
	for k, v := range sign.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if size > 0 {
		req.ContentLength = size
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusError("zincdrive: storage", resp.StatusCode, body)
	}
	return nil
}

func (z *ZincDrive) confirm(
	ctx context.Context,
	key, filename string,
	size int64,
	apiKey string,
	cfg plugin.Config,
) (string, error) {
	form := url.Values{}
	form.Set("key", key)
	form.Set("filename", filepath.Base(filename))
	form.Set("size", strconv.FormatInt(size, 10))

	if folder, ok := cfg["folder_id"]; ok && folder != "" {
		form.Set("folder", folder)
	}
	if visibility, ok := cfg["visibility"]; ok && visibility != "" {
		form.Set("visibility", visibility)
	} else {
		form.Set("visibility", "1") // public by default
	}

	status, body, err := zdDo(ctx, http.MethodPost, "/s3/confirm", apiKey, form)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", statusError("zincdrive", status, body)
	}

	var confirm zdConfirmResponse
	if err := json.Unmarshal(body, &confirm); err != nil {
		return "", fmt.Errorf("parsing confirm response: %w (body: %s)", err, describeBody(body))
	}
	if confirm.DownloadLink == "" {
		return "", fmt.Errorf("no download_link in response: %s", describeBody(body))
	}
	return confirm.DownloadLink, nil
}

// ── Multipart helpers ───────────────────────────────────────────────

// initMultipart opens a multipart upload. The vendor documents only that it
// "returns an uploadId and key", so the response is read tolerantly.
func (z *ZincDrive) initMultipart(ctx context.Context, filename, apiKey string) (string, string, error) {
	form := url.Values{}
	form.Set("filename", filepath.Base(filename))
	if mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); mimeType != "" {
		form.Set("type", mimeType)
	}

	status, body, err := zdDo(ctx, http.MethodPost, "/s3/multipart", apiKey, form)
	if err != nil {
		return "", "", err
	}
	if status < 200 || status >= 300 {
		return "", "", statusError("zincdrive", status, body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("parsing init response: %w (body: %s)", err, describeBody(body))
	}

	// Accept the common spellings: the response schema is undocumented.
	uploadID := zdFirstString(payload, "uploadId", "upload_id", "id")
	key := zdFirstString(payload, "key", "objectKey", "object_key")
	if uploadID == "" || key == "" {
		return "", "", fmt.Errorf("incomplete init response: %s", describeBody(body))
	}
	return uploadID, key, nil
}

// signPart returns the presigned PUT URL for one part.
func (z *ZincDrive) signPart(ctx context.Context, uploadID string, partNumber int, apiKey string) (string, error) {
	path := fmt.Sprintf("/s3/multipart/%s/%d", url.PathEscape(uploadID), partNumber)

	status, body, err := zdDo(ctx, http.MethodGet, path, apiKey, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", statusError("zincdrive: part sign", status, body)
	}

	if partURL := zdSignedURL(body); partURL != "" {
		return partURL, nil
	}
	return "", fmt.Errorf("no part URL in response: %s", describeBody(body))
}

// uploadParts streams the file one part at a time, so nothing is buffered.
func (z *ZincDrive) uploadParts(
	ctx context.Context,
	uploadID string,
	reader io.Reader,
	size int64,
	apiKey string,
) ([]zdPart, error) {
	total := int((size + zdMultipartPartSize - 1) / zdMultipartPartSize)
	parts := make([]zdPart, 0, total)

	for partNumber := 1; partNumber <= total; partNumber++ {
		partURL, err := z.signPart(ctx, uploadID, partNumber, apiKey)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", partNumber, err)
		}

		length := chunkSize(size, zdMultipartPartSize, partNumber)
		etag, err := z.putPart(ctx, partURL, reader, length, partNumber)
		if err != nil {
			return nil, err
		}

		parts = append(parts, zdPart{PartNumber: partNumber, ETag: etag})
	}

	return parts, nil
}

func (z *ZincDrive) putPart(
	ctx context.Context,
	partURL string,
	reader io.Reader,
	size int64,
	partNumber int,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, io.LimitReader(reader, size))
	if err != nil {
		return "", fmt.Errorf("part %d: creating request: %w", partNumber, err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return "", fmt.Errorf("part %d: upload failed: %w", partNumber, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", statusError("zincdrive: part "+strconv.Itoa(partNumber), resp.StatusCode, body)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("part %d: no ETag in response", partNumber)
	}
	return etag, nil
}

// completeMultipart finishes the upload. The vendor only says to "send the list
// of ETags", so both the AWS-style parts array and a plain etags list are sent;
// implementations ignore whichever they do not use.
func (z *ZincDrive) completeMultipart(ctx context.Context, uploadID string, parts []zdPart, apiKey string) error {
	etags := make([]string, 0, len(parts))
	for _, part := range parts {
		etags = append(etags, part.ETag)
	}

	payload := map[string]any{
		"parts": parts,
		"etags": etags,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding completion: %w", err)
	}

	path := fmt.Sprintf("/s3/multipart/%s/complete", url.PathEscape(uploadID))
	status, respBody, err := zdDoJSON(ctx, http.MethodPost, path, apiKey, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return statusError("zincdrive", status, respBody)
	}
	return nil
}

// ── Shared helpers ──────────────────────────────────────────────────

// zdDo sends an authenticated form request to the ZincDrive API.
func zdDo(ctx context.Context, method, path, apiKey string, form url.Values) (int, []byte, error) {
	var body io.Reader
	contentType := ""
	if form != nil {
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	return zdRequest(ctx, method, path, apiKey, contentType, body)
}

// zdDoJSON sends an authenticated JSON request to the ZincDrive API.
func zdDoJSON(ctx context.Context, method, path, apiKey string, payload []byte) (int, []byte, error) {
	return zdRequest(ctx, method, path, apiKey, "application/json", bytes.NewReader(payload))
}

func zdRequest(
	ctx context.Context,
	method, path, apiKey, contentType string,
	body io.Reader,
) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, zdBaseURL+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// zdSignedURL extracts a part URL, which may be a bare URL or a JSON object.
func zdSignedURL(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "http") {
		return trimmed
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return zdFirstString(payload, "url", "signed_url", "signedUrl", "part_url", "partUrl")
}

// zdFirstString returns the first non-empty string value among keys.
func zdFirstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// VerifyCredentials signs a throwaway object. An invalid key is answered with
// 401 {"status":"error","message":"Invalid API key."}; signing does not create
// a file, so this is a safe check.
func (z *ZincDrive) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return fmt.Errorf("zincdrive: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	form := url.Values{}
	form.Set("filename", "manyup-credential-check.txt")

	status, body, err := zdDo(ctx, http.MethodPost, "/s3/sign", apiKey, form)
	if err != nil {
		return fmt.Errorf("zincdrive: could not verify: %w", err)
	}
	return authCheckOutcome("zincdrive", status, body)
}
