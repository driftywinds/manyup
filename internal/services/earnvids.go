package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// earnvidsAPIBaseURL is a variable so tests can point the plugin at a local
// server.
var earnvidsAPIBaseURL = "https://earnvidsapi.com"

const earnvidsResponseLimit = 1 << 20

// Earnvids uploads videos to Earnvids.
//
// The upload flow is:
//  1. GET /api/upload/server?key=… to obtain a per-account upload server.
//  2. POST a multipart body with "key" and "file" to that server.
//  3. Resolve the public page through /api/file/list, because the upload
//     response only returns the file code.
//
// See api-refs/earnvids.md for the full contract.
type Earnvids struct{}

func RegisterEarnvids(r *plugin.Registry) {
	r.Register(&Earnvids{})
}

func (e *Earnvids) Name() string        { return "earnvids" }
func (e *Earnvids) DisplayName() string { return "Earnvids" }
func (e *Earnvids) Description() string {
	return "Earnvids video hosting (API key, upload server + multipart upload)"
}

func (e *Earnvids) RequiredCredentials() []string { return []string{"API_TOKEN"} }

func (e *Earnvids) SupportsLargeUpload() bool { return true }

type earnvidsEnvelope struct {
	Status     int             `json:"status"`
	Msg        string          `json:"msg"`
	ServerTime string          `json:"server_time"`
	Result     json.RawMessage `json:"result"`
	Files      []earnvidsFile  `json:"files"`
}

type earnvidsFile struct {
	FileCode string `json:"filecode"`
	Filename string `json:"filename"`
	Status   string `json:"status"`
}

type earnvidsList struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Result struct {
		Files []struct {
			FileCode string `json:"file_code"`
			Link     string `json:"link"`
		} `json:"files"`
	} `json:"result"`
}

func (e *Earnvids) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return nil, fmt.Errorf("earnvids: API_TOKEN required (the Earnvids API key)")
	}
	if size <= 0 {
		return nil, fmt.Errorf("earnvids: cannot upload an empty file")
	}

	httpStatus, body, err := earnvidsAPIGet(ctx, "/api/upload/server", apiKey, nil)
	if err != nil {
		return nil, fmt.Errorf("earnvids: requesting upload server: %w", err)
	}
	env, err := parseEarnvidsEnvelope("earnvids", httpStatus, body)
	if err != nil {
		return nil, err
	}
	if env.Status != http.StatusOK {
		return nil, earnvidsAPIError(env)
	}

	var uploadURL string
	if err := json.Unmarshal(env.Result, &uploadURL); err != nil {
		return nil, fmt.Errorf("earnvids: parsing upload server: %w", err)
	}
	if uploadURL == "" {
		return nil, fmt.Errorf("earnvids: upload server response has no url: %s", describeBody(body))
	}

	folder := optionValue(cfg, "folder")
	requestBody, contentType, contentLength, err := earnvidsMultipartBody(apiKey, folder, filename, reader, size)
	if err != nil {
		return nil, fmt.Errorf("earnvids: preparing upload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("earnvids: creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = contentLength

	// Videos can outlive the shared client's five-minute whole-request timeout;
	// the upload context still handles cancellation.
	resp, err := earnvidsStreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("earnvids: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, earnvidsResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("earnvids: reading upload response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError("earnvids", resp.StatusCode, respBody)
	}

	var uploadEnv earnvidsEnvelope
	if err := json.Unmarshal(respBody, &uploadEnv); err != nil {
		return nil, fmt.Errorf("earnvids: parsing upload response: %w (body: %s)", err, describeBody(respBody))
	}
	if uploadEnv.Status != http.StatusOK {
		return nil, earnvidsAPIError(&uploadEnv)
	}
	if len(uploadEnv.Files) == 0 {
		return nil, fmt.Errorf("earnvids: upload response has no files: %s", describeBody(respBody))
	}

	file := uploadEnv.Files[0]
	if file.FileCode == "" {
		return nil, earnvidsUploadFileError(file.Status)
	}
	if !strings.EqualFold(strings.TrimSpace(file.Status), "OK") {
		return nil, earnvidsUploadFileError(file.Status)
	}

	link, err := earnvidsPublicLink(ctx, apiKey, file.FileCode)
	if err != nil {
		return nil, err
	}

	return &plugin.UploadResult{
		Service:  e.Name(),
		Filename: filename,
		Size:     size,
		URL:      link,
	}, nil
}

// VerifyCredentials checks the API key against /api/account/info. Earnvids puts
// its API status in the JSON body, so the HTTP status alone is not enough.
func (e *Earnvids) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return fmt.Errorf("earnvids: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	httpStatus, body, err := earnvidsAPIGet(ctx, "/api/account/info", apiKey, nil)
	if err != nil {
		return fmt.Errorf("earnvids: could not verify: %w", err)
	}
	if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden {
		return authCheckOutcome("earnvids", httpStatus, body)
	}
	if httpStatus < 200 || httpStatus >= 300 {
		return authCheckOutcome("earnvids", httpStatus, body)
	}

	var env earnvidsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("earnvids: could not verify: %w", err)
	}
	switch {
	case env.Status == http.StatusOK:
		return nil
	case env.Status == http.StatusBadRequest || env.Status == http.StatusForbidden:
		return fmt.Errorf("earnvids: %w: %s", plugin.ErrCredentialRejected, env.Msg)
	case env.Status == http.StatusTooManyRequests || env.Status == 509 || env.Status >= 500:
		return fmt.Errorf("earnvids: could not verify, service answered status %d: %s", env.Status, env.Msg)
	default:
		return fmt.Errorf("earnvids: could not verify, service answered status %d: %s", env.Status, env.Msg)
	}
}

func earnvidsAPIGet(ctx context.Context, path, apiKey string, extra url.Values) (int, []byte, error) {
	params := url.Values{}
	params.Set("key", apiKey)
	for k, values := range extra {
		for _, value := range values {
			params.Add(k, value)
		}
	}

	endpoint := earnvidsAPIBaseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, earnvidsResponseLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func parseEarnvidsEnvelope(service string, httpStatus int, body []byte) (*earnvidsEnvelope, error) {
	if httpStatus < 200 || httpStatus >= 300 {
		return nil, statusError(service, httpStatus, body)
	}
	var env earnvidsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%s: parsing response: %w (body: %s)", service, err, describeBody(body))
	}
	return &env, nil
}

func earnvidsAPIError(env *earnvidsEnvelope) error {
	msg := strings.TrimSpace(env.Msg)
	if msg == "" {
		msg = "request failed"
	}
	return fmt.Errorf("earnvids: status %d: %s", env.Status, msg)
}

func earnvidsUploadFileError(status string) error {
	msg := strings.TrimSpace(status)
	if msg == "" {
		msg = "unknown upload error"
	}

	// "video is too short" and invalid-input errors are permanent. Other
	// per-file failures reported by an upload node are generally storage or
	// database availability problems, so mark them retryable by the uploader.
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "too short") || strings.Contains(lower, "invalid") {
		return fmt.Errorf("earnvids: upload failed: %s", msg)
	}
	return fmt.Errorf("earnvids: upload failed (status 503): %s", msg)
}

func earnvidsPublicLink(ctx context.Context, apiKey, fileCode string) (string, error) {
	httpStatus, body, err := earnvidsAPIGet(ctx, "/api/file/list", apiKey, url.Values{
		"per_page": {"100"},
		"page":     {"1"},
	})
	if err != nil {
		return "", fmt.Errorf("earnvids: looking up public link: %w", err)
	}
	if httpStatus < 200 || httpStatus >= 300 {
		return "", statusError("earnvids", httpStatus, body)
	}

	var list earnvidsList
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("earnvids: parsing file list: %w (body: %s)", err, describeBody(body))
	}
	if list.Status != http.StatusOK {
		return "", fmt.Errorf("earnvids: status %d: %s", list.Status, list.Msg)
	}
	for _, item := range list.Result.Files {
		if item.FileCode == fileCode && item.Link != "" {
			return item.Link, nil
		}
	}
	return "", fmt.Errorf("earnvids: uploaded file %s but no public link was found", fileCode)
}

// earnvidsMultipartBody builds the exact-length multipart body with the "key"
// field followed by the "file" part. The optional folder is sent as "fld_id".
func earnvidsMultipartBody(apiKey, folder, filename string, reader io.Reader, size int64) (io.Reader, string, int64, error) {
	boundary, err := multipartBoundary()
	if err != nil {
		return nil, "", 0, fmt.Errorf("generating boundary: %w", err)
	}

	var prefix strings.Builder
	prefix.WriteString("--" + boundary + "\r\n")
	prefix.WriteString(`Content-Disposition: form-data; name="key"` + "\r\n\r\n")
	prefix.WriteString(apiKey + "\r\n")

	if folder != "" {
		prefix.WriteString("--" + boundary + "\r\n")
		prefix.WriteString(`Content-Disposition: form-data; name="fld_id"` + "\r\n\r\n")
		prefix.WriteString(folder + "\r\n")
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
		io.LimitReader(reader, size),
		strings.NewReader(suffix),
	)
	length := int64(len(prefixText)) + size + int64(len(suffix))
	return body, "multipart/form-data; boundary=" + boundary, length, nil
}

func earnvidsStreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}
