package services

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	zxzUploadURL = "https://0x0.st"
)

// ZeroXZero uploads files to 0x0.st (The Null Pointer) — a free, anonymous
// temporary file hoster. Max file size is 512 MiB.
//
// Upload: multipart POST with field name "file" to https://0x0.st
// Response: plain text URL of the uploaded file (e.g. "https://0x0.st/abc.txt")
type ZeroXZero struct{}

func RegisterZeroXZero(r *plugin.Registry) {
	r.Register(&ZeroXZero{})
}

func (z *ZeroXZero) Name() string                { return "0x0" }
func (z *ZeroXZero) DisplayName() string          { return "0x0.st" }
func (z *ZeroXZero) Description() string           { return "0x0.st temporary file hosting (512 MB max, anonymous)" }
func (z *ZeroXZero) RequiredCredentials() []string { return nil } // no auth
func (z *ZeroXZero) SupportsLargeUpload() bool    { return false } // 512 MB cap, simple multipart

func (z *ZeroXZero) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	// Build multipart body streaming the file into the "file" field.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()

		part, err := mw.CreateFormFile("file", filepath.Base(filename))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("0x0: creating form file: %w", err))
			return
		}
		if _, err := httpclient.Copy(part, reader); err != nil {
			pw.CloseWithError(fmt.Errorf("0x0: streaming file: %w", err))
			return
		}
		if err := mw.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("0x0: closing multipart writer: %w", err))
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zxzUploadURL, pr)
	if err != nil {
		return nil, fmt.Errorf("0x0: creating request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("0x0: upload failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("0x0: server returned status %d: %s", resp.StatusCode, string(body))
	}

	// 0x0.st returns the download URL as plain text in the response body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("0x0: reading response: %w", err)
	}

	downloadURL := string(body)
	if downloadURL == "" {
		return nil, fmt.Errorf("0x0: empty response")
	}

	return &plugin.UploadResult{
		Service:  z.Name(),
		Filename: filename,
		Size:     size,
		URL:      downloadURL,
	}, nil
}