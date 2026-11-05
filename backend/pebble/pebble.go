// Package bdpebble is the public door to the Pebble key-value store that
// ships inside this module (internal/storage/pebble) for embedders that link
// beads as a library rather than running cmd/bd.
//
// Importing it registers the backend under Name, so a workspace whose
// .beads/metadata.json names "pebble" opens through the registry facade
// (beads.OpenBestAvailable and backend.Lookup). The store lives under the
// workspace's .beads directory (DataDirName) and is held by one process at a
// time; a second process that opens the same directory fails on Pebble's own
// lock. Open and OpenInMemory are here for embedders and their tests that
// want a store without a workspace.
package bdpebble

import (
	"context"
	"path/filepath"

	"github.com/steveyegge/beads/backend"
	"github.com/steveyegge/beads/internal/storage/pebble"
)

// Name is the backend name the registry knows the store under.
const Name = pebble.BackendName

// DataDirName is the directory under .beads that holds the store.
const DataDirName = pebble.DataDirName

// Open opens (creating if needed) the store of the workspace at beadsDir.
func Open(ctx context.Context, beadsDir string) (backend.DoltStorage, error) {
	return pebble.Open(ctx, filepath.Join(beadsDir, DataDirName))
}

// OpenInMemory opens a store that lives in process memory only.
func OpenInMemory(ctx context.Context) (backend.DoltStorage, error) {
	return pebble.OpenInMemory(ctx)
}
