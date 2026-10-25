package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// ffUploadBase and ffAPIBase are variables so tests can point the client at a
// local server.
var (
	ffUploadBase = "https://w.fuckingfast.net"
	ffAPIBase    = "https://fuckingfast.net"
)

// FuckingFast uploads files via raw PUT to w.fuckingfast.net/{filename}, the
// same protocol BuzzHeavier uses.
//
// Optional settings, taken from the service Options map:
//
//	parentId   upload into a directory: /{parentId}/{filename}
//	locationId store in a specific location: ?locationId=...
//	note       text shown under the download link (base64-encoded on the wire)
type FuckingFast struct{}

func RegisterFuckingFast(r *plugin.Registry) {
	r.Register(&FuckingFast{})
}

func (f *FuckingFast) Name() string        { return "fuckingfast" }
func (f *FuckingFast) DisplayName() string { return "FuckingFast" }
func (f *FuckingFast) Description() string {
	return "FuckingFast file hosting (PUT upload, anonymous or account)"
}

// RequiredCredentials returns nil: anonymous uploads are accepted.
func (f *FuckingFast) RequiredCredentials() []string { return nil }
func (f *FuckingFast) SupportsLargeUpload() bool     { return true }

// ffResponse is the API envelope: {code, data{...}} on success and
// {code, error} on failure.
type ffResponse struct {
	Code  int         `json:"code"`
	Data  *ffFileData `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

type ffFileData struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	LocationID string `json:"locationId"`
	Note       string `json:"note"`
}

func (f *FuckingFast) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	name := url.PathEscape(filepath.Base(filename))
	parentID := optionValue(cfg, "parentId")

	uploadURL := ffUploadBase + "/" + name
	if parentID != "" {
		uploadURL = ffUploadBase + "/" + url.PathEscape(parentID) + "/" + name
	}

	query := url.Values{}
	// A directory upload supersedes an explicit location: the directory already
	// determines where the file is stored.
	if parentID == "" {
		if locationID := optionValue(cfg, "locationId"); locationID != "" {
			query.Set("locationId", locationID)
		}
	}
	// The note travels base64-encoded and is echoed back decoded.
	if note := optionValue(cfg, "note"); note != "" {
		query.Set("note", base64.StdEncoding.EncodeToString([]byte(note)))
	}
	if len(query) > 0 {
		uploadURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
	if err != nil {
		return nil, fmt.Errorf("fuckingfast: creating request: %w", err)
	}

	if size > 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	// Optional: an account id ties the upload to an account.
	if token := strings.TrimSpace(creds["API_TOKEN"]); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fuckingfast: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fuckingfast: reading response: %w", err)
	}

	// Surface the status so transient failures stay retryable.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError("fuckingfast", resp.StatusCode, respBody)
	}

	var apiResp ffResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("fuckingfast: parsing response: %w (body: %s)", err, describeBody(respBody))
	}

	if apiResp.Data == nil {
		return nil, fmt.Errorf("fuckingfast: no data in response: %s", describeBody(respBody))
	}
	if apiResp.Data.ID == "" {
		return nil, fmt.Errorf("fuckingfast: no file id in response: %s", describeBody(respBody))
	}

	return &plugin.UploadResult{
		Service:  f.Name(),
		Filename: filename,
		Size:     apiResp.Data.Size,
		URL:      ffAPIBase + "/" + apiResp.Data.ID,
	}, nil
}

// VerifyCredentials checks an account token against the account endpoint, which
// answers 401 {"code":401,"error":"Unauthorized"} for an unknown token.
func (f *FuckingFast) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	token := strings.TrimSpace(creds["API_TOKEN"])
	if token == "" {
		return fmt.Errorf("fuckingfast: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	status, body, err := authGet(ctx, ffAPIBase+"/api/account", "Authorization", "Bearer "+token)
	if err != nil {
		return fmt.Errorf("fuckingfast: could not verify: %w", err)
	}
	return authCheckOutcome("fuckingfast", status, body)
}
