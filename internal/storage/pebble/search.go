package pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// Search is a full scan filtered in process to the predicate
// sqlbuild.BuildIssueFilterClauses renders for the SQL backends, ordered by
// the comparator the SQL ORDER BY and the issues+wisps merge share
// (sqlbuild.Less), and bounded the way issueops.searchInTx bounds a page:
// each leg to EffectiveSearchLimit, the merged set to Limit, then the MaxRows
// cap. Both planes live in one table here, so the merge is the scan itself:
// SkipWisps drops the wisps-plane rows, Ephemeral=true keeps only ephemeral
// rows (the wisps-table route), and nothing can appear twice.

// issueMatcher evaluates one IssueFilter (plus the free-text query) against
// rows. Everything that can fail to compile (label regex, metadata keys) is
// resolved once, up front.
type issueMatcher struct {
	r          kv
	filter     types.IssueFilter
	query      string
	lowerQuery string
	idLike     bool
	labels     []string
	labelsAny  []string
	labelGlob  *regexp.Regexp
	labelRegex *regexp.Regexp
	metaKeys   []string
	ids        map[string]bool
	now        time.Time
}

func newIssueMatcher(r kv, query string, filter types.IssueFilter) (*issueMatcher, error) {
	m := &issueMatcher{
		r:         r,
		filter:    filter,
		query:     query,
		labels:    sqlbuild.CompactNonEmptyStrings(filter.Labels),
		labelsAny: sqlbuild.CompactNonEmptyStrings(filter.LabelsAny),
		now:       nowUTC(),
	}
	if query != "" {
		m.lowerQuery = strings.ToLower(query)
		m.idLike = sqlbuild.LooksLikeIssueID(query)
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
	if len(filter.IDs) > 0 {
		m.ids = make(map[string]bool, len(filter.IDs))
		for _, id := range filter.IDs {
			m.ids[id] = true
		}
	}
	return m, nil
}

// globRegexp compiles a shell-style glob (* and ?) the way the SQL LIKE
// pattern sqlbuild.globToLikePattern renders it: anchored, case-sensitive.
func globRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`^(?s:`)
	for _, c := range pattern {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)$`)
	return regexp.MustCompile(b.String())
}

func containsFold(haystack, lowerNeedle string) bool {
	return strings.Contains(strings.ToLower(haystack), lowerNeedle)
}

func hasLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

func hasAnyLabel(labels, wanted []string) bool {
	for _, w := range wanted {
		if hasLabel(labels, w) {
			return true
		}
	}
	return false
}

func anyLabelMatches(labels []string, re *regexp.Regexp) bool {
	for _, l := range labels {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// labelSetMatches is sqlbuild.LabelSetClauses in process: every label of
// the AND set, at least one of the ANY set, none of the exclude set.
func labelSetMatches(have, labels, labelsAny, exclude []string) bool {
	for _, l := range labels {
		if !hasLabel(have, l) {
			return false
		}
	}
	if len(labelsAny) > 0 && !hasAnyLabel(have, labelsAny) {
		return false
	}
	if len(exclude) > 0 && hasAnyLabel(have, exclude) {
		return false
	}
	return true
}

// hasParentEdgeTo reports whether id carries a parent-child edge onto parent.
func hasParentEdgeTo(r kv, id, parent string) (bool, error) {
	_, ok, err := r.get(depKey(id, parent, string(types.DepParentChild)))
	return ok, err
}

// hasAnyParentEdge reports whether id carries any parent-child edge.
func hasAnyParentEdge(r kv, id string) (bool, error) {
	p, err := parentOf(r, id)
	return p != "", err
}

// childOrDottedDescendant is the SQL ParentID / MoleculeID arm: a row with a
// parent-child edge onto parent, or a dotted child id of parent that carries
// no parent-child edge at all (legacy hierarchical ids).
func childOrDottedDescendant(r kv, id, parent string) (bool, error) {
	if ok, err := hasParentEdgeTo(r, id, parent); err != nil || ok {
		return ok, err
	}
	if !strings.HasPrefix(id, parent+".") {
		return false, nil
	}
	parented, err := hasAnyParentEdge(r, id)
	return !parented, err
}

// metadataObject parses a row's metadata as a top-level object; absent or
// non-object metadata yields nil, which no key predicate matches.
func metadataObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// metadataUnquoted renders a JSON value the way JSON_UNQUOTE(JSON_EXTRACT())
// does: a string loses its quotes, anything else is its JSON text, and an
// absent or null value is NULL.
func metadataUnquoted(v json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(v))
	if t == "" || t == "null" {
		return "", false
	}
	if strings.HasPrefix(t, `"`) {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s, true
		}
	}
	return t, true
}

// metadataMatches is sqlbuild.AppendMetadataClauses in process.
func metadataMatches(raw json.RawMessage, hasKey string, keys []string, fields map[string]string) bool {
	if hasKey == "" && len(keys) == 0 {
		return true
	}
	meta := metadataObject(raw)
	if hasKey != "" {
		if _, ok := metadataUnquoted(meta[hasKey]); !ok {
			return false
		}
	}
	for _, k := range keys {
		v, ok := metadataUnquoted(meta[k])
		if !ok || v != fields[k] {
			return false
		}
	}
	return true
}

// timeAfter / timeBefore compare a nullable column with a bound: a NULL
// column never satisfies the comparison.
func timeAfter(col *time.Time, bound *time.Time) bool {
	return col != nil && col.After(*bound)
}

func timeBefore(col *time.Time, bound *time.Time) bool {
	return col != nil && col.Before(*bound)
}

func containsStatus(set []types.Status, s types.Status) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func containsType(set []types.IssueType, t types.IssueType) bool {
	for _, x := range set {
		if x == t {
			return true
		}
	}
	return false
}

// match evaluates every clause of sqlbuild.BuildIssueFilterClauses, in the
// builder's order so the two can be read side by side.
func (m *issueMatcher) match(rec *issueRecord) (bool, error) {
	i := &rec.Issue
	f := &m.filter
	extRef := ""
	if i.ExternalRef != nil {
		extRef = *i.ExternalRef
	}

	if m.query != "" {
		q := m.lowerQuery
		var hit bool
		if m.idLike {
			// id = ? / id LIKE ? compare the raw id with the lowercased
			// query under a binary collation; LOWER(external_ref) LIKE is
			// NULL, hence false, for a row without an external ref.
			hit = i.ID == q || strings.HasPrefix(i.ID, q) || containsFold(i.Title, q) ||
				(i.ExternalRef != nil && containsFold(extRef, q))
		} else {
			hit = containsFold(i.Title, q) || strings.Contains(i.ID, q)
		}
		if !hit {
			return false, nil
		}
	}
	if f.TitleSearch != "" && !containsFold(i.Title, strings.ToLower(f.TitleSearch)) {
		return false, nil
	}
	if f.TitleContains != "" && !containsFold(i.Title, strings.ToLower(f.TitleContains)) {
		return false, nil
	}
	if f.DescriptionContains != "" && !containsFold(i.Description, strings.ToLower(f.DescriptionContains)) {
		return false, nil
	}
	if f.NotesContains != "" && !containsFold(i.Notes, strings.ToLower(f.NotesContains)) {
		return false, nil
	}
	if f.ExternalRefContains != "" && (i.ExternalRef == nil || !containsFold(extRef, strings.ToLower(f.ExternalRefContains))) {
		return false, nil
	}
	if f.ExternalRef != nil && (i.ExternalRef == nil || extRef != *f.ExternalRef) {
		return false, nil
	}

	if f.Status != nil && i.Status != *f.Status {
		return false, nil
	}
	if len(f.Statuses) > 0 && !containsStatus(f.Statuses, i.Status) {
		return false, nil
	}
	if len(f.ExcludeStatus) > 0 && containsStatus(f.ExcludeStatus, i.Status) {
		return false, nil
	}
	if f.IssueType != nil && i.IssueType != *f.IssueType {
		return false, nil
	}
	if len(f.ExcludeTypes) > 0 && containsType(f.ExcludeTypes, i.IssueType) {
		return false, nil
	}
	if f.Assignee != nil && i.Assignee != *f.Assignee {
		return false, nil
	}
	if f.Priority != nil && i.Priority != *f.Priority {
		return false, nil
	}
	if f.PriorityMin != nil && i.Priority < *f.PriorityMin {
		return false, nil
	}
	if f.PriorityMax != nil && i.Priority > *f.PriorityMax {
		return false, nil
	}
	if m.ids != nil && !m.ids[i.ID] {
		return false, nil
	}
	if f.IDPrefix != "" && !strings.HasPrefix(i.ID, f.IDPrefix) {
		return false, nil
	}
	if f.SpecIDPrefix != "" && !strings.HasPrefix(i.SpecID, f.SpecIDPrefix) {
		return false, nil
	}

	if f.ParentID != nil {
		ok, err := childOrDottedDescendant(m.r, i.ID, *f.ParentID)
		if err != nil || !ok {
			return false, err
		}
	}
	if f.NoParent {
		parented, err := hasAnyParentEdge(m.r, i.ID)
		if err != nil || parented {
			return false, err
		}
	}
	if f.MolType != nil && i.MolType != *f.MolType {
		return false, nil
	}
	if f.WispType != nil && i.WispType != *f.WispType {
		return false, nil
	}

	if !labelSetMatches(i.Labels, m.labels, m.labelsAny, f.ExcludeLabels) {
		return false, nil
	}
	if f.NoLabels && len(i.Labels) > 0 {
		return false, nil
	}
	if m.labelGlob != nil && !anyLabelMatches(i.Labels, m.labelGlob) {
		return false, nil
	}
	if m.labelRegex != nil && !anyLabelMatches(i.Labels, m.labelRegex) {
		return false, nil
	}

	if f.Pinned != nil && i.Pinned != *f.Pinned {
		return false, nil
	}
	if f.SourceRepo != nil && rec.SourceRepo != *f.SourceRepo {
		return false, nil
	}
	if f.Ephemeral != nil && i.Ephemeral != *f.Ephemeral {
		return false, nil
	}
	if f.EphemeralTier != nil && (i.Ephemeral || i.WispType != "") != *f.EphemeralTier {
		return false, nil
	}
	if f.IsTemplate != nil && i.IsTemplate != *f.IsTemplate {
		return false, nil
	}
	if f.IsBlocked != nil && i.IsBlocked != *f.IsBlocked {
		return false, nil
	}
	if f.EmptyDescription && i.Description != "" {
		return false, nil
	}
	if f.NoAssignee && i.Assignee != "" {
		return false, nil
	}

	created, updated := i.CreatedAt, i.UpdatedAt
	if f.CreatedAfter != nil && !timeAfter(&created, f.CreatedAfter) {
		return false, nil
	}
	if f.CreatedBefore != nil && !timeBefore(&created, f.CreatedBefore) {
		return false, nil
	}
	if f.UpdatedAfter != nil && !timeAfter(&updated, f.UpdatedAfter) {
		return false, nil
	}
	if f.UpdatedBefore != nil && !timeBefore(&updated, f.UpdatedBefore) {
		return false, nil
	}
	if f.ClosedAfter != nil && !timeAfter(i.ClosedAt, f.ClosedAfter) {
		return false, nil
	}
	if f.ClosedBefore != nil && !timeBefore(i.ClosedAt, f.ClosedBefore) {
		return false, nil
	}
	if f.StartedAfter != nil && !timeAfter(i.StartedAt, f.StartedAfter) {
		return false, nil
	}
	if f.StartedBefore != nil && !timeBefore(i.StartedAt, f.StartedBefore) {
		return false, nil
	}
	if f.DeferAfter != nil && !timeAfter(i.DeferUntil, f.DeferAfter) {
		return false, nil
	}
	if f.DeferBefore != nil && !timeBefore(i.DeferUntil, f.DeferBefore) {
		return false, nil
	}
	if f.DueAfter != nil && !timeAfter(i.DueAt, f.DueAfter) {
		return false, nil
	}
	if f.DueBefore != nil && !timeBefore(i.DueAt, f.DueBefore) {
		return false, nil
	}

	// Keyset position: strictly after (AfterCreatedAt, AfterID) under
	// (created_at DESC, id ASC), nested inside the priority order when
	// AfterPriority is set (types.IssueFilter).
	if f.AfterCreatedAt != nil {
		ac := f.AfterCreatedAt.UTC()
		afterCreated := created.Before(ac) || (created.Equal(ac) && i.ID > f.AfterID)
		if f.AfterPriority != nil {
			ap := *f.AfterPriority
			if !(i.Priority > ap || (i.Priority == ap && afterCreated)) {
				return false, nil
			}
		} else if !afterCreated {
			return false, nil
		}
	}

	if f.Deferred && !(i.DeferUntil != nil || i.Status == types.StatusDeferred) {
		return false, nil
	}
	if f.Overdue && !(i.DueAt != nil && i.DueAt.Before(m.now) && i.Status != types.StatusClosed) {
		return false, nil
	}
	if !metadataMatches(i.Metadata, f.HasMetadataKey, m.metaKeys, f.MetadataFields) {
		return false, nil
	}
	return true, nil
}

// searchRecords returns every row the query and filter admit, in id order,
// honouring the plane routing of issueops.searchInTx (SkipWisps).
func searchRecords(r kv, query string, filter types.IssueFilter) ([]*issueRecord, error) {
	m, err := newIssueMatcher(r, query, filter)
	if err != nil {
		return nil, err
	}
	var out []*issueRecord
	err = scanRecords(r, func(rec *issueRecord) (bool, error) {
		if filter.SkipWisps && rec.Plane == planeWisps {
			return true, nil
		}
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
		return nil, fmt.Errorf("search issues: %w", err)
	}
	return out, nil
}

// liteIssue strips the heavy TEXT columns the lite projection does not
// select and marks the row partial (types.Issue.IsLitePartial).
func liteIssue(i *types.Issue) {
	i.Description = ""
	i.Design = ""
	i.AcceptanceCriteria = ""
	i.Notes = ""
	i.Payload = ""
	i.Waiters = nil
	i.IsLitePartial = true
}

// hydrateSearchIssue projects a row for a search result: lease fields, the
// label opt-out, dependency records on request, the lite shape on request.
func hydrateSearchIssue(r kv, rec *issueRecord, filter types.IssueFilter) (*types.Issue, error) {
	issue := rec.toIssue()
	if err := projectLease(r, issue); err != nil {
		return nil, err
	}
	if filter.SkipLabels {
		issue.Labels = nil
	}
	if filter.IncludeDependencies {
		deps, err := outgoingEdges(r, issue.ID)
		if err != nil {
			return nil, err
		}
		issue.Dependencies = deps
	}
	if filter.Lite {
		liteIssue(issue)
	}
	return issue, nil
}

func sortIssues(issues []*types.Issue, sortBy string, sortDesc bool) {
	stableSortBy(issues, func(a, b *types.Issue) bool { return sqlbuild.Less(a, b, sortBy, sortDesc) })
}

// boundSearch applies the page bound issueops.searchInTx applies on every
// exit path: the per-leg EffectiveSearchLimit, the Limit trim, then the
// MaxRows cap on the delivered count (*issueops.ErrTooManyRows).
func boundSearch[T any](items []T, filter types.IssueFilter) ([]T, error) {
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

// searchIssues is the SearchIssues body over r: the shared read the store
// and a transaction both run.
func searchIssues(r kv, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	recs, err := searchRecords(r, query, filter)
	if err != nil {
		return nil, err
	}
	issues := make([]*types.Issue, 0, len(recs))
	for _, rec := range recs {
		issue, err := hydrateSearchIssue(r, rec, filter)
		if err != nil {
			return nil, fmt.Errorf("search issues: hydrate %s: %w", rec.Issue.ID, err)
		}
		issues = append(issues, issue)
	}
	sortIssues(issues, filter.SortBy, filter.SortDesc)
	return boundSearch(issues, filter)
}

// searchIssueIDs is the id projection of searchIssues: the same rows in the
// same order, as bare ids.
func searchIssueIDs(r kv, query string, filter types.IssueFilter) ([]string, error) {
	recs, err := searchRecords(r, query, filter)
	if err != nil {
		return nil, err
	}
	issues := make([]*types.Issue, 0, len(recs))
	for _, rec := range recs {
		issues = append(issues, &rec.Issue)
	}
	sortIssues(issues, filter.SortBy, filter.SortDesc)
	issues, err = boundSearch(issues, filter)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(issues))
	for _, i := range issues {
		ids = append(ids, i.ID)
	}
	return ids, nil
}

// summaryOf projects the columns issueops.IssueSummaryColumns selects.
func summaryOf(i *types.Issue) *types.IssueSummary {
	return &types.IssueSummary{
		ID:           i.ID,
		Title:        i.Title,
		Status:       i.Status,
		Priority:     i.Priority,
		IssueType:    i.IssueType,
		Assignee:     i.Assignee,
		Pinned:       i.Pinned,
		Labels:       i.Labels,
		CreatedAt:    i.CreatedAt,
		UpdatedAt:    i.UpdatedAt,
		ClosedAt:     i.ClosedAt,
		Ephemeral:    i.Ephemeral,
		NoHistory:    i.NoHistory,
		WispType:     i.WispType,
		StorageClass: i.StorageClass,
	}
}

func searchIssueSummaries(r kv, query string, filter types.IssueFilter) ([]*types.IssueSummary, error) {
	// IncludeDependencies has nowhere to land on a summary (storage.go);
	// the lite shape is moot for a projection narrower than lite.
	f := filter
	f.IncludeDependencies = false
	f.Lite = false
	issues, err := searchIssues(r, query, f)
	if err != nil {
		return nil, err
	}
	out := make([]*types.IssueSummary, 0, len(issues))
	for _, i := range issues {
		out = append(out, summaryOf(i))
	}
	return out, nil
}

// countIssues is CountIssues over r: the matching cardinality, with Limit,
// Offset and the row cap ignored (issueops.CountIssuesInTx).
func countIssues(r kv, query string, filter types.IssueFilter) (int64, error) {
	recs, err := searchRecords(r, query, filter)
	if err != nil {
		return 0, err
	}
	return int64(len(recs)), nil
}

// countIssuesByGroup is issueops.CountIssuesByGroupInTx over r: per-bucket
// counts with the SQL key normalization (P-prefixed priorities, "(unassigned)",
// one count per label plus a "(no labels)" bucket only when it is non-zero).
func countIssuesByGroup(r kv, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	switch groupBy {
	case "status", "priority", "type", "assignee", "label":
	default:
		return nil, fmt.Errorf("unsupported groupBy: %s", groupBy)
	}
	recs, err := searchRecords(r, "", filter)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	noLabels := 0
	for _, rec := range recs {
		i := &rec.Issue
		switch groupBy {
		case "status":
			counts[string(i.Status)]++
		case "priority":
			counts["P"+strconv.Itoa(i.Priority)]++
		case "type":
			counts[string(i.IssueType)]++
		case "assignee":
			key := i.Assignee
			if key == "" {
				key = "(unassigned)"
			}
			counts[key]++
		case "label":
			if len(i.Labels) == 0 {
				noLabels++
			}
			for _, l := range i.Labels {
				counts[l]++
			}
		}
	}
	if groupBy == "label" && noLabels > 0 {
		counts["(no labels)"] = noLabels
	}
	return counts, nil
}

// searchIssuesWithCounts is SearchIssuesWithCounts over r: the search page
// hydrated with the counts the SQL mega-query computes (withCounts).
func searchIssuesWithCounts(r kv, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error) {
	recs, err := searchRecords(r, query, filter)
	if err != nil {
		return nil, err
	}
	hyd := countsHydration{skipLabels: filter.SkipLabels, skipCounts: filter.SkipCounts, lite: filter.Lite}
	items := make([]*types.IssueWithCounts, 0, len(recs))
	for _, rec := range recs {
		iwc, err := withCounts(r, rec, hyd)
		if err != nil {
			return nil, fmt.Errorf("search issues with counts: %s: %w", rec.Issue.ID, err)
		}
		items = append(items, iwc)
	}
	sortIssuesWithCounts(items, func(a, b *types.Issue) bool { return sqlbuild.Less(a, b, filter.SortBy, filter.SortDesc) })
	return boundSearch(items, filter)
}

// --- Store surface ------------------------------------------------------------

// SearchIssues finds the issues matching query and filter, both planes
// merged, ordered by filter.SortBy/SortDesc (priority ASC, created_at DESC,
// id ASC by default) and bounded by Limit and the MaxRows cap.
func (s *Store) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		v, err := searchIssues(r, query, filter)
		out = v
		return err
	})
	return out, err
}

// SearchIssueIDs is the narrow projection of SearchIssues: the matching ids
// in the same order.
func (s *Store) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	var out []string
	err := s.read(func(r kv) error {
		v, err := searchIssueIDs(r, query, filter)
		out = v
		return err
	})
	return out, err
}

// SearchIssueSummaries is the list-rendering projection of SearchIssues.
func (s *Store) SearchIssueSummaries(ctx context.Context, query string, filter types.IssueFilter) ([]*types.IssueSummary, error) {
	var out []*types.IssueSummary
	err := s.read(func(r kv) error {
		v, err := searchIssueSummaries(r, query, filter)
		out = v
		return err
	})
	return out, err
}

// SearchIssuesWithCounts is SearchIssues hydrated with the counts the SQL
// mega-query computes: outgoing blocks edges, incoming blocks edges,
// comments, the parent-child parent, and the dependency records.
func (s *Store) SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) ([]*types.IssueWithCounts, error) {
	var out []*types.IssueWithCounts
	err := s.read(func(r kv) error {
		v, err := searchIssuesWithCounts(r, query, filter)
		out = v
		return err
	})
	return out, err
}

// CountIssues returns the number of issues matching query and filter;
// Limit, Offset and MaxRows are ignored.
func (s *Store) CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error) {
	var out int64
	err := s.read(func(r kv) error {
		v, err := countIssues(r, query, filter)
		out = v
		return err
	})
	return out, err
}

// CountIssuesByGroup returns per-group counts for groupBy (status,
// priority, type, assignee or label) over the issues matching filter.
func (s *Store) CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	var out map[string]int
	err := s.read(func(r kv) error {
		v, err := countIssuesByGroup(r, filter, groupBy)
		out = v
		return err
	})
	return out, err
}
