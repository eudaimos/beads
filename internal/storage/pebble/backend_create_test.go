package pebble

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestCreateBackendKeepsTheStoreOutOfGit holds the Create hook to what the
// registry asks of it: the store directory it provisions under .beads ignores
// itself, since the standard .beads/.gitignore names only the built-in
// engines' directories. It then proves Pebble lives with the file it does not
// name: a row is written, flushed to a table and compacted with the ignore
// file in the directory, and reads back after a reopen with the file intact.
func TestCreateBackendKeepsTheStoreOutOfGit(t *testing.T) {
	ctx := context.Background()
	beadsDir := t.TempDir()
	if err := createBackend(ctx, beadsDir); err != nil {
		t.Fatalf("createBackend: %v", err)
	}
	ignore := filepath.Join(beadsDir, DataDirName, ".gitignore")
	readIgnore := func() string {
		t.Helper()
		got, err := os.ReadFile(ignore)
		if err != nil {
			t.Fatalf("read the store's .gitignore: %v", err)
		}
		return string(got)
	}
	if got := readIgnore(); !strings.Contains(got, "\n*\n") {
		t.Fatalf("the store's .gitignore = %q, want a line ignoring everything", got)
	}

	opened, err := openBackend(ctx, beadsDir)
	if err != nil {
		t.Fatalf("open the created store: %v", err)
	}
	s := opened.(*Store)
	if err := s.SetConfig(ctx, "issue_prefix", "ign"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	issue := &types.Issue{ID: "ign-1", Title: "kept", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}
	if err := s.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := s.db.Compact(ctx, []byte{0x00}, []byte{0xff}, false); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := openBackend(ctx, beadsDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.GetIssue(ctx, "ign-1")
	if err != nil || got == nil || got.Title != "kept" {
		t.Fatalf("read back after reopen: issue=%v err=%v", got, err)
	}
	if got := readIgnore(); !strings.Contains(got, "\n*\n") {
		t.Fatalf("the store's .gitignore after flush, compaction and reopen = %q", got)
	}
}

// TestCreateBackendAcceptsADirectoryItAlreadyRanOn holds the Create hook to
// the retry clause of its contract: bd init may call it again on a .beads
// directory where an earlier init failed after Create returned. The second
// call succeeds, and it does not empty a store that already holds data.
func TestCreateBackendAcceptsADirectoryItAlreadyRanOn(t *testing.T) {
	ctx := context.Background()
	beadsDir := t.TempDir()
	if err := createBackend(ctx, beadsDir); err != nil {
		t.Fatalf("first createBackend: %v", err)
	}
	opened, err := openBackend(ctx, beadsDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := opened.SetConfig(ctx, "issue_prefix", "twice"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	issue := &types.Issue{ID: "twice-1", Title: "survives", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}
	if err := opened.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := createBackend(ctx, beadsDir); err != nil {
		t.Fatalf("second createBackend on the same directory: %v", err)
	}
	reopened, err := openBackend(ctx, beadsDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got, err := reopened.GetIssue(ctx, "twice-1"); err != nil || got == nil || got.Title != "survives" {
		t.Fatalf("the row written between the two calls: issue=%v err=%v", got, err)
	}
}
