package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"

	"github.com/multiuploader/multiuploader/internal/plugin"
)

const (
	vfGetServerURL = "https://vikingfile.com/api/get-server"
	vfAPITimeout   = 300 * time.Second
)

// VikingFile uploads files via multipart POST to the legacy upload endpoint.
type VikingFile struct{}

func RegisterVikingFile(r *plugin.Registry) {
	r.Register(&VikingFile{})
}

func (v *VikingFile) Name() string                  { return "vikingfile" }
func (v *VikingFile) DisplayName() string            { return "VikingFile" }
func (v *VikingFile) Description() string             { return "VikingFile file hosting (multipart upload)" }
func (v *VikingFile) RequiredCredentials() []string   { return nil } // anonymous works
func (v *VikingFile) SupportsLargeUpload() bool       { return true }

// vfServerResponse is the response from get-server.
type vfServerResponse struct {
	Server string `json:"server"`
}

// vfUploadResponse is the response after uploading a file.
// Note: size may be returned as a string or number depending on the server.
type vfUploadResponse struct {
	Name string      `json:"name"`
	Size json.Number `json:"size"`
	Hash string      `json:"hash"`
	URL  string      `json:"url"`
}

func (v *VikingFile) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Step 1: Get the upload server.
	server, err := v.getServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("vikingfile: %w", err)
	}

	// Step 2: Build multipart body and upload.
	result, err := v.uploadFile(ctx, server, filename, reader, size, creds, cfg)
	if err != nil {
		return nil, fmt.Errorf("vikingfile: %w", err)
	}

	return result, nil
}

func (v *VikingFile) getServer(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vfGetServerURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating get-server request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get-server failed: %w", err)
	}
	defer resp.Body.Close()

	var serverResp vfServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		return "", fmt.Errorf("parsing server response: %w", err)
	}

	if serverResp.Server == "" {
		return "", fmt.Errorf("no server returned")
	}

	return serverResp.Server, nil
}

func (v *VikingFile) uploadFile(
	ctx context.Context,
	server string,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Stream multipart body via io.Pipe — never buffer the full file in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()

		// Add the file field.
		part, err := mw.CreateFormFile("file", filepath.Base(filename))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("creating form file: %w", err))
			return
		}
		if _, err := io.Copy(part, reader); err != nil {
			pw.CloseWithError(fmt.Errorf("streaming file: %w", err))
			return
		}

		// Add user field (empty for anonymous).
		userHash := ""
		if u, ok := creds["USER_HASH"]; ok {
			userHash = u
		}
		if err := mw.WriteField("user", userHash); err != nil {
			pw.CloseWithError(fmt.Errorf("writing user field: %w", err))
			return
		}

		// Add optional path.
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

	// Send POST to server with streaming body.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, pr)
	if err != nil {
		return nil, fmt.Errorf("creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{Timeout: vfAPITimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var uploadResp vfUploadResponse
	if err := json.Unmarshal(respBody, &uploadResp); err != nil {
		return nil, fmt.Errorf("parsing upload response (status %d): %s", resp.StatusCode, string(respBody))
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
