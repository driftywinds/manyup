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
	"github.com/multiuploader/manyup/internal/plugin"
)

// All returns a new registry pre-loaded with every built-in service plugin.
func All() *plugin.Registry {
	r := plugin.NewRegistry()
	// Each service's init() calls r.Register(), but since init runs on import
	// we register explicitly here for clarity and testability.
	RegisterBuzzHeavier(r)
	RegisterDataNodes(r)
	RegisterGoFile(r)
	RegisterVikingFile(r)
	RegisterZincDrive(r)
	return r
}
