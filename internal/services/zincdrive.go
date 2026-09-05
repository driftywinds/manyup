package services

import (
	"context"
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
	zdBaseURL  = "https://zincdrive.com/api/v1"
	zdAPITimeout = 300 * time.Second
)

// ZincDrive uploads files via presigned S3 direct upload.
type ZincDrive struct{}

func RegisterZincDrive(r *plugin.Registry) {
	r.Register(&ZincDrive{})
}

func (z *ZincDrive) Name() string                  { return "zincdrive" }
func (z *ZincDrive) DisplayName() string            { return "ZincDrive" }
func (z *ZincDrive) Description() string             { return "ZincDrive S3 direct upload (up to 10GB)" }
func (z *ZincDrive) RequiredCredentials() []string   { return []string{"API_TOKEN"} }
func (z *ZincDrive) SupportsLargeUpload() bool       { return true }

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

func (z *ZincDrive) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiKey, ok := creds["API_TOKEN"]
	if !ok || apiKey == "" {
		return nil, fmt.Errorf("zincdrive: API_TOKEN required")
	}

	// Step 1: Get presigned URL.
	signResp, err := z.getSignedURL(ctx, filename, apiKey)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: sign: %w", err)
	}

	// Step 2: PUT file to presigned URL.
	if err := z.putFile(ctx, signResp, reader); err != nil {
		return nil, fmt.Errorf("zincdrive: upload: %w", err)
	}

	// Step 3: Confirm upload.
	downloadLink, err := z.confirmUpload(ctx, signResp.Key, filename, size, apiKey, cfg)
	if err != nil {
		return nil, fmt.Errorf("zincdrive: confirm: %w", err)
	}

	return &plugin.UploadResult{
		Service:  z.Name(),
		Filename: filename,
		Size:     size,
		URL:      downloadLink,
	}, nil
}

func (z *ZincDrive) getSignedURL(ctx context.Context, filename, apiKey string) (*zdSignResponse, error) {
	body := url.Values{}
	body.Set("filename", filepath.Base(filename))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zdBaseURL+"/s3/sign", strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var signResp zdSignResponse
	if err := json.NewDecoder(resp.Body).Decode(&signResp); err != nil {
		return nil, fmt.Errorf("parsing sign response: %w", err)
	}
	if signResp.URL == "" || signResp.Key == "" {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("invalid sign response: %s", string(respBody))
	}

	return &signResp, nil
}

func (z *ZincDrive) putFile(ctx context.Context, sign *zdSignResponse, reader io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sign.URL, reader)
	if err != nil {
		return err
	}

	// Apply the headers from the sign response.
	for k, v := range sign.Headers {
		req.Header.Set(k, v)
	}
	// Ensure we send raw bytes if no content-type was specified.
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 PUT returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (z *ZincDrive) confirmUpload(ctx context.Context, key, filename string, size int64, apiKey string, cfg plugin.Config) (string, error) {
	body := url.Values{}
	body.Set("key", key)
	body.Set("filename", filepath.Base(filename))
	body.Set("size", fmt.Sprintf("%d", size))

	if folderID, ok := cfg["folder_id"]; ok && folderID != "" {
		body.Set("folder", folderID)
	}
	if vis, ok := cfg["visibility"]; ok && vis != "" {
		body.Set("visibility", vis)
	} else {
		body.Set("visibility", "1") // public by default
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zdBaseURL+"/s3/confirm", strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var confirmResp zdConfirmResponse
	if err := json.NewDecoder(resp.Body).Decode(&confirmResp); err != nil {
		return "", fmt.Errorf("parsing confirm response: %w", err)
	}
	if confirmResp.DownloadLink == "" {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("no download_link in response: %s", string(respBody))
	}

	return confirmResp.DownloadLink, nil
}
