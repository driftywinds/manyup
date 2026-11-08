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

type earnvidsTestServer struct {
	*httptest.Server

	mu sync.Mutex

	key, folder string

	serverHTTPStatus  int
	serverAPIStatus   int
	serverMsg         string
	uploadHTTPStatus  int
	uploadAPIStatus   int
	uploadMsg         string
	uploadFileCode    string
	uploadFileStatus  string
	listHTTPStatus    int
	listAPIStatus     int
	listLink          string
	accountHTTPStatus int
	accountAPIStatus  int
	accountMsg        string

	uploadCalls     int
	contentLength   int64
	bodyLength      int
	partCount       int
	fileName        string
	fileContentType string
	fileBody        []byte
}

func newEarnvidsTestServer(t *testing.T) *earnvidsTestServer {
	t.Helper()

	s := &earnvidsTestServer{
		serverHTTPStatus:  http.StatusOK,
		serverAPIStatus:   http.StatusOK,
		serverMsg:         "OK",
		uploadHTTPStatus:  http.StatusOK,
		uploadAPIStatus:   http.StatusOK,
		uploadMsg:         "OK",
		uploadFileCode:    "code123",
		uploadFileStatus:  "OK",
		listHTTPStatus:    http.StatusOK,
		listAPIStatus:     http.StatusOK,
		listLink:          "https://morencius.com/file/code123",
		accountHTTPStatus: http.StatusOK,
		accountAPIStatus:  http.StatusOK,
		accountMsg:        "OK",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/upload/server", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.key = r.URL.Query().Get("key")
		httpStatus, apiStatus, msg := s.serverHTTPStatus, s.serverAPIStatus, s.serverMsg
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    msg,
			"result": s.URL + "/upload/01",
		})
	})

	mux.HandleFunc("/upload/01", func(w http.ResponseWriter, r *http.Request) {
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
		key, folder, fileName, fileContentType := "", "", "", ""
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
			switch part.FormName() {
			case "key":
				value, _ := io.ReadAll(part)
				key = string(value)
			case "fld_id":
				value, _ := io.ReadAll(part)
				folder = string(value)
			case "file":
				fileName = part.FileName()
				fileContentType = part.Header.Get("Content-Type")
				fileBody, _ = io.ReadAll(part)
			default:
				t.Errorf("unexpected part %q", part.FormName())
			}
			part.Close()
		}

		s.mu.Lock()
		s.uploadCalls++
		s.key = key
		s.folder = folder
		s.contentLength = r.ContentLength
		s.bodyLength = len(body)
		s.partCount = partCount
		s.fileName = fileName
		s.fileContentType = fileContentType
		s.fileBody = fileBody
		code, fileStatus, apiStatus, msg := s.uploadFileCode, s.uploadFileStatus, s.uploadAPIStatus, s.uploadMsg
		httpStatus := s.uploadHTTPStatus
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    msg,
			"files": []map[string]any{{
				"filecode": code,
				"filename": fileName,
				"status":   fileStatus,
			}},
		})
	})

	mux.HandleFunc("/api/file/list", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		httpStatus, apiStatus, link := s.listHTTPStatus, s.listAPIStatus, s.listLink
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    "OK",
			"result": map[string]any{
				"files": []map[string]any{{
					"file_code": "code123",
					"link":      link,
				}},
			},
		})
	})

	mux.HandleFunc("/api/account/info", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.key = r.URL.Query().Get("key")
		httpStatus, apiStatus, msg := s.accountHTTPStatus, s.accountAPIStatus, s.accountMsg
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": apiStatus,
			"msg":    msg,
			"result": map[string]any{"login": "user", "files_total": "0"},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	original := earnvidsAPIBaseURL
	earnvidsAPIBaseURL = s.URL
	t.Cleanup(func() { earnvidsAPIBaseURL = original })

	return s
}

func (s *earnvidsTestServer) setUploadFileStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadFileStatus = status
	if status != "OK" {
		s.uploadFileCode = ""
	}
}

func (s *earnvidsTestServer) setUploadAPIStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadAPIStatus = status
}

func (s *earnvidsTestServer) setList(apiStatus int, link string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listAPIStatus = apiStatus
	s.listLink = link
}

func (s *earnvidsTestServer) setAccount(httpStatus, apiStatus int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountHTTPStatus = httpStatus
	s.accountAPIStatus = apiStatus
}

type earnvidsSnapshot struct {
	key, folder     string
	uploadCalls     int
	contentLength   int64
	bodyLength      int
	partCount       int
	fileName        string
	fileContentType string
	fileBody        []byte
}

func (s *earnvidsTestServer) snapshot() earnvidsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return earnvidsSnapshot{
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

func TestEarnvidsRegistered(t *testing.T) {
	registered, ok := All().Get("earnvids")
	if !ok {
		t.Fatal("earnvids is not registered")
	}
	if _, ok := registered.(*Earnvids); !ok {
		t.Errorf("registered plugin is %T, want *Earnvids", registered)
	}
}

func TestEarnvidsUpload(t *testing.T) {
	srv := newEarnvidsTestServer(t)
	payload := []byte("video-data")

	res, err := (&Earnvids{}).Upload(
		context.Background(),
		filepath.Join("clips", "movie.mp4"),
		bytes.NewReader(payload),
		int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "api-key"},
		plugin.Config{"folder": "25"},
	)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.Service, "earnvids"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.URL, "https://morencius.com/file/code123"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}

	got := srv.snapshot()
	if got.key != "api-key" {
		t.Errorf("key = %q, want api-key", got.key)
	}
	if got.folder != "25" {
		t.Errorf("folder = %q, want 25", got.folder)
	}
	if got.uploadCalls != 1 {
		t.Errorf("upload calls = %d, want 1", got.uploadCalls)
	}
	if got.contentLength != int64(got.bodyLength) {
		t.Errorf("Content-Length = %d, body = %d", got.contentLength, got.bodyLength)
	}
	if got.partCount != 3 {
		t.Errorf("parts = %d, want 3 (key, fld_id, file)", got.partCount)
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

func TestEarnvidsUploadToShortVideo(t *testing.T) {
	srv := newEarnvidsTestServer(t)
	srv.setUploadFileStatus("0:0:0:video is too short")

	_, err := (&Earnvids{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "api-key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("err = %v, want video-too-short error", err)
	}
	if strings.Contains(err.Error(), "status 503") {
		t.Errorf("permanent input error should not be marked retryable: %v", err)
	}
}

func TestEarnvidsUploadServerSideFileError(t *testing.T) {
	srv := newEarnvidsTestServer(t)
	srv.setUploadFileStatus("Can't connect to savedb.example:443")

	_, err := (&Earnvids{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "api-key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("err = %v, want retryable status 503", err)
	}
}

func TestEarnvidsUploadServerError(t *testing.T) {
	srv := newEarnvidsTestServer(t)
	srv.setUploadAPIStatus(http.StatusForbidden)

	_, err := (&Earnvids{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "api-key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("err = %v, want status 403", err)
	}
}

func TestEarnvidsMissingPublicLink(t *testing.T) {
	srv := newEarnvidsTestServer(t)
	srv.setList(http.StatusOK, "")

	_, err := (&Earnvids{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "api-key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no public link") {
		t.Fatalf("err = %v, want missing-link error", err)
	}
}

func TestEarnvidsMissingCredentials(t *testing.T) {
	_, err := (&Earnvids{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("err = %v, want API_TOKEN error", err)
	}
}

func TestEarnvidsRejectsEmptyFile(t *testing.T) {
	_, err := (&Earnvids{}).Upload(
		context.Background(), "empty.mp4", strings.NewReader(""), 0,
		plugin.Credentials{"API_TOKEN": "api-key"}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty-file error", err)
	}
}

func TestEarnvidsVerifyCredentials(t *testing.T) {
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
			srv := newEarnvidsTestServer(t)
			srv.setAccount(c.httpStatus, c.apiStatus)

			err := (&Earnvids{}).VerifyCredentials(
				context.Background(), plugin.Credentials{"API_TOKEN": "key"}, nil)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got := errors.Is(err, plugin.ErrCredentialRejected); got != c.wantRejected {
				t.Errorf("ErrCredentialRejected = %v, want %v", got, c.wantRejected)
			}
			if got := srv.snapshot().key; got != "key" {
				t.Errorf("key = %q, want key", got)
			}
		})
	}
}
