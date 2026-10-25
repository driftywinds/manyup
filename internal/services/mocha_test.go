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

// mochaTestServer stands in for the Mocha API and records what the plugin sends.
type mochaTestServer struct {
	*httptest.Server

	mu        sync.Mutex
	parts     map[int][]byte
	etagsSent map[int]string
	completed bool
	uploadIDs []string
	shareBody map[string]any
}

// newMochaTestServer wires the multipart endpoints. directPartUpload selects
// between the presigned and the deprecated relay path, and partSize controls
// how many parts the file is split into.
func newMochaTestServer(t *testing.T, directPartUpload bool, partSize int64) *mochaTestServer {
	t.Helper()

	s := &mochaTestServer{
		parts:     make(map[int][]byte),
		etagsSent: make(map[int]string),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/files/multipart/init", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("init: authorization = %q", got)
		}
		var req struct {
			OriginalName string `json:"originalName"`
			Size         int64  `json:"size"`
			MimeType     string `json:"mimeType"`
			Path         string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("init: decoding body: %v", err)
		}
		if req.OriginalName != "song.flac" {
			t.Errorf("init: originalName = %q", req.OriginalName)
		}
		if req.MimeType == "" {
			t.Errorf("init: mimeType was empty")
		}
		if req.Path != "/" {
			t.Errorf("init: path = %q, want /", req.Path)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"uploadId":              "upload-1",
			"key":                   "key-1",
			"nodeId":                "node-1",
			"originalName":          req.OriginalName,
			"purpose":               "normal",
			"partSizeBytes":         partSize,
			"directPartSizeBytes":   partSize,
			"partUploadConcurrency": 4,
			"directPartUpload":      directPartUpload,
		})
	})

	mux.HandleFunc("/api/files/multipart/presigned", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UploadID    string `json:"uploadId"`
			Key         string `json:"key"`
			NodeID      string `json:"nodeId"`
			PartNumbers []int  `json:"partNumbers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("presign: decoding body: %v", err)
		}
		if req.UploadID != "upload-1" || req.Key != "key-1" || req.NodeID != "node-1" {
			t.Errorf("presign: wrong session: %+v", req)
		}

		type part struct {
			PartNumber int    `json:"partNumber"`
			URL        string `json:"url"`
		}
		urls := make([]part, 0, len(req.PartNumbers))
		for _, n := range req.PartNumbers {
			urls = append(urls, part{
				PartNumber: n,
				URL:        s.URL + "/storage/part/" + strconv.Itoa(n),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"urls": urls})
	})

	mux.HandleFunc("/storage/part/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("storage: method = %s", r.Method)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/storage/part/"))
		if err != nil {
			t.Errorf("storage: bad part number in %q", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("storage: reading part %d: %v", n, err)
		}
		if r.ContentLength != int64(len(body)) {
			t.Errorf("storage: part %d content-length = %d, read %d", n, r.ContentLength, len(body))
		}
		if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("storage: part %d content-type = %q", n, got)
		}

		etag := `"etag-` + strconv.Itoa(n) + `"`
		s.mu.Lock()
		s.parts[n] = body
		s.etagsSent[n] = etag
		s.mu.Unlock()

		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/files/multipart/part", func(w http.ResponseWriter, r *http.Request) {
		// Deprecated relay path: chunk arrives with the session in the query.
		n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		if got := r.URL.Query().Get("uploadId"); got != "upload-1" {
			t.Errorf("relay: uploadId = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("relay: authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)

		etag := "relay-etag-" + strconv.Itoa(n)
		s.mu.Lock()
		s.parts[n] = body
		s.etagsSent[n] = etag
		s.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{"etag": etag})
	})

	mux.HandleFunc("/api/files/multipart/complete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UploadID string `json:"uploadId"`
			Size     int64  `json:"size"`
			Parts    []struct {
				PartNumber int    `json:"partNumber"`
				ETag       string `json:"etag"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("complete: decoding body: %v", err)
		}
		if req.UploadID != "upload-1" {
			t.Errorf("complete: uploadId = %q", req.UploadID)
		}
		if len(req.Parts) == 0 {
			t.Errorf("complete: no parts submitted")
		}
		for _, p := range req.Parts {
			want := s.etagsSent[p.PartNumber]
			if p.ETag != want {
				t.Errorf("complete: part %d etag = %q, want %q", p.PartNumber, p.ETag, want)
			}
		}
		s.mu.Lock()
		s.completed = true
		s.mu.Unlock()

		// Completion answers 201 Created, not 200.
		writeJSON(w, http.StatusCreated, map[string]any{
			"file": map[string]any{
				"id":            "file-uuid",
				"file_name":     "stored-name",
				"original_name": "song.flac",
				"size":          req.Size,
				"status":        "finalizing",
			},
		})
	})

	mux.HandleFunc("/api/files/multipart/abort", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})

	mux.HandleFunc("/api/shares", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("share: decoding body: %v", err)
		}
		if req["fileId"] != "file-uuid" {
			t.Errorf("share: fileId = %v", req["fileId"])
		}
		// A nil expiresInHours must be sent explicitly as JSON null.
		if v, present := req["expiresInHours"]; !present || v != nil {
			t.Errorf("share: expiresInHours = %v (present=%v), want explicit null", v, present)
		}
		s.mu.Lock()
		s.shareBody = req
		s.mu.Unlock()

		writeJSON(w, http.StatusCreated, map[string]any{
			"share": map[string]any{
				"token":             "share-token",
				"file_id":           "file-uuid",
				"is_permanent_link": false,
			},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	originalAPI := mochaAPIBase
	mochaAPIBase = s.URL
	t.Cleanup(func() { mochaAPIBase = originalAPI })

	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// joined returns the parts concatenated in part-number order.
func (s *mochaTestServer) joined() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []byte
	for n := 1; n <= len(s.parts); n++ {
		out = append(out, s.parts[n]...)
	}
	return out
}

func TestMochaUploadPresigned(t *testing.T) {
	srv := newMochaTestServer(t, true, 4)

	payload := []byte("abcdefghij") // 10 bytes → 3 parts at 4 bytes
	creds := plugin.Credentials{"API_TOKEN": "test-key"}

	res, err := (&Mocha{}).Upload(context.Background(), "/tmp/song.flac", bytes.NewReader(payload), int64(len(payload)), creds, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.URL, mochaShareBase+"share-token"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Service, "mocha"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}

	if got := srv.joined(); !bytes.Equal(got, payload) {
		t.Errorf("server received %q, want %q", got, payload)
	}
	if len(srv.parts) != 3 {
		t.Errorf("uploaded %d parts, want 3", len(srv.parts))
	}
	if !srv.completed {
		t.Errorf("upload was never completed")
	}
	if srv.shareBody == nil {
		t.Errorf("no share was created")
	}
}

func TestMochaUploadRelayFallback(t *testing.T) {
	// directPartUpload=false means the node cannot presign, so the plugin must
	// fall back to the relay endpoint.
	srv := newMochaTestServer(t, false, 4)

	payload := []byte("0123456789abcdef") // 16 bytes → 4 parts at 4 bytes
	creds := plugin.Credentials{"API_TOKEN": "test-key"}

	res, err := (&Mocha{}).Upload(context.Background(), "song.flac", bytes.NewReader(payload), int64(len(payload)), creds, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if got, want := res.URL, mochaShareBase+"share-token"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got := srv.joined(); !bytes.Equal(got, payload) {
		t.Errorf("server received %q, want %q", got, payload)
	}
	if len(srv.parts) != 4 {
		t.Errorf("uploaded %d parts, want 4", len(srv.parts))
	}
}

func TestMochaRequiresAPIKey(t *testing.T) {
	newMochaTestServer(t, true, 4)

	_, err := (&Mocha{}).Upload(context.Background(), "song.flac", strings.NewReader("data"), 4, plugin.Credentials{}, nil)
	if err == nil {
		t.Fatal("expected an error when API_TOKEN is missing")
	}
	if !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("error = %v, want it to mention API_TOKEN", err)
	}
}

func TestMochaRequiresSize(t *testing.T) {
	newMochaTestServer(t, true, 4)

	_, err := (&Mocha{}).Upload(context.Background(), "song.flac", strings.NewReader("data"), 0,
		plugin.Credentials{"API_TOKEN": "test-key"}, nil)
	if err == nil {
		t.Fatal("expected an error when the size is unknown")
	}
}

func TestMochaUploadPropagatesStatus(t *testing.T) {
	// The retry classifier downstream looks for "status <code>", so a server
	// failure has to keep its status code in the error message.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/files/multipart/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "No storage nodes available"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	originalAPI := mochaAPIBase
	mochaAPIBase = srv.URL
	defer func() { mochaAPIBase = originalAPI }()

	_, err := (&Mocha{}).Upload(context.Background(), "song.flac", strings.NewReader("data"), 4,
		plugin.Credentials{"API_TOKEN": "test-key"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Errorf("error = %v, want it to contain %q", err, "status 503")
	}
	if !strings.Contains(err.Error(), "No storage nodes available") {
		t.Errorf("error = %v, want it to carry the API message", err)
	}
}

func TestMochaChunkSize(t *testing.T) {
	cases := []struct {
		size       int64
		partSize   int64
		partNumber int
		want       int64
	}{
		{10, 4, 1, 4},
		{10, 4, 2, 4},
		{10, 4, 3, 2}, // last part is short
		{8, 4, 2, 4},
		{1, 4, 1, 1},
	}
	for _, c := range cases {
		if got := mochaChunkSize(c.size, c.partSize, c.partNumber); got != c.want {
			t.Errorf("mochaChunkSize(%d, %d, %d) = %d, want %d", c.size, c.partSize, c.partNumber, got, c.want)
		}
	}
}

func TestMochaPath(t *testing.T) {
	cases := []struct {
		name string
		cfg  plugin.Config
		want string
	}{
		{"nil config", nil, "/"},
		{"empty", plugin.Config{}, "/"},
		{"blank value", plugin.Config{"path": "   "}, "/"},
		{"root", plugin.Config{"path": "/"}, "/"},
		{"no trailing slash", plugin.Config{"path": "/test"}, "/test/"},
		{"no leading slash", plugin.Config{"path": "test/"}, "/test/"},
		{"already normalised", plugin.Config{"path": "/test/"}, "/test/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mochaPath(c.cfg); got != c.want {
				t.Errorf("mochaPath(%v) = %q, want %q", c.cfg, got, c.want)
			}
		})
	}
}

func TestMochaMimeType(t *testing.T) {
	// The exact spelling of a known type comes from the platform's MIME
	// registry (audio/flac vs audio/x-flac), so only the family is asserted.
	if got := mochaMimeType("song.flac"); !strings.HasPrefix(got, "audio/") {
		t.Errorf("mochaMimeType(song.flac) = %q, want an audio/* type", got)
	}
	if got := mochaMimeType("archive.unknown-ext"); got != "application/octet-stream" {
		t.Errorf("mochaMimeType(unknown) = %q, want application/octet-stream", got)
	}
}
