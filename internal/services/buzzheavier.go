package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	bhUploadBase   = "https://w.buzzheavier.com"
	bhAPITimeout   = 300 * time.Second // 5 min for large files
)

// BuzzHeavier uploads files via raw PUT to w.buzzheavier.com/{filename}.
type BuzzHeavier struct{}

func RegisterBuzzHeavier(r *plugin.Registry) {
	r.Register(&BuzzHeavier{})
}

func (b *BuzzHeavier) Name() string                  { return "buzzheavier" }
func (b *BuzzHeavier) DisplayName() string            { return "BuzzHeavier" }
func (b *BuzzHeavier) Description() string             { return "BuzzHeavier fast file hosting (PUT upload)" }
func (b *BuzzHeavier) RequiredCredentials() []string   { return nil } // anonymous works
func (b *BuzzHeavier) SupportsLargeUpload() bool       { return true }

// bhResponse is the BuzzHeavier API response envelope.
type bhResponse struct {
	Code int          `json:"code"`
	Data *bhFileData  `json:"data,omitempty"`
}

// bhFileData holds the file metadata returned after upload.
type bhFileData struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	LocationID string `json:"locationId"`
}

func (b *BuzzHeavier) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Build the upload URL: https://w.buzzheavier.com/{name}
	// Optionally append ?parentId=... or ?locationId=...
	uploadURL := bhUploadBase + "/" + url.PathEscape(filepath.Base(filename))

	if parentID, ok := cfg["parentId"]; ok && parentID != "" {
		uploadURL = bhUploadBase + "/" + url.PathEscape(parentID) + "/" + url.PathEscape(filepath.Base(filename))
	} else if locationID, ok := cfg["locationId"]; ok && locationID != "" {
		uploadURL += "?locationId=" + url.QueryEscape(locationID)
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

	// Parse JSON response.
	var apiResp bhResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("buzzheavier: parsing response (status %d): %s", resp.StatusCode, string(respBody))
	}

	if apiResp.Data == nil {
		return nil, fmt.Errorf("buzzheavier: no data in response (status %d): %s", resp.StatusCode, string(respBody))
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
