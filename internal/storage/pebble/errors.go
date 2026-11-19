package pebble

import "github.com/steveyegge/beads/internal/storage"

// errUnsupported is what the generated shell (unsupported_gen.go) returns
// for every method this spike does not implement.
func errUnsupported(op string) error {
	return &storage.ErrUnsupported{Op: op, Backend: BackendName}
}
