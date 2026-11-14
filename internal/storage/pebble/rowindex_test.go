package pebble

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// seedRowGraph is seedReadyGraph plus what the row-order indexes decide on:
// updated_at spread over the last 90 days, runs of rows sharing one
// created_at (the descending walk's tie order), templates, epics with
// closed children, and a round of writes through the public surface so every
// index entry has moved at least once.
func seedRowGraph(t *testing.T, s *Store, rows int, seed int64) {
	t.Helper()
	ctx := context.Background()
	seedReadyGraph(t, s, rows, seed)
	rng := rand.New(rand.NewSource(seed + 1))
	now := time.Now().UTC()
	var batch []*types.Issue
	for i := 0; i < rows/2; i++ {
		id := fmt.Sprintf("rdy-%05d", rows+i)
		created := now.Add(-time.Duration(rng.Intn(100)) * time.Hour)
		if i%10 < 4 {
			// Four rows in a row share a timestamp.
			created = now.Add(-time.Duration(i/10) * 24 * time.Hour)
		}
		issue := &types.Issue{
			ID: id, Title: fmt.Sprintf("row %d %s", i, []string{"alpha", "beta", "gamma"}[i%3]),
			Status:    []types.Status{types.StatusOpen, types.StatusInProgress, types.StatusClosed, types.StatusDeferred}[rng.Intn(4)],
			IssueType: []types.IssueType{types.TypeTask, types.TypeEpic, types.TypeGate, types.TypeBug}[rng.Intn(4)],
			Priority:  rng.Intn(5),
			CreatedAt: created,
			UpdatedAt: now.Add(-time.Duration(rng.Intn(90*24)) * time.Hour),
		}
		if issue.UpdatedAt.Before(issue.CreatedAt) {
			issue.CreatedAt = issue.UpdatedAt
		}
		if rng.Intn(9) == 0 {
			issue.IsTemplate = true
		}
		if rng.Intn(12) == 0 {
			issue.Ephemeral = true
		}
		if rng.Intn(3) == 0 {
			issue.Labels = []string{"alpha"}
		}
		if rng.Intn(5) == 0 {
			issue.Labels = append(issue.Labels, "beta")
		}
		batch = append(batch, issue)
	}
	if err := s.CreateIssues(ctx, batch, "seed"); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	// Epics with children: some all closed, some not.
	for i := 0; i < 6; i++ {
		epic := fmt.Sprintf("rdy-epic-%d", i)
		if err := s.CreateIssue(ctx, &types.Issue{ID: epic, Title: "epic " + epic, Status: types.StatusOpen, IssueType: types.TypeEpic, Priority: 1}, "seed"); err != nil {
			t.Fatalf("seed epic: %v", err)
		}
		for c := 0; c < 3; c++ {
			child := fmt.Sprintf("%s-c%d", epic, c)
			if err := s.CreateIssue(ctx, &types.Issue{ID: child, Title: "child", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}, "seed"); err != nil {
				t.Fatalf("seed child: %v", err)
			}
			if err := s.AddDependency(ctx, &types.Dependency{IssueID: child, DependsOnID: epic, Type: types.DepParentChild}, "seed"); err != nil {
				t.Fatalf("seed child edge: %v", err)
			}
			if c < i%4 {
				if err := s.CloseIssue(ctx, child, "done", "seed", ""); err != nil {
					t.Fatalf("close child: %v", err)
				}
			}
		}
	}
	// Writes through the public surface: every kind of move an index entry
	// can make.
	for i := 0; i < rows; i += 7 {
		id := fmt.Sprintf("rdy-%05d", i)
		var err error
		switch i % 5 {
		case 0:
			err = s.CloseIssue(ctx, id, "swept", "seed", "")
		case 1:
			err = s.UpdateIssue(ctx, id, map[string]interface{}{"priority": (i + 2) % 5, "title": "moved " + id}, "seed")
		case 2:
			err = s.UpdateIssue(ctx, id, map[string]interface{}{"status": string(types.StatusInProgress)}, "seed")
		case 3:
			err = s.AddLabel(ctx, id, "alpha", "seed")
		case 4:
			err = s.DeleteIssue(ctx, id)
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrValidation) {
			t.Logf("seed write %d on %s: %v", i%5, id, err)
		}
	}
	if err := s.UpdateIssueType(ctx, "rdy-00001", string(types.TypeEpic), "seed"); err != nil {
		t.Logf("retype: %v", err)
	}
}

func searchFilters() []struct {
	name     string
	query    string
	filter   types.IssueFilter
	strategy searchStrategy
} {
	open := types.StatusOpen
	epic := types.TypeEpic
	p2 := 2
	no := false
	after := time.Now().UTC().Add(-50 * time.Hour)
	return []struct {
		name     string
		query    string
		filter   types.IssueFilter
		strategy searchStrategy
	}{
		{"open, default sort, limit 50", "", types.IssueFilter{Status: &open, Limit: 50}, searchByOrder},
		{"open, default sort desc, limit 50", "", types.IssueFilter{Status: &open, Limit: 50, SortDesc: true}, searchByOrder},
		{"open, created, limit 50", "", types.IssueFilter{Status: &open, SortBy: "created", Limit: 50}, searchByOrder},
		{"open, created asc, limit 50", "", types.IssueFilter{Status: &open, SortBy: "created", SortDesc: true, Limit: 50}, searchByOrder},
		{"all, created, limit 7", "", types.IssueFilter{SortBy: "created", Limit: 7}, searchByOrder},
		{"all, priority, limit 1", "", types.IssueFilter{SortBy: "priority", Limit: 1}, searchByOrder},
		{"gascity list shape", "", types.IssueFilter{Ephemeral: &no, SortBy: "created", Limit: 20, IncludeDependencies: true}, searchByOrder},
		{"skip wisps, limit 20", "", types.IssueFilter{SkipWisps: true, Limit: 20}, searchByOrder},
		{"epics, limit 5", "", types.IssueFilter{IssueType: &epic, Limit: 5}, searchByOrder},
		{"priority 2, limit 10", "", types.IssueFilter{Priority: &p2, Limit: 10}, searchByOrder},
		{"label alpha, limit 10", "", types.IssueFilter{Labels: []string{"alpha"}, Limit: 10}, searchByLabel},
		{"labels alpha and beta, unbounded", "", types.IssueFilter{Labels: []string{"alpha", "beta"}}, searchByLabel},
		{"labels any beta or gamma, skip wisps", "", types.IssueFilter{LabelsAny: []string{"beta", "gamma"}, SkipWisps: true, SortBy: "created", Limit: 15}, searchByLabel},
		{"label with exclude", "", types.IssueFilter{Labels: []string{"alpha"}, ExcludeLabels: []string{"beta"}, Limit: 10}, searchByLabel},
		{"ids with skip wisps, unbounded", "", types.IssueFilter{IDs: []string{"rdy-00003", "rdy-00003", "rdy-00042", "missing"}, SkipWisps: true}, searchByIDs},
		{"text query, limit 10", "row 1", types.IssueFilter{Limit: 10}, searchByOrder},
		{"limit 25 under a cap of 10", "", types.IssueFilter{Limit: 25, MaxRows: 10}, searchByOrder},
		{"cap of 10, no limit", "", types.IssueFilter{MaxRows: 10}, searchByOrder},
		{"cap above the matches", "", types.IssueFilter{Labels: []string{"beta"}, Status: &open, MaxRows: 100000}, searchByLabel},
		{"unbounded", "", types.IssueFilter{Status: &open}, searchByScan},
		{"updated sort, limit 10", "", types.IssueFilter{SortBy: "updated", Limit: 10}, searchByScan},
		{"title sort desc, limit 10", "", types.IssueFilter{SortBy: "title", SortDesc: true, Limit: 10}, searchByScan},
		{"explicit ids", "", types.IssueFilter{IDs: []string{"rdy-00003", "rdy-00005", "rdy-00008"}, Limit: 10}, searchByIDs},
		{"keyset page", "", types.IssueFilter{SortBy: "created", AfterCreatedAt: &after, AfterID: "rdy-00000", Limit: 10}, searchByScan},
	}
}

// referenceSearch is the scan form of a search page: every match, sorted by
// the shared comparator, bounded. It is what the indexed walk must equal.
func referenceSearch(r kv, query string, filter types.IssueFilter) ([]string, error) {
	recs, err := searchRecords(r, query, filter)
	if err != nil {
		return nil, err
	}
	sortRecords(recs, filter.SortBy, filter.SortDesc)
	recs, err = boundSearch(recs, filter)
	if err != nil {
		return nil, err
	}
	return recordIDs(recs), nil
}

func recordIDs(recs []*issueRecord) []string {
	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.Issue.ID)
	}
	return ids
}

func issueIDs(issues []*types.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, i := range issues {
		ids = append(ids, i.ID)
	}
	return ids
}

// checkRowIndexes compares every indexed read with its scan definition over
// the store's current rows.
func checkRowIndexes(t *testing.T, s *Store) {
	t.Helper()
	// The search matrix runs with the ordered walk's budget as shipped and
	// then with a budget of one, which makes every ordered walk run out and
	// finish by the scan, so both halves of that path are held to the same
	// definition.
	for _, budget := range []int{orderWalkBudget, 1} {
		budget := budget
		t.Run(fmt.Sprintf("budget%d", budget), func(t *testing.T) {
			saved := orderWalkBudget
			orderWalkBudget = budget
			defer func() { orderWalkBudget = saved }()
			checkSearchMatrix(t, s)
		})
	}
	checkDerivedReads(t, s)
}

func checkSearchMatrix(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range searchFilters() {
		tc := tc
		t.Run("search/"+tc.name, func(t *testing.T) {
			if got := chooseSearchStrategy(tc.filter); got != tc.strategy {
				t.Fatalf("chooseSearchStrategy = %v, want %v", got, tc.strategy)
			}
			err := s.read(func(r kv) error {
				want, wantErr := referenceSearch(r, tc.query, tc.filter)
				got, gotErr := searchPage(r, tc.query, tc.filter)
				if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) {
					t.Fatalf("errors differ: indexed %v, scan %v", gotErr, wantErr)
				}
				if wantErr != nil {
					var tooMany *storageissueops.ErrTooManyRows
					if !errors.As(gotErr, &tooMany) {
						t.Fatalf("error %v is not ErrTooManyRows", gotErr)
					}
					return nil
				}
				if strings.Join(recordIDs(got), " ") != strings.Join(want, " ") {
					t.Errorf("page differs:\n indexed %v\n scan    %v", recordIDs(got), want)
				}
				if len(want) == 0 {
					t.Logf("filter %s matched nothing", tc.name)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// The public surface delivers the same page, hydrated.
			if tc.filter.MaxRows == 0 || tc.filter.MaxRows > 1000 {
				issues, err := s.SearchIssues(ctx, tc.query, tc.filter)
				if err != nil {
					t.Fatal(err)
				}
				ids, err := s.SearchIssueIDs(ctx, tc.query, tc.filter)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Join(issueIDs(issues), " ") != strings.Join(ids, " ") {
					t.Errorf("SearchIssues and SearchIssueIDs disagree:\n %v\n %v", issueIDs(issues), ids)
				}
			}
		})
	}
}

func checkDerivedReads(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	t.Run("statistics", func(t *testing.T) {
		for _, withBlocked := range []bool{true, false} {
			err := s.read(func(r kv) error {
				want, err := statisticsByScan(r, withBlocked)
				if err != nil {
					return err
				}
				got, err := statistics(r, withBlocked)
				if err != nil {
					return err
				}
				if statsString(got) != statsString(want) {
					t.Errorf("withBlocked=%v:\n counters %s\n scan     %s", withBlocked, statsString(got), statsString(want))
				}
				if want.TotalIssues == 0 {
					t.Error("the scan counted no rows")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	})
	staleFilters := []types.StaleFilter{
		{Days: 30}, {Days: 3}, {Days: 60, Limit: 5}, {Days: 1, Status: "closed"}, {Days: 1, Status: "deferred", Limit: 3},
		{Days: 10, Labels: []string{"alpha"}}, {Days: 10, LabelsAny: []string{"alpha", "beta"}, ExcludeLabels: []string{"gamma"}},
		{Days: 0}, {Days: 400},
	}
	for n, f := range staleFilters {
		f := f
		t.Run(fmt.Sprintf("stale/%d", n), func(t *testing.T) {
			err := s.read(func(r kv) error {
				want, err := staleIssuesByScan(r, f)
				if err != nil {
					return err
				}
				got, err := staleIssues(r, f)
				if err != nil {
					return err
				}
				if strings.Join(issueIDs(got), " ") != strings.Join(issueIDs(want), " ") {
					t.Errorf("stale %+v:\n indexed %v\n scan    %v", f, issueIDs(got), issueIDs(want))
				}
				if len(want) == 0 && f.Days < 100 {
					t.Logf("stale filter %+v matched nothing", f)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("epics", func(t *testing.T) {
		err := s.read(func(r kv) error {
			want, err := openEpicsByScan(r)
			if err != nil {
				return err
			}
			got, err := openEpics(r)
			if err != nil {
				return err
			}
			if strings.Join(recordIDs(got), " ") != strings.Join(recordIDs(want), " ") {
				t.Errorf("open epics:\n indexed %v\n scan    %v", recordIDs(got), recordIDs(want))
			}
			if len(want) == 0 {
				t.Error("the scan found no open epics")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		eligible, err := s.GetEpicsEligibleForClosure(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var closable int
		for _, e := range eligible {
			if e.EligibleForClose {
				closable++
			}
		}
		if closable == 0 {
			t.Error("no epic is eligible for closure; the seed meant some to be")
		}
	})
}

// statsString renders a Statistics with its pointer fields by value, so two
// can be compared as text.
func statsString(st *types.Statistics) string {
	flat := *st
	flat.BlockedIssues, flat.ReadyIssues = nil, nil
	return fmt.Sprintf("%+v blocked=%v ready=%v", flat, intPtr(st.BlockedIssues), intPtr(st.ReadyIssues))
}

func intPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestRowIndexesMatchScan checks the indexed list page, the statistics
// counters, the indexed stale read and the type index against their scan
// definitions, after a seed that moves every kind of entry, and again after
// a second round of writes.
func TestRowIndexesMatchScan(t *testing.T) {
	ctx := context.Background()
	s, err := OpenInMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetConfig(ctx, "issue_prefix", "rdy"); err != nil {
		t.Fatal(err)
	}
	seedRowGraph(t, s, 400, 11)
	checkRowIndexes(t, s)
	// A second round: reopen rows, move priorities, delete, and sweep a
	// closed set, then the same checks.
	for i := 2; i < 400; i += 11 {
		id := fmt.Sprintf("rdy-%05d", i)
		switch i % 3 {
		case 0:
			_ = s.ReopenIssue(ctx, id, "again", "seed")
		case 1:
			_ = s.UpdateIssue(ctx, id, map[string]interface{}{"priority": 0, "pinned": true}, "seed")
		case 2:
			_ = s.DeleteIssue(ctx, id)
		}
	}
	if _, err := s.DeleteIssues(ctx, []string{"rdy-00010", "rdy-00020", "rdy-epic-1"}, true, true, false); err != nil {
		t.Logf("delete issues: %v", err)
	}
	checkRowIndexes(t, s)
}

// TestCounterIDMode checks counter mode on the pebble store against the SQL
// rule: sequential ids per prefix for durable rows, seeded past the highest
// numeric suffix already present, skipping occupied values; wisps and
// sub-prefixed rows keep the hash.
func TestCounterIDMode(t *testing.T) {
	ctx := context.Background()
	s, err := OpenInMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for k, v := range map[string]string{"issue_prefix": "ctr", "issue_id_mode": "counter"} {
		if err := s.SetConfig(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	// A pinned row seeds the counter past itself.
	if err := s.CreateIssue(ctx, &types.Issue{ID: "ctr-7", Title: "pinned", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}, "t"); err != nil {
		t.Fatal(err)
	}
	mint := func(issue *types.Issue) string {
		t.Helper()
		if err := s.CreateIssue(ctx, issue, "t"); err != nil {
			t.Fatal(err)
		}
		return issue.ID
	}
	if got := mint(&types.Issue{Title: "a", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}); got != "ctr-8" {
		t.Fatalf("first counter id = %s, want ctr-8", got)
	}
	// An occupied value is passed over.
	if err := s.CreateIssue(ctx, &types.Issue{ID: "ctr-9", Title: "pinned too", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}, "t"); err != nil {
		t.Fatal(err)
	}
	if got := mint(&types.Issue{Title: "b", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}); got != "ctr-10" {
		t.Fatalf("second counter id = %s, want ctr-10", got)
	}
	// A batch claims its ids: two minted in one create are distinct and
	// sequential.
	batch := []*types.Issue{
		{Title: "c", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2},
		{Title: "d", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2},
	}
	if err := s.CreateIssues(ctx, batch, "t"); err != nil {
		t.Fatal(err)
	}
	if batch[0].ID != "ctr-11" || batch[1].ID != "ctr-12" {
		t.Fatalf("batch ids = %s, %s; want ctr-11, ctr-12", batch[0].ID, batch[1].ID)
	}
	// A wisp keeps the hash under the -wisp prefix; a sub-prefix keeps the
	// hash under its own prefix.
	wisp := mint(&types.Issue{Title: "w", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2, Ephemeral: true})
	if !strings.HasPrefix(wisp, "ctr-wisp-") || strings.Count(wisp, "-") != 2 {
		t.Fatalf("wisp id = %s, want ctr-wisp-<hash>", wisp)
	}
	sub := mint(&types.Issue{Title: "s", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2, IDPrefix: "sub"})
	if !strings.HasPrefix(sub, "ctr-sub-") {
		t.Fatalf("sub-prefixed id = %s, want ctr-sub-<hash>", sub)
	}
	// Off: back to hashes.
	if err := s.SetConfig(ctx, "issue_id_mode", "hash"); err != nil {
		t.Fatal(err)
	}
	hashed := mint(&types.Issue{Title: "h", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2})
	if strings.HasPrefix(hashed, "ctr-1") && len(hashed) <= len("ctr-13") {
		t.Fatalf("hash mode minted %s", hashed)
	}
}

// TestAdaptiveIDLength checks that the hash length follows the row count
// the way the SQL backends' does: three characters on an empty workspace,
// longer as the top-level rows under the prefix grow, and never below the
// configured minimum.
func TestAdaptiveIDLength(t *testing.T) {
	ctx := context.Background()
	s, err := OpenInMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetConfig(ctx, "issue_prefix", "ad"); err != nil {
		t.Fatal(err)
	}
	hashLen := func(id string) int { return len(strings.TrimPrefix(id, "ad-")) }
	first := &types.Issue{Title: "first", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}
	if err := s.CreateIssue(ctx, first, "t"); err != nil {
		t.Fatal(err)
	}
	if hashLen(first.ID) != 3 {
		t.Fatalf("empty workspace minted %s, want a 3-character hash", first.ID)
	}
	var batch []*types.Issue
	for i := 0; i < 300; i++ {
		batch = append(batch, &types.Issue{Title: fmt.Sprintf("row %d", i), Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2})
	}
	if err := s.CreateIssues(ctx, batch, "t"); err != nil {
		t.Fatal(err)
	}
	// Children do not count toward the length.
	if err := s.CreateIssue(ctx, &types.Issue{ID: first.ID + ".1", Title: "child", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}, "t"); err != nil {
		t.Fatal(err)
	}
	next := &types.Issue{Title: "next", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}
	if err := s.CreateIssue(ctx, next, "t"); err != nil {
		t.Fatal(err)
	}
	want := storageissueops.ComputeAdaptiveLength(302, storageissueops.DefaultAdaptiveConfig())
	if hashLen(next.ID) != want {
		t.Fatalf("at 302 rows minted %s (%d chars), want %d", next.ID, hashLen(next.ID), want)
	}
	if err := s.SetConfig(ctx, "min_hash_length", "7"); err != nil {
		t.Fatal(err)
	}
	long := &types.Issue{Title: "long", Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}
	if err := s.CreateIssue(ctx, long, "t"); err != nil {
		t.Fatal(err)
	}
	if hashLen(long.ID) != 7 {
		t.Fatalf("min_hash_length=7 minted %s", long.ID)
	}
}
