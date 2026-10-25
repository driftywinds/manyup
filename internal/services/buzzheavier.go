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
	"time"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	bhAccountURL = "https://buzzheavier.com/api/account"
	bhAPITimeout = 300 * time.Second // 5 min for large files
)

// bhUploadBase is a variable so tests can point the client at a local server.
var bhUploadBase = "https://w.buzzheavier.com"

// BuzzHeavier uploads files via raw PUT to w.buzzheavier.com/{filename}.
//
// Optional settings, taken from the service Options map:
//
//	parentId   upload into a directory: /{parentId}/{filename}
//	locationId store in a specific location: ?locationId=...
//	note       text shown under the download link (base64-encoded on the wire)
type BuzzHeavier struct{}

func RegisterBuzzHeavier(r *plugin.Registry) {
	r.Register(&BuzzHeavier{})
}

func (b *BuzzHeavier) Name() string                  { return "buzzheavier" }
func (b *BuzzHeavier) DisplayName() string           { return "BuzzHeavier" }
func (b *BuzzHeavier) Description() string           { return "BuzzHeavier fast file hosting (PUT upload)" }
func (b *BuzzHeavier) RequiredCredentials() []string { return nil } // anonymous works
func (b *BuzzHeavier) SupportsLargeUpload() bool     { return true }

// VerifyCredentials checks an account token against the account endpoint, which
// answers 401 {"code":401,"error":"Unauthorized"} for an unknown token.
func (b *BuzzHeavier) VerifyCredentials(ctx context.Context, creds plugin.Credentials, cfg plugin.Config) error {
	token := strings.TrimSpace(creds["API_TOKEN"])
	if token == "" {
		return fmt.Errorf("buzzheavier: %w: API_TOKEN is empty", plugin.ErrCredentialRejected)
	}

	status, body, err := authGet(ctx, bhAccountURL, "Authorization", "Bearer "+token)
	if err != nil {
		return fmt.Errorf("buzzheavier: could not verify: %w", err)
	}
	return authCheckOutcome("buzzheavier", status, body)
}

// bhResponse is the BuzzHeavier API response envelope.
type bhResponse struct {
	Code int         `json:"code"`
	Data *bhFileData `json:"data,omitempty"`
}

// bhFileData holds the file metadata returned after upload.
type bhFileData struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	LocationID string `json:"locationId"`
	Note       string `json:"note"`
}

func (b *BuzzHeavier) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Build the upload URL: https://w.buzzheavier.com/{name}, optionally with a
	// parent directory in the path and locationId / note as query parameters.
	name := url.PathEscape(filepath.Base(filename))
	parentID := optionValue(cfg, "parentId")

	uploadURL := bhUploadBase + "/" + name
	if parentID != "" {
		uploadURL = bhUploadBase + "/" + url.PathEscape(parentID) + "/" + name
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

	// Create PUT request with streaming body.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
	if err != nil {
		return nil, fmt.Errorf("buzzheavier: creating request: %w", err)
	}

	if size > 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	// Add auth header if account ID is provided.
	if token, ok := creds["API_TOKEN"]; ok && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("buzzheavier: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("buzzheavier: reading response: %w", err)
	}

	// Surface the status code: without it a 429 or 5xx is not recognised as
	// retryable by the uploader.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError("buzzheavier", resp.StatusCode, respBody)
	}

	// Parse JSON response.
	var apiResp bhResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("buzzheavier: parsing response: %w (body: %s)", err, describeBody(respBody))
	}

	if apiResp.Data == nil {
		return nil, fmt.Errorf("buzzheavier: no data in response: %s", describeBody(respBody))
	}

	// Construct download URL from the file ID.
	downloadURL := "https://buzzheavier.com/" + apiResp.Data.ID

	return &plugin.UploadResult{
		Service:  b.Name(),
		Filename: filename,
		Size:     apiResp.Data.Size,
		URL:      downloadURL,
	}, nil
}
