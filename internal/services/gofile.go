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

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	gofileUploadURL = "https://upload.gofile.io/uploadfile"
	gofileAPITimeout = 120 * time.Second
)

// GoFile uploads files to gofile.io — supports anonymous guest uploads and token-authenticated uploads.
type GoFile struct{}

func RegisterGoFile(r *plugin.Registry) {
	r.Register(&GoFile{})
}

func (g *GoFile) Name() string                  { return "gofile" }
func (g *GoFile) DisplayName() string            { return "GoFile" }
func (g *GoFile) Description() string             { return "GoFile anonymous file hosting (unlimited size, free)" }
func (g *GoFile) RequiredCredentials() []string   { return nil } // token is optional
func (g *GoFile) SupportsLargeUpload() bool       { return true }

// gofileUploadResponse is the GoFile API response envelope for upload.
type gofileUploadResponse struct {
	Status string         `json:"status"`
	Data   *gofileFileData `json:"data,omitempty"`
}

type gofileFileData struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`
	Name             string   `json:"name"`
	ParentFolder     string   `json:"parentFolder"`
	ParentFolderCode string   `json:"parentFolderCode"`
	DownloadPage     string   `json:"downloadPage"`
	Code             string   `json:"code"`
	Size             int64    `json:"size"`
	MD5              string   `json:"md5"`
	MimeType         string   `json:"mimetype"`
	CreateTime       int64    `json:"createTime"`
	ModTime          int64    `json:"modTime"`
	Servers          []string `json:"servers"`
}

func (g *GoFile) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Build multipart body in memory-efficient way.
	// For very large files we stream into the multipart writer.
	body, contentType, err := g.buildMultipartBody(filename, reader, size, creds, cfg)
	if err != nil {
		return nil, fmt.Errorf("gofile: building multipart body: %w", err)
	}

	// Create HTTP request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gofileUploadURL, body)
	if err != nil {
		return nil, fmt.Errorf("gofile: creating request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	// Add auth header if token is available.
	if token, ok := creds["TOKEN"]; ok && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gofile: upload failed: %w", err)
	}
	defer resp.Body.Close()

	// Read and parse response.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gofile: reading response: %w", err)
	}

	var apiResp gofileUploadResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("gofile: parsing response: %w", err)
	}

	if apiResp.Status != "ok" {
		return nil, fmt.Errorf("gofile: API error: %s", apiResp.Status)
	}

	if apiResp.Data == nil {
		return nil, fmt.Errorf("gofile: empty response data")
	}

	// Build result.
	result := &plugin.UploadResult{
		Service:  g.Name(),
		Filename: filename,
		Size:     apiResp.Data.Size,
	}

	// Prefer downloadPage, fallback to code-based URL.
	if apiResp.Data.DownloadPage != "" {
		result.URL = apiResp.Data.DownloadPage
	} else if apiResp.Data.Code != "" {
		result.URL = "https://gofile.io/d/" + apiResp.Data.Code
	} else if apiResp.Data.ParentFolderCode != "" {
		result.URL = "https://gofile.io/d/" + apiResp.Data.ParentFolderCode
	}

	return result, nil
}

// buildMultipartBody creates a streaming multipart body from a file reader.
func (g *GoFile) buildMultipartBody(
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (io.Reader, string, error) {
	// Use a pipe for streaming — no need to buffer entire file in memory.
	pr, pw := io.Pipe()

	mw := multipart.NewWriter(pw)

	// Start writing in a goroutine so we don't block.
	go func() {
		defer pw.Close()

		// Add the file field.
		part, err := mw.CreateFormFile("file", filepath.Base(filename))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("creating form file: %w", err))
			return
		}			// Stream the file content directly into the multipart writer.
			if _, err := httpclient.Copy(part, reader); err != nil {
			pw.CloseWithError(fmt.Errorf("streaming file: %w", err))
			return
		}

		// Add optional folderId if configured.
		if folderID, ok := cfg["folderId"]; ok && folderID != "" {
			if err := mw.WriteField("folderId", folderID); err != nil {
				pw.CloseWithError(fmt.Errorf("writing folderId: %w", err))
				return
			}
		}

		// Close multipart writer to write the boundary.
		if err := mw.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("closing multipart writer: %w", err))
			return
		}
	}()

	return pr, mw.FormDataContentType(), nil
}

// StreamCloser wraps an io.Reader with a no-op Close for http.Request.
type streamCloser struct {
	io.Reader
}

func (sc streamCloser) Close() error { return nil }
