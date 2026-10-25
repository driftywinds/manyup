// Package config manages persistent configuration: API keys, selected services,
// and upload mode preferences. Config lives in a JSON file at a well-known path.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// UploadMode controls how files are sent to selected services.
type UploadMode string

const (
	ModeParallel   UploadMode = "parallel"
	ModeSequential UploadMode = "sequential"
)

const (
	// DefaultMaxRetries is the number of re-attempts per service after the
	// initial upload fails, when the config does not override it.
	DefaultMaxRetries = 2
	// DefaultRetryDelaySeconds is the base wait between attempts. Each
	// subsequent retry doubles it (exponential backoff).
	DefaultRetryDelaySeconds = 2
)

// ServiceConfig holds credentials and optional overrides for one service.
type ServiceConfig struct {
	Enabled     bool              `json:"enabled"`
	Credentials map[string]string `json:"credentials"`
	Options     map[string]string `json:"options,omitempty"` // extra key-value overrides
}

// AppConfig is the top-level persisted config.
type AppConfig struct {
	SelectedServices []string   `json:"selected_services"`
	UploadMode       UploadMode `json:"upload_mode"`
	// MaxRetries is the number of retries per service after a failed
	// upload attempt. Pointer so absent keys fall back to
	// DefaultMaxRetries while an explicit 0 disables retries.
	MaxRetries *int `json:"max_retries,omitempty"`
	// RetryDelaySeconds is the base backoff delay between attempts.
	// Pointer so absent keys fall back to DefaultRetryDelaySeconds.
	RetryDelaySeconds *int                      `json:"retry_delay_seconds,omitempty"`
	Services          map[string]*ServiceConfig `json:"services"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *AppConfig {
	return &AppConfig{
		SelectedServices: []string{},
		UploadMode:       ModeParallel,
		Services:         make(map[string]*ServiceConfig),
	}
}

// IsPortable is set via ldflags at build time to enable portable mode.
// When true, config lives next to the executable instead of %APPDATA%.
var IsPortable = "false"

// ConfigDir returns the config directory.
// In portable mode: <executable_dir>/config
// Otherwise:        %APPDATA%/manyup (Windows) or ~/.config/manyup (others)
func ConfigDir() string {
	if IsPortable == "true" {
		if execPath, err := os.Executable(); err == nil {
			return filepath.Join(filepath.Dir(execPath), "config")
		}
	}
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

// Path returns the full path of the config file, whether or not it exists yet.
// It reflects portable mode, so it follows the executable's own directory when
// that build is used.
func Path() string {
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
	return &cfg, nil
}

// Save writes config to disk, creating directories as needed.
func (c *AppConfig) Save() error {
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

// SetCredential sets an API key for a given service.
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

// RetrySettings returns the configured retry behaviour: how many times a
// failed service upload is re-attempted (after the first try) and the base
// delay in seconds between attempts. Unset values fall back to defaults.
func (c *AppConfig) RetrySettings() (maxRetries, delaySeconds int) {
	maxRetries, delaySeconds = DefaultMaxRetries, DefaultRetryDelaySeconds
	if c.MaxRetries != nil {
		maxRetries = *c.MaxRetries
	}
	if c.RetryDelaySeconds != nil {
		delaySeconds = *c.RetryDelaySeconds
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	if delaySeconds < 0 {
		delaySeconds = 0
	}
	return maxRetries, delaySeconds
}

// SetRetrySettings persists the retry settings; a maxRetries of 0 disables
// retries entirely.
func (c *AppConfig) SetRetrySettings(maxRetries, delaySeconds int) {
	mr, ds := maxRetries, delaySeconds
	c.MaxRetries = &mr
	c.RetryDelaySeconds = &ds
}
