package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	pdAPIBase = "https://pixeldrain.com/api"
)

// PixelDrain uploads files via raw PUT to pixeldrain.com/api/file/{filename}.
// Authentication is via HTTP Basic Auth (empty username, API key as password).
// Follows the same pattern as BuzzHeavier (PUT upload), but uses a different
// auth mechanism and response format.
type PixelDrain struct{}

func RegisterPixelDrain(r *plugin.Registry) {
	r.Register(&PixelDrain{})
}

func (p *PixelDrain) Name() string                { return "pixeldrain" }
func (p *PixelDrain) DisplayName() string          { return "PixelDrain" }
func (p *PixelDrain) Description() string          { return "PixelDrain file hosting (PUT upload, free tier)" }
func (p *PixelDrain) RequiredCredentials() []string { return nil } // token is optional (anonymous may work)
func (p *PixelDrain) SupportsLargeUpload() bool    { return true }

// pdUploadResponse is the JSON envelope returned by the PixelDrain PUT /api/file/{name} endpoint.
type pdUploadResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Value   string `json:"value,omitempty"`  // machine-readable error code
	Message string `json:"message,omitempty"` // human-readable error
}

func (p *PixelDrain) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Build upload URL: PUT https://pixeldrain.com/api/file/{filename}
	uploadURL := pdAPIBase + "/file/" + filepath.Base(filename)

	// Create PUT request with streaming body (raw upload, no multipart).
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
	if err != nil {
		return nil, fmt.Errorf("pixeldrain: creating request: %w", err)
	}

	if size > 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	// Add auth header if API key is provided.
	// PixelDrain uses HTTP Basic Auth: username is empty, password is the API key.
	if token, ok := creds["API_TOKEN"]; ok && token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(":" + token))
		req.Header.Set("Authorization", "Basic "+auth)
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("pixeldrain: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("pixeldrain: reading response: %w", err)
	}

	// Parse JSON response.
	var apiResp pdUploadResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("pixeldrain: parsing response (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Check for API-level error.
	if !apiResp.Success {
		errMsg := fmt.Sprintf("pixeldrain: API error (status %d)", resp.StatusCode)
		if apiResp.Value != "" {
			errMsg += ": " + apiResp.Value
		}
		if apiResp.Message != "" {
			errMsg += " — " + apiResp.Message
		}
		return nil, fmt.Errorf("%s", errMsg)
	}

	if apiResp.ID == "" {
		return nil, fmt.Errorf("pixeldrain: no file ID in response (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Construct download URL from the file ID.
	downloadURL := "https://pixeldrain.com/u/" + apiResp.ID

	return &plugin.UploadResult{
		Service:  p.Name(),
		Filename: filename,
		Size:     apiResp.Size,
		URL:      downloadURL,
	}, nil
}