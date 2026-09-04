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
// On Windows: %APPDATA%/multiuploader
// On others:  ~/.config/multiuploader
func ConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "multiuploader")
	}
	if home := os.Getenv("APPDATA"); home != "" {
		return filepath.Join(home, "multiuploader")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "multiuploader")
}

func configPath() string {
	return filepath.Join(ConfigDir(), "config.json")
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
