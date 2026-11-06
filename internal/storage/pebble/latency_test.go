package pebble

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	// Gas City runs with the journal on, and the Dolt numbers the gate is
	// measured against were taken that way (journal_test tag in the
	// assessment); BEADS_PEBBLE_LATENCY_JOURNAL=0 measures the bare write.
	journal := os.Getenv("BEADS_PEBBLE_LATENCY_JOURNAL") != "0"
	s.SetEventsJournalEnabled(journal)
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
		issue := &types.Issue{ID: fmt.Sprintf("lat-%06d", i), Title: fmt.Sprintf("row %d", i), Status: types.StatusOpen, IssueType: types.TypeTask, Priority: i % 5,
			Labels: []string{fmt.Sprintf("lane-%d", i%10)}}
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
	// The reads that still scan the issues namespace (search, stats, stale,
	// epics), measured the same way so the cost of a full scan at this size
	// is on record beside the indexed ready page.
	open := types.StatusOpen
	scanReads := []struct {
		name string
		run  func() error
		d    []time.Duration
	}{
		{"list open, limit 50, with deps (bd list / Reader.List)", func() error {
			_, err := s.SearchIssues(ctx, "", types.IssueFilter{Status: &open, Limit: 50, IncludeDependencies: true})
			return err
		}, nil},
		{"list ids, status open (hydration-free)", func() error {
			_, err := s.SearchIssueIDs(ctx, "", types.IssueFilter{Status: &open})
			return err
		}, nil},
		{"list by label, limit 50", func() error {
			_, err := s.SearchIssues(ctx, "", types.IssueFilter{Labels: []string{"lane-3"}, Limit: 50})
			return err
		}, nil},
		{"text query 'row 123', limit 50", func() error {
			_, err := s.SearchIssues(ctx, "row 123", types.IssueFilter{Limit: 50})
			return err
		}, nil},
		{"statistics", func() error { _, err := s.GetStatistics(ctx); return err }, nil},
		{"stale (30 days)", func() error {
			_, err := s.GetStaleIssues(ctx, types.StaleFilter{Days: 30})
			return err
		}, nil},
		{"epics eligible for closure", func() error { _, err := s.GetEpicsEligibleForClosure(ctx); return err }, nil},
	}
	for i := range scanReads {
		for n := 0; n < 10; n++ {
			start := time.Now()
			if err := scanReads[i].run(); err != nil {
				t.Fatalf("%s: %v", scanReads[i].name, err)
			}
			scanReads[i].d = append(scanReads[i].d, time.Since(start))
		}
	}
	t.Logf("backdrop: %d rows, %d edges; %d ops; in-memory=%v journal=%v", rows, rows/2, ops, os.Getenv("BEADS_PEBBLE_LATENCY_MEM") != "", journal)
	for _, m := range []struct {
		name string
		d    []time.Duration
	}{{"create (Lifecycle, no-history wisp, pinned id)", createD}, {"close (Lifecycle.Update, fenced)", closeD},
		{"edge add (related)", edgeAddD}, {"edge remove", edgeRmD}, {"ready (limit 50)", readyD},
		{"ready with counts and total (limit 50)", readyCountsD}} {
		t.Logf("%-56s %s", m.name, summarize(m.d))
	}
	for _, m := range scanReads {
		t.Logf("%-56s %s", m.name, summarize(m.d))
	}
	// Logical size by key namespace: what the rows cost against what the
	// derived indexes cost, as key+value bytes before Pebble's compression.
	sizes := map[string][2]int64{}
	var order []string
	err = s.read(func(r kv) error {
		return r.scan(nil, func(k, v []byte) (bool, error) {
			ns := string(k[:1])
			if _, seen := sizes[ns]; !seen {
				order = append(order, ns)
			}
			e := sizes[ns]
			e[0]++
			e[1] += int64(len(k) + len(v))
			sizes[ns] = e
			return true, nil
		})
	})
	if err != nil {
		t.Fatalf("size scan: %v", err)
	}
	sort.Strings(order)
	var total int64
	for _, ns := range order {
		total += sizes[ns][1]
	}
	for _, ns := range order {
		e := sizes[ns]
		t.Logf("namespace %-3s keys=%-7d bytes=%-10d (%.1f%%)", ns, e[0], e[1], 100*float64(e[1])/float64(total))
	}
	if dir := s.Dir(); dir != "" {
		var onDisk int64
		_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					onDisk += info.Size()
				}
			}
			return nil
		})
		t.Logf("logical bytes=%d on-disk bytes=%d", total, onDisk)
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
