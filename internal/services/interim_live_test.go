//go:build live

package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/driftywinds/manyup/internal/plugin"
)

// TestInterimLive exercises a small single-PUT upload against the real interim
// instance. It is excluded from the normal build and runs only with
// `go test -tags live` plus INTERIM_API_TOKEN.
func TestInterimLive(t *testing.T) {
	token := interimLiveToken(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	interimLiveVerify(t, ctx, token)

	payload := []byte(fmt.Sprintf("manyup interim live check %d\n", time.Now().UnixNano()))
	interimLiveRoundTrip(t, ctx, token, "manyup-interim-check.txt", payload)
}

// TestInterimMultipartLive exercises an upload above the instance's multipart
// threshold. Set INTERIM_MULTIPART_MB to a size larger than that threshold
// (65 MiB when this was written); it is skipped otherwise.
func TestInterimMultipartLive(t *testing.T) {
	token := interimLiveToken(t)

	megabytes, err := strconv.Atoi(strings.TrimSpace(os.Getenv("INTERIM_MULTIPART_MB")))
	if err != nil || megabytes <= 0 {
		t.Skip("set INTERIM_MULTIPART_MB above the instance's multipart threshold")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	interimLiveVerify(t, ctx, token)

	size := int64(megabytes) << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	interimLiveRoundTrip(t, ctx, token, "manyup-interim-multipart.bin", payload)
}

func interimLiveToken(t *testing.T) string {
	t.Helper()
	token := strings.TrimSpace(os.Getenv("INTERIM_API_TOKEN"))
	if token == "" {
		t.Skip("INTERIM_API_TOKEN is not set")
	}
	return token
}

func interimLiveVerify(t *testing.T, ctx context.Context, token string) {
	t.Helper()
	if err := (&Interim{}).VerifyCredentials(ctx, plugin.Credentials{"API_TOKEN": token}, nil); err != nil {
		t.Fatalf("VerifyCredentials: %v", err)
	}
	t.Log("credential verified")
}

// interimLiveRoundTrip uploads the payload, downloads it through a fresh
// download link, compares the SHA-256 digest, and deletes the file afterwards.
func interimLiveRoundTrip(t *testing.T, ctx context.Context, token, filename string, payload []byte) *plugin.UploadResult {
	t.Helper()

	res, err := (&Interim{}).Upload(ctx, filename,
		bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": token}, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Upload reported %d bytes, want %d", got, want)
	}
	t.Logf("uploaded %d bytes -> %s", res.Size, res.URL)

	fileID := interimLiveFileID(t, res.URL)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		status, body := interimLiveRequest(t, cleanupCtx, http.MethodDelete,
			interimBaseURL+"/files/"+url.PathEscape(fileID), token, nil)
		if status != http.StatusNoContent {
			t.Errorf("delete file: status %d: %s", status, body)
		}
	})

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

	// A freshly committed multipart object can briefly answer 403 while the
	// storage side settles; retry with a fresh link before declaring failure.
	var resp *http.Response
	for attempt := 1; attempt <= 6; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
		if err != nil {
			t.Fatalf("creating download request: %v", err)
		}
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("fetching download URL: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			break
		}

		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		t.Logf("download attempt %d: status %d: %s", attempt, resp.StatusCode, errorBody)
		if attempt == 6 {
			t.Fatalf("download did not become available")
		}

		select {
		case <-ctx.Done():
			t.Fatalf("download cancelled: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}

		status, body = interimLiveRequest(t, ctx, http.MethodPost,
			interimBaseURL+"/files/"+url.PathEscape(fileID)+"/download", token, nil)
		if status != http.StatusOK {
			t.Fatalf("create download link: status %d: %s", status, body)
		}
		if err := json.Unmarshal(body, &link); err != nil {
			t.Fatalf("parsing download link: %v (%s)", err, body)
		}
	}
	defer resp.Body.Close()

	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(resp.Body, int64(len(payload))+1))
	if err != nil {
		t.Fatalf("reading download: %v", err)
	}
	if written != int64(len(payload)) {
		t.Fatalf("downloaded %d bytes, want %d", written, len(payload))
	}
	want := sha256.Sum256(payload)
	if !bytes.Equal(hasher.Sum(nil), want[:]) {
		t.Error("downloaded content does not match the uploaded payload")
	}
	t.Logf("download round-trip verified (%d bytes)", written)

	return res
}

func interimLiveFileID(t *testing.T, pageURL string) string {
	t.Helper()

	parsed, err := url.Parse(pageURL)
	if err != nil {
		t.Fatalf("parsing page URL %q: %v", pageURL, err)
	}
	segments := strings.Split(strings.TrimSuffix(parsed.Path, "/"), "/")
	fileID := segments[len(segments)-1]
	if fileID == "" {
		t.Fatalf("no file id in page URL %q", pageURL)
	}
	return fileID
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
