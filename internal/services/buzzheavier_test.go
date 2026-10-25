package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/driftywinds/manyup/internal/plugin"
)

// bhTestServer records the upload request the plugin makes.
type bhTestServer struct {
	*httptest.Server

	mu     sync.Mutex
	method string
	path   string
	query  string
	body   []byte
	auth   string
}

func newBHTestServer(t *testing.T) *bhTestServer {
	t.Helper()

	s := &bhTestServer{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		s.method = r.Method
		s.path = r.URL.EscapedPath()
		s.query = r.URL.RawQuery
		s.body = body
		s.auth = r.Header.Get("Authorization")
		s.mu.Unlock()

		// The real service answers 201 with {code, data{...}}.
		writeJSON(w, http.StatusCreated, map[string]any{
			"code": 201,
			"data": map[string]any{
				"id":         "bh123abc",
				"name":       "test.bin",
				"size":       len(body),
				"locationId": "loc-1",
				"note":       "",
			},
		})
	}))
	t.Cleanup(srv.Close)
	s.Server = srv

	original := bhUploadBase
	bhUploadBase = srv.URL
	t.Cleanup(func() { bhUploadBase = original })

	return s
}

func (s *bhTestServer) snapshot() (method, path, query string, body []byte, auth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.method, s.path, s.query, s.body, s.auth
}

func TestBuzzHeavierUploadAnonymous(t *testing.T) {
	srv := newBHTestServer(t)

	payload := []byte("hello buzzheavier")
	res, err := (&BuzzHeavier{}).Upload(context.Background(), "my file.bin",
		bytes.NewReader(payload), int64(len(payload)), plugin.Credentials{}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	method, path, query, body, auth := srv.snapshot()
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
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
	if got, want := res.URL, "https://buzzheavier.com/bh123abc"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
}

func TestBuzzHeavierUploadToDirectory(t *testing.T) {
	srv := newBHTestServer(t)

	creds := plugin.Credentials{"API_TOKEN": "account-id"}
	cfg := plugin.Config{"parentId": "parent-42", "locationId": "ignored-when-parent-set"}
	_, err := (&BuzzHeavier{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, creds, cfg)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	_, path, query, _, auth := srv.snapshot()
	if want := "/parent-42/test.bin"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if strings.Contains(query, "locationId") {
		t.Errorf("query = %q, want no locationId with parentId", query)
	}
	if auth != "Bearer account-id" {
		t.Errorf("authorization = %q, want the account token", auth)
	}
}

func TestBuzzHeavierUploadWithLocationAndNote(t *testing.T) {
	srv := newBHTestServer(t)

	cfg := plugin.Config{"locationId": "loc-9", "note": "hello from manyup"}
	_, err := (&BuzzHeavier{}).Upload(context.Background(), "test.bin",
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

func TestBuzzHeavierReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":503,"error":"upstream unavailable"}`))
	}))
	defer srv.Close()

	original := bhUploadBase
	bhUploadBase = srv.URL
	defer func() { bhUploadBase = original }()

	_, err := (&BuzzHeavier{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, plugin.Credentials{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The retry classifier matches this form.
	if !strings.Contains(err.Error(), "status 503") {
		t.Errorf("error = %v, want it to contain %q", err, "status 503")
	}
}

func TestBuzzHeavierRejectsEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 200})
	}))
	defer srv.Close()

	original := bhUploadBase
	bhUploadBase = srv.URL
	defer func() { bhUploadBase = original }()

	_, err := (&BuzzHeavier{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, plugin.Credentials{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("err = %v, want a 'no data' error", err)
	}
}
