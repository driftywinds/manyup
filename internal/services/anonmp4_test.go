package services

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const anonMP4SuccessBody = `{
  "success": true,
  "video_id": "NLEusIzEvLs3s3B",
  "title": "BigBuckBunny",
  "thumbnail": "https://cdn.anonmp4.to/thumbs/NLEusIzEvLs3s3B.webp",
  "watch_url": "https://anonmp4.to/v/NLEusIzEvLs3s3B",
  "embed_url": "https://anonmp4.to/embed/NLEusIzEvLs3s3B",
  "delete_url": "https://anonmp4.to/delete?id=NLEusIzEvLs3s3B&hash=uniqehash",
  "upload_date": "2026-01-08T10:30:00Z",
  "message": "File received and queued for processing"
}`

// anonMP4TestServer stands in for the AnonMP4 upload endpoint.
type anonMP4TestServer struct {
	*httptest.Server

	mu              sync.Mutex
	status          int
	responseBody    string
	uploadCalls     int
	method          string
	path            string
	authorization   string
	contentLength   int64
	bodyLength      int
	partCount       int
	fileName        string
	fileContentType string
	fileBody        []byte
}

func newAnonMP4TestServer(t *testing.T) *anonMP4TestServer {
	t.Helper()

	s := &anonMP4TestServer{
		status:       http.StatusOK,
		responseBody: anonMP4SuccessBody,
	}

	mux := http.NewServeMux()
	mux.HandleFunc(anonMP4UploadPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("parsing Content-Type %q: %v", r.Header.Get("Content-Type"), err)
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		if mediaType != "multipart/form-data" {
			t.Errorf("Content-Type media type = %q, want multipart/form-data", mediaType)
		}

		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		partCount := 0
		fileName := ""
		fileContentType := ""
		var fileBody []byte

		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("reading multipart part: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			partCount++
			if part.FormName() != "file" {
				t.Errorf("part form name = %q, want file", part.FormName())
			}
			fileName = part.FileName()
			fileContentType = part.Header.Get("Content-Type")
			fileBody, _ = io.ReadAll(part)
			part.Close()
		}

		s.mu.Lock()
		s.uploadCalls++
		s.method = r.Method
		s.path = r.URL.Path
		s.authorization = r.Header.Get("Authorization")
		s.contentLength = r.ContentLength
		s.bodyLength = len(body)
		s.partCount = partCount
		s.fileName = fileName
		s.fileContentType = fileContentType
		s.fileBody = fileBody
		status, responseBody := s.status, s.responseBody
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, responseBody)
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	// These tests exercise the documented single-POST fallback. Point the live
	// node API at a server that refuses allocations so Upload falls through to
	// the legacy endpoint.
	nodeUnavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "node unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(nodeUnavailable.Close)

	originalBase := anonMP4APIBase
	anonMP4APIBase = s.URL
	t.Cleanup(func() { anonMP4APIBase = originalBase })

	originalNodeBase := anonMP4WebBaseURL
	anonMP4WebBaseURL = nodeUnavailable.URL
	t.Cleanup(func() { anonMP4WebBaseURL = originalNodeBase })

	return s
}

func (s *anonMP4TestServer) setResponse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.responseBody = body
}

type anonMP4Snapshot struct {
	uploadCalls     int
	method          string
	path            string
	authorization   string
	contentLength   int64
	bodyLength      int
	partCount       int
	fileName        string
	fileContentType string
	fileBody        []byte
}

func (s *anonMP4TestServer) snapshot() anonMP4Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return anonMP4Snapshot{
		uploadCalls:     s.uploadCalls,
		method:          s.method,
		path:            s.path,
		authorization:   s.authorization,
		contentLength:   s.contentLength,
		bodyLength:      s.bodyLength,
		partCount:       s.partCount,
		fileName:        s.fileName,
		fileContentType: s.fileContentType,
		fileBody:        append([]byte(nil), s.fileBody...),
	}
}

func TestAnonMP4Registered(t *testing.T) {
	registered, ok := All().Get("anonmp4")
	if !ok {
		t.Fatal("anonmp4 is not registered")
	}
	if _, ok := registered.(*AnonMP4); !ok {
		t.Errorf("registered plugin is %T, want *AnonMP4", registered)
	}
}

func TestAnonMP4Upload(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	payload := []byte("pretend this is a video")

	res, err := (&AnonMP4{}).Upload(
		context.Background(),
		filepath.Join("clips", "movie.mkv"),
		bytes.NewReader(payload),
		int64(len(payload)),
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.Service, "anonmp4"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.URL, "https://anonmp4.to/v/NLEusIzEvLs3s3B"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
	if got, want := res.Filename, filepath.Join("clips", "movie.mkv"); got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}

	got := srv.snapshot()
	if got.uploadCalls != 1 {
		t.Errorf("upload calls = %d, want 1", got.uploadCalls)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/upload" {
		t.Errorf("path = %q, want /upload", got.path)
	}
	if got.authorization != "" {
		t.Errorf("Authorization = %q, want none", got.authorization)
	}
	if got.contentLength != int64(got.bodyLength) {
		t.Errorf("Content-Length = %d, actual body = %d", got.contentLength, got.bodyLength)
	}
	if got.partCount != 1 {
		t.Errorf("multipart parts = %d, want 1", got.partCount)
	}
	if got.fileName != "movie.mkv" {
		t.Errorf("multipart filename = %q, want movie.mkv", got.fileName)
	}
	if got.fileContentType != "video/x-matroska" {
		t.Errorf("multipart Content-Type = %q, want video/x-matroska", got.fileContentType)
	}
	if !bytes.Equal(got.fileBody, payload) {
		t.Errorf("multipart file body = %q, want %q", got.fileBody, payload)
	}
}

func TestAnonMP4UploadStreamsLargeBody(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	payload := bytes.Repeat([]byte("fake-video-frame"), 64*1024) // ~1 MiB

	res, err := (&AnonMP4{}).Upload(
		context.Background(), "large.mp4", bytes.NewReader(payload), int64(len(payload)), nil, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}

	got := srv.snapshot()
	if got.contentLength != int64(got.bodyLength) {
		t.Errorf("Content-Length = %d, actual body = %d", got.contentLength, got.bodyLength)
	}
	if !bytes.Equal(got.fileBody, payload) {
		t.Error("streamed file body does not match the payload")
	}
}

func TestAnonMP4UploadEscapesFilename(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	payload := []byte("x")

	if _, err := (&AnonMP4{}).Upload(
		context.Background(), `a"b.mp4`, bytes.NewReader(payload), int64(len(payload)), nil, nil); err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := srv.snapshot().fileName, `a"b.mp4`; got != want {
		t.Errorf("multipart filename = %q, want %q", got, want)
	}
}

func TestAnonMP4UploadErrorEnvelope(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	srv.setResponse(http.StatusRequestEntityTooLarge, `{
  "success": false,
  "error": {
    "code": "E_INVALID_SIZE",
    "message": "File size exceeds the 20GB limit",
    "details": {"max_size": 53687091200, "received_size": 107374182400}
  }
}`)

	_, err := (&AnonMP4{}).Upload(
		context.Background(), "huge.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"status 413", "E_INVALID_SIZE", "File size exceeds", "max_size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestAnonMP4UploadRateLimitKeepsStatus(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	srv.setResponse(http.StatusTooManyRequests, "slow down")

	_, err := (&AnonMP4{}).Upload(
		context.Background(), "video.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Errorf("error = %v, want it to contain %q", err, "status 429")
	}
}

func TestAnonMP4UploadSuccessFalse(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	srv.setResponse(http.StatusOK, `{
  "success": false,
  "error": {"code": "E_UPLOAD_FAILED", "message": "Upload processing failed"}
}`)

	_, err := (&AnonMP4{}).Upload(
		context.Background(), "video.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "E_UPLOAD_FAILED") {
		t.Errorf("error = %v, want it to contain E_UPLOAD_FAILED", err)
	}
}

func TestAnonMP4UploadMissingWatchURL(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	srv.setResponse(http.StatusOK, `{"success": true, "video_id": "abc"}`)

	_, err := (&AnonMP4{}).Upload(
		context.Background(), "video.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "watch_url") {
		t.Fatalf("err = %v, want a missing watch_url error", err)
	}
}

func TestAnonMP4UploadMalformedResponse(t *testing.T) {
	srv := newAnonMP4TestServer(t)
	srv.setResponse(http.StatusOK, "<html>not json</html>")

	_, err := (&AnonMP4{}).Upload(
		context.Background(), "video.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "parsing response") {
		t.Fatalf("err = %v, want a parsing error", err)
	}
}

func TestAnonMP4RejectsEmptyFile(t *testing.T) {
	_, err := (&AnonMP4{}).Upload(
		context.Background(), "empty.mp4", strings.NewReader(""), 0, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty-file error", err)
	}
}

func TestAnonMP4ContentType(t *testing.T) {
	cases := map[string]string{
		"movie.mp4":         "video/mp4",
		"movie.MKV":         "video/x-matroska",
		"movie.webm":        "video/webm",
		"movie.avi":         "video/x-msvideo",
		"movie.mov":         "video/quicktime",
		"movie.unknown":     "application/octet-stream",
		"movie.audio-file":  "application/octet-stream",
	}
	for name, want := range cases {
		if got := anonMP4ContentType(name); got != want {
			t.Errorf("anonMP4ContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAnonMP4FileName(t *testing.T) {
	cases := map[string]string{
		"movie.mp4":                 "movie.mp4",
		filepath.Join("a", "b.mp4"): "b.mp4",
		"":                          "video",
		".hidden":                   ".hidden",
		"bad\r\nname.mp4":           "bad__name.mp4",
	}
	for input, want := range cases {
		if got := anonMP4FileName(input); got != want {
			t.Errorf("anonMP4FileName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAnonMP4ErrorString(t *testing.T) {
	full := &anonMP4Error{
		Code:    "E_INVALID_SIZE",
		Message: "File size exceeds the 20GB limit",
		Details: json.RawMessage(`{"max_size": 10, "received_size": 20}`),
	}
	if got := full.String(); !strings.Contains(got, "E_INVALID_SIZE") || !strings.Contains(got, "max_size") {
		t.Errorf("String() = %q, want code and details", got)
	}

	if got, want := (&anonMP4Error{Code: "E_NO_FILE"}).String(), "E_NO_FILE"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (*anonMP4Error)(nil).String(), "unknown error"; got != want {
		t.Errorf("nil String() = %q, want %q", got, want)
	}
}
