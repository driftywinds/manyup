package update

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Keep download progress dots out of the test output.
	progressOut = io.Discard
	os.Exit(m.Run())
}

// fakeBinary builds a payload that passes the size and header checks.
func fakeBinary(fill byte) []byte {
	payload := bytes.Repeat([]byte{fill}, minBinarySize+1024)

	switch runtime.GOOS {
	case "windows":
		copy(payload, []byte{'M', 'Z'})
	case "linux":
		copy(payload, []byte{0x7F, 'E', 'L', 'F'})
	case "darwin":
		copy(payload, []byte{0xCF, 0xFA, 0xED, 0xFE})
	}
	return payload
}

func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tempExec writes an executable placeholder into a fresh directory.
func tempExec(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manyup.exe")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("writing placeholder binary: %v", err)
	}
	return path
}

// leftoverTemps reports any download temp files left in dir.
func leftoverTemps(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".manyup-update-*"))
	if err != nil {
		t.Fatalf("globbing temp files: %v", err)
	}
	return matches
}

func TestHasExecutableHeaderWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific header expectations")
	}
	if !hasExecutableHeader([]byte{'M', 'Z', 0x90, 0x00}) {
		t.Error("MZ header should be accepted")
	}
	if hasExecutableHeader([]byte("<htm")) {
		t.Error("HTML should be rejected")
	}
	if hasExecutableHeader([]byte{'M', 'Z'}) {
		t.Error("a short header should be rejected")
	}
}

func TestApplyToReplacesBinary(t *testing.T) {
	oldContent := fakeBinary(0x01)
	newContent := fakeBinary(0x02)

	execPath := tempExec(t, oldContent)
	srv := serve(t, newContent)

	if err := ApplyTo(execPath, srv.URL); err != nil {
		t.Fatalf("ApplyTo returned error: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	if !bytes.Equal(got, newContent) {
		t.Errorf("installed binary has %d bytes, want %d", len(got), len(newContent))
	}

	stale := execPath + ".old"
	if runtime.GOOS == "windows" {
		// The previous image is renamed aside so it can be deleted next start.
		prev, err := os.ReadFile(stale)
		if err != nil {
			t.Fatalf("expected the previous binary at %s: %v", stale, err)
		}
		if !bytes.Equal(prev, oldContent) {
			t.Error("the saved previous binary does not match the original")
		}
	} else if _, err := os.Stat(stale); err == nil {
		t.Error("no .old file should be left behind on this platform")
	}

	if leftovers := leftoverTemps(t, filepath.Dir(execPath)); len(leftovers) != 0 {
		t.Errorf("left temporary files behind: %v", leftovers)
	}
}

func TestApplyToRemovesStaleOldFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the .old file only exists on Windows")
	}

	execPath := tempExec(t, fakeBinary(0x01))
	if err := os.WriteFile(execPath+".old", []byte("leftover from a previous update"), 0o644); err != nil {
		t.Fatalf("creating stale file: %v", err)
	}

	srv := serve(t, fakeBinary(0x03))
	if err := ApplyTo(execPath, srv.URL); err != nil {
		t.Fatalf("ApplyTo returned error: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	if !bytes.Equal(got, fakeBinary(0x03)) {
		t.Error("binary was not replaced")
	}
}

func TestApplyToRejectsHTMLResponse(t *testing.T) {
	original := fakeBinary(0x01)
	execPath := tempExec(t, original)

	// A 404-style HTML body still has HTTP 200 here, so only the header check
	// can catch it.
	srv := serve(t, []byte(strings.Repeat("<html>not a binary</html>", 60000)))

	err := ApplyTo(execPath, srv.URL)
	if err == nil {
		t.Fatal("expected an error for a non-executable download")
	}
	if !strings.Contains(err.Error(), "not a") {
		t.Errorf("error = %v, want it to explain the rejected file", err)
	}

	got, readErr := os.ReadFile(execPath)
	if readErr != nil {
		t.Fatalf("reading the binary: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Error("the original binary must be left untouched when the download is rejected")
	}
	if leftovers := leftoverTemps(t, filepath.Dir(execPath)); len(leftovers) != 0 {
		t.Errorf("left temporary files behind: %v", leftovers)
	}
}

func TestApplyToRejectsSmallDownload(t *testing.T) {
	execPath := tempExec(t, fakeBinary(0x01))

	srv := serve(t, []byte("MZ"))
	err := ApplyTo(execPath, srv.URL)
	if err == nil {
		t.Fatal("expected an error for a tiny download")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("error = %v, want it to mention the size", err)
	}
}

func TestApplyToReportsBadStatus(t *testing.T) {
	execPath := tempExec(t, fakeBinary(0x01))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := ApplyTo(execPath, srv.URL)
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Errorf("error = %v, want it to include the status", err)
	}
}
