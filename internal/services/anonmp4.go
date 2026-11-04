package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// anonMP4APIBase is a variable so tests can point the plugin at a local server.
var anonMP4APIBase = "https://anonmp4api.xyz"

const (
	// anonMP4UploadPath is the only endpoint the plugin needs. The API also
	// exposes GET /info/{video_id}, which manyup does not call.
	anonMP4UploadPath = "/upload"

	// anonMP4ResponseLimit caps how much of a response is read. A successful
	// response is a small JSON object; a proxy error can be an HTML page.
	anonMP4ResponseLimit = 1 << 20
)

// AnonMP4 uploads videos to anonmp4.to, an anonymous video host. The API has
// no authentication and takes one streaming multipart/form-data POST with the
// file in a field named "file".
//
// The server accepts videos up to 20 GB and processes them asynchronously; the
// upload response contains the public watch URL, which is what manyup reports.
//
// See api-refs/anonmp4.md for the full contract.
type AnonMP4 struct{}

func RegisterAnonMP4(r *plugin.Registry) {
	r.Register(&AnonMP4{})
}

func (a *AnonMP4) Name() string        { return "anonmp4" }
func (a *AnonMP4) DisplayName() string { return "AnonMP4" }
func (a *AnonMP4) Description() string {
	return "AnonMP4 anonymous video hosting (20 GB max, streaming multipart upload)"
}

// RequiredCredentials is empty: the API is completely anonymous.
func (a *AnonMP4) RequiredCredentials() []string { return nil }

// SupportsLargeUpload reports true because the file is streamed straight into
// the multipart body and never buffered in memory.
func (a *AnonMP4) SupportsLargeUpload() bool { return true }

// ── Response types ──────────────────────────────────────────────────

// anonMP4Error is the API's error envelope.
type anonMP4Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

// anonMP4UploadResponse is the success (and failure) envelope of POST /upload.
type anonMP4UploadResponse struct {
	Success    bool          `json:"success"`
	VideoID    string        `json:"video_id"`
	Title      string        `json:"title"`
	Thumbnail  string        `json:"thumbnail"`
	WatchURL   string        `json:"watch_url"`
	EmbedURL   string        `json:"embed_url"`
	DeleteURL  string        `json:"delete_url"`
	UploadDate string        `json:"upload_date"`
	Message    string        `json:"message"`
	Error      *anonMP4Error `json:"error"`
}

// ── Upload ──────────────────────────────────────────────────────────

func (a *AnonMP4) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	if size <= 0 {
		return nil, fmt.Errorf("anonmp4: cannot upload an empty file")
	}

	body, contentType, contentLength, err := anonMP4MultipartBody(filename, reader, size)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: preparing upload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anonMP4APIBase+anonMP4UploadPath, body)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: creating request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = contentLength

	// A 20 GB upload can legitimately outlive the shared client's five-minute
	// overall timeout. The request context still cancels the transfer.
	resp, err := anonMP4StreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, anonMP4ResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("anonmp4: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, anonMP4ResponseError(resp.StatusCode, respBody)
	}

	var payload anonMP4UploadResponse
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("anonmp4: parsing response: %w (body: %s)", err, describeBody(respBody))
	}
	if payload.Error != nil {
		return nil, anonMP4PayloadError(payload.Error)
	}
	if !payload.Success {
		return nil, fmt.Errorf("anonmp4: upload failed: %s", describeBody(respBody))
	}
	if payload.WatchURL == "" {
		return nil, fmt.Errorf("anonmp4: response has no watch_url: %s", describeBody(respBody))
	}

	return &plugin.UploadResult{
		Service:  a.Name(),
		Filename: filename,
		Size:     size,
		URL:      payload.WatchURL,
	}, nil
}

// anonMP4MultipartBody builds a streaming multipart body with a known length.
// The exact Content-Length lets the server enforce its own maximum before the
// whole body arrives and avoids chunked transfer encoding.
func anonMP4MultipartBody(filename string, reader io.Reader, size int64) (io.Reader, string, int64, error) {
	boundary, err := anonMP4Boundary()
	if err != nil {
		return nil, "", 0, fmt.Errorf("generating boundary: %w", err)
	}

	name := anonMP4FileName(filename)
	contentType := anonMP4ContentType(name)

	// This mirrors the part headers mime/multipart would write for a file
	// field, while leaving the file bytes as a streaming section so the body
	// length can be computed without buffering.
	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="` + anonMP4EscapeQuotes(name) + `"` + "\r\n" +
		"Content-Type: " + contentType + "\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"

	body := io.MultiReader(
		strings.NewReader(prefix),
		io.LimitReader(reader, size),
		strings.NewReader(suffix),
	)
	length := int64(len(prefix)) + size + int64(len(suffix))
	return body, "multipart/form-data; boundary=" + boundary, length, nil
}

// anonMP4Boundary returns a multipart boundary that cannot appear in the file
// bytes by accident in practice. The length stays under the RFC 2046 limit.
func anonMP4Boundary() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "----manyup" + hex.EncodeToString(raw[:]), nil
}

// anonMP4StreamingClient returns the shared client without its overall timeout.
// Context cancellation (Ctrl+C) still aborts the upload.
func anonMP4StreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}

// ── Errors ─────────────────────────────────────────────────────────

// anonMP4ResponseError turns a non-2xx response into an error that keeps the
// "status NNN" form the uploader's retry classifier matches.
func anonMP4ResponseError(status int, body []byte) error {
	var payload struct {
		Error *anonMP4Error `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != nil {
		return fmt.Errorf("anonmp4: status %d: %s", status, payload.Error.String())
	}
	return statusError("anonmp4", status, body)
}

// anonMP4PayloadError reports an error envelope delivered with a 2xx status.
func anonMP4PayloadError(apiErr *anonMP4Error) error {
	return fmt.Errorf("anonmp4: %s", apiErr.String())
}

// String renders the API error code, message, and details compactly.
func (e *anonMP4Error) String() string {
	if e == nil {
		return "unknown error"
	}
	code := strings.TrimSpace(e.Code)
	message := strings.TrimSpace(e.Message)

	switch {
	case code != "" && message != "":
		text := code + ": " + message
		if len(e.Details) > 0 && string(e.Details) != "null" {
			text += " (" + describeBody(e.Details) + ")"
		}
		return text
	case code != "":
		return code
	case message != "":
		return message
	default:
		return "unknown error"
	}
}

// ── File naming and content type ───────────────────────────────────

// anonMP4VideoTypes maps the common video extensions to explicit MIME types.
// Relying on the host's mime registry is unreliable, especially on Windows,
// and a generic application/octet-stream part can be rejected by the API.
var anonMP4VideoTypes = map[string]string{
	".3g2":  "video/3gpp2",
	".3gp":  "video/3gpp",
	".asf":  "video/x-ms-asf",
	".avi":  "video/x-msvideo",
	".divx": "video/divx",
	".f4v":  "video/x-f4v",
	".flv":  "video/x-flv",
	".m2ts": "video/mp2t",
	".m4v":  "video/mp4",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".mpeg": "video/mpeg",
	".mpg":  "video/mpeg",
	".mts":  "video/mp2t",
	".ogv":  "video/ogg",
	".ts":   "video/mp2t",
	".vob":  "video/mpeg",
	".webm": "video/webm",
	".wmv":  "video/x-ms-wmv",
}

func anonMP4ContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if contentType, ok := anonMP4VideoTypes[ext]; ok {
		return contentType
	}
	if contentType := mime.TypeByExtension(ext); strings.HasPrefix(strings.ToLower(contentType), "video/") {
		return contentType
	}
	return "application/octet-stream"
}

// anonMP4FileName returns the base name with CR/LF removed so a crafted name
// cannot inject extra part headers.
func anonMP4FileName(filename string) string {
	name := filepath.Base(filename)
	name = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "video"
	}
	return name
}

// anonMP4EscapeQuotes escapes the characters that would end the quoted
// filename parameter, matching mime/multipart's own escaping.
func anonMP4EscapeQuotes(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}
