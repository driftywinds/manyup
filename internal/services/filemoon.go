package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

var filemoonAPIBaseURL = "https://filemoon.org/api/v1"

var (
	// Files above this size use FileMoon's chunked upload path. The API
	// documentation recommends 90 MB chunks for large files so the edge proxy
	// does not reject one enormous request.
	filemoonChunkThreshold int64 = 90 << 20
	filemoonChunkSize      int64 = 90 << 20
)

const filemoonResponseLimit = 1 << 20

// FileMoon uploads videos through a Bearer-token API. Small files go in one
// multipart POST; larger files are split into 90 MB chunks at the same
// /files/upload endpoint. The upload response only gives a public file ID and
// download/watch URLs, so no separate file lookup is needed.
//
// See api-refs/filemoon.md for the full contract.
type FileMoon struct{}

func RegisterFileMoon(r *plugin.Registry) {
	r.Register(&FileMoon{})
}

func (f *FileMoon) Name() string        { return "filemoon" }
func (f *FileMoon) DisplayName() string { return "FileMoon" }
func (f *FileMoon) Description() string {
	return "FileMoon video hosting (Bearer token, single or 90 MB chunked upload)"
}

func (f *FileMoon) RequiredCredentials() []string { return []string{"API_TOKEN"} }

func (f *FileMoon) SupportsLargeUpload() bool { return true }

type filemoonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type filemoonFileData struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Filename string `json:"filename"`
	URLs     struct {
		Page  string `json:"page"`
		Watch string `json:"watch"`
		Embed string `json:"embed"`
	} `json:"urls"`
}

// filemoonUploadEnvelope covers both response shapes seen in the wild:
// the current documentation's {success,data} envelope and the legacy
// {type,shared_id,download_link,file} upload response.
type filemoonUploadEnvelope struct {
	Type         string            `json:"type"`
	Status       string            `json:"status"`
	Chunk        bool              `json:"chunk"`
	Finished     *bool             `json:"finished"`
	SharedID     string            `json:"shared_id"`
	DownloadLink string            `json:"download_link"`
	Success      bool              `json:"success"`
	Error        *filemoonError    `json:"error"`
	Data         *filemoonFileData `json:"data"`
	File         struct {
		WatchURL string `json:"watch_url"`
	} `json:"file"`
}

func (f *FileMoon) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiToken := strings.TrimSpace(creds["API_TOKEN"])
	if apiToken == "" {
		return nil, fmt.Errorf("filemoon: API_TOKEN required (the FileMoon Bearer token)")
	}
	if size <= 0 {
		return nil, fmt.Errorf("filemoon: cannot upload an empty file")
	}

	visibility := optionValue(cfg, "visibility")
	if size > filemoonChunkThreshold {
		return f.uploadChunked(ctx, apiToken, filename, reader, size, visibility)
	}
	return f.uploadSingle(ctx, apiToken, filename, reader, size, visibility)
}

func (f *FileMoon) uploadSingle(
	ctx context.Context,
	apiToken, filename string,
	reader io.Reader,
	size int64,
	visibility string,
) (*plugin.UploadResult, error) {
	body, contentType, contentLength, err := filemoonMultipartBody(filename, reader, size, size, visibility, nil)
	if err != nil {
		return nil, fmt.Errorf("filemoon: preparing upload: %w", err)
	}

	env, err := filemoonPostUpload(ctx, apiToken, body, contentType, contentLength)
	if err != nil {
		return nil, err
	}
	return filemoonResult(env, filename, size)
}

func (f *FileMoon) uploadChunked(
	ctx context.Context,
	apiToken, filename string,
	reader io.Reader,
	size int64,
	visibility string,
) (*plugin.UploadResult, error) {
	uid, err := filemoonChunkUUID()
	if err != nil {
		return nil, fmt.Errorf("filemoon: generating chunk id: %w", err)
	}

	totalChunks := int((size + filemoonChunkSize - 1) / filemoonChunkSize)
	reporter, _ := plugin.ProgressReporterFromContext(ctx)
	var uploaded int64

	for index := 0; index < totalChunks; index++ {
		start := int64(index) * filemoonChunkSize
		length := filemoonChunkSize
		if remaining := size - start; remaining < length {
			length = remaining
		}

		chunk := &filemoonChunk{
			UID:        uid,
			Index:      index,
			Total:      totalChunks,
			ChunkSize:  filemoonChunkSize,
			TotalSize:  size,
			ByteOffset: start,
		}
		body, contentType, contentLength, err := filemoonMultipartBody(filename, reader, length, size, visibility, chunk)
		if err != nil {
			return nil, fmt.Errorf("filemoon: preparing chunk %d: %w", index+1, err)
		}

		env, err := filemoonPostUpload(ctx, apiToken, body, contentType, contentLength)
		if err != nil {
			return nil, fmt.Errorf("filemoon: chunk %d/%d: %w", index+1, totalChunks, err)
		}

		uploaded += length
		if reporter != nil {
			reporter.ReportUploaded(uploaded)
		}

		// The final legacy response has shared_id/download_link and no
		// "finished" field; intermediate chunks explicitly say finished:false.
		if env.SharedID != "" || (env.Data != nil && env.Data.ID != "") {
			return filemoonResult(env, filename, size)
		}
		if env.Finished != nil && *env.Finished {
			return filemoonResult(env, filename, size)
		}
		if env.Finished == nil {
			return nil, fmt.Errorf("filemoon: chunk %d/%d returned no file id", index+1, totalChunks)
		}
	}

	return nil, fmt.Errorf("filemoon: chunked upload completed without a final response")
}

func filemoonPostUpload(
	ctx context.Context,
	apiToken string,
	body io.Reader,
	contentType string,
	contentLength int64,
) (*filemoonUploadEnvelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, filemoonAPIBaseURL+"/files/upload", body)
	if err != nil {
		return nil, fmt.Errorf("filemoon: creating upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = contentLength

	// Videos can outlive the shared client's five-minute whole-request timeout;
	// context cancellation still aborts.
	resp, err := filemoonStreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("filemoon: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, filemoonResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("filemoon: reading upload response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError("filemoon", resp.StatusCode, respBody)
	}

	var env filemoonUploadEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("filemoon: parsing upload response: %w (body: %s)", err, describeBody(respBody))
	}
	if env.Error != nil {
		return nil, fmt.Errorf("filemoon: %s: %s", env.Error.Code, env.Error.Message)
	}
	if !env.Success && env.Type == "" && env.SharedID == "" && (env.Data == nil || env.Data.ID == "") {
		return nil, fmt.Errorf("filemoon: upload failed: %s", describeBody(respBody))
	}
	return &env, nil
}

func filemoonResult(env *filemoonUploadEnvelope, filename string, size int64) (*plugin.UploadResult, error) {
	id, pageURL := env.SharedID, env.DownloadLink
	if env.Data != nil {
		if env.Data.ID != "" {
			id = env.Data.ID
		}
		if env.Data.URLs.Page != "" {
			pageURL = env.Data.URLs.Page
		} else if env.Data.URLs.Watch != "" {
			pageURL = env.Data.URLs.Watch
		}
	}
	if pageURL == "" {
		pageURL = env.File.WatchURL
	}
	if id == "" {
		return nil, fmt.Errorf("filemoon: upload response has no file id")
	}
	if pageURL == "" {
		pageURL = "https://filemoon.org/" + id + "/file"
	}

	return &plugin.UploadResult{
		Service:  "filemoon",
		Filename: filename,
		Size:     size,
		URL:      pageURL,
	}, nil
}

// VerifyCredentials calls GET /account, the documented account endpoint.
func (f *FileMoon) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	apiToken := strings.TrimSpace(creds["API_TOKEN"])
	if apiToken == "" {
		return fmt.Errorf("filemoon: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	status, body, err := authGet(ctx, filemoonAPIBaseURL+"/account", "Authorization", "Bearer "+apiToken)
	if err != nil {
		return fmt.Errorf("filemoon: could not verify: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return authCheckOutcome("filemoon", status, body)
	}
	if status < 200 || status >= 300 {
		return authCheckOutcome("filemoon", status, body)
	}

	var payload struct {
		Success bool           `json:"success"`
		Error   *filemoonError `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("filemoon: could not verify: %w", err)
	}
	if !payload.Success {
		msg := "account check failed"
		if payload.Error != nil && payload.Error.Message != "" {
			msg = payload.Error.Message
		}
		return fmt.Errorf("filemoon: %w: %s", plugin.ErrCredentialRejected, msg)
	}
	return nil
}

type filemoonChunk struct {
	UID        string
	Index      int
	Total      int
	ChunkSize  int64
	TotalSize  int64
	ByteOffset int64
}

// filemoonMultipartBody builds one exact-length multipart request. partSize is
// the number of file bytes in this request; totalSize is the whole file.
func filemoonMultipartBody(
	filename string,
	reader io.Reader,
	partSize, totalSize int64,
	visibility string,
	chunk *filemoonChunk,
) (io.Reader, string, int64, error) {
	boundary, err := multipartBoundary()
	if err != nil {
		return nil, "", 0, fmt.Errorf("generating boundary: %w", err)
	}

	var prefix strings.Builder
	addField := func(name, value string) {
		prefix.WriteString("--" + boundary + "\r\n")
		prefix.WriteString(`Content-Disposition: form-data; name="` + name + `"` + "\r\n\r\n")
		prefix.WriteString(value + "\r\n")
	}

	if visibility != "" {
		addField("visibility", visibility)
	}
	if chunk != nil {
		addField("dzuuid", chunk.UID)
		addField("dzchunkindex", fmt.Sprintf("%d", chunk.Index))
		addField("dztotalchunkcount", fmt.Sprintf("%d", chunk.Total))
		addField("dzchunksize", fmt.Sprintf("%d", chunk.ChunkSize))
		addField("dztotalfilesize", fmt.Sprintf("%d", chunk.TotalSize))
		addField("dzchunkbyteoffset", fmt.Sprintf("%d", chunk.ByteOffset))
	}

	name := multipartFileName(filename)
	contentType := videoContentType(name)
	prefix.WriteString("--" + boundary + "\r\n")
	prefix.WriteString(`Content-Disposition: form-data; name="file"; filename="` + escapeMultipartQuotes(name) + `"` + "\r\n")
	prefix.WriteString("Content-Type: " + contentType + "\r\n\r\n")
	suffix := "\r\n--" + boundary + "--\r\n"
	prefixText := prefix.String()

	body := io.MultiReader(
		strings.NewReader(prefixText),
		io.LimitReader(reader, partSize),
		strings.NewReader(suffix),
	)
	length := int64(len(prefixText)) + partSize + int64(len(suffix))
	return body, "multipart/form-data; boundary=" + boundary, length, nil
}

func filemoonChunkUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "manyup-" + hex.EncodeToString(raw[:]), nil
}

func filemoonStreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}
