package pebble

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// Ready work is the predicate sqlbuild.BuildReadyWorkWhere renders, with the
// id sets issueops.buildReadyWorkPredicates resolves first (children of
// future-deferred parents, the transitive descendants of ParentID), ordered
// by issueops.sortReadyIssues and bounded like GetReadyWorkInTx. Both planes
// are one scan here, so the wisp merge of the SQL path is the scan itself:
// ephemeral rows are hidden unless IncludeEphemeral, and no-history
// wisps-plane rows (ephemeral=0) are ready like any durable row.

// readyMatcher evaluates one WorkFilter against rows.
type readyMatcher struct {
	r                kv
	filter           types.WorkFilter
	now              time.Time
	activeCustom     map[string]bool
	excludeTypes     map[types.IssueType]bool
	excludeIDs       map[string]bool
	deferredChildren map[string]bool
	descendants      map[string]bool
	labels           []string
	labelsAny        []string
	labelGlob        *regexp.Regexp
	labelRegex       *regexp.Regexp
	metaKeys         []string
}

func newReadyMatcher(r kv, filter types.WorkFilter) (*readyMatcher, error) {
	m := &readyMatcher{
		r:         r,
		filter:    filter,
		now:       nowUTC(),
		labels:    sqlbuild.CompactNonEmptyStrings(filter.Labels),
		labelsAny: sqlbuild.CompactNonEmptyStrings(filter.LabelsAny),
	}
	if filter.Status == types.StatusOpen {
		// The singular open pin means the ready-eligible set: built-in open
		// plus category-active custom statuses (sqlbuild.readyStatusClause).
		customs, err := customStatuses(r)
		if err != nil {
			return nil, err
		}
		m.activeCustom = make(map[string]bool, len(customs))
		for _, c := range customs {
			if c.Category == types.CategoryActive {
				m.activeCustom[c.Name] = true
			}
		}
	}
	if filter.Type == "" {
		m.excludeTypes = make(map[types.IssueType]bool)
		for _, t := range sqlbuild.ReadyWorkExcludeTypes(filter.ExcludeTypes) {
			m.excludeTypes[t] = true
		}
	}
	if len(filter.ExcludeIDs) > 0 {
		m.excludeIDs = make(map[string]bool, len(filter.ExcludeIDs))
		for _, id := range filter.ExcludeIDs {
			m.excludeIDs[id] = true
		}
	}
	if !filter.IncludeDeferred {
		set, err := childrenOfDeferredParents(r, m.now)
		if err != nil {
			return nil, fmt.Errorf("compute deferred parent children: %w", err)
		}
		m.deferredChildren = set
	}
	if filter.ParentID != nil {
		ids, err := descendantIDs(r, *filter.ParentID)
		if err != nil {
			return nil, fmt.Errorf("get parent descendants: %w", err)
		}
		m.descendants = make(map[string]bool, len(ids))
		for _, id := range ids {
			m.descendants[id] = true
		}
	}
	if filter.LabelPattern != "" {
		m.labelGlob = globRegexp(filter.LabelPattern)
	}
	if filter.LabelRegex != "" {
		re, err := regexp.Compile(filter.LabelRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid label regex %q: %w", filter.LabelRegex, err)
		}
		m.labelRegex = re
	}
	if filter.HasMetadataKey != "" {
		if err := storage.ValidateMetadataKey(filter.HasMetadataKey); err != nil {
			return nil, err
		}
	}
	for k := range filter.MetadataFields {
		if err := storage.ValidateMetadataKey(k); err != nil {
			return nil, err
		}
		m.metaKeys = append(m.metaKeys, k)
	}
	return m, nil
}

// childrenOfDeferredParents is issueops.getChildrenOfDeferredParentsInTx:
// the parent-child children of every row whose defer_until is still ahead.
func childrenOfDeferredParents(r kv, now time.Time) (map[string]bool, error) {
	var parents []string
	err := scanRecords(r, func(rec *issueRecord) (bool, error) {
		if rec.Issue.DeferUntil != nil && rec.Issue.DeferUntil.After(now) {
			parents = append(parents, rec.Issue.ID)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		return nil, nil
	}
	out := make(map[string]bool)
	for _, p := range parents {
		children, err := childrenOf(r, p)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			out[c] = true
		}
	}
	return out, nil
}

// descendantIDs is issueops.GetDescendantIDsInTx: every row reachable from
// root by following parent-child edges downward, in breadth-first order.
func descendantIDs(r kv, root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	seen := map[string]bool{root: true}
	queue := []string{root}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		children, err := childrenOf(r, cur)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out, nil
}

// hasOwnReadyAffectingEdge is sqlbuild.OwnReadyAffectingEdgeSQL: whether the
// row carries an edge of a type the blocked recompute can mark it for. A set
// flag on a row without one is stale and does not hide the row (GH#3268).
func hasOwnReadyAffectingEdge(r kv, id string) (bool, error) {
	found := false
	err := r.scan(depPrefix(id), func(k, _ []byte) (bool, error) {
		parts := splitKey(nsDep, k)
		if len(parts) == 3 && types.DependencyType(parts[2]).AffectsReadyWork() {
			found = true
			return false, nil
		}
		return true, nil
	})
	return found, err
}

// match evaluates the ready predicate for one row, clause by clause in the
// order sqlbuild.BuildReadyWorkWhere renders them.
func (m *readyMatcher) match(rec *issueRecord) (bool, error) {
	i := &rec.Issue
	f := &m.filter

	switch {
	case f.Status == types.StatusOpen:
		// The custom-status widening joins the custom_statuses table, which
		// the SQL path renders for the issues table only; the wisps leg
		// projects the pin as an exact status (readyWorkWispIssueFilter).
		if i.Status != types.StatusOpen && !(rec.Plane == planeIssues && m.activeCustom[string(i.Status)]) {
			return false, nil
		}
	case f.Status != "":
		if i.Status != f.Status {
			return false, nil
		}
	case len(f.Statuses) > 0:
		if !containsStatus(f.Statuses, i.Status) {
			return false, nil
		}
	default:
		if i.Status != types.StatusOpen && i.Status != types.StatusInProgress {
			return false, nil
		}
	}
	if i.Pinned {
		return false, nil
	}
	if i.IsBlocked {
		own, err := hasOwnReadyAffectingEdge(m.r, i.ID)
		if err != nil || own {
			return false, err
		}
	}
	if !f.IncludeEphemeral && i.Ephemeral {
		return false, nil
	}
	if f.Priority != nil && i.Priority != *f.Priority {
		return false, nil
	}
	if f.Type != "" {
		if i.IssueType != types.IssueType(f.Type) {
			return false, nil
		}
	} else if m.excludeTypes[i.IssueType] {
		return false, nil
	}
	if f.Unassigned {
		if i.Assignee != "" {
			return false, nil
		}
	} else if f.Assignee != nil && i.Assignee != *f.Assignee {
		return false, nil
	}
	if !f.IncludeDeferred {
		if i.DeferUntil != nil && i.DeferUntil.After(m.now) {
			return false, nil
		}
		if m.deferredChildren[i.ID] {
			return false, nil
		}
	}
	if m.excludeIDs[i.ID] {
		return false, nil
	}
	if !labelSetMatches(i.Labels, m.labels, m.labelsAny, f.ExcludeLabels) {
		return false, nil
	}
	if m.labelGlob != nil && !anyLabelMatches(i.Labels, m.labelGlob) {
		return false, nil
	}
	if m.labelRegex != nil && !anyLabelMatches(i.Labels, m.labelRegex) {
		return false, nil
	}
	if f.ParentID != nil && !m.descendants[i.ID] {
		// The dotted-id arm admits a legacy hierarchical child that carries
		// no parent-child edge (sqlbuild.readyParentClause).
		if !strings.HasPrefix(i.ID, *f.ParentID+".") {
			return false, nil
		}
		parented, err := hasAnyParentEdge(m.r, i.ID)
		if err != nil || parented {
			return false, err
		}
	}
	if f.MoleculeID != "" {
		ok, err := childOrDottedDescendant(m.r, i.ID, f.MoleculeID)
		if err != nil || !ok {
			return false, err
		}
	}
	if !metadataMatches(i.Metadata, f.HasMetadataKey, m.metaKeys, f.MetadataFields) {
		return false, nil
	}
	return true, nil
}

// readyRecords returns every ready row, in id order.
func readyRecords(r kv, filter types.WorkFilter) ([]*issueRecord, error) {
	m, err := newReadyMatcher(r, filter)
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}
	var out []*issueRecord
	err = scanRecords(r, func(rec *issueRecord) (bool, error) {
		ok, err := m.match(rec)
		if err != nil {
			return false, err
		}
		if ok {
			out = append(out, rec)
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}
	return out, nil
}

// readyLess is issueops.sortReadyIssues as a comparator: the hybrid policy
// puts rows created within 48 hours first, by priority, and the rest by age.
func readyLess(policy types.SortPolicy, now time.Time) func(a, b *types.Issue) bool {
	recentCutoff := now.Add(-48 * time.Hour)
	createdBefore := func(a, b *types.Issue) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	}
	priorityBefore := func(a, b *types.Issue) bool {
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return createdBefore(a, b)
	}
	return func(a, b *types.Issue) bool {
		switch policy {
		case types.SortPolicyOldest:
			return createdBefore(a, b)
		case types.SortPolicyPriority:
			return priorityBefore(a, b)
		case types.SortPolicyHybrid, "":
			aRecent := !a.CreatedAt.Before(recentCutoff)
			bRecent := !b.CreatedAt.Before(recentCutoff)
			if aRecent != bRecent {
				return aRecent
			}
			if aRecent && a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			return createdBefore(a, b)
		default:
			return priorityBefore(a, b)
		}
	}
}

// boundReady is the page bound of GetReadyWorkInTx / finishReadyWorkWithCounts:
// the EffectiveSearchLimit each leg ran under, the Limit trim of the merged
// set, then the MaxRows cap on the delivered count.
func boundReady[T any](items []T, filter types.WorkFilter) ([]T, error) {
	if eff := storageissueops.EffectiveSearchLimit(filter.Limit, filter.MaxRows); eff > 0 && len(items) > eff {
		items = items[:eff]
	}
	if filter.Limit > 0 && len(items) > filter.Limit {
		items = items[:filter.Limit]
	}
	if err := storageissueops.EnforceMaxRowsCap(len(items), filter.MaxRows, filter.MaxRowsSource); err != nil {
		return nil, err
	}
	return items, nil
}

// readyWork is the GetReadyWork body over r.
func readyWork(r kv, filter types.WorkFilter) ([]*types.Issue, error) {
	recs, err := readyRecords(r, filter)
	if err != nil {
		return nil, err
	}
	issues := make([]*types.Issue, 0, len(recs))
	for _, rec := range recs {
		issue := rec.toIssue()
		if err := projectLease(r, issue); err != nil {
			return nil, fmt.Errorf("get ready work: lease %s: %w", issue.ID, err)
		}
		issues = append(issues, issue)
	}
	stableSortBy(issues, readyLess(filter.SortPolicy, nowUTC()))
	return boundReady(issues, filter)
}

// --- counts hydration ------------------------------------------------------------

// countsHydration mirrors sqlbuild.CountsHydration: the opt-outs of the
// counts mega-query's SELECT list.
type countsHydration struct {
	skipLabels bool
	skipCounts bool
	lite       bool
}

// withCounts hydrates one row the way sqlbuild.SearchCountsSQL projects it:
// DependencyCount is the row's outgoing blocks edges, DependentCount the
// blocks edges onto it, CommentCount its comments, Parent the smallest
// parent-child target, and Dependencies every outgoing edge (deps_json).
func withCounts(r kv, rec *issueRecord, hyd countsHydration) (*types.IssueWithCounts, error) {
	issue := rec.toIssue()
	if err := projectLease(r, issue); err != nil {
		return nil, err
	}
	if hyd.skipLabels {
		issue.Labels = nil
	}
	outs, err := outgoingEdges(r, issue.ID)
	if err != nil {
		return nil, err
	}
	issue.Dependencies = outs
	iwc := &types.IssueWithCounts{Issue: issue}
	for _, d := range outs {
		if d.Type == types.DepParentChild && (iwc.Parent == nil || d.DependsOnID < *iwc.Parent) {
			p := d.DependsOnID
			iwc.Parent = &p
		}
	}
	if !hyd.skipCounts {
		for _, d := range outs {
			if d.Type == types.DepBlocks {
				iwc.DependencyCount++
			}
		}
		ins, err := incomingEdges(r, issue.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range ins {
			if d.Type == types.DepBlocks {
				iwc.DependentCount++
			}
		}
		comments, err := commentsOf(r, issue.ID)
		if err != nil {
			return nil, err
		}
		iwc.CommentCount = len(comments)
	}
	if hyd.lite {
		liteIssue(issue)
	}
	return iwc, nil
}

func sortIssuesWithCounts(items []*types.IssueWithCounts, less func(a, b *types.Issue) bool) {
	stableSortBy(items, func(a, b *types.IssueWithCounts) bool {
		if a == nil || a.Issue == nil {
			return false
		}
		if b == nil || b.Issue == nil {
			return true
		}
		return less(a.Issue, b.Issue)
	})
}

// readyWorkWithCounts is the GetReadyWorkWithCounts(AndTotal) body: the
// ready page hydrated with counts, plus the size of the whole ready set the
// page was cut from.
func readyWorkWithCounts(r kv, filter types.WorkFilter) ([]*types.IssueWithCounts, int, error) {
	recs, err := readyRecords(r, filter)
	if err != nil {
		return nil, 0, err
	}
	hyd := countsHydration{lite: filter.Lite}
	items := make([]*types.IssueWithCounts, 0, len(recs))
	for _, rec := range recs {
		iwc, err := withCounts(r, rec, hyd)
		if err != nil {
			return nil, 0, fmt.Errorf("get ready work with counts: %s: %w", rec.Issue.ID, err)
		}
		items = append(items, iwc)
	}
	sortIssuesWithCounts(items, readyLess(filter.SortPolicy, nowUTC()))
	total := len(items)
	items, err = boundReady(items, filter)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// --- blocked issues ------------------------------------------------------------------

// blockedIssues is issueops.GetBlockedIssuesInTx over r: every open row whose
// flag is set, with the open targets of its blocks / conditional-blocks /
// waits-for edges as its blockers, or its parent when it has none of its own
// (inherited blocking); a flagged row with neither is left out.
func blockedIssues(r kv, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	var results []*types.BlockedIssue
	err := scanRecords(r, func(rec *issueRecord) (bool, error) {
		i := &rec.Issue
		if !i.IsBlocked || !open(i.Status) {
			return true, nil
		}
		if !labelSetMatches(i.Labels, filter.Labels, filter.LabelsAny, filter.ExcludeLabels) {
			return true, nil
		}
		outs, err := outgoingEdges(r, i.ID)
		if err != nil {
			return false, err
		}
		var blockers []string
		for _, d := range outs {
			switch d.Type {
			case types.DepBlocks, types.DepWaitsFor, types.DepConditionalBlocks:
				target, err := getRecord(r, d.DependsOnID)
				if err != nil {
					if isNotFound(err) {
						continue
					}
					return false, err
				}
				if open(target.Issue.Status) {
					blockers = append(blockers, d.DependsOnID)
				}
			}
		}
		if len(blockers) == 0 {
			parent, err := parentOf(r, i.ID)
			if err != nil {
				return false, err
			}
			if parent != "" {
				blockers = []string{parent}
			}
		}
		if len(blockers) == 0 {
			return true, nil
		}
		if filter.ParentID != nil {
			direct, err := hasParentEdgeTo(r, i.ID, *filter.ParentID)
			if err != nil {
				return false, err
			}
			if !direct && !strings.HasPrefix(i.ID, *filter.ParentID+".") {
				return true, nil
			}
		}
		issue := rec.toIssue()
		if err := projectLease(r, issue); err != nil {
			return false, err
		}
		results = append(results, &types.BlockedIssue{Issue: *issue, BlockedByCount: len(blockers), BlockedBy: blockers})
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get blocked issues: %w", err)
	}
	// priority ASC, created_at DESC as the SQL path sorts, with the id as a
	// tie-break the SQL path leaves unspecified.
	stableSortBy(results, func(a, b *types.BlockedIssue) bool {
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return results, nil
}

// --- Store surface ------------------------------------------------------------

// GetReadyWork returns the issues ready to work: matching the status pin
// (open and in_progress by default), not pinned, not blocked, not deferred,
// not ephemeral unless asked, and not of an infrastructure type, ordered by
// filter.SortPolicy.
func (s *Store) GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		v, err := readyWork(r, filter)
		out = v
		return err
	})
	return out, err
}

// GetReadyWorkWithCounts is GetReadyWork hydrated with dependency,
// dependent and comment counts and the parent-child parent.
func (s *Store) GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) ([]*types.IssueWithCounts, error) {
	items, _, err := s.GetReadyWorkWithCountsAndTotal(ctx, filter)
	return items, err
}

// GetReadyWorkWithCountsAndTotal is GetReadyWorkWithCounts plus the size of
// the whole ready set the page was cut from.
func (s *Store) GetReadyWorkWithCountsAndTotal(ctx context.Context, filter types.WorkFilter) ([]*types.IssueWithCounts, int, error) {
	var out []*types.IssueWithCounts
	var total int
	err := s.read(func(r kv) error {
		v, n, err := readyWorkWithCounts(r, filter)
		out, total = v, n
		return err
	})
	return out, total, err
}

// CountReadyWork returns the size of the ready set for filter, ignoring
// Limit (storage.ReadyWorkCounter).
func (s *Store) CountReadyWork(ctx context.Context, filter types.WorkFilter) (int, error) {
	var n int
	err := s.read(func(r kv) error {
		recs, err := readyRecords(r, filter)
		n = len(recs)
		return err
	})
	return n, err
}

// GetBlockedIssues returns the open issues whose blocked flag is set, each
// with the ids it is blocked by.
func (s *Store) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	var out []*types.BlockedIssue
	err := s.read(func(r kv) error {
		v, err := blockedIssues(r, filter)
		out = v
		return err
	})
	return out, err
}
