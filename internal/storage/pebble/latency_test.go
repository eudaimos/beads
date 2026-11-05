package pebble

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// TestOpWriteLatency measures the Phase 0 gate from the assessment: Gas
// City's op-journal shape (a no-history wisp created at a pinned id through
// the Lifecycle role, then closed through a fenced update) on an on-disk
// store with fsync, plus an edge add/remove and a ready read against a
// pre-populated graph. BEADS_PEBBLE_LATENCY_ROWS sizes the backdrop (default
// 2000; the assessment's target is 40000 rows / 20000 edges),
// BEADS_PEBBLE_LATENCY_OPS the number of measured ops (default 200), and
// BEADS_PEBBLE_LATENCY_MEM=1 swaps the on-disk store for the in-memory one
// to show the engine cost without the WAL fsync.
func TestOpWriteLatency(t *testing.T) {
	rows := envInt("BEADS_PEBBLE_LATENCY_ROWS", 2000)
	ops := envInt("BEADS_PEBBLE_LATENCY_OPS", 200)
	ctx := context.Background()
	var s *Store
	var err error
	if os.Getenv("BEADS_PEBBLE_LATENCY_MEM") != "" {
		// In memory: no WAL fsync, so the numbers are the engine's own cost.
		s, err = OpenInMemory(ctx)
	} else {
		s, err = Open(ctx, t.TempDir())
	}
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.SetConfig(ctx, "issue_prefix", "lat"); err != nil {
		t.Fatal(err)
	}
	// Backdrop: rows open issues, every other row blocking on an earlier one
	// (about rows/2 edges), created in batches.
	batch := make([]*types.Issue, 0, 500)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.CreateIssues(ctx, batch, "seed"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		batch = batch[:0]
	}
	for i := 0; i < rows; i++ {
		issue := &types.Issue{ID: fmt.Sprintf("lat-%06d", i), Title: fmt.Sprintf("row %d", i), Status: types.StatusOpen, IssueType: types.TypeTask, Priority: i % 5}
		if i%2 == 1 {
			issue.Dependencies = []*types.Dependency{{IssueID: issue.ID, DependsOnID: fmt.Sprintf("lat-%06d", i/2), Type: types.DepBlocks}}
		}
		batch = append(batch, issue)
		if len(batch) == cap(batch) {
			flush()
		}
	}
	flush()

	lc, err := s.IssueLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	var createD, closeD, edgeAddD, edgeRmD, readyD, readyCountsD []time.Duration
	for i := 0; i < ops; i++ {
		id := fmt.Sprintf("lat-op%05d", i)
		start := time.Now()
		res, err := lc.Create(ctx, publicops.CreateRequest{
			Actor: "gc", ForceIDPrefix: true,
			Issue: &types.Issue{ID: id, Title: "op " + id, IssueType: types.TypeTask, Status: types.StatusOpen, NoHistory: true, Priority: 2},
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		createD = append(createD, time.Since(start))

		start = time.Now()
		version := res.Issue.RowVersion
		if _, err := lc.Update(ctx, publicops.UpdateRequest{
			Actor: "gc", IssueID: id, ExpectedVersion: &version, ForceClosePolicy: true,
			Patch: publicops.IssuePatch{Status: publicops.Field[types.Status]{Set: true, Value: types.StatusClosed}},
		}); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
		closeD = append(closeD, time.Since(start))

		src, dst := fmt.Sprintf("lat-%06d", (i*7)%rows), fmt.Sprintf("lat-%06d", (i*7+3)%rows)
		if src == dst {
			continue
		}
		start = time.Now()
		err = s.AddDependency(ctx, &types.Dependency{IssueID: src, DependsOnID: dst, Type: types.DepRelated}, "gc")
		if err != nil {
			t.Fatalf("add edge: %v", err)
		}
		edgeAddD = append(edgeAddD, time.Since(start))
		start = time.Now()
		if err := s.RemoveDependency(ctx, src, dst, "gc"); err != nil {
			t.Fatalf("remove edge: %v", err)
		}
		edgeRmD = append(edgeRmD, time.Since(start))
	}
	for i := 0; i < 10; i++ {
		start := time.Now()
		if _, err := s.GetReadyWork(ctx, types.WorkFilter{Limit: 50}); err != nil {
			t.Fatalf("ready: %v", err)
		}
		readyD = append(readyD, time.Since(start))
		start = time.Now()
		if _, _, err := s.GetReadyWorkWithCountsAndTotal(ctx, types.WorkFilter{Limit: 50}); err != nil {
			t.Fatalf("ready with counts: %v", err)
		}
		readyCountsD = append(readyCountsD, time.Since(start))
	}
	t.Logf("backdrop: %d rows, %d edges; %d ops; in-memory=%v", rows, rows/2, ops, os.Getenv("BEADS_PEBBLE_LATENCY_MEM") != "")
	for _, m := range []struct {
		name string
		d    []time.Duration
	}{{"create (Lifecycle, no-history wisp, pinned id)", createD}, {"close (Lifecycle.Update, fenced)", closeD},
		{"edge add (related)", edgeAddD}, {"edge remove", edgeRmD}, {"ready (limit 50)", readyD},
		{"ready with counts and total (limit 50)", readyCountsD}} {
		t.Logf("%-48s %s", m.name, summarize(m.d))
	}
}

func summarize(d []time.Duration) string {
	if len(d) == 0 {
		return "n/a"
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, v := range sorted {
		total += v
	}
	pct := func(p float64) time.Duration { return sorted[int(float64(len(sorted)-1)*p)] }
	return fmt.Sprintf("n=%d mean=%s p50=%s p95=%s p99=%s max=%s", len(sorted), total/time.Duration(len(sorted)), pct(0.5), pct(0.95), pct(0.99), sorted[len(sorted)-1])
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
