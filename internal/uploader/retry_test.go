package uploader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/driftywinds/manyup/internal/config"
	"github.com/driftywinds/manyup/internal/plugin"
)

func TestIsRetryableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "windows wsasend connection reset (real-world report)",
			err: errors.New(`Put "https://w.buzzheavier.com/BOMBANANA%21.7z": write tcp 192.168.1.38:55722->85.159.92.194:443: ` +
				`wsasend: An existing connection was forcibly closed by the remote host.`),
			want: true,
		},
		{"wrapped connection reset", fmt.Errorf("buzzheavier: upload failed: %w", errors.New(`Put "https://x": write tcp: connection reset by peer`)), true},
		{"unexpected eof", errors.New("buzzheavier: reading response: unexpected EOF"), true},
		{"client timeout", errors.New(`Get "https://x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`), true},
		{"http 502", errors.New("buzzheavier: parsing response (status 502): <html>bad gateway</html>"), true},
		{"http 429", errors.New("datanodes: status 429: rate limited"), true},
		{"http 503", errors.New("zincdrive: upload: S3 PUT returned status 503: slow down"), true},
		{"http 403 forbidden", errors.New("buzzheavier: no data in response (status 403): denied"), false},
		{"http 400 bad request", errors.New("buzzheavier: no data in response (status 400): bad"), false},
		{"api auth error", errors.New("zincdrive: API_TOKEN required"), false},
		{"context canceled", context.Canceled, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableError(tc.err); got != tc.want {
				t.Fatalf("isRetryableError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryBackoff(t *testing.T) {
	base := 2 * time.Second
	if got := retryBackoff(1, base); got < base*3/4 || got > base*5/4 {
		t.Fatalf("first backoff = %v, want within 2s ±25%%", got)
	}
	// Attempt 4 must hit the 30s cap (2,4,8,16 -> next would be 32).
	if got := retryBackoff(5, base); got > maxBackoff*5/4 {
		t.Fatalf("backoff = %v, exceeds capped maximum with jitter", got)
	}
	if got := retryBackoff(1, 0); got != 0 {
		t.Fatalf("zero base delay should retry immediately, got %v", got)
	}
}

func TestShortError(t *testing.T) {
	err := fmt.Errorf("buzzheavier: upload failed: %w", errors.New("wsasend: reset"))
	if got := shortError(err); got != "wsasend: reset" {
		t.Fatalf("shortError() = %q, want root cause", got)
	}
	long := strings.Repeat("x", 100)
	if got := shortError(errors.New(long)); len([]rune(got)) > 44 {
		t.Fatalf("shortError() did not truncate: %d runes", len([]rune(got)))
	}
}

// fakeUploader fails with errs[i] on call i, then succeeds.
type fakeUploader struct {
	name  string
	calls int
	errs  []error
}

func (f *fakeUploader) Name() string                  { return f.name }
func (f *fakeUploader) DisplayName() string           { return "Fake" }
func (f *fakeUploader) Description() string           { return "fake service for tests" }
func (f *fakeUploader) RequiredCredentials() []string { return nil }
func (f *fakeUploader) SupportsLargeUpload() bool     { return true }

func (f *fakeUploader) Upload(ctx context.Context, filename string, reader io.Reader, size int64, creds plugin.Credentials, cfg plugin.Config) (*plugin.UploadResult, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) {
		return nil, f.errs[i]
	}
	return &plugin.UploadResult{Service: f.name, URL: "https://fake.test/" + filename}, nil
}

// newTestManager wires a fake uploader with the given retry config.
func newTestManager(t *testing.T, fake *fakeUploader, retries, delay int) *UploadManager {
	t.Helper()
	reg := plugin.NewRegistry()
	reg.Register(fake)
	cfg := &config.AppConfig{
		SelectedServices: []string{fake.name},
		UploadMode:       config.ModeParallel,
		Services:         map[string]*config.ServiceConfig{},
	}
	if retries >= 0 {
		cfg.SetRetrySettings(retries, delay)
	}
	return New(reg, cfg)
}

func uploadViaManager(t *testing.T, mgr *UploadManager, path string) (*MultiResult, error) {
	t.Helper()
	progressCh := make(chan Progress, 500)
	res, err := mgr.UploadFile(context.Background(), path, progressCh)
	close(progressCh)
	return res, err
}

func writeTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	fake := &fakeUploader{name: "fake", errs: []error{
		errors.New("connection reset by peer"),
		errors.New("read tcp: connection timed out"),
	}}
	// retries < 0 leaves MaxRetries nil so config defaults apply (2 retries).
	mgr := newTestManager(t, fake, -1, 0)

	path := writeTestFile(t)
	res, err := uploadViaManager(t, mgr, path)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3 (1 initial + 2 default retries)", fake.calls)
	}
	r := res.Results[0]
	if r.Error != nil {
		t.Fatalf("unexpected error after retries: %v", r.Error)
	}
	if r.URL != "https://fake.test/file.bin" {
		t.Fatalf("URL = %q", r.URL)
	}
}

func TestNoRetryOnPermanentFailure(t *testing.T) {
	fake := &fakeUploader{name: "fake", errs: []error{
		errors.New("buzzheavier: no data in response (status 403): forbidden"),
	}}
	mgr := newTestManager(t, fake, 2, 0)

	path := writeTestFile(t)
	res, err := uploadViaManager(t, mgr, path)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1 (permanent error must not retry)", fake.calls)
	}
	if res.Results[0].Error == nil {
		t.Fatal("expected an error on the result")
	}
}

func TestRetriesExhaustedAnnotatesError(t *testing.T) {
	fake := &fakeUploader{name: "fake", errs: []error{
		errors.New("connection reset by peer"),
		errors.New("connection reset by peer"),
		errors.New("connection reset by peer"),
	}}
	mgr := newTestManager(t, fake, 2, 0)

	path := writeTestFile(t)
	res, err := uploadViaManager(t, mgr, path)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3", fake.calls)
	}
	rerr := res.Results[0].Error
	if rerr == nil || !strings.Contains(rerr.Error(), "(after 3 attempts)") {
		t.Fatalf("error = %v, want attempt annotation", rerr)
	}
}

func TestZeroRetriesDisablesRetry(t *testing.T) {
	fake := &fakeUploader{name: "fake", errs: []error{
		errors.New("connection reset by peer"),
	}}
	mgr := newTestManager(t, fake, 0, 0)

	path := writeTestFile(t)
	res, err := uploadViaManager(t, mgr, path)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1 (retries disabled)", fake.calls)
	}
	if res.Results[0].Error == nil {
		t.Fatal("expected an error on the result")
	}
}

func TestRetryInSequentialMode(t *testing.T) {
	fake := &fakeUploader{name: "fake", errs: []error{
		errors.New("wsasend: An existing connection was forcibly closed by the remote host."),
	}}
	mgr := newTestManager(t, fake, 2, 0)
	mgr.cfg.UploadMode = config.ModeSequential

	path := writeTestFile(t)
	res, err := uploadViaManager(t, mgr, path)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("calls = %d, want 2", fake.calls)
	}
	if res.Results[0].Error != nil {
		t.Fatalf("unexpected error after retry: %v", res.Results[0].Error)
	}
}
