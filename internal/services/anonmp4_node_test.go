package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type anonMP4ChunkRequest struct {
	fileID             string
	filename           string
	contentRange       string
	contentDisposition string
	contentType        string
	uploadMode         string
	totalChunks        int
	data               []byte
}

// anonMP4NodeTestServer stands in for the website's upload-node control API and
// the Cloudflare Worker that receives chunks.
type anonMP4NodeTestServer struct {
	*httptest.Server

	mu             sync.Mutex
	uploadNodeHits int
	videoNodeHits  int
	chunkHits      int
	chunkFailures  int
	clientToken    string
	uploadFilename string
	videoFilename  string
	videoStatus    int
	videoBody      string
	chunks         map[int]anonMP4ChunkRequest
}

type anonMP4NodeSnapshot struct {
	uploadNodeHits int
	videoNodeHits  int
	chunkHits      int
	clientToken    string
	uploadFilename string
	videoFilename  string
	chunks         map[int]anonMP4ChunkRequest
}

func newAnonMP4NodeTestServer(t *testing.T) *anonMP4NodeTestServer {
	t.Helper()

	s := &anonMP4NodeTestServer{
		chunks: make(map[int]anonMP4ChunkRequest),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/get-upload-node", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("get-upload-node form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.uploadNodeHits++
		s.clientToken = r.FormValue("client_token")
		s.uploadFilename = r.FormValue("filename")
		s.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     true,
			"node_id":    "node-1",
			"upload_url": "http://" + r.Host + "/chunk",
			"node_token": "node-token-1",
		})
	})

	mux.HandleFunc("/chunk", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("chunk method = %s, want POST", r.Method)
		}

		index, err := strconv.Atoi(r.URL.Query().Get("chunkIndex"))
		if err != nil {
			t.Errorf("chunkIndex = %q: %v", r.URL.Query().Get("chunkIndex"), err)
			http.Error(w, "bad index", http.StatusBadRequest)
			return
		}
		totalChunks, _ := strconv.Atoi(r.URL.Query().Get("totalChunks"))
		data, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		s.chunkHits++
		s.chunks[index] = anonMP4ChunkRequest{
			fileID:             r.URL.Query().Get("file_id"),
			filename:           r.URL.Query().Get("filename"),
			contentRange:       r.Header.Get("Content-Range"),
			contentDisposition: r.Header.Get("Content-Disposition"),
			contentType:        r.Header.Get("Content-Type"),
			uploadMode:         r.Header.Get("X-Uploading-Mode"),
			totalChunks:        totalChunks,
			data:               data,
		}
		fail := s.chunkFailures > 0
		if fail {
			s.chunkFailures--
		}
		s.mu.Unlock()

		if fail {
			http.Error(w, "transient chunk failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%d-%d/%d", 0, len(data)-1, len(data))
	})

	mux.HandleFunc("/get-video-node", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("get-video-node form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.videoNodeHits++
		s.videoFilename = r.FormValue("filename")
		status, body := s.videoStatus, s.videoBody
		s.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":    true,
			"video_id":   "video-1",
			"title":      "video.mp4",
			"watch_url":  "https://anonmp4.art/v/video-1",
			"embed_url":  "https://anonmp4.art/embed/video-1",
			"delete_url": "https://anonmp4.to/delete?id=video-1&hash=test",
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	originalNodeBase := anonMP4WebBaseURL
	anonMP4WebBaseURL = s.URL
	t.Cleanup(func() { anonMP4WebBaseURL = originalNodeBase })

	return s
}

func (s *anonMP4NodeTestServer) snapshot() anonMP4NodeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunks := make(map[int]anonMP4ChunkRequest, len(s.chunks))
	for index, request := range s.chunks {
		request.data = append([]byte(nil), request.data...)
		chunks[index] = request
	}
	return anonMP4NodeSnapshot{
		uploadNodeHits: s.uploadNodeHits,
		videoNodeHits:  s.videoNodeHits,
		chunkHits:      s.chunkHits,
		clientToken:    s.clientToken,
		uploadFilename: s.uploadFilename,
		videoFilename:  s.videoFilename,
		chunks:         chunks,
	}
}

func (s *anonMP4NodeTestServer) failNextChunks(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkFailures = n
}

func (s *anonMP4NodeTestServer) setVideoResponse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videoStatus = status
	s.videoBody = body
}

func TestAnonMP4NodeUpload(t *testing.T) {
	srv := newAnonMP4NodeTestServer(t)

	size := int(2*anonMP4ChunkSize) + 123
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	res, err := (&AnonMP4{}).Upload(
		context.Background(), "video.mp4", bytes.NewReader(payload), int64(size), nil, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if got, want := res.Service, "anonmp4"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.URL, "https://anonmp4.art/v/video-1"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(size); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}

	got := srv.snapshot()
	if got.uploadNodeHits != 1 {
		t.Errorf("get-upload-node hits = %d, want 1", got.uploadNodeHits)
	}
	if got.videoNodeHits != 1 {
		t.Errorf("get-video-node hits = %d, want 1", got.videoNodeHits)
	}
	if got.chunkHits != 3 {
		t.Errorf("chunk hits = %d, want 3", got.chunkHits)
	}
	if got.uploadFilename != "video.mp4" || got.videoFilename != "video.mp4" {
		t.Errorf("filenames = (%q, %q), want video.mp4", got.uploadFilename, got.videoFilename)
	}
	if got.clientToken != anonMP4ClientToken {
		t.Errorf("client token = %q, want the default token", got.clientToken)
	}

	for index := 1; index <= 3; index++ {
		chunk, ok := got.chunks[index]
		if !ok {
			t.Errorf("chunk %d was not uploaded", index)
			continue
		}
		start := int64(index-1) * anonMP4ChunkSize
		end := start + anonMP4ChunkSize - 1
		if index == 3 {
			end = int64(size) - 1
		}
		if want := fmt.Sprintf("bytes %d-%d/%d", start, end, size); chunk.contentRange != want {
			t.Errorf("chunk %d Content-Range = %q, want %q", index, chunk.contentRange, want)
		}
		if want := "video.mp4"; chunk.filename != want {
			t.Errorf("chunk %d filename = %q, want %q", index, chunk.filename, want)
		}
		if want := "node-token-1"; chunk.fileID != want {
			t.Errorf("chunk %d file_id = %q, want %q", index, chunk.fileID, want)
		}
		if chunk.totalChunks != 3 {
			t.Errorf("chunk %d totalChunks = %d, want 3", index, chunk.totalChunks)
		}
		if chunk.contentType != "application/octet-stream" {
			t.Errorf("chunk %d Content-Type = %q, want application/octet-stream", index, chunk.contentType)
		}
		if chunk.uploadMode != "parallel" {
			t.Errorf("chunk %d X-Uploading-Mode = %q, want parallel", index, chunk.uploadMode)
		}
		if !strings.Contains(chunk.contentDisposition, "video.mp4") {
			t.Errorf("chunk %d Content-Disposition = %q, want video.mp4", index, chunk.contentDisposition)
		}
		startByte := int(start)
		endByte := int(end) + 1
		if !bytes.Equal(chunk.data, payload[startByte:endByte]) {
			t.Errorf("chunk %d payload does not match", index)
		}
	}
}

func TestAnonMP4NodeUploadRetriesChunk(t *testing.T) {
	srv := newAnonMP4NodeTestServer(t)
	srv.failNextChunks(1)

	originalDelay := anonMP4ChunkRetryDelay
	anonMP4ChunkRetryDelay = time.Millisecond
	t.Cleanup(func() { anonMP4ChunkRetryDelay = originalDelay })

	payload := bytes.Repeat([]byte("retry"), 1024)
	if _, err := (&AnonMP4{}).Upload(
		context.Background(), "retry.mp4", bytes.NewReader(payload), int64(len(payload)), nil, nil); err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := srv.snapshot().chunkHits, 2; got != want {
		t.Errorf("chunk hits = %d, want %d", got, want)
	}
}

func TestAnonMP4NodeVideoInfoFailure(t *testing.T) {
	srv := newAnonMP4NodeTestServer(t)
	srv.setVideoResponse(http.StatusOK, `{"success": false, "message": "processing rejected"}`)

	_, err := (&AnonMP4{}).Upload(
		context.Background(), "bad.mp4", strings.NewReader("data"), 4, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "watch_url") && !strings.Contains(err.Error(), "processing rejected") {
		t.Errorf("error = %v, want a video-info failure", err)
	}
}

func TestAnonMP4ChunkRejection(t *testing.T) {
	cases := map[string]string{
		"0-1023/1024": "",
		`{"status":"success"}`: "",
		`{"status":"error","message":"bad chunk"}`: "bad chunk",
		`{"success":false,"message":"nope"}`:       "nope",
		`{"error":{"code":"E_CHUNK","message":"failed"}}`: "E_CHUNK: failed",
	}
	for body, want := range cases {
		if got := anonMP4ChunkRejection([]byte(body)); got != want {
			t.Errorf("anonMP4ChunkRejection(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestAnonMP4EncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"video.mp4":          "video.mp4",
		"a b.mp4":            "a%20b.mp4",
		"a&b.mp4":            "a%26b.mp4",
		"a+b.mp4":            "a%2Bb.mp4",
		"a'b!(c)*.mp4":       "a'b!(c)*.mp4",
	}
	for input, want := range cases {
		if got := anonMP4EncodeURIComponent(input); got != want {
			t.Errorf("anonMP4EncodeURIComponent(%q) = %q, want %q", input, got, want)
		}
	}
}
