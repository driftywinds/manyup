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

// stTestServer fakes the storage.to API.
type stTestServer struct {
	*httptest.Server

	multipart bool

	mu           sync.Mutex
	singleBody   []byte
	singleCT     string
	partBodies   map[int][]byte
	complete     map[string]any
	confirm      map[string]any
	partsRequest map[string]any
	partsOwner   string
	aborted      bool
}

func newSTTestServer(t *testing.T, multipart bool) *stTestServer {
	t.Helper()

	s := &stTestServer{multipart: multipart, partBodies: make(map[int][]byte)}
	mux := http.NewServeMux()

	mux.HandleFunc("/upload/init", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("init: method = %s", r.Method)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("init: decoding: %v", err)
		}
		if req["filename"] == "" || req["content_type"] == "" || req["size"] == nil {
			t.Errorf("init: incomplete body: %v", req)
		}

		if s.multipart {
			writeJSON(w, http.StatusOK, map[string]any{
				"success":     true,
				"type":        "multipart",
				"upload_id":   "u1",
				"r2_key":      "r2-mp",
				"part_size":   4,
				"total_parts": 3,
				// Part 2 is deliberately missing, so /upload/parts is exercised.
				"initial_urls": map[string]string{
					"1": s.URL + "/storage/part/1",
					"3": s.URL + "/storage/part/3",
				},
				"owner_token": "owner-1",
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"type":       "single",
			"upload_url": s.URL + "/storage/single",
			// Header values arrive as arrays.
			"headers": map[string]any{"Content-Type": []string{"text/plain"}},
			"r2_key":  "r2-single",
		})
	})

	mux.HandleFunc("/storage/single", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.singleBody = body
		s.singleCT = r.Header.Get("Content-Type")
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/storage/part/", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/storage/part/"))
		if err != nil {
			t.Errorf("part upload: bad part number in %q", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.partBodies[n] = body
		s.mu.Unlock()

		w.Header().Set("ETag", `"etag-`+strconv.Itoa(n)+`"`)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/upload/parts", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)

		s.mu.Lock()
		s.partsRequest = req
		s.partsOwner = r.Header.Get("X-Owner-Token")
		s.mu.Unlock()

		urls := map[string]string{}
		if numbers, ok := req["part_numbers"].([]any); ok {
			for _, raw := range numbers {
				if n, ok := raw.(float64); ok {
					num := int(n)
					urls[strconv.Itoa(num)] = s.URL + "/storage/part/" + strconv.Itoa(num)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "urls": urls})
	})

	mux.HandleFunc("/upload/complete-multipart", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.complete = req
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "r2_key": "ignored"})
	})

	mux.HandleFunc("/upload/abort", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.aborted = true
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})

	mux.HandleFunc("/upload/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.confirm = req
		s.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"file": map[string]any{
				"id":       "FQxyz1234",
				"url":      "https://storage.to/FQxyz1234",
				"filename": req["filename"],
				"size":     req["size"],
			},
			"owner_token": "owner-2",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.Server = srv

	original := stBaseURL
	stBaseURL = srv.URL
	t.Cleanup(func() { stBaseURL = original })

	return s
}

func TestStorageToSingleUpload(t *testing.T) {
	srv := newSTTestServer(t, false)

	payload := []byte("hello storage.to")
	res, err := (&StorageTo{}).Upload(context.Background(), "notes.txt",
		bytes.NewReader(payload), int64(len(payload)), plugin.Credentials{}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	srv.mu.Lock()
	body, contentType, confirm := srv.singleBody, srv.singleCT, srv.confirm
	srv.mu.Unlock()

	if !bytes.Equal(body, payload) {
		t.Errorf("uploaded %q, want %q", body, payload)
	}
	// The signed header from init must be applied to the PUT.
	if contentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain (from init headers)", contentType)
	}
	if got, want := confirm["r2_key"], "r2-single"; got != want {
		t.Errorf("confirm r2_key = %v, want %v", got, want)
	}
	if got, want := confirm["content_type"], "text/plain; charset=utf-8"; got != want {
		t.Errorf("confirm content_type = %v, want %v", got, want)
	}
	if got, want := res.URL, "https://storage.to/FQxyz1234"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

func TestStorageToMultipartUpload(t *testing.T) {
	srv := newSTTestServer(t, true)

	payload := []byte("0123456789") // 10 bytes, 4-byte parts -> 4,4,2
	res, err := (&StorageTo{}).Upload(context.Background(), "song.flac",
		bytes.NewReader(payload), int64(len(payload)), plugin.Credentials{}, nil)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	srv.mu.Lock()
	partsRequest, partsOwner, complete, partBodies, aborted :=
		srv.partsRequest, srv.partsOwner, srv.complete, srv.partBodies, srv.aborted
	srv.mu.Unlock()

	if aborted {
		t.Error("upload was aborted despite succeeding")
	}

	// The missing part must have been requested.
	numbers, _ := partsRequest["part_numbers"].([]any)
	if len(numbers) != 1 || int(numbers[0].(float64)) != 2 {
		t.Errorf("part_numbers = %v, want [2]", partsRequest["part_numbers"])
	}
	if partsOwner != "owner-1" {
		t.Errorf("owner token header = %q, want owner-1", partsOwner)
	}

	// Parts must arrive in order and reassemble to the original bytes.
	var rebuilt []byte
	for n := 1; n <= 3; n++ {
		rebuilt = append(rebuilt, partBodies[n]...)
	}
	if !bytes.Equal(rebuilt, payload) {
		t.Errorf("parts reassemble to %q, want %q", rebuilt, payload)
	}

	// Completion carries the upload id and every ETag in order.
	if got, want := complete["upload_id"], "u1"; got != want {
		t.Errorf("complete upload_id = %v, want %v", got, want)
	}
	parts, _ := complete["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("complete parts = %v, want 3", complete["parts"])
	}
	for i, raw := range parts {
		part := raw.(map[string]any)
		if got := int(part["partNumber"].(float64)); got != i+1 {
			t.Errorf("part %d number = %d", i+1, got)
		}
		if part["etag"] == "" {
			t.Errorf("part %d has no etag", i+1)
		}
	}

	if got, want := res.URL, "https://storage.to/FQxyz1234"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

func TestStorageToReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"error":"rate limited"}`))
	}))
	defer srv.Close()

	original := stBaseURL
	stBaseURL = srv.URL
	defer func() { stBaseURL = original }()

	_, err := (&StorageTo{}).Upload(context.Background(), "x.txt",
		strings.NewReader("data"), 4, plugin.Credentials{}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 429") {
		t.Errorf("err = %v, want it to contain %q", err, "status 429")
	}
}

func TestStorageToVerifyCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("path = %q, want /user", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 42, "name": "Ada"})
	}))
	defer srv.Close()

	original := stBaseURL
	stBaseURL = srv.URL
	defer func() { stBaseURL = original }()

	if err := (&StorageTo{}).VerifyCredentials(context.Background(),
		plugin.Credentials{"API_TOKEN": "good"}, nil); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	if err := (&StorageTo{}).VerifyCredentials(context.Background(),
		plugin.Credentials{"API_TOKEN": "bad"}, nil); err == nil {
		t.Error("invalid token accepted")
	}
}

func TestStorageToHeaderValues(t *testing.T) {
	if got := stHeaderValues("text/plain"); len(got) != 1 || got[0] != "text/plain" {
		t.Errorf("string header = %v", got)
	}
	if got := stHeaderValues([]any{"a", 1, "b"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("array header = %v", got)
	}
	if got := stHeaderValues(42); got != nil {
		t.Errorf("non-header value = %v", got)
	}
}
