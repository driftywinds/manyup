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
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/driftywinds/manyup/internal/plugin"
)

type filemoonTestServer struct {
	*httptest.Server

	mu sync.Mutex

	auth           string
	uploadCalls    int
	chunkCalls     int
	singleCalls    int
	partCount      int
	contentLength  int64
	bodyLength     int
	fileName       string
	fileBody       []byte
	visibility     string
	chunkUID       string
	chunkIndexes   []int
	chunkOffsets   []int64
	chunkSizes     []int64
	chunkTotals    []int
	accountStatus  int
	accountSuccess bool
	premiumKnown   bool
	premiumActive  bool
	modernResponse bool
}

func newFileMoonTestServer(t *testing.T) *filemoonTestServer {
	t.Helper()

	s := &filemoonTestServer{accountSuccess: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/account", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auth = r.Header.Get("Authorization")
		status := s.accountStatus
		success := s.accountSuccess
		premiumKnown := s.premiumKnown
		premiumActive := s.premiumActive
		s.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if success {
			data := map[string]any{"id": "acct", "username": "user"}
			if premiumKnown {
				premiumStatus := "free"
				if premiumActive {
					premiumStatus = "premium"
				}
				data["premium"] = map[string]any{"active": premiumActive, "status": premiumStatus}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
			return
		}
		_, _ = io.WriteString(w, `{"success":false,"error":{"code":"UNAUTHENTICATED","message":"bad token"}}`)
	})

	mux.HandleFunc("/api/v1/files/upload", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		modern := s.modernResponse
		s.mu.Unlock()
		if modern {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":"modern123","urls":{"page":"https://filemoon.org/modern123/file"}}}`)
			return
		}

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
			t.Errorf("media type = %q", mediaType)
		}

		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		fields := map[string]string{}
		fileName := ""
		var fileBody []byte
		partCount := 0
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("multipart part: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			partCount++
			if part.FormName() == "file" {
				fileName = part.FileName()
				fileBody, _ = io.ReadAll(part)
			} else {
				value, _ := io.ReadAll(part)
				fields[part.FormName()] = string(value)
			}
			part.Close()
		}

		s.mu.Lock()
		s.uploadCalls++
		s.partCount = partCount
		s.contentLength = r.ContentLength
		s.bodyLength = len(body)
		s.fileName = fileName
		s.fileBody = append([]byte(nil), fileBody...)
		s.visibility = fields["visibility"]
		chunkIndex := -1
		totalChunks := 0
		isChunk := fields["dzuuid"] != ""
		if isChunk {
			s.chunkCalls++
			s.chunkUID = fields["dzuuid"]
			chunkIndex, _ = strconv.Atoi(fields["dzchunkindex"])
			totalChunks, _ = strconv.Atoi(fields["dztotalchunkcount"])
			s.chunkIndexes = append(s.chunkIndexes, chunkIndex)
			offset, _ := strconv.ParseInt(fields["dzchunkbyteoffset"], 10, 64)
			chunkSize, _ := strconv.ParseInt(fields["dzchunksize"], 10, 64)
			s.chunkOffsets = append(s.chunkOffsets, offset)
			s.chunkSizes = append(s.chunkSizes, chunkSize)
			s.chunkTotals = append(s.chunkTotals, totalChunks)
		} else {
			s.singleCalls++
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if isChunk && chunkIndex >= 0 && chunkIndex < totalChunks-1 {
			// Live FileMoon responses are not consistent about the optional
			// "finished" field; the uploader must still treat this as partial.
			_, _ = io.WriteString(w, `{"type":"success","status":"success","chunk":true,"message":"Chunk uploaded successfully."}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":          "success",
			"shared_id":     "file123",
			"download_link": "https://filemoon.org/file123/file",
			"file": map[string]any{
				"name":      "movie.mp4",
				"watch_url": "https://filemoon.org/file123/watch",
			},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	originalBase := filemoonAPIBaseURL
	originalThreshold := filemoonChunkThreshold
	originalChunkSize := filemoonChunkSize
	filemoonAPIBaseURL = s.URL + "/api/v1"
	t.Cleanup(func() {
		filemoonAPIBaseURL = originalBase
		filemoonChunkThreshold = originalThreshold
		filemoonChunkSize = originalChunkSize
	})
	return s
}

type filemoonSnapshot struct {
	auth          string
	uploadCalls   int
	chunkCalls    int
	singleCalls   int
	partCount     int
	contentLength int64
	bodyLength    int
	fileName      string
	fileBody      []byte
	visibility    string
	chunkUID      string
	chunkIndexes  []int
	chunkOffsets  []int64
	chunkSizes    []int64
	chunkTotals   []int
}

func (s *filemoonTestServer) snapshot() filemoonSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return filemoonSnapshot{
		auth:          s.auth,
		uploadCalls:   s.uploadCalls,
		chunkCalls:    s.chunkCalls,
		singleCalls:   s.singleCalls,
		partCount:     s.partCount,
		contentLength: s.contentLength,
		bodyLength:    s.bodyLength,
		fileName:      s.fileName,
		fileBody:      append([]byte(nil), s.fileBody...),
		visibility:    s.visibility,
		chunkUID:      s.chunkUID,
		chunkIndexes:  append([]int(nil), s.chunkIndexes...),
		chunkOffsets:  append([]int64(nil), s.chunkOffsets...),
		chunkSizes:    append([]int64(nil), s.chunkSizes...),
		chunkTotals:   append([]int(nil), s.chunkTotals...),
	}
}

func TestFileMoonRegistered(t *testing.T) {
	registered, ok := All().Get("filemoon")
	if !ok {
		t.Fatal("filemoon is not registered")
	}
	if _, ok := registered.(*FileMoon); !ok {
		t.Errorf("registered plugin is %T, want *FileMoon", registered)
	}
}

func TestFileMoonUploadSingle(t *testing.T) {
	srv := newFileMoonTestServer(t)
	payload := []byte("video-data")

	res, err := (&FileMoon{}).Upload(
		context.Background(),
		filepath.Join("clips", "movie.mp4"),
		bytes.NewReader(payload),
		int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "token"},
		plugin.Config{"visibility": "1"},
	)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.URL != "https://filemoon.org/file123/file" {
		t.Errorf("URL = %q", res.URL)
	}

	got := srv.snapshot()
	if got.singleCalls != 1 || got.chunkCalls != 0 {
		t.Errorf("single=%d chunk=%d, want single only", got.singleCalls, got.chunkCalls)
	}
	if got.visibility != "1" {
		t.Errorf("visibility = %q, want 1", got.visibility)
	}
	if got.contentLength != int64(got.bodyLength) {
		t.Errorf("Content-Length = %d body = %d", got.contentLength, got.bodyLength)
	}
	if got.fileName != "movie.mp4" {
		t.Errorf("filename = %q", got.fileName)
	}
	if !bytes.Equal(got.fileBody, payload) {
		t.Errorf("body = %q", got.fileBody)
	}
}

func TestFileMoonUploadChunked(t *testing.T) {
	srv := newFileMoonTestServer(t)
	filemoonChunkThreshold = 0
	filemoonChunkSize = 4
	payload := []byte("abcdefghij") // 3 chunks: 4,4,2

	res, err := (&FileMoon{}).Upload(
		context.Background(), "movie.mp4", bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "token"}, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.URL != "https://filemoon.org/file123/file" {
		t.Errorf("URL = %q", res.URL)
	}

	got := srv.snapshot()
	if got.chunkCalls != 3 || got.singleCalls != 0 {
		t.Fatalf("chunk calls = %d single = %d, want 3 chunk calls", got.chunkCalls, got.singleCalls)
	}
	if got.chunkUID == "" {
		t.Error("missing dzuuid")
	}
	wantIndexes := []int{0, 1, 2}
	wantOffsets := []int64{0, 4, 8}
	wantSizes := []int64{4, 4, 4}
	for i := range wantIndexes {
		if got.chunkIndexes[i] != wantIndexes[i] {
			t.Errorf("index[%d] = %d, want %d", i, got.chunkIndexes[i], wantIndexes[i])
		}
		if got.chunkOffsets[i] != wantOffsets[i] {
			t.Errorf("offset[%d] = %d, want %d", i, got.chunkOffsets[i], wantOffsets[i])
		}
		if got.chunkSizes[i] != wantSizes[i] {
			t.Errorf("chunk size[%d] = %d, want %d", i, got.chunkSizes[i], wantSizes[i])
		}
		if got.chunkTotals[i] != 3 {
			t.Errorf("total[%d] = %d, want 3", i, got.chunkTotals[i])
		}
	}
	if !bytes.Equal(got.fileBody, []byte("ij")) {
		t.Errorf("last chunk body = %q, want ij", got.fileBody)
	}
}

func TestFileMoonModernResponse(t *testing.T) {
	srv := newFileMoonTestServer(t)
	srv.mu.Lock()
	srv.modernResponse = true
	srv.mu.Unlock()

	res, err := (&FileMoon{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "token"}, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.URL != "https://filemoon.org/modern123/file" {
		t.Errorf("URL = %q", res.URL)
	}
}

func TestFileMoonUploadError(t *testing.T) {
	newFileMoonTestServer(t)
	filemoonAPIBaseURL = "http://127.0.0.1:1/api/v1"

	_, err := (&FileMoon{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1,
		plugin.Credentials{"API_TOKEN": "token"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestFileMoonFreeAccountRejectsOverFourGB(t *testing.T) {
	srv := newFileMoonTestServer(t)
	srv.mu.Lock()
	srv.premiumKnown = true
	srv.premiumActive = false
	srv.mu.Unlock()

	_, err := (&FileMoon{}).Upload(
		context.Background(), "huge.mkv", strings.NewReader("x"), 5<<30,
		plugin.Credentials{"API_TOKEN": "token"}, nil)
	if err == nil || !strings.Contains(err.Error(), "4 GB") {
		t.Fatalf("err = %v, want free-account limit error", err)
	}
}

func TestFileMoonRejectsOverFifteenGB(t *testing.T) {
	_, err := (&FileMoon{}).Upload(
		context.Background(), "huge.mkv", strings.NewReader("x"), 16<<30,
		plugin.Credentials{"API_TOKEN": "token"}, nil)
	if err == nil || !strings.Contains(err.Error(), "15 GB") {
		t.Fatalf("err = %v, want 15 GB limit error", err)
	}
}

func TestFileMoonMissingCredentials(t *testing.T) {
	_, err := (&FileMoon{}).Upload(
		context.Background(), "movie.mp4", strings.NewReader("x"), 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("err = %v, want API_TOKEN error", err)
	}
}

func TestFileMoonRejectsEmptyFile(t *testing.T) {
	_, err := (&FileMoon{}).Upload(
		context.Background(), "empty.mp4", strings.NewReader(""), 0,
		plugin.Credentials{"API_TOKEN": "token"}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty-file error", err)
	}
}

func TestFileMoonVerifyCredentials(t *testing.T) {
	cases := []struct {
		name          string
		accountStatus int
		success       bool
		wantErr       bool
		wantRejected  bool
	}{
		{name: "valid", success: true},
		{name: "invalid json success false", accountStatus: http.StatusOK, wantErr: true, wantRejected: true},
		{name: "unauthorized", accountStatus: http.StatusUnauthorized, success: true, wantErr: true, wantRejected: true},
		{name: "server error", accountStatus: http.StatusServiceUnavailable, success: false, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newFileMoonTestServer(t)
			srv.mu.Lock()
			srv.accountStatus = c.accountStatus
			srv.accountSuccess = c.success
			srv.mu.Unlock()

			err := (&FileMoon{}).VerifyCredentials(
				context.Background(), plugin.Credentials{"API_TOKEN": "token"}, nil)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got := errors.Is(err, plugin.ErrCredentialRejected); got != c.wantRejected {
				t.Errorf("ErrCredentialRejected = %v, want %v", got, c.wantRejected)
			}
			if got := srv.snapshot().auth; got != "Bearer token" {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
}
