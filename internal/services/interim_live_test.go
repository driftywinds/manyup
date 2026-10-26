//go:build live

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/driftywinds/manyup/internal/plugin"
)

// TestInterimLive exercises the real interim instance. It is excluded from the
// normal build and runs only with `go test -tags live` plus INTERIM_API_TOKEN.
func TestInterimLive(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("INTERIM_API_TOKEN"))
	if token == "" {
		t.Skip("INTERIM_API_TOKEN is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	interim := &Interim{}
	if err := interim.VerifyCredentials(ctx, plugin.Credentials{"API_TOKEN": token}, nil); err != nil {
		t.Fatalf("VerifyCredentials: %v", err)
	}
	t.Log("credential verified and approved")

	payload := []byte(fmt.Sprintf("manyup interim live check %d\n", time.Now().UnixNano()))
	res, err := interim.Upload(ctx, "manyup-interim-check.txt",
		bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": token}, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	t.Logf("uploaded %d bytes -> %s", res.Size, res.URL)

	parsed, err := url.Parse(res.URL)
	if err != nil {
		t.Fatalf("parsing page URL %q: %v", res.URL, err)
	}
	segments := strings.Split(strings.TrimSuffix(parsed.Path, "/"), "/")
	fileID := segments[len(segments)-1]
	if fileID == "" {
		t.Fatalf("no file id in page URL %q", res.URL)
	}

	// Clean up whatever happens next.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		status, body := interimLiveRequest(t, cleanupCtx, http.MethodDelete,
			interimBaseURL+"/files/"+url.PathEscape(fileID), token, nil)
		if status != http.StatusNoContent {
			t.Errorf("delete file: status %d: %s", status, body)
		}
	})

	// Ask for a fresh download link and fetch the bytes back.
	status, body := interimLiveRequest(t, ctx, http.MethodPost,
		interimBaseURL+"/files/"+url.PathEscape(fileID)+"/download", token, nil)
	if status != http.StatusOK {
		t.Fatalf("create download link: status %d: %s", status, body)
	}
	var link struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &link); err != nil {
		t.Fatalf("parsing download link: %v (%s)", err, body)
	}

	resp, err := http.Get(link.URL)
	if err != nil {
		t.Fatalf("fetching download URL: %v", err)
	}
	defer resp.Body.Close()
	downloaded, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading download: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d: %s", resp.StatusCode, downloaded)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Errorf("downloaded %q, want %q", downloaded, payload)
	}
	t.Logf("download round-trip verified (%d bytes)", len(downloaded))
}

func interimLiveRequest(t *testing.T, ctx context.Context, method, endpoint, token string, payload any) (int, []byte) {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encoding payload: %v", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return resp.StatusCode, data
}
