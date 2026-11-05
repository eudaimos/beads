package pebble

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/steveyegge/beads/internal/types"
)

// seedReadyGraph fills s with a graph that exercises every clause the index
// decides: both planes, every status, priorities 0-4, rows
// on both sides of the 48-hour hybrid cutoff, deferred rows and their
// children, pinned rows, blocked rows and stale flags, labels, assignees and
// excluded types.
func seedReadyGraph(t *testing.T, s *Store, rows int, seed int64) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	now := time.Now().UTC()
	statuses := []types.Status{types.StatusOpen, types.StatusOpen, types.StatusInProgress, types.StatusClosed, types.StatusPinned, types.StatusBlocked}
	kinds := []types.IssueType{types.TypeTask, types.TypeBug, types.TypeEpic, types.TypeMessage, types.TypeGate, types.TypeMolecule}
	labels := []string{"alpha", "beta", "gamma", "release-1", "release-2"}
	var batch []*types.Issue
	var edges []*types.Dependency
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("rdy-%05d", i)
		issue := &types.Issue{
			ID: id, Title: "row " + id,
			Status:    statuses[rng.Intn(len(statuses))],
			IssueType: kinds[rng.Intn(len(kinds))],
			Priority:  rng.Intn(5),
			CreatedAt: now.Add(-time.Duration(rng.Intn(100)) * time.Hour),
		}
		if rng.Intn(10) == 0 {
			issue.Ephemeral = true
		}
		if rng.Intn(8) == 0 {
			issue.Assignee = fmt.Sprintf("user-%d", rng.Intn(3))
		}
		if rng.Intn(12) == 0 {
			d := now.Add(time.Duration(rng.Intn(48)-24) * time.Hour)
			issue.DeferUntil = &d
		}
		for _, l := range labels {
			if rng.Intn(4) == 0 {
				issue.Labels = append(issue.Labels, l)
			}
		}
		if i > 0 && i <= 6 {
			// A parent with children for the ParentID filter.
			edges = append(edges, &types.Dependency{IssueID: id, DependsOnID: "rdy-00000", Type: types.DepParentChild})
		} else if i > 0 {
			switch rng.Intn(5) {
			case 0:
				edges = append(edges, &types.Dependency{IssueID: id, DependsOnID: fmt.Sprintf("rdy-%05d", rng.Intn(i)), Type: types.DepBlocks})
			case 1:
				edges = append(edges, &types.Dependency{IssueID: id, DependsOnID: fmt.Sprintf("rdy-%05d", rng.Intn(i)), Type: types.DepParentChild})
			}
		}
		batch = append(batch, issue)
	}
	if err := s.CreateIssues(ctx, batch, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Edges after the rows: a batch may not carry edges across the wisp
	// and durable buckets.
	for _, d := range edges {
		if err := s.AddDependency(ctx, d, "seed"); err != nil {
			t.Fatalf("seed edge %s -> %s: %v", d.IssueID, d.DependsOnID, err)
		}
	}
	// A few rows pinned by the flag rather than the status, and a few with
	// a stale blocked flag (set, but no ready-affecting edge of their own).
	for i := 0; i < rows; i += 17 {
		id := fmt.Sprintf("rdy-%05d", i)
		if err := s.UpdateIssue(ctx, id, map[string]interface{}{"pinned": true}, "seed"); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}
	if err := s.run(ctx, func(t *txn) error {
		for i := 3; i < rows; i += 23 {
			rec, err := getRecord(t.kv, fmt.Sprintf("rdy-%05d", i))
			if err != nil {
				return err
			}
			old := *rec
			rec.Issue.IsBlocked = true
			if err := putRecord(t.kv, &old, rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("stale flags: %v", err)
	}
}

func readyFilters() []types.WorkFilter {
	p2 := 2
	user := "user-1"
	parent := "rdy-00000"
	var out []types.WorkFilter
	for _, policy := range []types.SortPolicy{"", types.SortPolicyHybrid, types.SortPolicyOldest, types.SortPolicyPriority, types.SortPolicy("other")} {
		out = append(out,
			types.WorkFilter{SortPolicy: policy},
			types.WorkFilter{SortPolicy: policy, Limit: 7},
			types.WorkFilter{SortPolicy: policy, Limit: 25, IncludeEphemeral: true, IncludeDeferred: true},
			types.WorkFilter{SortPolicy: policy, Status: types.StatusOpen, Limit: 10},
			types.WorkFilter{SortPolicy: policy, Status: types.StatusInProgress},
			types.WorkFilter{SortPolicy: policy, Statuses: []types.Status{types.StatusInProgress, types.StatusOpen}, Limit: 5},
			types.WorkFilter{SortPolicy: policy, Priority: &p2, Limit: 10},
			types.WorkFilter{SortPolicy: policy, Type: "epic"},
			types.WorkFilter{SortPolicy: policy, Assignee: &user},
			types.WorkFilter{SortPolicy: policy, Unassigned: true, Limit: 10},
			types.WorkFilter{SortPolicy: policy, Labels: []string{"alpha"}, Limit: 10},
			types.WorkFilter{SortPolicy: policy, LabelsAny: []string{"beta", "gamma"}, ExcludeLabels: []string{"alpha"}},
			types.WorkFilter{SortPolicy: policy, LabelPattern: "release-*", Limit: 3},
			types.WorkFilter{SortPolicy: policy, ExcludeIDs: []string{"rdy-00002", "rdy-00004"}, Limit: 10},
			types.WorkFilter{SortPolicy: policy, ParentID: &parent},
			types.WorkFilter{SortPolicy: policy, Limit: 10, MaxRows: 4},
		)
	}
	return out
}

func pageIDs(p readyPage) []string {
	ids := make([]string, 0, len(p.recs))
	for _, rec := range p.recs {
		ids = append(ids, rec.Issue.ID)
	}
	return ids
}

// TestReadyIndexMatchesScan checks the indexed ready page against the
// full-scan evaluation of the same predicate, page and total, over every
// sort policy and the filter clauses the index screens on its own.
func TestReadyIndexMatchesScan(t *testing.T) {
	ctx := context.Background()
	s, err := OpenInMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetConfig(ctx, "issue_prefix", "rdy"); err != nil {
		t.Fatal(err)
	}
	seedReadyGraph(t, s, 400, 1)
	for n, filter := range readyFilters() {
		filter := filter
		t.Run(fmt.Sprintf("filter%02d", n), func(t *testing.T) {
			err := s.read(func(r kv) error {
				m, err := newReadyMatcher(r, filter)
				if err != nil {
					return err
				}
				if !m.indexable() {
					t.Fatalf("filter %+v took the full scan", filter)
				}
				limit := pageLimit(filter)
				want, err := readyScanFull(r, m, limit)
				if err != nil {
					return err
				}
				got, err := readyScanIndexed(r, m, limit, true)
				if err != nil {
					return err
				}
				if got.total != want.total {
					t.Errorf("total: indexed %d, scan %d", got.total, want.total)
				}
				if fmt.Sprint(pageIDs(got)) != fmt.Sprint(pageIDs(want)) {
					t.Errorf("page:\n indexed %v\n scan    %v", pageIDs(got), pageIDs(want))
				}
				count, err := readyScanIndexed(r, m, -1, true)
				if err != nil {
					return err
				}
				if count.total != want.total || len(count.recs) != 0 {
					t.Errorf("count-only: total %d rows %d, want total %d and no rows", count.total, len(count.recs), want.total)
				}
				if want.total == 0 {
					t.Logf("filter %+v matched nothing", filter)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestReadyIndexRebuilt drops the derived indexes and their version marker
// from a store on disk and checks that reopening rebuilds them.
func TestReadyIndexRebuilt(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "pebble")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfig(ctx, "issue_prefix", "rdy"); err != nil {
		t.Fatal(err)
	}
	seedReadyGraph(t, s, 120, 2)
	want, err := s.GetReadyWork(ctx, types.WorkFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	blockedWant, err := s.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := pebble.Open(dir, &pebble.Options{Logger: silentLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	raw := dbKV{db: db}
	for _, ns := range []string{nsReady, nsBlocked, nsDefer} {
		if err := deletePrefix(raw, []byte(ns)); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.del(internalKey("indexes")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetReadyWork(ctx, types.WorkFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(want) == 0 {
		t.Fatalf("ready after rebuild: %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("ready after rebuild: row %d is %s, want %s", i, got[i].ID, want[i].ID)
		}
	}
	blockedGot, err := s.GetBlockedIssues(ctx, types.WorkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(blockedGot) != len(blockedWant) {
		t.Fatalf("blocked after rebuild: %d rows, want %d", len(blockedGot), len(blockedWant))
	}
}
