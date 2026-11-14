package bdpebble_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/backend"
	bdpebble "github.com/steveyegge/beads/backend/pebble"
)

// The registry key and the directory are spelled literally, the way a
// workspace's metadata.json and its .beads directory carry them: a test that
// read them from the code under test would stay green through a rename that
// orphans every existing Pebble workspace.
const (
	backendName = "pebble"
	dataDirName = "pebble"
)

// TestImportRegistersTheBackend drives the reason this package exists: code
// holding nothing but the public surface gets "pebble" into the registry that
// store dispatch reads, with the Create hook bd init provisions through.
func TestImportRegistersTheBackend(t *testing.T) {
	if bdpebble.Name != backendName || bdpebble.DataDirName != dataDirName {
		t.Fatalf("Name = %q, DataDirName = %q; want %q and %q", bdpebble.Name, bdpebble.DataDirName, backendName, dataDirName)
	}
	registered, ok := backend.Lookup(backendName)
	if !ok {
		t.Fatalf("Lookup(%q) found nothing: importing the package must register the backend", backendName)
	}
	if registered.Open == nil || registered.OpenReadOnly == nil {
		t.Error("the registered pebble backend is missing an open")
	}
	if registered.Create == nil {
		t.Error("the registered pebble backend has no Create hook; bd init could not provision it")
	}
	if !registered.WorkspaceIsBeadsDir {
		t.Error("a pebble workspace is the .beads directory; there is no database file to discover")
	}
}

// TestOpenServesAWorkspaceTheRegistryReopens writes a bead through the public
// Open and reads it back through the registry's open, the path a later bd
// command or library open takes from metadata.json.
func TestOpenServesAWorkspaceTheRegistryReopens(t *testing.T) {
	ctx := context.Background()
	beadsDir := t.TempDir()

	store, err := bdpebble.Open(ctx, beadsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.SetConfig(ctx, "issue_prefix", "emb"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	issue := &backend.Issue{ID: "emb-1", Title: "from an embedder", Status: backend.StatusOpen, IssueType: backend.TypeTask, Priority: 2}
	if err := store.CreateIssue(ctx, issue, "embedder"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if info, err := os.Stat(filepath.Join(beadsDir, dataDirName)); err != nil || !info.IsDir() {
		t.Fatalf("the store directory under the workspace: %v", err)
	}

	registered, _ := backend.Lookup(backendName)
	reopened, err := registered.Open(ctx, beadsDir)
	if err != nil {
		t.Fatalf("reopen through the registry: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.GetIssue(ctx, "emb-1")
	if err != nil || got == nil || got.Title != "from an embedder" {
		t.Fatalf("read back through the registry: issue=%v err=%v", got, err)
	}
}

// TestOpenInMemoryNeedsNoWorkspace is the embedder's test fixture: a store
// with no directory behind it.
func TestOpenInMemoryNeedsNoWorkspace(t *testing.T) {
	ctx := context.Background()
	store, err := bdpebble.OpenInMemory(ctx)
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.SetConfig(ctx, "issue_prefix", "mem"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	issue := &backend.Issue{ID: "mem-1", Title: "in memory", Status: backend.StatusOpen, IssueType: backend.TypeTask, Priority: 2}
	if err := store.CreateIssue(ctx, issue, "embedder"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := store.GetIssue(ctx, "mem-1"); err != nil || got == nil {
		t.Fatalf("read back: issue=%v err=%v", got, err)
	}
}
