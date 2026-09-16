package uploader

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxBackoff caps how long a single retry wait can grow to.
const maxBackoff = 30 * time.Second

// transientErrnos are OS-level errors that indicate a transient network
// problem (reset, aborted, refused, unreachable, ...) rather than a request
// the server actually answered.
var transientErrnos = []error{
	syscall.ECONNRESET,
	syscall.ECONNABORTED,
	syscall.ECONNREFUSED,
	syscall.EPIPE,
	syscall.ETIMEDOUT,
	syscall.EHOSTUNREACH,
	syscall.ENETUNREACH,
	syscall.ENETDOWN,
}

// transientPatterns are substrings (matched against the lowercased error
// message) that indicate transient network failures worth retrying.
// They complement transientErrnos for errors that arrive as opaque strings
// from HTTP clients, CDNs, or wrapped plugin messages.
var transientPatterns = []string{
	"connection reset",
	"forcibly closed", // Windows WSAECONNRESET phrasing
	"broken pipe",
	"connection refused",
	"connection aborted",
	"connection canceled",
	"timeout",           // i/o timeout, Client.Timeout, TLS handshake timeout, ...
	"timed out",         // "connection timed out", "operation timed out"
	"deadline exceeded", // context deadlines
	"eof",               // io.EOF / "unexpected EOF" mid-transfer
	"no such host",
	"temporary failure", // transient DNS resolver failure
	"no route to host",
	"unreachable",
	"network is down",
	"server closed idle connection",
	"goaway", // HTTP/2 GOAWAY
	"use of closed network connection",
	"did not properly respond", // Windows socket timeout phrasing
}

// statusPattern extracts an HTTP status code from plugin error messages,
// which embed it as e.g. "status 502" or "(status 429)".
var statusPattern = regexp.MustCompile(`status (\d{3})`)

// isRetryableError reports whether err looks like a transient failure — a
// dropped connection, a timeout, rate limiting, or a server-side error —
// that a fresh attempt might survive. Permanent failures (bad request, bad
// credentials, unknown file) return false so retries are not wasted.
func isRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, target := range transientErrnos {
		if errors.Is(err, target) {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, p := range transientPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	if m := statusPattern.FindStringSubmatch(msg); m != nil {
		if code, convErr := strconv.Atoi(m[1]); convErr == nil && retryableStatus(code) {
			return true
		}
	}
	return false
}

// retryableStatus reports whether an HTTP status code is worth retrying:
// request timeouts, rate limiting, and server-side errors (including CDN
// edges like Cloudflare's 52x range). Everything else — auth failures, bad
// requests, not-found — is permanent.
func retryableStatus(code int) bool {
	return code == 408 || code == 425 || code == 429 || code >= 500
}

// retryBackoff returns the wait before the retry following failedAttempt
// (1-based). It doubles per attempt (exponential backoff), is capped at
// maxBackoff, and carries ±25% jitter so services uploading in parallel do
// not all reconnect in lockstep.
func retryBackoff(failedAttempt int, base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < failedAttempt && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return time.Duration(float64(d) * (0.75 + 0.5*rand.Float64()))
}

// sleepContext waits for d, returning false if ctx was cancelled first
// (e.g. the user pressed Ctrl+C during a backoff wait).
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// shortError renders err as a compact one-line root cause for the progress
// display: the innermost wrapped message, single-line, truncated.
func shortError(err error) string {
	msg := err.Error()
	for unwrapped := errors.Unwrap(err); unwrapped != nil; unwrapped = errors.Unwrap(unwrapped) {
		msg = unwrapped.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	runes := []rune(msg)
	const max = 44
	if len(runes) > max {
		return string(runes[:max-1]) + "…"
	}
	return msg
}
