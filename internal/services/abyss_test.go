package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/driftywinds/manyup/internal/plugin"
)

const abyssSuccessBody = `{"status":true,"msg":"Complete","slug":"abc123","urlIframe":"https://player.abyssplayer.com/abc123"}`

type abyssTestServer struct {
	*httptest.Server

	mu              sync.Mutex
	status          int
	body            string
	aboutStatus     int
	aboutBody       string
	aboutKey        string
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

func newAbyssTestServer(t *testing.T) *abyssTestServer {
	t.Helper()

	s := &abyssTestServer{
		status:      http.StatusOK,
		body:        abyssSuccessBody,
		aboutStatus: http.StatusOK,
		aboutBody:   `{"storageQuota":{"limit":1,"actual":0,"usage":0},"maxUploadSize":10737418240}`,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/about", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.aboutKey = r.URL.Query().Get("key")
		status, body := s.aboutStatus, s.aboutBody
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading upload body: %v", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("parsing Content-Type: %v", err)
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		if mediaType != "multipart/form-data" {
			t.Errorf("media type = %q, want multipart/form-data", mediaType)
		}

		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		partCount := 0
		fileName, fileContentType := "", ""
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
		status, responseBody := s.status, s.body
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, responseBody)
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	originalUpload := abyssUploadBaseURL
	originalAPI := abyssAPIBaseURL
	abyssUploadBaseURL = s.URL
	abyssAPIBaseURL = s.URL
	t.Cleanup(func() {
		abyssUploadBaseURL = originalUpload
		abyssAPIBaseURL = originalAPI
	})

	return s
}

func (s *abyssTestServer) setUploadResponse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.body = body
}

func (s *abyssTestServer) setAboutResponse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aboutStatus = status
	s.aboutBody = body
}

type abyssSnapshot struct {
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
	aboutKey        string
}

func (s *abyssTestServer) snapshot() abyssSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return abyssSnapshot{
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
		aboutKey:        s.aboutKey,
	}
}

func TestAbyssRegistered(t *testing.T) {
	registered, ok := All().Get("abyss")
	if !ok {
		t.Fatal("abyss is not registered")
	}
	if _, ok := registered.(*Abyss); !ok {
		t.Errorf("registered plugin is %T, want *Abyss", registered)
	}
}

func TestAbyssUpload(t *testing.T) {
	srv := newAbyssTestServer(t)
	payload := []byte("pretend this is a video")

	res, err := (&Abyss{}).Upload(
		context.Background(),
		filepath.Join("clips", "movie.mkv"),
		bytes.NewReader(payload),
		int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "test-api-key"},
		nil,
	)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.Service, "abyss"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.URL, "https://abysscdn.com/?v=abc123"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}

	got := srv.snapshot()
	if got.uploadCalls != 1 {
		t.Errorf("upload calls = %d, want 1", got.uploadCalls)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/test-api-key" {
		t.Errorf("path = %q, want /test-api-key", got.path)
	}
	if got.authorization != "" {
		t.Errorf("Authorization = %q, want none", got.authorization)
	}
	if got.contentLength != int64(got.bodyLength) {
		t.Errorf("Content-Length = %d, body = %d", got.contentLength, got.bodyLength)
	}
	if got.partCount != 1 {
		t.Errorf("parts = %d, want 1", got.partCount)
	}
	if got.fileName != "movie.mkv" {
		t.Errorf("filename = %q, want movie.mkv", got.fileName)
	}
	if got.fileContentType != "video/x-matroska" {
		t.Errorf("part Content-Type = %q, want video/x-matroska", got.fileContentType)
	}
	if !bytes.Equal(got.fileBody, payload) {
		t.Errorf("file body = %q, want %q", got.fileBody, payload)
	}
}

func TestAbyssUploadErrorStatus(t *testing.T) {
	srv := newAbyssTestServer(t)
	srv.setUploadResponse(http.StatusUnsupportedMediaType, `{"status":false,"msg":"unsupported format"}`)

	_, err := (&Abyss{}).Upload(
		context.Background(), "bad.txt", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "test-api-key"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status 415") {
		t.Errorf("error = %v, want status 415", err)
	}
}

func TestAbyssUploadStatusFalse(t *testing.T) {
	srv := newAbyssTestServer(t)
	srv.setUploadResponse(http.StatusOK, `{"status":false,"msg":"bad key"}`)

	_, err := (&Abyss{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "bad"}, nil)
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err = %v, want bad key", err)
	}
}

func TestAbyssUploadMissingSlug(t *testing.T) {
	srv := newAbyssTestServer(t)
	srv.setUploadResponse(http.StatusOK, `{"status":true,"msg":"Complete"}`)

	_, err := (&Abyss{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "slug") {
		t.Fatalf("err = %v, want a missing-slug error", err)
	}
}

func TestAbyssRejectsEmptyFile(t *testing.T) {
	_, err := (&Abyss{}).Upload(
		context.Background(), "empty.mp4", strings.NewReader(""), 0,
		plugin.Credentials{"API_TOKEN": "key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty-file error", err)
	}
}

func TestAbyssUploadRequiresKey(t *testing.T) {
	_, err := (&Abyss{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("err = %v, want an API_TOKEN error", err)
	}
}

func TestAbyssVerifyCredentials(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantErr      bool
		wantRejected bool
	}{
		{name: "valid", status: http.StatusOK},
		{name: "invalid", status: http.StatusUnauthorized, wantErr: true, wantRejected: true},
		{name: "service down", status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newAbyssTestServer(t)
			srv.setAboutResponse(c.status, `{"error":"nope"}`)

			err := (&Abyss{}).VerifyCredentials(
				context.Background(), plugin.Credentials{"API_TOKEN": "test-key"}, nil)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got := errors.Is(err, plugin.ErrCredentialRejected); got != c.wantRejected {
				t.Errorf("ErrCredentialRejected = %v, want %v", got, c.wantRejected)
			}
			if got, want := srv.snapshot().aboutKey, "test-key"; got != want {
				t.Errorf("about key = %q, want %q", got, want)
			}
		})
	}
}

func TestAbyssVerifyRejectsEmptyKey(t *testing.T) {
	err := (&Abyss{}).VerifyCredentials(context.Background(), nil, nil)
	if !errors.Is(err, plugin.ErrCredentialRejected) {
		t.Errorf("err = %v, want ErrCredentialRejected", err)
	}
}

func TestAbyssUploadResponseJSON(t *testing.T) {
	var response abyssUploadResponse
	if err := json.Unmarshal([]byte(abyssSuccessBody), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if response.Status == nil || !*response.Status || response.Slug != "abc123" {
		t.Errorf("response = %+v", response)
	}
}
