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

var (
	// These are variables so tests can point the plugin at local servers.
	abyssAPIBaseURL    = "https://api.abyss.to"
	abyssUploadBaseURL = "https://up.abyss.to"
	abyssWatchBaseURL  = "https://abysscdn.com/?v="
)

const abyssResponseLimit = 1 << 20

// Abyss uploads videos to Abyss.to, a permanent video host.
//
// The upload API is unusual: the API key is part of the upload URL, and the
// request is a single multipart POST with the file in a field named "file".
// The response contains the public slug, from which the watch page
// https://abysscdn.com/?v={slug} is built.
//
// See api-refs/abyss.md for the full contract.
type Abyss struct{}

func RegisterAbyss(r *plugin.Registry) {
	r.Register(&Abyss{})
}

func (a *Abyss) Name() string        { return "abyss" }
func (a *Abyss) DisplayName() string { return "Abyss.to" }
func (a *Abyss) Description() string {
	return "Abyss.to permanent video hosting (API key, streaming multipart upload)"
}

func (a *Abyss) RequiredCredentials() []string { return []string{"API_TOKEN"} }

func (a *Abyss) SupportsLargeUpload() bool { return true }

// abyssUploadResponse is the upload response. The live service also returns
// urlIframe; only slug is required by the documented contract.
type abyssUploadResponse struct {
	Status    *bool  `json:"status"`
	Msg       string `json:"msg"`
	Slug      string `json:"slug"`
	URLIframe string `json:"urlIframe"`
}

func (a *Abyss) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return nil, fmt.Errorf("abyss: API_TOKEN required (the Abyss API key)")
	}
	if size <= 0 {
		return nil, fmt.Errorf("abyss: cannot upload an empty file")
	}

	body, contentType, contentLength, err := abyssMultipartBody(filename, reader, size)
	if err != nil {
		return nil, fmt.Errorf("abyss: preparing upload: %w", err)
	}

	endpoint := abyssUploadBaseURL + "/" + url.PathEscape(apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("abyss: creating request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = contentLength

	// A 10 GB upload can outlive the shared client's five-minute overall
	// timeout. The request context still aborts on Ctrl+C.
	resp, err := abyssStreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("abyss: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, abyssResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("abyss: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError("abyss", resp.StatusCode, respBody)
	}

	var payload abyssUploadResponse
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("abyss: parsing response: %w (body: %s)", err, describeBody(respBody))
	}
	if payload.Status != nil && !*payload.Status {
		if payload.Msg != "" {
			return nil, fmt.Errorf("abyss: %s", payload.Msg)
		}
		return nil, fmt.Errorf("abyss: upload failed: %s", describeBody(respBody))
	}
	if payload.Slug == "" {
		return nil, fmt.Errorf("abyss: response has no slug: %s", describeBody(respBody))
	}

	return &plugin.UploadResult{
		Service:  a.Name(),
		Filename: filename,
		Size:     size,
		URL:      abyssWatchBaseURL + url.QueryEscape(payload.Slug),
	}, nil
}

// VerifyCredentials checks the API key against GET /v1/about, which is one cheap
// authenticated request. The documented query-key form is used because the API
// key is not a JWT.
func (a *Abyss) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	apiKey := strings.TrimSpace(creds["API_TOKEN"])
	if apiKey == "" {
		return fmt.Errorf("abyss: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	endpoint := abyssAPIBaseURL + "/v1/about?key=" + url.QueryEscape(apiKey)
	status, body, err := authGet(ctx, endpoint, "", "")
	if err != nil {
		return fmt.Errorf("abyss: could not verify: %w", err)
	}
	return authCheckOutcome("abyss", status, body)
}

// abyssMultipartBody builds a streaming multipart body with an exact
// Content-Length, so the 10 GB upload is neither buffered nor chunked.
func abyssMultipartBody(filename string, reader io.Reader, size int64) (io.Reader, string, int64, error) {
	boundary, err := multipartBoundary()
	if err != nil {
		return nil, "", 0, fmt.Errorf("generating boundary: %w", err)
	}

	name := multipartFileName(filename)
	contentType := videoContentType(name)

	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="` + escapeMultipartQuotes(name) + `"` + "\r\n" +
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

func abyssStreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}
