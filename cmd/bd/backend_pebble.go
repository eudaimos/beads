//go:build pebblegozstd

package main

// The experimental Pebble backend is linked only in a build that carries the
// pebblegozstd tag. The tag is Pebble's own switch for its pure-Go zstd: its
// default zstd is a cgo library with the libzstd symbols Dolt's gozstd
// already bundles, so a bd that links both has to be built with it, and a
// default build leaves Pebble out rather than make every build carry the tag.
import _ "github.com/steveyegge/beads/internal/storage/pebble"
