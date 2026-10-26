package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/driftywinds/manyup/internal/plugin"
)

// interimTestServer stands in for the interim API and its S3 endpoint.
type interimTestServer struct {
	*httptest.Server

	mu               sync.Mutex
	intentBody       map[string]any
	intentAuth       string
	intentStatus     int
	putMethod        string
	putHeaders       http.Header
	putBody          []byte
	putContentLength int64
	completeCalls    int
	completeStatuses []int
	completeToken    string
	completeAuth     string
	abortCalls       int
	abortToken       string
	keyApproved      bool
	keyStatus        int
}

func newInterimTestServer(t *testing.T) *interimTestServer {
	t.Helper()

	s := &interimTestServer{
		intentStatus: http.StatusCreated,
		keyApproved:  true,
		keyStatus:    http.StatusOK,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("intent: method = %s, want POST", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("intent: decoding body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.intentAuth = r.Header.Get("Authorization")
		s.intentBody = payload
		status := s.intentStatus
		s.mu.Unlock()

		if status != http.StatusCreated {
			writeJSON(w, status, map[string]any{
				"error": map[string]string{"code": "rate_limited", "message": "slow down"},
			})
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"upload_id": "up-1",
			"url":       s.URL + "/storage/object",
			"method":    "PUT",
			"headers": map[string]string{
				"Content-Type":        "application/pdf",
				"Content-Disposition": `attachment; filename="test.pdf"`,
			},
			"expires_at":          "2026-10-26T12:00:00Z",
			"intent_expires_at":   "2026-10-26T12:15:00Z",
			"upload_token":        "upload-token",
			"complete_url":        s.URL + "/api/v1/uploads/up-1/complete",
			"abort_url":           s.URL + "/api/v1/uploads/up-1",
			"delete_token":        "delete-token",
			"delete_token_header": "X-Delete-Token",
		})
	})

	mux.HandleFunc("/storage/object", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		s.putMethod = r.Method
		s.putHeaders = r.Header.Clone()
		s.putBody = body
		s.putContentLength = r.ContentLength
		s.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/v1/uploads/up-1/complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("complete: method = %s, want POST", r.Method)
		}

		s.mu.Lock()
		s.completeCalls++
		s.completeToken = r.Header.Get("X-Upload-Token")
		s.completeAuth = r.Header.Get("Authorization")
		status := http.StatusCreated
		if len(s.completeStatuses) > 0 {
			status = s.completeStatuses[0]
			s.completeStatuses = s.completeStatuses[1:]
		}
		declaredSize := int64(0)
		if value, ok := s.intentBody["size"].(float64); ok {
			declaredSize = int64(value)
		}
		s.mu.Unlock()

		switch status {
		case http.StatusCreated:
			writeJSON(w, status, map[string]any{
				"upload_id": "up-1",
				"file": map[string]any{
					"id":       "file-1",
					"name":     "test.pdf",
					"size":     declaredSize,
					"page_url": "https://interim.drifty.win/f/file-1",
				},
			})
		case http.StatusConflict:
			writeJSON(w, status, map[string]any{
				"error": map[string]string{
					"code":    "upload_missing",
					"message": "The uploaded object is not visible yet.",
				},
			})
		default:
			writeJSON(w, status, map[string]any{
				"error": map[string]string{"code": "internal", "message": "boom"},
			})
		}
	})

	mux.HandleFunc("/api/v1/uploads/up-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("abort: method = %s, want DELETE", r.Method)
		}

		s.mu.Lock()
		s.abortCalls++
		s.abortToken = r.Header.Get("X-Upload-Token")
		s.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/v1/keys/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("keys/me: method = %s, want GET", r.Method)
		}

		s.mu.Lock()
		status, approved := s.keyStatus, s.keyApproved
		s.mu.Unlock()

		if status != http.StatusOK {
			writeJSON(w, status, map[string]any{
				"error": map[string]string{"code": "unauthorized", "message": "invalid user hash"},
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"id":           "key-1",
			"key_prefix":   "interim_test",
			"approved":     approved,
			"created_at":   "2026-10-26T12:00:00Z",
			"files":        1,
			"bytes":        123,
			"last_used_at": "2026-10-26T12:00:00Z",
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	originalBase := interimBaseURL
	interimBaseURL = s.URL + "/api/v1"
	t.Cleanup(func() { interimBaseURL = originalBase })

	originalDelay := interimCompleteRetryDelay
	interimCompleteRetryDelay = time.Millisecond
	t.Cleanup(func() { interimCompleteRetryDelay = originalDelay })

	return s
}

func (s *interimTestServer) setCompleteStatuses(statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeStatuses = statuses
}

func TestInterimRegistered(t *testing.T) {
	registered, ok := All().Get("interim")
	if !ok {
		t.Fatal("interim is not registered")
	}
	if _, ok := registered.(*Interim); !ok {
		t.Errorf("registered plugin is %T, want *Interim", registered)
	}
}

func TestInterimUpload(t *testing.T) {
	srv := newInterimTestServer(t)

	payload := []byte("hello interim")
	res, err := (&Interim{}).Upload(
		context.Background(),
		filepath.Join("some", "dir", "test.pdf"),
		bytes.NewReader(payload),
		int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "interim_test"},
		nil,
	)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	if got, want := res.Service, "interim"; got != want {
		t.Errorf("Service = %q, want %q", got, want)
	}
	if got, want := res.URL, "https://interim.drifty.win/f/file-1"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := res.Size, int64(len(payload)); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
	if got, want := res.Filename, filepath.Join("some", "dir", "test.pdf"); got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}

	srv.mu.Lock()
	intent := srv.intentBody
	intentAuth := srv.intentAuth
	putMethod := srv.putMethod
	putHeaders := srv.putHeaders
	putBody := srv.putBody
	putContentLength := srv.putContentLength
	completeCalls := srv.completeCalls
	completeToken := srv.completeToken
	completeAuth := srv.completeAuth
	abortCalls := srv.abortCalls
	srv.mu.Unlock()

	if got, want := intentAuth, "Bearer interim_test"; got != want {
		t.Errorf("intent Authorization = %q, want %q", got, want)
	}
	if got, want := intent["name"], "test.pdf"; got != want {
		t.Errorf("intent name = %v, want %q", got, want)
	}
	if got, want := intent["size"], float64(len(payload)); got != want {
		t.Errorf("intent size = %v, want %v", got, want)
	}
	if got, want := intent["content_type"], "application/pdf"; got != want {
		t.Errorf("intent content_type = %v, want %q", got, want)
	}

	if got, want := putMethod, http.MethodPut; got != want {
		t.Errorf("PUT method = %q, want %q", got, want)
	}
	if got, want := string(putBody), string(payload); got != want {
		t.Errorf("PUT body = %q, want %q", got, want)
	}
	if got, want := putContentLength, int64(len(payload)); got != want {
		t.Errorf("PUT ContentLength = %d, want %d", got, want)
	}
	if got, want := putHeaders.Get("Content-Type"), "application/pdf"; got != want {
		t.Errorf("PUT Content-Type = %q, want %q", got, want)
	}
	if got, want := putHeaders.Get("Content-Disposition"), `attachment; filename="test.pdf"`; got != want {
		t.Errorf("PUT Content-Disposition = %q, want %q", got, want)
	}

	if got, want := completeCalls, 1; got != want {
		t.Errorf("completion calls = %d, want %d", got, want)
	}
	if got, want := completeToken, "upload-token"; got != want {
		t.Errorf("completion X-Upload-Token = %q, want %q", got, want)
	}
	if completeAuth != "" {
		t.Errorf("completion sent Authorization %q; only the upload token should be used", completeAuth)
	}
	if abortCalls != 0 {
		t.Errorf("abort calls = %d, want none on success", abortCalls)
	}
}

func TestInterimCompleteRetriesUploadMissing(t *testing.T) {
	srv := newInterimTestServer(t)
	srv.setCompleteStatuses(http.StatusConflict, http.StatusConflict, http.StatusCreated)

	payload := []byte("eventually visible")
	res, err := (&Interim{}).Upload(
		context.Background(), "test.pdf", bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if got, want := res.URL, "https://interim.drifty.win/f/file-1"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}

	srv.mu.Lock()
	calls, aborts := srv.completeCalls, srv.abortCalls
	srv.mu.Unlock()

	if got, want := calls, 3; got != want {
		t.Errorf("completion calls = %d, want %d", got, want)
	}
	if aborts != 0 {
		t.Errorf("abort calls = %d, want none after a successful retry", aborts)
	}
}

func TestInterimAbortsAfterPersistentUploadMissing(t *testing.T) {
	srv := newInterimTestServer(t)
	srv.setCompleteStatuses(
		http.StatusConflict, http.StatusConflict, http.StatusConflict,
		http.StatusConflict, http.StatusConflict,
	)

	payload := []byte("never visible")
	_, err := (&Interim{}).Upload(
		context.Background(), "test.pdf", bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status 409") {
		t.Errorf("error = %v, want it to contain %q", err, "status 409")
	}

	srv.mu.Lock()
	calls, aborts, abortToken := srv.completeCalls, srv.abortCalls, srv.abortToken
	srv.mu.Unlock()

	if got, want := calls, interimCompleteAttempts; got != want {
		t.Errorf("completion calls = %d, want %d", got, want)
	}
	if got, want := aborts, 1; got != want {
		t.Errorf("abort calls = %d, want %d", got, want)
	}
	if got, want := abortToken, "upload-token"; got != want {
		t.Errorf("abort X-Upload-Token = %q, want %q", got, want)
	}
}

func TestInterimAbortsWhenCompleteFails(t *testing.T) {
	srv := newInterimTestServer(t)
	srv.setCompleteStatuses(http.StatusInternalServerError)

	payload := []byte("boom")
	_, err := (&Interim{}).Upload(
		context.Background(), "test.pdf", bytes.NewReader(payload), int64(len(payload)),
		plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The retry classifier in internal/uploader matches this exact form.
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error = %v, want it to contain %q", err, "status 500")
	}

	srv.mu.Lock()
	calls, aborts := srv.completeCalls, srv.abortCalls
	srv.mu.Unlock()

	if got, want := calls, 1; got != want {
		t.Errorf("completion calls = %d, want %d", got, want)
	}
	if got, want := aborts, 1; got != want {
		t.Errorf("abort calls = %d, want %d", got, want)
	}
}

func TestInterimReportsIntentStatus(t *testing.T) {
	srv := newInterimTestServer(t)
	srv.mu.Lock()
	srv.intentStatus = http.StatusTooManyRequests
	srv.mu.Unlock()

	_, err := (&Interim{}).Upload(
		context.Background(), "test.pdf", strings.NewReader("data"), 4,
		plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Errorf("error = %v, want it to contain %q", err, "status 429")
	}

	srv.mu.Lock()
	aborts := srv.abortCalls
	srv.mu.Unlock()
	if aborts != 0 {
		t.Errorf("abort calls = %d, want none when no intent was created", aborts)
	}
}

func TestInterimVerifyCredentials(t *testing.T) {
	t.Run("approved hash", func(t *testing.T) {
		newInterimTestServer(t)
		if err := (&Interim{}).VerifyCredentials(
			context.Background(), plugin.Credentials{"API_TOKEN": "interim_test"}, nil); err != nil {
			t.Errorf("VerifyCredentials returned %v, want nil", err)
		}
	})

	t.Run("unapproved hash is rejected", func(t *testing.T) {
		srv := newInterimTestServer(t)
		srv.mu.Lock()
		srv.keyApproved = false
		srv.mu.Unlock()

		err := (&Interim{}).VerifyCredentials(
			context.Background(), plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
		if !errors.Is(err, plugin.ErrCredentialRejected) {
			t.Errorf("err = %v, want ErrCredentialRejected", err)
		}
	})

	t.Run("bad hash is rejected", func(t *testing.T) {
		srv := newInterimTestServer(t)
		srv.mu.Lock()
		srv.keyStatus = http.StatusUnauthorized
		srv.mu.Unlock()

		err := (&Interim{}).VerifyCredentials(
			context.Background(), plugin.Credentials{"API_TOKEN": "interim_wrong"}, nil)
		if !errors.Is(err, plugin.ErrCredentialRejected) {
			t.Errorf("err = %v, want ErrCredentialRejected", err)
		}
	})

	t.Run("rate limiting is inconclusive", func(t *testing.T) {
		srv := newInterimTestServer(t)
		srv.mu.Lock()
		srv.keyStatus = http.StatusTooManyRequests
		srv.mu.Unlock()

		err := (&Interim{}).VerifyCredentials(
			context.Background(), plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, plugin.ErrCredentialRejected) {
			t.Error("a 429 must not reject the credential")
		}
	})

	t.Run("empty hash is rejected", func(t *testing.T) {
		err := (&Interim{}).VerifyCredentials(
			context.Background(), plugin.Credentials{}, nil)
		if !errors.Is(err, plugin.ErrCredentialRejected) {
			t.Errorf("err = %v, want ErrCredentialRejected", err)
		}
	})
}

func TestInterimRejectsUnusableFiles(t *testing.T) {
	interim := &Interim{}

	if _, err := interim.Upload(context.Background(), "x.bin", strings.NewReader("x"), 1,
		plugin.Credentials{}, nil); err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("err = %v, want an API_TOKEN error", err)
	}

	if _, err := interim.Upload(context.Background(), "empty.bin", strings.NewReader(""), 0,
		plugin.Credentials{"API_TOKEN": "interim_test"}, nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want an empty-file error", err)
	}

	_, err := interim.Upload(context.Background(), "huge.bin", strings.NewReader("x"),
		interimSinglePutLimit+1, plugin.Credentials{"API_TOKEN": "interim_test"}, nil)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want an over-limit error", err)
	}
}

func TestInterimFileName(t *testing.T) {
	if got, want := interimFileName(filepath.Join("dir", "song.flac")), "song.flac"; got != want {
		t.Errorf("interimFileName = %q, want %q", got, want)
	}

	long := strings.Repeat("a", 200) + ".pdf"
	got := interimFileName(long)
	if len([]rune(got)) != interimNameLimit {
		t.Errorf("truncated name has %d characters, want %d", len([]rune(got)), interimNameLimit)
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("truncated name = %q, want it to keep the extension", got)
	}
}

func TestInterimContentType(t *testing.T) {
	if got, want := interimContentType("file.pdf"), "application/pdf"; got != want {
		t.Errorf("interimContentType(file.pdf) = %q, want %q", got, want)
	}
	if got, want := interimContentType("file.unknown-ext"), "application/octet-stream"; got != want {
		t.Errorf("interimContentType(file.unknown-ext) = %q, want %q", got, want)
	}
}
