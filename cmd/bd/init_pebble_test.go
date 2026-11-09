//go:build pebblegozstd

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/backends"
	"github.com/steveyegge/beads/internal/storage/pebble"
)

// TestInitProvisionsPebbleWorkspace runs bd init on the pebble backend and
// checks what a later command opens: metadata.json naming the backend, the
// store directory and the ignore files that keep it out of git, the issue
// prefix and the identity the store holds.
func TestInitProvisionsPebbleWorkspace(t *testing.T) {
	if _, ok := backends.Lookup(pebble.BackendName); !ok {
		t.Fatalf("backend %q is not registered in bd; the tagged import in backend_pebble.go is missing", pebble.BackendName)
	}
	tmpDir := registeredInitWorkdir(t)

	if err := runRegisteredInit("--backend", pebble.BackendName, "--prefix", "pbl", "--quiet"); err != nil {
		t.Fatalf("bd init --backend=pebble: %v", err)
	}

	beadsDir := filepath.Join(tmpDir, ".beads")
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		t.Fatalf("load metadata.json: cfg=%v err=%v", cfg, err)
	}
	if cfg.GetBackend() != pebble.BackendName {
		t.Fatalf("metadata.json backend = %q, want %q", cfg.GetBackend(), pebble.BackendName)
	}
	if cfg.ProjectID == "" {
		t.Fatal("metadata.json has no project_id")
	}
	if info, err := os.Stat(filepath.Join(beadsDir, pebble.DataDirName)); err != nil || !info.IsDir() {
		t.Fatalf("store directory: %v", err)
	}
	// Nothing of the store may reach git: the standard .beads/.gitignore covers
	// the runtime files, and the store directory ignores itself.
	if _, err := os.Stat(filepath.Join(beadsDir, ".gitignore")); err != nil {
		t.Fatalf("the standard .beads/.gitignore: %v", err)
	}
	if ignore, err := os.ReadFile(filepath.Join(beadsDir, pebble.DataDirName, ".gitignore")); err != nil || !strings.Contains(string(ignore), "\n*\n") {
		t.Fatalf("the store directory's own .gitignore: %q, %v", ignore, err)
	}

	ctx := context.Background()
	opened, err := newRegisteredBackendStore(ctx, cfg.GetBackend(), beadsDir, false, true)
	if err != nil {
		t.Fatalf("open the workspace through the registry: %v", err)
	}
	defer opened.Close()
	if got, err := opened.GetConfig(ctx, "issue_prefix"); err != nil || got != "pbl" {
		t.Errorf("issue_prefix = %q, %v; want %q", got, err, "pbl")
	}
	if got, err := opened.GetMetadata(ctx, "_project_id"); err != nil || got != cfg.ProjectID {
		t.Errorf("_project_id = %q, %v; want %q", got, err, cfg.ProjectID)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The gate file every later command leaves beside .beads is covered too.
	if ignore, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore")); err != nil || !strings.Contains(string(ignore), "*.gate.lock*") {
		t.Fatalf("the project .gitignore: %q, %v", ignore, err)
	}

	if err := runRegisteredInit("--backend", pebble.BackendName, "--prefix", "pbl", "--quiet"); err == nil {
		t.Fatal("a second bd init on an initialized pebble workspace succeeded; want a refusal")
	}
	if err := runRegisteredInit("--backend", pebble.BackendName, "--prefix", "pbl", "--quiet", "--init-if-missing"); err != nil {
		t.Fatalf("bd init --init-if-missing on an initialized pebble workspace: %v", err)
	}
	// A flag the route cannot honor is refused on the real backend as on the
	// fixture: nothing about Pebble takes --server.
	if err := runRegisteredInit("--backend", pebble.BackendName, "--prefix", "pbl", "--quiet", "--server"); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("bd init --backend=pebble --server: err = %v, want a refusal naming the flag", err)
	}
}
