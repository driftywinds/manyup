package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/driftywinds/manyup/internal/config"
)

func TestCredentialEncryptionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("XDG_CONFIG_HOME")

	// Create a config with a credential and save it.
	cfg := config.DefaultConfig()
	cfg.SetCredential("datanodes", "API_TOKEN", "super-secret-key-123")
	cfg.SetCredential("gofile", "API_TOKEN", "gofile-token-abc")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// Verify the file on disk contains encrypted values (enc:// prefix).
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	if !contains(contents, "enc://") {
		t.Fatal("config file does not contain encrypted values (missing enc:// prefix)")
	}
	if contains(contents, "super-secret-key-123") {
		t.Fatal("plaintext credential found in config file on disk")
	}

	// Load it back and verify credentials are decrypted.
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	sc, ok := loaded.Services["datanodes"]
	if !ok {
		t.Fatal("datanodes service not found after load")
	}
	if token, ok := sc.Credentials["API_TOKEN"]; !ok || token != "super-secret-key-123" {
		t.Fatalf("datanodes API_TOKEN mismatch: got %q", token)
	}

	gf, ok := loaded.Services["gofile"]
	if !ok {
		t.Fatal("gofile service not found after load")
	}
	if token, ok := gf.Credentials["API_TOKEN"]; !ok || token != "gofile-token-abc" {
		t.Fatalf("gofile API_TOKEN mismatch: got %q", token)
	}
}

func TestLoadPlaintextConfigFromBeforeEncryption(t *testing.T) {
	// Simulate an old config.json that was written before encryption was added.
	dir := t.TempDir()
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("XDG_CONFIG_HOME")

	plaintextConfig := `{
		"selected_services": ["datanodes"],
		"upload_mode": "parallel",
		"services": {
			"datanodes": {
				"enabled": true,
				"credentials": {
					"API_TOKEN": "old-plaintext-key"
				}
			}
		}
	}`
	configDir := filepath.Join(dir, "manyup")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(plaintextConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	sc, ok := loaded.Services["datanodes"]
	if !ok {
		t.Fatal("datanodes service not found")
	}
	if token, ok := sc.Credentials["API_TOKEN"]; !ok || token != "old-plaintext-key" {
		t.Fatalf("plaintext cred not preserved: got %q", token)
	}

	// Saving again should encrypt it now.
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	newData, _ := os.ReadFile(filepath.Join(dir, "manyup", "config.json"))
	if !contains(string(newData), "enc://") {
		t.Fatal("re-saved config not encrypted")
	}
}

func TestEnvVarCredentialsStillWork(t *testing.T) {
	dir := t.TempDir()
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("XDG_CONFIG_HOME")

	os.Setenv("MANYUP_DATANODES_API_TOKEN", "from-env-token")

	cfg := config.DefaultConfig()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// Verify the file exists and is valid JSON with no credentials.
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("config file not found at %s: %v", config.ConfigPath(), err)
	}
	var raw struct {
		Services map[string]struct {
			Credentials map[string]string `json:"credentials"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("config file not valid JSON: %v", err)
	}
	if len(raw.Services) != 0 {
		t.Fatal("expected empty services in config file")
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// No credentials were set in the config, so the map should be empty.
	if len(loaded.Services) != 0 {
		t.Fatalf("expected empty services, got %d", len(loaded.Services))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}


