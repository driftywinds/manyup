// Package plugin defines the core interface that all upload service plugins must implement,
// plus a global registry for service discovery.
package plugin

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
)

// UploadResult holds the outcome of a single upload attempt.
type UploadResult struct {
	Service   string // human-readable service name
	URL       string // direct download URL returned by the service
	Filename  string
	Size      int64
	Error     error
	Duration  float64 // seconds
}

// Credentials holds API key / token data needed by a service.
type Credentials map[string]string

// Config holds per-service configuration beyond credentials (e.g. upload URL overrides).
type Config map[string]string

// Uploader is the interface every upload service plugin must implement.
type Uploader interface {
	// Name returns the unique slug for this service (e.g. "gofile", "buzzheavier").
	Name() string

	// DisplayName returns a human-readable name (e.g. "GoFile", "BuzzHeavier").
	DisplayName() string

	// Description returns a short one-liner about the service.
	Description() string

	// RequiredCredentials returns the env-var / config keys this service needs
	// (e.g. "API_KEY", "TOKEN"). The registry uses this for validation.
	RequiredCredentials() []string

	// Upload streams the file to the service and returns a result.
	// It must respect context cancellation for fast abort.
	Upload(ctx context.Context, filename string, reader io.Reader, size int64, creds Credentials, cfg Config) (*UploadResult, error)

	// SupportsLargeUpload returns true if the plugin handles chunked/multipart
	// streaming (for files that shouldn't be buffered entirely in memory).
	SupportsLargeUpload() bool
}

// Registry holds all registered service plugins.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]Uploader
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]Uploader)}
}

// Register adds a plugin. Panics on duplicate names.
func (r *Registry) Register(u Uploader) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := u.Name()
	if _, dup := r.plugins[name]; dup {
		panic(fmt.Sprintf("plugin: duplicate registration for %q", name))
	}
	r.plugins[name] = u
}

// Get returns a plugin by name, or error if not found.
func (r *Registry) Get(name string) (Uploader, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.plugins[name]
	return u, ok
}

// All returns a copy of all registered plugins keyed by name.
func (r *Registry) All() map[string]Uploader {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Uploader, len(r.plugins))
	for k, v := range r.plugins {
		out[k] = v
	}
	return out
}

// Names returns sorted list of all registered plugin names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.plugins))
	for k := range r.plugins {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
