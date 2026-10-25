package services

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/driftywinds/manyup/internal/plugin"
)

// zincTestServer stands in for the ZincDrive API.
type zincTestServer struct {
	*httptest.Server

	mu        sync.Mutex
	objects   map[string][]byte // uploaded part bodies, keyed by path
	etags     map[string]string
	complete  map[string]any
	confirmed map[string]string
}

func newZincTestServer(t *testing.T) *zincTestServer {
	t.Helper()

	s := &zincTestServer{
		objects:   make(map[string][]byte),
		etags:     make(map[string]string),
		confirmed: make(map[string]string),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/s3/sign", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "test-key" {
			t.Errorf("sign: api key = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("sign: parsing form: %v", err)
		}
		if r.PostForm.Get("filename") == "" {
			t.Errorf("sign: filename missing")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"url":     s.URL + "/storage/single",
			"method":  "PUT",
			"headers": map[string]string{"Content-Type": "application/octet-stream"},
			"key":     "uploads/single.bin",
		})
	})

	mux.HandleFunc("/storage/single", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.objects["single"] = body
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/s3/multipart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("multipart init: method = %s", r.Method)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"uploadId": "up-1",
			"key":      "uploads/multipart.bin",
		})
	})

	mux.HandleFunc("/s3/multipart/up-1/complete", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("complete: decoding: %v", err)
		}
		s.mu.Lock()
		s.complete = payload
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
	})

	// Part signing: "/s3/multipart/up-1/{n}".
	mux.HandleFunc("/s3/multipart/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/s3/multipart/"), "/")
		if len(parts) != 2 {
			t.Errorf("part sign: unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			t.Errorf("part sign: bad part number in %q", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Half the parts answer with JSON, half with a bare URL, so both
		// parsing paths are exercised.
		if n%2 == 0 {
			_, _ = w.Write([]byte(s.URL + "/storage/part/" + strconv.Itoa(n)))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"url": s.URL + "/storage/part/" + strconv.Itoa(n)})
	})

	mux.HandleFunc("/storage/part/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path := r.URL.Path
		etag := `"etag-` + strings.TrimPrefix(path, "/storage/part/") + `"`

		s.mu.Lock()
		s.objects[path] = body
		s.etags[path] = etag
		s.mu.Unlock()

		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/s3/confirm", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("confirm: parsing form: %v", err)
		}
		s.mu.Lock()
		s.confirmed["key"] = r.PostForm.Get("key")
		s.confirmed["filename"] = r.PostForm.Get("filename")
		s.confirmed["size"] = r.PostForm.Get("size")
		s.confirmed["visibility"] = r.PostForm.Get("visibility")
		s.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{
			"type":          "success",
			"shared_id":     "xyz789",
			"download_link": "https://zincdrive.com/file/xyz789",
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	originalBase := zdBaseURL
	zdBaseURL = s.URL
	t.Cleanup(func() { zdBaseURL = originalBase })

	return s
}

func (s *zincTestServer) uploaded(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[path]
}

func TestZincDriveUploadSinglePart(t *testing.T) {
	srv := newZincTestServer(t)

	payload := []byte("hello zincdrive")
	res, err := (&ZincDrive{}).Upload(context.Background(), "test.bin",
		bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "test-key"}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.URL, "https://zincdrive.com/file/xyz789"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got := srv.uploaded("single"); !bytes.Equal(got, payload) {
		t.Errorf("uploaded %q, want %q", got, payload)
	}
	if got, want := srv.confirmed["key"], "uploads/single.bin"; got != want {
		t.Errorf("confirmed key = %q, want %q", got, want)
	}
	if got, want := srv.confirmed["size"], strconv.Itoa(len(payload)); got != want {
		t.Errorf("confirmed size = %q, want %q", got, want)
	}
}

func TestZincDriveUploadMultipart(t *testing.T) {
	srv := newZincTestServer(t)

	// Shrink the part size so a handful of bytes span several parts.
	originalPart := zdMultipartPartSize
	zdMultipartPartSize = 4
	t.Cleanup(func() { zdMultipartPartSize = originalPart })

	payload := []byte("0123456789") // 10 bytes -> 3 parts
	creds := plugin.Credentials{"API_TOKEN": "test-key"}

	res, err := (&ZincDrive{}).uploadMultipart(context.Background(), "song.flac",
		bytes.NewReader(payload), int64(len(payload)), creds["API_TOKEN"], nil)
	if err != nil {
		t.Fatalf("uploadMultipart returned error: %v", err)
	}

	if got, want := res.URL, "https://zincdrive.com/file/xyz789"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}

	// Parts must be stored in order and reassemble to the original payload.
	var rebuilt []byte
	for n := 1; n <= 3; n++ {
		rebuilt = append(rebuilt, srv.uploaded("/storage/part/"+strconv.Itoa(n))...)
	}
	if !bytes.Equal(rebuilt, payload) {
		t.Errorf("parts reassemble to %q, want %q", rebuilt, payload)
	}

	// Completion must carry every ETag, in the AWS parts form.
	srv.mu.Lock()
	complete := srv.complete
	srv.mu.Unlock()

	parts, ok := complete["parts"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("complete parts = %#v, want 3 entries", complete["parts"])
	}
	for i, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("part %d is %T, want object", i+1, raw)
		}
		if got := part["ETag"]; got == nil || got == "" {
			t.Errorf("part %d has no ETag", i+1)
		}
		if got := int(part["PartNumber"].(float64)); got != i+1 {
			t.Errorf("part %d PartNumber = %d", i+1, got)
		}
	}

	if got, want := srv.confirmed["key"], "uploads/multipart.bin"; got != want {
		t.Errorf("confirmed key = %q, want %q", got, want)
	}
}

func TestZincDriveDispatchesBySize(t *testing.T) {
	srv := newZincTestServer(t)

	// Pretend the single-part ceiling is tiny so a short payload must take the
	// multipart path.
	originalMax := zdSinglePartMax
	originalPart := zdMultipartPartSize
	zdSinglePartMax = 8
	zdMultipartPartSize = 64
	t.Cleanup(func() {
		zdSinglePartMax = originalMax
		zdMultipartPartSize = originalPart
	})

	payload := []byte("0123456789abcdef")
	_, err := (&ZincDrive{}).Upload(context.Background(), "big.bin",
		bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "test-key"}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if srv.uploaded("single") != nil {
		t.Error("a large file should not use the single-part endpoint")
	}
	if got := srv.uploaded("/storage/part/1"); !bytes.Equal(got, payload) {
		t.Errorf("multipart part 1 = %q, want %q", got, payload)
	}
}

func TestZincDriveReportsStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/s3/sign", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"error","message":"Rate limit exceeded"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	originalBase := zdBaseURL
	zdBaseURL = srv.URL
	defer func() { zdBaseURL = originalBase }()

	_, err := (&ZincDrive{}).Upload(context.Background(), "test.bin",
		strings.NewReader("data"), 4, plugin.Credentials{"API_TOKEN": "test-key"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The retry classifier in internal/uploader matches this exact form.
	if !strings.Contains(err.Error(), "status 429") {
		t.Errorf("error = %v, want it to contain %q", err, "status 429")
	}
}

func TestStatusErrorShape(t *testing.T) {
	err := statusError("svc", http.StatusServiceUnavailable, []byte(`{"error":"down"}`))
	msg := err.Error()
	if !strings.Contains(msg, "status 503") {
		t.Errorf("error = %q, want it to contain %q", msg, "status 503")
	}
	if !strings.Contains(msg, "svc") {
		t.Errorf("error = %q, want it to name the service", msg)
	}
	if !strings.Contains(msg, "down") {
		t.Errorf("error = %q, want it to carry the response body", msg)
	}
}

func TestZincSignedURL(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bare url", "https://s3.example/part?x=1", "https://s3.example/part?x=1"},
		{"json url", `{"url":"https://s3.example/part?x=2"}`, "https://s3.example/part?x=2"},
		{"json signed_url", `{"signed_url":"https://s3.example/part?x=3"}`, "https://s3.example/part?x=3"},
		{"unusable", `{"status":"error"}`, ""},
		{"garbage", "<html>nope</html>", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := zdSignedURL([]byte(c.body)); got != c.want {
				t.Errorf("zdSignedURL(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

func TestZincDriveRequiresAPIKey(t *testing.T) {
	_, err := (&ZincDrive{}).Upload(context.Background(), "test.bin", strings.NewReader("x"), 1, plugin.Credentials{}, nil)
	if err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("err = %v, want an API_TOKEN error", err)
	}
}
