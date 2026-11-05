package pebble

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage"
)

// unsupportedAllowlist is every method the generated shell answers with
// ErrUnsupported, read from unsupported_gen.go itself so the declared set
// can never drift from the stubs that implement it.
func unsupportedAllowlist(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile("unsupported_gen.go")
	if err != nil {
		t.Fatalf("read unsupported_gen.go: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "unsupported_gen.go", src, 0)
	if err != nil {
		t.Fatalf("parse unsupported_gen.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if ident, ok := fn.Recv.List[0].Type.(*ast.Ident); ok && ident.Name == "unsupportedDoltStorage" {
			out[fn.Name.Name] = "not implemented by the Phase 0 spike"
		}
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
