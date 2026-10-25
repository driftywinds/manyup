package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

// This file holds the small helpers shared by the services that implement
// plugin.CredentialVerifier. Each verifier calls one cheap, authenticated
// endpoint and lets authCheckOutcome decide what the status means, so every
// service reports "bad key" and "could not check" consistently.

// authGet performs an authenticated GET and returns the status and a size-capped
// body. Transport failures are returned as errors, which callers treat as an
// inconclusive check rather than a rejection.
func authGet(ctx context.Context, endpoint, header, value string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("creating request: %w", err)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, body, nil
}

// authCheckOutcome translates an HTTP status from an auth-check endpoint into
// the CredentialVerifier contract:
//
//   - 2xx: the credential works.
//   - 401/403 (and other unexpected 4xx): the service refused it.
//   - 429/5xx: the service could not answer, so the check is inconclusive.
//
// Keeping rate limits and server errors out of the "rejected" bucket means a
// valid key is never discarded because a service happened to be down.
func authCheckOutcome(service string, status int, body []byte) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401 || status == 403:
		return fmt.Errorf("%s: %w: %s", service, plugin.ErrCredentialRejected, describeBody(body))
	case status == 429 || status >= 500:
		return fmt.Errorf("%s: could not verify, service answered status %d", service, status)
	default:
		return fmt.Errorf("%s: %w: status %d: %s", service, plugin.ErrCredentialRejected, status, describeBody(body))
	}
}

// describeBody renders a response body as a short single-line detail.
func describeBody(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if text == "" {
		return "no response body"
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}
