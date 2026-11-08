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

const streamtapeSuccessBody = `{"status":200,"msg":"Upload finished!","result":{"url":"https://streamtape.com/v/abc123/movie.mp4","sha256":"deadbeef","name":"movie.mp4","size":"9","content_type":"video/mp4","id":"abc123"}}`

type streamtapeTestServer struct {
	*httptest.Server

	mu sync.Mutex

	login, key, folder string

	slotHTTPStatus    int
	slotAPIStatus     int
	slotMsg           string
	uploadHTTPStatus  int
	uploadAPIStatus   int
	uploadMsg         string
	accountHTTPStatus int
	accountAPIStatus  int
	accountMsg        string

	uploadCalls     int
	contentType     string
	contentLength   int64
	bodyLength      int
	partCount       int
	fileName        string
	fileContentType string
	fileBody        []byte
}

func newStreamtapeTestServer(t *testing.T) *streamtapeTestServer {
	t.Helper()

	s := &streamtapeTestServer{
		slotHTTPStatus:    http.StatusOK,
		slotAPIStatus:     http.StatusOK,
		slotMsg:           "OK",
		uploadHTTPStatus:  http.StatusOK,
		uploadAPIStatus:   http.StatusOK,
		uploadMsg:         "Upload finished!",
		accountHTTPStatus: http.StatusOK,
		accountAPIStatus:  http.StatusOK,
		accountMsg:        "OK",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/file/ul", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.login = r.URL.Query().Get("login")
		s.key = r.URL.Query().Get("key")
		s.folder = r.URL.Query().Get("folder")
		httpStatus, apiStatus, msg := s.slotHTTPStatus, s.slotAPIStatus, s.slotMsg
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    msg,
			"result": map[string]any{
				"url":         s.URL + "/upload-slot",
				"valid_until": "2099-01-01 00:00:00",
			},
		})
	})

	mux.HandleFunc("/upload-slot", func(w http.ResponseWriter, r *http.Request) {
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
			if part.FormName() != "file1" {
				t.Errorf("part form name = %q, want file1", part.FormName())
			}
			fileName = part.FileName()
			fileContentType = part.Header.Get("Content-Type")
			fileBody, _ = io.ReadAll(part)
			part.Close()
		}

		s.mu.Lock()
		s.uploadCalls++
		s.contentType = r.Header.Get("Content-Type")
		s.contentLength = r.ContentLength
		s.bodyLength = len(body)
		s.partCount = partCount
		s.fileName = fileName
		s.fileContentType = fileContentType
		s.fileBody = fileBody
		httpStatus, apiStatus, msg := s.uploadHTTPStatus, s.uploadAPIStatus, s.uploadMsg
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		if apiStatus == http.StatusOK {
			_, _ = io.WriteString(w, streamtapeSuccessBody)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    msg,
			"result": nil,
		})
	})

	mux.HandleFunc("/account/info", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.login = r.URL.Query().Get("login")
		s.key = r.URL.Query().Get("key")
		httpStatus, apiStatus, msg := s.accountHTTPStatus, s.accountAPIStatus, s.accountMsg
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    msg,
			"result": map[string]any{"apiid": "key"},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	original := streamtapeAPIBaseURL
	streamtapeAPIBaseURL = s.URL
	t.Cleanup(func() { streamtapeAPIBaseURL = original })

	return s
}

func (s *streamtapeTestServer) setSlot(apiStatus int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slotAPIStatus = apiStatus
}

func (s *streamtapeTestServer) setUpload(apiStatus int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadAPIStatus = apiStatus
}

func (s *streamtapeTestServer) setAccount(httpStatus, apiStatus int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountHTTPStatus = httpStatus
	s.accountAPIStatus = apiStatus
}

type streamtapeSnapshot struct {
	login, key, folder string
	uploadCalls        int
	contentLength      int64
	bodyLength         int
	partCount          int
	fileName           string
	fileContentType    string
	fileBody           []byte
}

func (s *streamtapeTestServer) snapshot() streamtapeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return streamtapeSnapshot{
		login:           s.login,
		key:             s.key,
		folder:          s.folder,
		uploadCalls:     s.uploadCalls,
		contentLength:   s.contentLength,
		bodyLength:      s.bodyLength,
		partCount:       s.partCount,
		fileName:        s.fileName,
		fileContentType: s.fileContentType,
		fileBody:        append([]byte(nil), s.fileBody...),
	}
}

func TestStreamtapeRegistered(t *testing.T) {
	registered, ok := All().Get("streamtape")
	if !ok {
		t.Fatal("streamtape is not registered")
	}
	if _, ok := registered.(*Streamtape); !ok {
		t.Errorf("registered plugin is %T, want *Streamtape", registered)
	}
}

func TestStreamtapeRequirements(t *testing.T) {
	svc := &Streamtape{}
	required := svc.RequiredCredentials()
	if len(required) != 2 || required[0] != "USERNAME" || required[1] != "API_TOKEN" {
		t.Errorf("RequiredCredentials = %v, want USERNAME and API_TOKEN", required)
	}
	if !svc.SupportsLargeUpload() {
		t.Error("SupportsLargeUpload = false, want true")
	}
}

func TestStreamtapeUpload(t *testing.T) {
	srv := newStreamtapeTestServer(t)
	payload := []byte("video-data")

	res, err := (&Streamtape{}).Upload(
		context.Background(),
		filepath.Join("clips", "movie.mp4"),
		bytes.NewReader(payload),
		int64(len(payload)),
		plugin.Credentials{"USERNAME": "api-login", "API_TOKEN": "api-key"},
		plugin.Config{"folder": "folder-1"},
	)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.Service, "streamtape"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.URL, "https://streamtape.com/v/abc123/movie.mp4"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}

	got := srv.snapshot()
	if got.login != "api-login" || got.key != "api-key" {
		t.Errorf("credentials = (%q, %q)", got.login, got.key)
	}
	if got.folder != "folder-1" {
		t.Errorf("folder = %q, want folder-1", got.folder)
	}
	if got.uploadCalls != 1 {
		t.Errorf("upload calls = %d, want 1", got.uploadCalls)
	}
	if got.contentLength != int64(got.bodyLength) {
		t.Errorf("Content-Length = %d, body = %d", got.contentLength, got.bodyLength)
	}
	if got.partCount != 1 {
		t.Errorf("parts = %d, want 1", got.partCount)
	}
	if got.fileName != "movie.mp4" {
		t.Errorf("filename = %q, want movie.mp4", got.fileName)
	}
	if got.fileContentType != "video/mp4" {
		t.Errorf("part Content-Type = %q, want video/mp4", got.fileContentType)
	}
	if !bytes.Equal(got.fileBody, payload) {
		t.Errorf("file body = %q, want %q", got.fileBody, payload)
	}
}

func TestStreamtapeUploadSlotError(t *testing.T) {
	srv := newStreamtapeTestServer(t)
	srv.setSlot(http.StatusForbidden)

	_, err := (&Streamtape{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"USERNAME": "login", "API_TOKEN": "key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("err = %v, want status 403", err)
	}
}

func TestStreamtapeUploadError(t *testing.T) {
	srv := newStreamtapeTestServer(t)
	srv.setUpload(http.StatusInternalServerError)

	_, err := (&Streamtape{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"USERNAME": "login", "API_TOKEN": "key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v, want status 500", err)
	}
}

func TestStreamtapeMissingCredentials(t *testing.T) {
	for _, creds := range []plugin.Credentials{
		{"API_TOKEN": "key"},
		{"USERNAME": "login"},
		nil,
	} {
		_, err := (&Streamtape{}).Upload(
			context.Background(), "movie.mp4", strings.NewReader("x"), 1, creds, nil)
		if err == nil || !strings.Contains(err.Error(), "USERNAME") {
			t.Errorf("Upload(%v) err = %v, want a credential error", creds, err)
		}
	}
}

func TestStreamtapeRejectsEmptyFile(t *testing.T) {
	_, err := (&Streamtape{}).Upload(
		context.Background(), "empty.mp4", strings.NewReader(""), 0,
		plugin.Credentials{"USERNAME": "login", "API_TOKEN": "key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty-file error", err)
	}
}

func TestStreamtapeVerifyCredentials(t *testing.T) {
	cases := []struct {
		name         string
		httpStatus   int
		apiStatus    int
		wantErr      bool
		wantRejected bool
	}{
		{name: "valid", httpStatus: http.StatusOK, apiStatus: http.StatusOK},
		{name: "bad api key", httpStatus: http.StatusOK, apiStatus: http.StatusForbidden, wantErr: true, wantRejected: true},
		{name: "bad http auth", httpStatus: http.StatusForbidden, apiStatus: http.StatusOK, wantErr: true, wantRejected: true},
		{name: "bandwidth", httpStatus: http.StatusOK, apiStatus: 509, wantErr: true},
		{name: "server error", httpStatus: http.StatusOK, apiStatus: http.StatusInternalServerError, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStreamtapeTestServer(t)
			srv.setAccount(c.httpStatus, c.apiStatus)

			err := (&Streamtape{}).VerifyCredentials(
				context.Background(),
				plugin.Credentials{"USERNAME": "login", "API_TOKEN": "key"},
				nil,
			)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got := errors.Is(err, plugin.ErrCredentialRejected); got != c.wantRejected {
				t.Errorf("ErrCredentialRejected = %v, want %v", got, c.wantRejected)
			}
			if got := srv.snapshot(); got.login != "login" || got.key != "key" {
				t.Errorf("credentials = (%q, %q)", got.login, got.key)
			}
		})
	}
}

func TestStreamtapeVerifyMissingCounterpart(t *testing.T) {
	err := (&Streamtape{}).VerifyCredentials(
		context.Background(), plugin.Credentials{"USERNAME": "login"}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing API_TOKEN")
	}
	if errors.Is(err, plugin.ErrCredentialRejected) {
		t.Errorf("missing counterpart must not reject the credential: %v", err)
	}
}
