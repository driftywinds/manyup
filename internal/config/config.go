// Package config manages persistent configuration: API keys, selected services,
// and upload mode preferences. Config lives in a JSON file at a well-known path.
// Credentials are encrypted with a machine-local master key before storage.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/driftywinds/manyup/internal/secret"
)

// UploadMode controls how files are sent to selected services.
type UploadMode string

const (
	ModeParallel  UploadMode = "parallel"
	ModeSequential UploadMode = "sequential"
)

// ServiceConfig holds credentials and optional overrides for one service.
type ServiceConfig struct {
	Enabled     bool              `json:"enabled"`
	Credentials map[string]string `json:"credentials"`
	Options     map[string]string `json:"options,omitempty"` // extra key-value overrides
}

// AppConfig is the top-level persisted config.
type AppConfig struct {
	SelectedServices []string                   `json:"selected_services"`
	UploadMode       UploadMode                 `json:"upload_mode"`
	Services         map[string]*ServiceConfig  `json:"services"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *AppConfig {
	return &AppConfig{
		SelectedServices: []string{},
		UploadMode:       ModeParallel,
		Services:         make(map[string]*ServiceConfig),
	}
}

// ConfigDir returns the platform-appropriate config directory.
// On Windows: %APPDATA%/manyup
// On others:  ~/.config/manyup
func ConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "manyup")
	}
	if home := os.Getenv("APPDATA"); home != "" {
		return filepath.Join(home, "manyup")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "manyup")
}

func configPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// ConfigPath returns the absolute path to the config file. It is useful for
// tests and debugging.
func ConfigPath() string {
	return configPath()
}

// Load reads config from disk. Returns defaults if no file exists.
func Load() (*AppConfig, error) {
	path := configPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Services == nil {
		cfg.Services = make(map[string]*ServiceConfig)
	}
	// Decrypt any encrypted credential values stored in the config file.
	if err := cfg.decryptCredentials(); err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	return &cfg, nil
}

// Save writes config to disk, creating directories as needed.
// Credentials are encrypted with a machine-local master key before storage.
func (c *AppConfig) Save() error {
	// Ensure the master key exists (creates it on first run) and get it.
	key, err := secret.EnsureMasterKey()
	if err != nil {
		return fmt.Errorf("secret: %w", err)
	}

	// Encrypt all credential values in place.
	c.encryptCredentials(key)

	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(configPath(), data, 0o600)
}

// SetCredential sets an API token for a given service.
// The value is stored encrypted in the config file; it is decrypted on Load().
func (c *AppConfig) SetCredential(service, key, value string) {
	sc, ok := c.Services[service]
	if !ok {
		sc = &ServiceConfig{
			Enabled:     true,
			Credentials: make(map[string]string),
			Options:     make(map[string]string),
		}
		c.Services[service] = sc
	}
	if sc.Credentials == nil {
		sc.Credentials = make(map[string]string)
	}
	sc.Credentials[key] = value
	sc.Enabled = true
}

// ToggleService adds or removes a service from the selected list.
func (c *AppConfig) ToggleService(service string) {
	for i, s := range c.SelectedServices {
		if s == service {
			c.SelectedServices = append(c.SelectedServices[:i], c.SelectedServices[i+1:]...)
			return
		}
	}
	c.SelectedServices = append(c.SelectedServices, service)
}

// encryptCredentials encrypts every credential value in the config using the
// provided master key. It mutates the config in place.
func (c *AppConfig) encryptCredentials(key secret.MasterKey) {
	for _, sc := range c.Services {
		if sc.Credentials == nil {
			continue
		}
		for k, v := range sc.Credentials {
			if strings.HasPrefix(v, secret.EncryptedPrefix) {
				continue // already encrypted
			}
			enc, err := secret.Encrypt(key, v)
			if err != nil {
				continue // best-effort: skip values we can't encrypt
			}
			sc.Credentials[k] = enc
		}
	}
}

// decryptCredentials decrypts every credential value that has the EncryptedPrefix
// back to plaintext. It mutates the config in place. Plaintext values (including
// those set via env vars at runtime) are left as-is.
func (c *AppConfig) decryptCredentials() error {
	key, err := secret.EnsureMasterKey()
	if err != nil {
		return err
	}
	for svcName, sc := range c.Services {
		if sc.Credentials == nil {
			continue
		}
		for k, v := range sc.Credentials {
			if !strings.HasPrefix(v, secret.EncryptedPrefix) {
				continue
			}
			plain, err := secret.Decrypt(key, v)
			if err != nil {
				return fmt.Errorf("service %q key %q: %w", svcName, k, err)
			}
			sc.Credentials[k] = plain
		}
	}
	return nil
}

