package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	dnUploadServerURL = "https://datanodes.to/api/upload/server"
	dnBaseURL         = "https://datanodes.to"
)

// DataNodes uploads files via a two-step API:
//  1. GET /api/upload/server?key=... returns an upload server URL + session id.
//  2. POST the file to that server as multipart/form-data (sess_id, utype, file_0).
//
// See https://datanodes.to/pages/api for the official docs.
type DataNodes struct{}

func RegisterDataNodes(r *plugin.Registry) {
	r.Register(&DataNodes{})
}

func (d *DataNodes) Name() string                  { return "datanodes" }
func (d *DataNodes) DisplayName() string            { return "DataNodes" }
func (d *DataNodes) Description() string             { return "DataNodes file hosting (API key, multipart upload)" }
func (d *DataNodes) RequiredCredentials() []string   { return []string{"API_KEY"} }
func (d *DataNodes) SupportsLargeUpload() bool       { return true }

// dnServerResponse is returned by GET /api/upload/server.
type dnServerResponse struct {
	Msg    string `json:"msg"`
	Status int    `json:"status"`
	SessID string `json:"sess_id"`
	Result string `json:"result"`
}

// dnUploadResponseEntry is one element of the JSON array returned by the
// upload server after a file is posted.
type dnUploadResponseEntry struct {
	FileCode   string `json:"file_code"`
	FileStatus string `json:"file_status"`
}

func (d *DataNodes) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	apiKey, ok := creds["API_KEY"]
	if !ok || apiKey == "" {
		return nil, fmt.Errorf("datanodes: API_KEY required")
	}

	// Step 1: Request an upload server and session id.
	srv, err := d.getUploadServer(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("datanodes: upload server: %w", err)
	}

	// Step 2: Stream the file to the returned server.
	fileCode, err := d.putFile(ctx, srv, filename, reader)
	if err != nil {
		return nil, fmt.Errorf("datanodes: upload: %w", err)
	}

	return &plugin.UploadResult{
		Service:  d.Name(),
		Filename: filename,
		Size:     size,
		URL:      dnBaseURL + "/" + fileCode,
	}, nil
}

// getUploadServer asks DataNodes for the upload server URL and a session id.
func (d *DataNodes) getUploadServer(ctx context.Context, apiKey string) (*dnServerResponse, error) {
	endpoint := dnUploadServerURL + "?key=" + url.QueryEscape(apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

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

	var srv dnServerResponse
	if err := json.Unmarshal(respBody, &srv); err != nil {
		return nil, fmt.Errorf("parsing response: %w (body: %s)", err, string(respBody))
	}

	if srv.SessID == "" || srv.Result == "" {
		return nil, fmt.Errorf("invalid response: %s", string(respBody))
	}

	return &srv, nil
}

// putFile streams the file to the upload server as multipart/form-data and
// returns the resulting file code. The body is piped so large files are never
// buffered in memory.
func (d *DataNodes) putFile(
	ctx context.Context,
	srv *dnServerResponse,
	filename string,
	reader io.Reader,
) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()

		if err := mw.WriteField("sess_id", srv.SessID); err != nil {
			pw.CloseWithError(fmt.Errorf("writing sess_id: %w", err))
			return
		}
		if err := mw.WriteField("utype", "prem"); err != nil {
			pw.CloseWithError(fmt.Errorf("writing utype: %w", err))
			return
		}

		part, err := mw.CreateFormFile("file_0", filepath.Base(filename))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("creating form file: %w", err))
			return
		}
		if _, err := httpclient.Copy(part, reader); err != nil {
			pw.CloseWithError(fmt.Errorf("streaming file: %w", err))
			return
		}

		if err := mw.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("closing multipart writer: %w", err))
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.Result, pr)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var entries []dnUploadResponseEntry
	if err := json.Unmarshal(respBody, &entries); err != nil {
		return "", fmt.Errorf("parsing response: %w (body: %s)", err, string(respBody))
	}

	if len(entries) == 0 {
		return "", fmt.Errorf("empty response: %s", string(respBody))
	}

	entry := entries[0]
	if entry.FileStatus != "OK" {
		return "", fmt.Errorf("file status %q: %s", entry.FileStatus, string(respBody))
	}
	if entry.FileCode == "" {
		return "", fmt.Errorf("no file_code in response: %s", string(respBody))
	}

	return entry.FileCode, nil
}
