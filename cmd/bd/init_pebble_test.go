package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/backends"
	"github.com/steveyegge/beads/internal/storage/pebble"
)

// TestInitProvisionsPebbleWorkspace runs bd init on the pebble backend and
// checks what a later command opens: metadata.json naming the backend, the
// store directory, the issue prefix and the identity the store holds.
func TestInitProvisionsPebbleWorkspace(t *testing.T) {
	if _, ok := backends.Lookup(pebble.BackendName); !ok {
		t.Fatalf("backend %q is not registered in bd; the blank import in main.go is missing", pebble.BackendName)
	}
	origDBPath := dbPath
	origStore := store
	t.Cleanup(func() {
		if store != nil && store != origStore {
			store.Close()
		}
		store = origStore
		dbPath = origDBPath
		_ = initCmd.Flags().Set("backend", "")
		_ = initCmd.Flags().Set("prefix", "")
		_ = initCmd.Flags().Set("quiet", "false")
		_ = initCmd.Flags().Set("init-if-missing", "false")
	})
	dbPath = ""
	store = nil
	isolateBeadsDirForTest(t)
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	rootCmd.SetArgs([]string{"init", "--backend", pebble.BackendName, "--prefix", "pbl", "--quiet"})
	if err := rootCmd.Execute(); err != nil {
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

	ctx := context.Background()
	opened, err := newRegisteredBackendStore(ctx, cfg.GetBackend(), beadsDir, false)
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

	rootCmd.SetArgs([]string{"init", "--backend", pebble.BackendName, "--prefix", "pbl", "--quiet"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("a second bd init on an initialized pebble workspace succeeded; want a refusal")
	}
	rootCmd.SetArgs([]string{"init", "--backend", pebble.BackendName, "--prefix", "pbl", "--quiet", "--init-if-missing"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("bd init --init-if-missing on an initialized pebble workspace: %v", err)
	}
}
