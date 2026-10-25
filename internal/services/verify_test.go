package services

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/driftywinds/manyup/internal/plugin"
)

func TestAuthCheckOutcome(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantErr      bool
		wantRejected bool
	}{
		{name: "ok", status: http.StatusOK},
		{name: "created", status: http.StatusCreated},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true, wantRejected: true},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true, wantRejected: true},
		{name: "not found", status: http.StatusNotFound, wantErr: true, wantRejected: true},
		// Rate limits and server errors must not be mistaken for a bad key.
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := authCheckOutcome("test", c.status, []byte(`{"error":"nope"}`))
			if (err != nil) != c.wantErr {
				t.Fatalf("status %d: err = %v, wantErr = %v", c.status, err, c.wantErr)
			}
			if got := errors.Is(err, plugin.ErrCredentialRejected); got != c.wantRejected {
				t.Errorf("status %d: ErrCredentialRejected = %v, want %v", c.status, got, c.wantRejected)
			}
		})
	}
}

func TestMochaVerifyCredentials(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantErr      bool
		wantRejected bool
	}{
		{name: "valid key", status: http.StatusOK},
		{name: "invalid key", status: http.StatusUnauthorized, wantErr: true, wantRejected: true},
		{name: "service down", status: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/profile/summary" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
			}))
			defer srv.Close()

			original := mochaAPIBase
			mochaAPIBase = srv.URL
			defer func() { mochaAPIBase = original }()

			err := (&Mocha{}).VerifyCredentials(context.Background(),
				plugin.Credentials{"API_TOKEN": "mocha_test"}, nil)

			if gotAuth != "Bearer mocha_test" {
				t.Errorf("authorization header = %q", gotAuth)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if got := errors.Is(err, plugin.ErrCredentialRejected); got != c.wantRejected {
				t.Errorf("ErrCredentialRejected = %v, want %v", got, c.wantRejected)
			}
		})
	}
}

func TestMochaVerifyCredentialsRejectsEmptyKey(t *testing.T) {
	err := (&Mocha{}).VerifyCredentials(context.Background(), plugin.Credentials{}, nil)
	if !errors.Is(err, plugin.ErrCredentialRejected) {
		t.Errorf("err = %v, want ErrCredentialRejected", err)
	}
}
