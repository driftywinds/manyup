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
	"time"

	"github.com/multiuploader/manyup/internal/plugin"
)

const (
	vfGetServerURL   = "https://vikingfile.com/api/get-server"
	vfGetUploadURL   = "https://vikingfile.com/api/get-upload-url"
	vfCompleteURL    = "https://vikingfile.com/api/complete-upload"
	vfAPITimeout     = 300 * time.Second
	vfConnectTimeout = 10 * time.Second
)

// sharedClient is reused across all VikingFile requests for connection pooling.
var sharedClient = &http.Client{
	Timeout: vfAPITimeout,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: vfConnectTimeout,
	},
}

// VikingFile uploads files via single-POST multipart (primary) with
// chunked presigned-URL fallback for very large files.
type VikingFile struct{}

func RegisterVikingFile(r *plugin.Registry) {
	r.Register(&VikingFile{})
}

func (v *VikingFile) Name() string                { return "vikingfile" }
func (v *VikingFile) DisplayName() string          { return "VikingFile" }
func (v *VikingFile) Description() string          { return "VikingFile file hosting (streaming upload)" }
func (v *VikingFile) RequiredCredentials() []string { return nil }
func (v *VikingFile) SupportsLargeUpload() bool    { return true }

// ── Response types ──────────────────────────────────────────────────

type vfServerResponse struct {
	Server string `json:"server"`
}

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

// ── Upload entry point ──────────────────────────────────────────────

func (v *VikingFile) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Fast path: legacy single-POST multipart upload.
	// One connection, one request, streams the whole file — maximum throughput.
	result, err := v.uploadLegacy(ctx, filename, reader, size, creds, cfg)
	if err == nil {
		return result, nil
	}

	// Fallback: chunked presigned-URL upload for when legacy fails
	// (e.g. server returns an error for large files).
	return v.uploadChunked(ctx, filename, reader, size, creds, cfg)
}

// ── Legacy upload (fast path) ──────────────────────────────────────

func (v *VikingFile) uploadLegacy(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Step 1: Get upload server.
	server, err := v.getServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("vikingfile legacy: %w", err)
	}

	// Step 2: Build streaming multipart body via io.Pipe.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()

		part, err := mw.CreateFormFile("file", filepath.Base(filename))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("creating form file: %w", err))
			return
		}
		if _, err := io.Copy(part, reader); err != nil {
			pw.CloseWithError(fmt.Errorf("streaming file: %w", err))
			return
		}

		userHash := ""
		if u, ok := creds["USER_HASH"]; ok {
			userHash = u
		}
		if err := mw.WriteField("user", userHash); err != nil {
			pw.CloseWithError(fmt.Errorf("writing user field: %w", err))
			return
		}

		if path, ok := cfg["path"]; ok && path != "" {
			if err := mw.WriteField("path", path); err != nil {
				pw.CloseWithError(fmt.Errorf("writing path field: %w", err))
				return
			}
		}

		if err := mw.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("closing multipart writer: %w", err))
		}
	}()

	// Step 3: Single POST — one connection, streaming body.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, pr)
	if err != nil {
		return nil, fmt.Errorf("creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := sharedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(respBody))
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

func (v *VikingFile) getServer(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vfGetServerURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating get-server request: %w", err)
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("get-server failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get-server returned status %d: %s", resp.StatusCode, string(body))
	}

	var serverResp vfServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		return "", fmt.Errorf("parsing server response: %w", err)
	}

	if serverResp.Server == "" {
		return "", fmt.Errorf("no server returned")
	}

	return serverResp.Server, nil
}

// ── Chunked upload (fallback) ──────────────────────────────────────

func (v *VikingFile) uploadChunked(
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
		return nil, fmt.Errorf("vikingfile chunked: get-upload-url: %w", err)
	}

	// Step 2: Upload each part via presigned PUT.
	parts, err := v.uploadParts(ctx, uploadInfo, reader, size)
	if err != nil {
		return nil, fmt.Errorf("vikingfile chunked: upload parts: %w", err)
	}

	// Step 3: Complete the upload.
	result, err := v.completeUpload(ctx, uploadInfo, filename, parts, creds, cfg)
	if err != nil {
		return nil, fmt.Errorf("vikingfile chunked: complete: %w", err)
	}

	return result, nil
}

func (v *VikingFile) getUploadURL(ctx context.Context, size int64) (*vfGetUploadURLResponse, error) {
	body := strings.NewReader("size=" + fmt.Sprintf("%d", size))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vfGetUploadURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating get-upload-url request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := sharedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get-upload-url failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get-upload-url returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var uploadInfo vfGetUploadURLResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadInfo); err != nil {
		return nil, fmt.Errorf("parsing get-upload-url response: %w", err)
	}

	if len(uploadInfo.URLs) == 0 {
		return nil, fmt.Errorf("no upload URLs returned")
	}

	return &uploadInfo, nil
}

func (v *VikingFile) uploadParts(
	ctx context.Context,
	info *vfGetUploadURLResponse,
	reader io.Reader,
	size int64,
) ([]vfPartInfo, error) {
	parts := make([]vfPartInfo, 0, info.NumberParts)

	for i, uploadURL := range info.URLs {
		partNum := i + 1

		bytesRead := int64(i) * info.PartSize
		partSize := info.PartSize
		if remaining := size - bytesRead; remaining < partSize {
			partSize = remaining
		}

		limitedReader := io.LimitReader(reader, partSize)
		etag, err := v.putPart(ctx, uploadURL, limitedReader, partSize)
		if err != nil {
			return nil, fmt.Errorf("uploading part %d: %w", partNum, err)
		}

		parts = append(parts, vfPartInfo{
			PartNumber: partNum,
			ETag:       etag,
		})
	}

	return parts, nil
}

func (v *VikingFile) putPart(ctx context.Context, uploadURL string, reader io.Reader, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := sharedClient.Do(req)
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
		return nil, fmt.Errorf("creating complete-upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := sharedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("complete-upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("complete-upload returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var uploadResp vfUploadResponse
	if err := json.Unmarshal(respBody, &uploadResp); err != nil {
		return nil, fmt.Errorf("parsing complete-upload response: %w (body: %s)", err, string(respBody))
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
