package pebble

import (
	"context"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage"
)

// unsupportedAllowlist is every method the generated shell answers with
// ErrUnsupported: the method set of the generated type itself, so the
// declared set can never drift from the stubs that implement it.
func unsupportedAllowlist(t *testing.T) map[string]string {
	t.Helper()
	shell := reflect.TypeOf(unsupportedDoltStorage{})
	out := map[string]string{}
	for i := 0; i < shell.NumMethod(); i++ {
		out[shell.Method(i).Name] = "not implemented by the Phase 0 spike"
	}
	if len(out) == 0 {
		t.Fatal("no generated stubs found; regenerate unsupported_gen.go")
	}
	return out
}

// newTestStore returns a fresh in-memory store with the config bd init would
// leave behind.
func newTestStore(t *testing.T) storage.DoltStorage {
	t.Helper()
	s, err := OpenInMemory(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetConfig(context.Background(), "issue_prefix", "test"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	return s
}

func TestUnsupportedContract(t *testing.T) {
	conformance.RunUnsupportedContract(t, &Store{}, unsupportedAllowlist(t))
}

func TestConformance(t *testing.T) {
	conformance.RunAll(t, func(t *testing.T) storage.DoltStorage {
		return newTestStore(t)
	})
}
