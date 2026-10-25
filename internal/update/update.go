package update

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ReleaseInfo represents the latest GitHub release data
type ReleaseInfo struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
	Version SemVer
}

// Asset represents a release asset
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

// SemVer represents a semantic version
type SemVer struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

// ParseSemVer parses a version string like "v0.2.2" or "0.2.2" into SemVer
func ParseSemVer(v string) (SemVer, error) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return SemVer{}, fmt.Errorf("invalid version format: %s (expected X.Y.Z)", v)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid major version: %s", parts[0])
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid minor version: %s", parts[1])
	}

	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid patch version: %s", parts[2])
	}

	return SemVer{Major: major, Minor: minor, Patch: patch, Raw: v}, nil
}

// Compare returns -1 if v < other, 0 if equal, 1 if v > other
func (v SemVer) Compare(other SemVer) int {
	if v.Major != other.Major {
		if v.Major > other.Major {
			return 1
		}
		return -1
	}
	if v.Minor != other.Minor {
		if v.Minor > other.Minor {
			return 1
		}
		return -1
	}
	if v.Patch != other.Patch {
		if v.Patch > other.Patch {
			return 1
		}
		return -1
	}
	return 0
}

// IsNewer returns true if v is newer than other
func (v SemVer) IsNewer(other SemVer) bool {
	return v.Compare(other) > 0
}

// String returns the version in X.Y.Z format
func (v SemVer) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// CheckLatest fetches the latest release from GitHub
func CheckLatest() (*ReleaseInfo, error) {
	client := &http.Client{}

	resp, err := client.Get("https://api.github.com/repos/driftywinds/manyup/releases/latest")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release data: %w", err)
	}

	// Parse the semver from the tag
	release.Version, err = ParseSemVer(release.TagName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse version from tag %s: %w", release.TagName, err)
	}

	return &release, nil
}

// FindAsset finds the download URL for the specified platform
func (r *ReleaseInfo) FindAsset(platform string) (string, error) {
	// Map runtime platform strings to release asset names
	// Release assets use format: manyup-<os>-<arch> or manyup-<os>-<arch>.exe
	normalizedPlatform := normalizePlatform(platform)

	for _, asset := range r.Assets {
		if asset.Name == normalizedPlatform || asset.Name == normalizedPlatform+".exe" {
			return asset.DownloadURL, nil
		}
	}

	// Try fuzzy matching for arm64 vs arm
	for _, asset := range r.Assets {
		if strings.HasPrefix(asset.Name, "manyup-") &&
			strings.HasSuffix(asset.Name, ".exe") == strings.HasSuffix(normalizedPlatform, ".exe") {
			// Check if OS and arch match
			parts := strings.Split(strings.TrimSuffix(asset.Name, ".exe"), "-")
			if len(parts) == 3 && parts[1] == getOS() && matchesArch(parts[2], getArch()) {
				return asset.DownloadURL, nil
			}
		}
	}

	return "", fmt.Errorf("no release asset found for platform %s", platform)
}

// minBinarySize is the smallest plausible manyup binary. Anything smaller is a
// truncated download or an error page, not an executable.
const minBinarySize = 1 << 20 // 1 MiB

// downloadTimeout bounds the whole download, not each read.
const downloadTimeout = 5 * time.Minute

// Apply downloads the release at downloadURL and swaps it in for the currently
// running executable, then returns. The caller keeps running the old image in
// memory; the next launch picks up the new binary.
//
// The new binary is written into the install folder, so the final step is a
// rename on the same volume rather than a copy. That matters on Windows, where
// the running executable cannot be opened for writing but can be renamed aside.
func Apply(downloadURL string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(execPath); resolveErr == nil {
		execPath = resolved
	}
	return ApplyTo(execPath, downloadURL)
}

// ApplyTo replaces the binary at execPath. It is exported separately so tests
// can point it at a scratch file instead of the running process image.
func ApplyTo(execPath, downloadURL string) error {
	dir := filepath.Dir(execPath)

	// Fail fast and clearly when the install folder is read-only, instead of
	// downloading for a minute and then failing on the rename.
	tmpFile, err := os.CreateTemp(dir, ".manyup-update-*")
	if err != nil {
		return fmt.Errorf("the install folder %q is not writable (try running as administrator): %w", dir, err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath) // a no-op once the rename has moved the file

	if err := downloadTo(downloadURL, tmpPath); err != nil {
		return err
	}
	if err := validateBinary(tmpPath); err != nil {
		return err
	}

	stalePath := execPath + ".old"
	if _, statErr := os.Stat(stalePath); statErr == nil {
		// Left behind by an earlier update whose process had not exited yet.
		if rmErr := os.Remove(stalePath); rmErr != nil {
			return fmt.Errorf("cannot remove the previous binary %q: %w (close any other manyup processes and retry)", stalePath, rmErr)
		}
	}

	if runtime.GOOS == "windows" {
		// A running image cannot be overwritten, but it can be renamed.
		if err := os.Rename(execPath, stalePath); err != nil {
			return fmt.Errorf("could not move the current binary aside: %w", err)
		}
		if err := os.Rename(tmpPath, execPath); err != nil {
			// Never leave the install without a working binary.
			_ = os.Rename(stalePath, execPath)
			return fmt.Errorf("could not install the new binary: %w", err)
		}
		return nil
	}

	// On Unix the rename replaces the target atomically.
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("could not mark the new binary executable: %w", err)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		return fmt.Errorf("could not install the new binary: %w", err)
	}
	return nil
}

// CleanupPreviousBinary deletes the "<exe>.old" copy that a Windows update
// leaves behind. Call it once at start-up; it is a no-op elsewhere and never
// reports errors.
func CleanupPreviousBinary() {
	if runtime.GOOS != "windows" {
		return
	}
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, resolveErr := filepath.EvalSymlinks(execPath); resolveErr == nil {
		execPath = resolved
	}
	_ = os.Remove(execPath + ".old")
}

// downloadTo fetches downloadURL into path, rejecting anything that does not
// arrive completely or is too small to be a real binary.
func downloadTo(downloadURL, path string) error {
	client := &http.Client{Timeout: downloadTimeout}

	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("creating the download request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "manyup-updater")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: server returned status %d", resp.StatusCode)
	}

	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("opening the download target: %w", err)
	}

	progress := &progressWriter{}
	written, copyErr := io.Copy(dst, io.TeeReader(resp.Body, progress))
	closeErr := dst.Close()
	fmt.Println()

	if copyErr != nil {
		return fmt.Errorf("download failed: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("writing the downloaded binary: %w", closeErr)
	}

	if expected := resp.ContentLength; expected > 0 && written != expected {
		return fmt.Errorf("download was incomplete: got %d of %d bytes", written, expected)
	}
	if written < minBinarySize {
		return fmt.Errorf("download is only %d bytes, which is too small to be manyup", written)
	}
	return nil
}

// validateBinary checks the file header, which catches an HTML error page or a
// file truncated mid-header before it can be installed.
func validateBinary(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading the downloaded file: %w", err)
	}
	defer f.Close()

	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("the downloaded file is truncated: %w", err)
	}
	if !hasExecutableHeader(head) {
		return fmt.Errorf("the downloaded file is not a %s executable", runtime.GOOS)
	}
	return nil
}

// hasExecutableHeader reports whether head starts with a known executable magic
// for the current platform.
func hasExecutableHeader(head []byte) bool {
	if len(head) < 4 {
		return false
	}

	switch runtime.GOOS {
	case "windows":
		return head[0] == 'M' && head[1] == 'Z'

	case "linux":
		return bytes.HasPrefix(head, []byte{0x7F, 'E', 'L', 'F'})

	case "darwin":
		// Mach-O in either byte order, thin or universal.
		be := uint32(head[0])<<24 | uint32(head[1])<<16 | uint32(head[2])<<8 | uint32(head[3])
		le := uint32(head[3])<<24 | uint32(head[2])<<16 | uint32(head[1])<<8 | uint32(head[0])
		const (
			machO32 = 0xFEEDFACE
			machO64 = 0xFEEDFACF
			fat     = 0xCAFEBABE
		)
		for _, magic := range []uint32{be, le} {
			if magic == machO32 || magic == machO64 || magic == fat {
				return true
			}
		}
		return false
	}

	// Unknown platform: the size check above is all we can do.
	return true
}

// normalizePlatform converts runtime platform string to release asset format
func normalizePlatform(platform string) string {
	parts := strings.Split(platform, "-")
	if len(parts) != 2 {
		return platform
	}

	osName := normalizeOS(parts[0])
	archName := normalizeArch(parts[1])

	return fmt.Sprintf("manyup-%s-%s", osName, archName)
}

// normalizeOS normalizes OS names to match release naming
func normalizeOS(osName string) string {
	switch osName {
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	case "windows":
		return "windows"
	default:
		return osName
	}
}

// normalizeArch normalizes architecture names to match release naming
func normalizeArch(archName string) string {
	switch archName {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm64" // Assume arm64 for arm
	default:
		return archName
	}
}

// getOS returns the normalized OS name
func getOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	case "windows":
		return "windows"
	default:
		return runtime.GOOS
	}
}

// getArch returns the normalized architecture name
func getArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// matchesArch checks if two architecture strings match (handling arm vs arm64)
func matchesArch(releaseArch, runtimeArch string) bool {
	if releaseArch == runtimeArch {
		return true
	}
	// Handle arm vs arm64
	if releaseArch == "arm64" && runtimeArch == "arm" {
		return true
	}
	return false
}

// progressOut is where download progress dots are written. Tests replace it to
// keep their output clean.
var progressOut io.Writer = os.Stdout

// progressWriter provides basic download progress
type progressWriter struct {
	lastPrint time.Time
	count     int64
}

func (pw *progressWriter) Write(p []byte) (n int, err error) {
	pw.count += int64(len(p))

	// Print a dot every ~500 KB or every 500 ms.
	if pw.count >= 512000 || time.Since(pw.lastPrint) > 500*time.Millisecond {
		fmt.Fprint(progressOut, ".")
		pw.count = 0
		pw.lastPrint = time.Now()
	}
	return len(p), nil
}
