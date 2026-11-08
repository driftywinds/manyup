// Package services contains all upload service plugin implementations.
// Each service is registered via an init() function so importing this package
// automatically populates the registry.
//
// To add a new service:
//  1. Create a new file in this package implementing plugin.Uploader.
//  2. Register it in init() with registry.Register().
//  3. The CLI will automatically discover it.
package services

import (
	"github.com/driftywinds/manyup/internal/config"
	"github.com/driftywinds/manyup/internal/plugin"
)

// All returns a new registry pre-loaded with every built-in service plugin and
// the default interim instance. Configured interim instances are added by
// AllWithConfig.
func All() *plugin.Registry {
	return buildRegistry(nil)
}

// AllWithConfig returns a registry that also contains one plugin per configured
// interim instance, so each instance can be selected and uploaded to
// independently. A nil config is equivalent to All.
func AllWithConfig(cfg *config.AppConfig) *plugin.Registry {
	return buildRegistry(cfg)
}

func buildRegistry(cfg *config.AppConfig) *plugin.Registry {
	r := plugin.NewRegistry()
	// Each service's init() calls r.Register(), but since init runs on import
	// we register explicitly here for clarity and testability.
	RegisterAbyss(r)
	RegisterAnonMP4(r)
	RegisterBuzzHeavier(r)
	RegisterDataNodes(r)
	RegisterFuckingFast(r)
	RegisterGoFile(r)
	RegisterInterimConfigured(r, cfg)
	RegisterMocha(r)
	RegisterZeroXZero(r)
	RegisterPixelDrain(r)
	RegisterStorageTo(r)
	RegisterStreamtape(r)
	RegisterVikingFile(r)
	RegisterZincDrive(r)
	return r
}
