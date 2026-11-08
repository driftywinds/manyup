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

// streamtapeAPIBaseURL is a variable so tests can point the plugin at a local
// server.
var streamtapeAPIBaseURL = "https://api.streamtape.com"

const streamtapeResponseLimit = 1 << 20

// Streamtape uploads videos to Streamtape.
//
// The upload is a two-step flow: GET /file/ul returns a short-lived upload URL,
// then the file is POSTed to that URL as multipart/form-data in the field
// "file1". The upload response contains the public watch URL.
//
// See api-refs/streamtape.md for the full contract.
type Streamtape struct{}

func RegisterStreamtape(r *plugin.Registry) {
	r.Register(&Streamtape{})
}

func (s *Streamtape) Name() string        { return "streamtape" }
func (s *Streamtape) DisplayName() string { return "Streamtape" }
func (s *Streamtape) Description() string {
	return "Streamtape video hosting (API login/key, two-step multipart upload)"
}

// Streamtape authenticates every API call with both the account API login and
// API key, so both are mandatory.
func (s *Streamtape) RequiredCredentials() []string {
	return []string{"USERNAME", "API_TOKEN"}
}

func (s *Streamtape) SupportsLargeUpload() bool { return true }

type streamtapeEnvelope struct {
	Status int             `json:"status"`
	Msg    string          `json:"msg"`
	Result json.RawMessage `json:"result"`
}

type streamtapeUploadSlot struct {
	URL string `json:"url"`
}

type streamtapeUploadResult struct {
	URL  string `json:"url"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Streamtape) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	login, apiKey, err := streamtapeCredentials(creds)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, fmt.Errorf("streamtape: cannot upload an empty file")
	}

	params := url.Values{}
	params.Set("login", login)
	params.Set("key", apiKey)
	if folder := optionValue(cfg, "folder"); folder != "" {
		params.Set("folder", folder)
	}

	httpStatus, body, err := streamtapeAPIGet(ctx, "/file/ul", params)
	if err != nil {
		return nil, fmt.Errorf("streamtape: requesting upload URL: %w", err)
	}
	env, err := parseStreamtapeEnvelope("streamtape", httpStatus, body)
	if err != nil {
		return nil, err
	}
	if env.Status != http.StatusOK {
		return nil, streamtapeAPIError(env)
	}

	var slot streamtapeUploadSlot
	if err := json.Unmarshal(env.Result, &slot); err != nil {
		return nil, fmt.Errorf("streamtape: parsing upload URL: %w", err)
	}
	if slot.URL == "" {
		return nil, fmt.Errorf("streamtape: upload URL response has no url: %s", describeBody(body))
	}

	uploadBody, contentType, contentLength, err := streamtapeMultipartBody(filename, reader, size)
	if err != nil {
		return nil, fmt.Errorf("streamtape: preparing upload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slot.URL, uploadBody)
	if err != nil {
		return nil, fmt.Errorf("streamtape: creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = contentLength

	// Videos can be much larger than the shared client's five-minute whole
	// request timeout; cancellation still comes from the upload context.
	resp, err := streamtapeStreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("streamtape: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, streamtapeResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("streamtape: reading upload response: %w", err)
	}
	uploadEnv, err := parseStreamtapeEnvelope("streamtape", resp.StatusCode, respBody)
	if err != nil {
		return nil, err
	}
	if uploadEnv.Status != http.StatusOK {
		return nil, streamtapeAPIError(uploadEnv)
	}

	var uploaded streamtapeUploadResult
	if err := json.Unmarshal(uploadEnv.Result, &uploaded); err != nil {
		return nil, fmt.Errorf("streamtape: parsing upload result: %w", err)
	}
	if uploaded.URL == "" {
		return nil, fmt.Errorf("streamtape: upload response has no url: %s", describeBody(respBody))
	}

	return &plugin.UploadResult{
		Service:  s.Name(),
		Filename: filename,
		Size:     size,
		URL:      uploaded.URL,
	}, nil
}

// VerifyCredentials checks both account values against GET /account/info.
// Streamtape returns its API status in the JSON body, often with HTTP 200, so
// this cannot rely on authCheckOutcome alone.
func (s *Streamtape) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	login, apiKey, err := streamtapeCredentials(creds)
	if err != nil {
		// The two values are set separately. Missing one is not a rejected
		// credential; config set saves it and verifies after the other exists.
		return err
	}

	params := url.Values{}
	params.Set("login", login)
	params.Set("key", apiKey)

	httpStatus, body, err := streamtapeAPIGet(ctx, "/account/info", params)
	if err != nil {
		return fmt.Errorf("streamtape: could not verify: %w", err)
	}
	if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden {
		return authCheckOutcome("streamtape", httpStatus, body)
	}
	if httpStatus < 200 || httpStatus >= 300 {
		return authCheckOutcome("streamtape", httpStatus, body)
	}

	var env streamtapeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("streamtape: could not verify: %w", err)
	}
	switch {
	case env.Status == http.StatusOK:
		return nil
	case env.Status == http.StatusBadRequest || env.Status == http.StatusForbidden:
		return fmt.Errorf("streamtape: %w: %s", plugin.ErrCredentialRejected, env.Msg)
	case env.Status == http.StatusTooManyRequests || env.Status == 509 || env.Status >= 500:
		return fmt.Errorf("streamtape: could not verify, service answered status %d: %s", env.Status, env.Msg)
	default:
		return fmt.Errorf("streamtape: could not verify, service answered status %d: %s", env.Status, env.Msg)
	}
}

func streamtapeCredentials(creds plugin.Credentials) (string, string, error) {
	login := strings.TrimSpace(creds["USERNAME"])
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if login == "" || apiKey == "" {
		return "", "", fmt.Errorf("streamtape: USERNAME (API login) and API_TOKEN (API key) are required")
	}
	return login, apiKey, nil
}

func streamtapeAPIGet(ctx context.Context, path string, params url.Values) (int, []byte, error) {
	endpoint := streamtapeAPIBaseURL + path + "?" + params.Encode()
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, streamtapeResponseLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func parseStreamtapeEnvelope(service string, httpStatus int, body []byte) (*streamtapeEnvelope, error) {
	if httpStatus < 200 || httpStatus >= 300 {
		return nil, statusError(service, httpStatus, body)
	}
	var env streamtapeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%s: parsing response: %w (body: %s)", service, err, describeBody(body))
	}
	return &env, nil
}

func streamtapeAPIError(env *streamtapeEnvelope) error {
	msg := strings.TrimSpace(env.Msg)
	if msg == "" {
		msg = "request failed"
	}
	return fmt.Errorf("streamtape: status %d: %s", env.Status, msg)
}

// streamtapeMultipartBody builds the exact-length multipart body Streamtape
// expects. The field name is "file1" per the upload documentation.
func streamtapeMultipartBody(filename string, reader io.Reader, size int64) (io.Reader, string, int64, error) {
	boundary, err := multipartBoundary()
	if err != nil {
		return nil, "", 0, fmt.Errorf("generating boundary: %w", err)
	}

	name := multipartFileName(filename)
	contentType := videoContentType(name)

	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file1"; filename="` + escapeMultipartQuotes(name) + `"` + "\r\n" +
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

func streamtapeStreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}
