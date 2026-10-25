package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/driftywinds/manyup/internal/plugin"
)

// ffTestServer records the upload request the plugin makes.
type ffTestServer struct {
	*httptest.Server

	mu     sync.Mutex
	method string
	path   string
	query  string
	body   []byte
	auth   string
	sent   int
}

func newFFTestServer(t *testing.T) *ffTestServer {
	t.Helper()

	s := &ffTestServer{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		s.method = r.Method
		s.path = r.URL.EscapedPath()
		s.query = r.URL.RawQuery
		s.body = body
		s.auth = r.Header.Get("Authorization")
		s.sent++
		s.mu.Unlock()

		// Mirror the real envelope: {code, data{...}} with HTTP 201.
		writeJSON(w, http.StatusCreated, map[string]any{
			"code": 201,
			"data": map[string]any{
				"id":         "abc123xyz",
				"name":       "test.bin",
				"size":       len(body),
				"locationId": "loc-1",
				"note":       "",
			},
		})
	}))
	t.Cleanup(srv.Close)
	s.Server = srv

	originalUpload, originalAPI := ffUploadBase, ffAPIBase
	ffUploadBase = srv.URL
	ffAPIBase = srv.URL
	t.Cleanup(func() {
		ffUploadBase = originalUpload
		ffAPIBase = originalAPI
	})

	return s
}

// snapshot returns the recorded request under the lock.
func (s *ffTestServer) snapshot() (method, path, query string, body []byte, auth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.method, s.path, s.query, s.body, s.auth
}

func TestFuckingFastUploadAnonymous(t *testing.T) {
	srv := newFFTestServer(t)

	payload := []byte("hello fuckingfast")
	res, err := (&FuckingFast{}).Upload(context.Background(), "my file.bin",
		bytes.NewReader(payload), int64(len(payload)), plugin.Credentials{}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	method, path, query, body, auth := srv.snapshot()
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
	// The file name is path-escaped.
	if want := "/my%20file.bin"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if query != "" {
		t.Errorf("query = %q, want none", query)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("body = %q, want %q", body, payload)
	}
	if auth != "" {
		t.Errorf("authorization = %q, want empty for anonymous upload", auth)
	}
	if got, want := res.URL, srv.URL+"/abc123xyz"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
}

func TestFuckingFastUploadToDirectory(t *testing.T) {
	srv := newFFTestServer(t)

	creds := plugin.Credentials{"API_TOKEN": "account-id"}
	cfg := plugin.Config{"parentId": "parent-42", "locationId": "ignored-when-parent-set"}
	_, err := (&FuckingFast{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, creds, cfg)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	_, path, query, _, auth := srv.snapshot()
	if want := "/parent-42/test.bin"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	// A directory upload supersedes an explicit location.
	if strings.Contains(query, "locationId") {
		t.Errorf("query = %q, want no locationId with parentId", query)
	}
	if auth != "Bearer account-id" {
		t.Errorf("authorization = %q, want the account token", auth)
	}
}

func TestFuckingFastUploadWithLocationAndNote(t *testing.T) {
	srv := newFFTestServer(t)

	cfg := plugin.Config{"locationId": "loc-9", "note": "hello from manyup"}
	_, err := (&FuckingFast{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, plugin.Credentials{}, cfg)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	_, _, query, _, _ := srv.snapshot()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parsing query %q: %v", query, err)
	}

	if got, want := values.Get("locationId"), "loc-9"; got != want {
		t.Errorf("locationId = %q, want %q", got, want)
	}
	if got, want := values.Get("note"), base64.StdEncoding.EncodeToString([]byte("hello from manyup")); got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

func TestFuckingFastReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":429,"error":"Too many requests"}`))
	}))
	defer srv.Close()

	originalUpload, originalAPI := ffUploadBase, ffAPIBase
	ffUploadBase, ffAPIBase = srv.URL, srv.URL
	defer func() { ffUploadBase, ffAPIBase = originalUpload, originalAPI }()

	_, err := (&FuckingFast{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, plugin.Credentials{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Errorf("error = %v, want it to contain %q", err, "status 429")
	}
}

func TestFuckingFastRejectsEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 200})
	}))
	defer srv.Close()

	originalUpload, originalAPI := ffUploadBase, ffAPIBase
	ffUploadBase, ffAPIBase = srv.URL, srv.URL
	defer func() { ffUploadBase, ffAPIBase = originalUpload, originalAPI }()

	_, err := (&FuckingFast{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, plugin.Credentials{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("err = %v, want a 'no data' error", err)
	}
}

func TestFuckingFastVerifyCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/account" {
			t.Errorf("path = %q, want /api/account", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"error":"Unauthorized"}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": 200, "data": map[string]any{"id": "x"}})
	}))
	defer srv.Close()

	originalAPI := ffAPIBase
	ffAPIBase = srv.URL
	defer func() { ffAPIBase = originalAPI }()

	if err := (&FuckingFast{}).VerifyCredentials(context.Background(),
		plugin.Credentials{"API_TOKEN": "good-token"}, nil); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	if err := (&FuckingFast{}).VerifyCredentials(context.Background(),
		plugin.Credentials{"API_TOKEN": "bad-token"}, nil); err == nil {
		t.Error("invalid token was accepted")
	}
}

// The provider should advertise itself like the other plugins.
func TestFuckingFastMetadata(t *testing.T) {
	f := &FuckingFast{}
	if f.Name() != "fuckingfast" {
		t.Errorf("Name = %q", f.Name())
	}
	if f.RequiredCredentials() != nil {
		t.Errorf("RequiredCredentials = %v, want nil (anonymous works)", f.RequiredCredentials())
	}
	if !f.SupportsLargeUpload() {
		t.Error("SupportsLargeUpload should be true")
	}
	if f.Description() == "" || f.DisplayName() == "" {
		t.Error("DisplayName and Description must be set")
	}
}

// Guard the envelope shape against a silent change.
func TestFFResponseEnvelope(t *testing.T) {
	var resp ffResponse
	if err := json.Unmarshal([]byte(`{"code":201,"data":{"id":"a","size":7}}`), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Code != 201 || resp.Data == nil || resp.Data.ID != "a" || resp.Data.Size != 7 {
		t.Errorf("decoded %+v", resp)
	}
}
