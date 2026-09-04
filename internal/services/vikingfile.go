package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/multiuploader/manyup/internal/httpclient"
	"github.com/multiuploader/manyup/internal/plugin"
)

const (
	vfGetUploadURL = "https://vikingfile.com/api/get-upload-url"
	vfCompleteURL  = "https://vikingfile.com/api/complete-upload"
)

// VikingFile uploads files via chunked presigned-URL upload.
// Each part is streamed via io.Pipe so disk reads overlap with network uploads.
type VikingFile struct{}

func RegisterVikingFile(r *plugin.Registry) {
	r.Register(&VikingFile{})
}

func (v *VikingFile) Name() string                 { return "vikingfile" }
func (v *VikingFile) DisplayName() string           { return "VikingFile" }
func (v *VikingFile) Description() string           { return "VikingFile file hosting (chunked upload)" }
func (v *VikingFile) RequiredCredentials() []string  { return nil }
func (v *VikingFile) SupportsLargeUpload() bool      { return true }

// ── Response types ──────────────────────────────────────────────────

type vfGetUploadURLResponse struct {
	UploadID    string   `json:"uploadId"`
	Key         string   `json:"key"`
	PartSize    int64    `json:"partSize"`
	NumberParts int      `json:"numberParts"`
	URLs        []string `json:"urls"`
}

type vfPartInfo struct {
	PartNumber int
	ETag       string
}

type vfUploadResponse struct {
	Name string      `json:"name"`
	Size json.Number `json:"size"`
	Hash string      `json:"hash"`
	URL  string      `json:"url"`
}

// ── Upload ──────────────────────────────────────────────────────────

func (v *VikingFile) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Step 1: Get presigned upload URLs.
	uploadInfo, err := v.getUploadURL(ctx, size)
	if err != nil {
		return nil, fmt.Errorf("vikingfile: get-upload-url: %w", err)
	}

	// Step 2: Upload each part via presigned PUT.
	parts, err := v.uploadParts(ctx, uploadInfo, reader, size)
	if err != nil {
		return nil, fmt.Errorf("vikingfile: upload parts: %w", err)
	}

	// Step 3: Complete the upload.
	result, err := v.completeUpload(ctx, uploadInfo, filename, parts, creds, cfg)
	if err != nil {
		return nil, fmt.Errorf("vikingfile: complete: %w", err)
	}

	return result, nil
}

// getUploadURL asks VikingFile for presigned URLs to upload the file in parts.
func (v *VikingFile) getUploadURL(ctx context.Context, size int64) (*vfGetUploadURLResponse, error) {
	body := strings.NewReader("size=" + fmt.Sprintf("%d", size))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vfGetUploadURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var info vfGetUploadURLResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(info.URLs) == 0 {
		return nil, fmt.Errorf("no upload URLs returned")
	}

	return &info, nil
}

// uploadParts streams each part to its presigned URL via io.Pipe.
// No part is buffered in memory — disk reads overlap with network uploads.
func (v *VikingFile) uploadParts(
	ctx context.Context,
	info *vfGetUploadURLResponse,
	reader io.Reader,
	size int64,
) ([]vfPartInfo, error) {
	parts := make([]vfPartInfo, 0, info.NumberParts)

	for i, uploadURL := range info.URLs {
		partNum := i + 1

		// Calculate how many bytes this part covers.
		bytesRead := int64(i) * info.PartSize
		partSize := info.PartSize
		if remaining := size - bytesRead; remaining < partSize {
			partSize = remaining
		}

		// Stream the part via io.Pipe: goroutine reads from source, HTTP
		// client reads from the pipe — disk I/O and network overlap.
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			limited := io.LimitReader(reader, partSize)
			if _, err := io.Copy(pw, limited); err != nil {
				pw.CloseWithError(err)
			}
		}()

		etag, err := v.putPart(ctx, uploadURL, pr, partSize)
		if err != nil {
			return nil, fmt.Errorf("PUT part %d: %w", partNum, err)
		}

		parts = append(parts, vfPartInfo{
			PartNumber: partNum,
			ETag:       etag,
		})
	}

	return parts, nil
}

// putPart sends one streamed part to its presigned URL and returns the ETag.
func (v *VikingFile) putPart(ctx context.Context, uploadURL string, reader io.Reader, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PUT returned status %d: %s", resp.StatusCode, string(body))
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("no ETag in response headers")
	}

	return etag, nil
}

// completeUpload tells VikingFile the upload is done and gets back the download URL.
func (v *VikingFile) completeUpload(
	ctx context.Context,
	info *vfGetUploadURLResponse,
	filename string,
	parts []vfPartInfo,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	mw.WriteField("key", info.Key)
	mw.WriteField("uploadId", info.UploadID)
	mw.WriteField("name", filepath.Base(filename))

	// User hash (empty for anonymous).
	userHash := ""
	if u, ok := creds["USER_HASH"]; ok {
		userHash = u
	}
	mw.WriteField("user", userHash)

	if path, ok := cfg["path"]; ok && path != "" {
		mw.WriteField("path", path)
	}

	for i, part := range parts {
		mw.WriteField(fmt.Sprintf("parts[%d][PartNumber]", i), fmt.Sprintf("%d", part.PartNumber))
		mw.WriteField(fmt.Sprintf("parts[%d][ETag]", i), part.ETag)
	}

	mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vfCompleteURL, &body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var uploadResp vfUploadResponse
	if err := json.Unmarshal(respBody, &uploadResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w (body: %s)", err, string(respBody))
	}

	if uploadResp.URL == "" {
		return nil, fmt.Errorf("no URL in response: %s", string(respBody))
	}

	fileSize, _ := uploadResp.Size.Int64()
	return &plugin.UploadResult{
		Service:  v.Name(),
		Filename: filename,
		Size:     fileSize,
		URL:      uploadResp.URL,
	}, nil
}
