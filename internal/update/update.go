package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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

// DownloadAndReplace downloads the new binary and replaces the current one
func DownloadAndReplace(downloadURL, currentVersion string) error {
	// Create an HTTP client with tuned settings for speed
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     10 * time.Second,
		},
	}

	// Download the new binary
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Create a temporary file to download to - use temp dir on same filesystem for fast rename
	tmpDir, err := os.MkdirTemp("", "manyup-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile, err := os.CreateTemp(tmpDir, "manyup-binary-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	// Download with progress tracking
	progress := &progressWriter{}
	teeReader := io.TeeReader(resp.Body, progress)
	n, err := io.Copy(tmpFile, teeReader)
	tmpFile.Close()
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("downloaded file is empty")
	}
	fmt.Println() // New line after progress dots

	// Get the current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Get the directory of the executable for the rename
	execDir := filepath.Dir(execPath)

	// On Windows, we can't replace a running executable directly
	if runtime.GOOS == "windows" {
		return replaceOnWindows(execPath, tmpFile.Name())
	}

	// Move temp file to the executable's directory for atomic rename
	// This ensures we're on the same filesystem for a fast rename
	finalTempPath := filepath.Join(execDir, ".manyup_update_tmp")
	if err := os.Rename(tmpFile.Name(), finalTempPath); err != nil {
		return fmt.Errorf("failed to move temp file: %w", err)
	}
	defer os.Remove(finalTempPath)

	// Make the temp file executable
	if err := os.Chmod(finalTempPath, 0755); err != nil {
		return fmt.Errorf("failed to make temp file executable: %w", err)
	}

	// Replace the current binary with atomic rename
	if err := os.Rename(finalTempPath, execPath); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}


// replaceOnWindows handles Windows-specific binary replacement
func replaceOnWindows(execPath, tmpFile string) error {
	// On Windows, we need to:
	// 1. Copy the new binary to a temp location
	// 2. Create a batch script that will replace the binary and relaunch
	// 3. Run the batch script and exit

	// Get the directory of the executable
	execDir := filepath.Dir(execPath)
	tmpBatch := filepath.Join(execDir, ".manyup_update.bat")

	// Create the batch script
	batchContent := fmt.Sprintf(`@echo off
timeout /t 1 /nobreak >nul
copy /Y "%s" "%s" >nul
del "%s"
start "" "%s"
exit
`, tmpFile, execPath, tmpBatch, execPath)

	if err := os.WriteFile(tmpBatch, []byte(batchContent), 0644); err != nil {
		return fmt.Errorf("failed to create update script: %w", err)
	}
	defer os.Remove(tmpBatch)

	// Run the batch script
	cmd := exec.Command("cmd", "/c", tmpBatch)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start update: %w", err)
	}

	// Exit the current process - the batch script will handle the rest
	os.Exit(0)

	return nil
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

// progressWriter provides basic download progress
type progressWriter struct {
	lastPrint time.Time
	count     int64
}

func (pw progressWriter) Write(p []byte) (n int, err error) {
	n, err = io.Discard.Write(p)
	if err != nil {
		return
	}

	pw.count += int64(n)

	// Print progress every 500KB or 500ms
	if pw.count >= 512000 || time.Since(pw.lastPrint) > 500*time.Millisecond {
		fmt.Printf(".")
		pw.count = 0
		pw.lastPrint = time.Now()
	}
	return
}
