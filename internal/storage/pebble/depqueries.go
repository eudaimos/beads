package pebble

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Dependency verbs and the DependencyQueryStore surface. The edge rules the
// SQL backends enforce in issueops.addDependencyInTx (self-edge, missing
// ends, blocking-hierarchy, one edge per pair, scheduling-graph cycles) are
// applied here before the shared addEdge writes the row; the readers are
// in-process forms of the issueops query functions over the edge indexes.

const (
	defaultDependentRecordsLimit = 100
	maxDependentRecordsLimit     = 500
	maxWispDependentResults      = 10000
)

// --- edge writes --------------------------------------------------------------

// isExternalTarget mirrors the SQL routing: an "external:" reference or a
// target under another prefix has no local row to validate against.
func isExternalTarget(src, dst string) bool {
	return strings.HasPrefix(dst, "external:") || types.ExtractPrefix(src) != types.ExtractPrefix(dst)
}

// addDependency validates and writes one edge the way
// issueops.addDependencyInTx does, then hands the row to addEdge. It is the
// shared body of AddDependency(WithOptions) on the store and the transaction.
func (t *txn) addDependency(dep *types.Dependency, actor string, opts storage.DependencyAddOptions) error {
	if dep == nil {
		return fmt.Errorf("%w: dependency must not be nil", storage.ErrValidation)
	}
	if dep.IssueID == "" || dep.DependsOnID == "" {
		return fmt.Errorf("%w: dependency needs both ends", storage.ErrValidation)
	}
	if dep.Type == "" {
		dep.Type = types.DepBlocks
	}
	if !dep.Type.IsValid() {
		return fmt.Errorf("%w: invalid dependency type %q", storage.ErrValidation, dep.Type)
	}
	if dep.IssueID == dep.DependsOnID {
		return fmt.Errorf("%w: %s cannot depend on itself", domain.ErrSelfDependency, dep.IssueID)
	}
	if strings.HasPrefix(dep.DependsOnID, "external:") && dep.Type == types.DepParentChild {
		return fmt.Errorf("%w: external capability dependencies cannot use parent-child edges", storage.ErrValidation)
	}
	if dep.Metadata == "" {
		dep.Metadata = "{}"
	}
	src, err := getRecord(t.kv, dep.IssueID)
	if err != nil {
		if isNotFound(err) {
			return storageissueops.MissingDependencySource(dep.IssueID, dep.DependsOnID)
		}
		return err
	}
	external := isExternalTarget(dep.IssueID, dep.DependsOnID)
	targetExists, err := recordExists(t.kv, dep.DependsOnID)
	if err != nil {
		return err
	}
	if !targetExists && !external {
		return storageissueops.MissingDependencyTarget(dep.IssueID, dep.DependsOnID)
	}
	if targetExists {
		if err := checkBlockingHierarchy(t.kv, dep); err != nil {
			return err
		}
	}
	existing, err := edgeBetween(t.kv, dep.IssueID, dep.DependsOnID)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Type != dep.Type {
			return &domain.DependencyTypeConflictError{
				IssueID: dep.IssueID, DependsOnID: dep.DependsOnID,
				ExistingType: string(existing.Type), RequestedType: string(dep.Type),
			}
		}
		// Same pair and type: idempotent. A re-add naming a thread moves it;
		// one naming none keeps the stored thread. Metadata is refreshed.
		thread := existing.ThreadID
		if dep.ThreadID != "" {
			thread = dep.ThreadID
		}
		if thread != existing.ThreadID || !storageissueops.DependencyMetadataEqual(existing.Metadata, dep.Metadata) {
			existing.ThreadID = thread
			existing.Metadata = dep.Metadata
			if err := putEdge(t.kv, existing); err != nil {
				return fmt.Errorf("update dependency metadata: %w", err)
			}
		}
		dep.ID, dep.CreatedAt, dep.CreatedBy, dep.ThreadID = existing.ID, existing.CreatedAt, existing.CreatedBy, existing.ThreadID
		return nil
	}
	if !opts.SkipCycleCheck && types.IsSchedulingEdge(dep.Type) {
		reachable, err := schedulingReachable(t.kv, dep.DependsOnID, dep.IssueID)
		if err != nil {
			return fmt.Errorf("failed to check for dependency cycle: %w", err)
		}
		if reachable {
			return fmt.Errorf("%w: %s -> %s", domain.ErrDependencyCycle, dep.IssueID, dep.DependsOnID)
		}
	}
	if !targetExists {
		// addEdge tolerates a missing target only for a blocks edge whose id
		// carries a colon; the SQL backends accept any external target, so the
		// row is written here with the same tail addEdge applies.
		return t.writeExternalEdge(src, dep, actor, opts)
	}
	return t.addEdge(dep, actor, storage.DependencyAddOptions{SkipCycleCheck: true, EmitEvent: opts.EmitEvent})
}

// writeExternalEdge is addEdge's write tail for a target with no local row.
func (t *txn) writeExternalEdge(src *issueRecord, dep *types.Dependency, actor string, opts storage.DependencyAddOptions) error {
	if dep.CreatedAt.IsZero() {
		dep.CreatedAt = nowUTC()
	} else {
		dep.CreatedAt = dep.CreatedAt.UTC()
	}
	if dep.CreatedBy == "" {
		dep.CreatedBy = actor
	}
	if dep.ID == "" {
		dep.ID = dependencyID(dep)
	}
	if err := putEdge(t.kv, dep); err != nil {
		return err
	}
	if opts.EmitEvent {
		if err := t.recordEvent(src, types.EventDependencyAdded, actor, nil, strPtr(dep.DependsOnID), strPtr(fmt.Sprintf("Added dependency: %s -> %s (%s)", dep.IssueID, dep.DependsOnID, dep.Type))); err != nil {
			return err
		}
	}
	if dep.Type.AffectsReadyWork() {
		t.markBlockedDirty(dep.IssueID, dep.DependsOnID)
	}
	return nil
}

// edgeBetween returns the edge from src to dst of any type, or nil.
func edgeBetween(r kv, src, dst string) (*types.Dependency, error) {
	outs, err := outgoingEdges(r, src)
	if err != nil {
		return nil, err
	}
	for _, d := range outs {
		if d.DependsOnID == dst {
			return d, nil
		}
	}
	return nil, nil
}

// checkBlockingHierarchy mirrors issueops.CheckBlockingHierarchyInTx: a
// blocks or conditional-blocks edge onto the source's own ancestor or
// descendant is refused.
func checkBlockingHierarchy(r kv, dep *types.Dependency) error {
	if dep.Type != types.DepBlocks && dep.Type != types.DepConditionalBlocks {
		return nil
	}
	if up, err := isAncestor(r, dep.IssueID, dep.DependsOnID); err != nil {
		return fmt.Errorf("failed to check blocker ancestry: %w", err)
	} else if up {
		return &domain.DependencyHierarchyConflictError{IssueID: dep.IssueID, BlockerID: dep.DependsOnID, BlockerIsAncestor: true}
	}
	if down, err := isAncestor(r, dep.DependsOnID, dep.IssueID); err != nil {
		return fmt.Errorf("failed to check blocker ancestry: %w", err)
	} else if down {
		return &domain.DependencyHierarchyConflictError{IssueID: dep.IssueID, BlockerID: dep.DependsOnID}
	}
	return nil
}

// isAncestor reports whether candidate is reached from node by walking
// parent-child edges upward (child -> parent only).
func isAncestor(r kv, node, candidate string) (bool, error) {
	seen := map[string]bool{node: true}
	queue := []string{node}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		outs, err := outgoingEdges(r, cur)
		if err != nil {
			return false, err
		}
		for _, d := range outs {
			if d.Type != types.DepParentChild || seen[d.DependsOnID] {
				continue
			}
			if d.DependsOnID == candidate {
				return true, nil
			}
			seen[d.DependsOnID] = true
			queue = append(queue, d.DependsOnID)
		}
	}
	return false, nil
}

// schedulingReachable reports whether goal is reachable from start along
// scheduling edges (blocks, conditional-blocks, parent-child), the anchor
// included, as the SQL backends' combined cycle probe does.
func schedulingReachable(r kv, start, goal string) (bool, error) {
	if start == goal {
		return true, nil
	}
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		outs, err := outgoingEdges(r, cur)
		if err != nil {
			return false, err
		}
		for _, d := range outs {
			if !types.IsSchedulingEdge(d.Type) || seen[d.DependsOnID] {
				continue
			}
			if d.DependsOnID == goal {
				return true, nil
			}
			seen[d.DependsOnID] = true
			queue = append(queue, d.DependsOnID)
		}
	}
	return false, nil
}

// AddDependency adds an edge without recording a dependency_added event.
func (s *Store) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return s.AddDependencyWithOptions(ctx, dep, actor, storage.DependencyAddOptions{})
}

// AddDependencyWithOptions adds an edge; EmitEvent records the history event.
func (s *Store) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts storage.DependencyAddOptions) error {
	return s.run(ctx, func(t *txn) error {
		if err := t.addDependency(dep, actor, opts); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// RemoveDependency removes every edge from issueID to dependsOnID without
// recording an event.
func (s *Store) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return s.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, storage.DependencyRemoveOptions{})
}

// RemoveDependencyWithOptions removes every edge from issueID to dependsOnID;
// EmitEvent records the history event when one existed.
func (s *Store) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts storage.DependencyRemoveOptions) error {
	return s.run(ctx, func(t *txn) error {
		if err := t.removeEdge(issueID, dependsOnID, actor, opts); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// --- record helpers (the Transaction contract) ---------------------------------

// normalizeDep gives an edge the JSON-empty metadata the SQL rows default to.
func normalizeDep(d *types.Dependency) *types.Dependency {
	if d.Metadata == "" {
		d.Metadata = "{}"
	}
	return d
}

// dependencyRecords returns the outgoing edges of issueID in (target, type)
// order.
func dependencyRecords(r kv, issueID string) ([]*types.Dependency, error) {
	outs, err := outgoingEdges(r, issueID)
	if err != nil {
		return nil, err
	}
	for _, d := range outs {
		normalizeDep(d)
	}
	return outs, nil
}

// dependentRecords returns the inbound edges of targetID, filtered by
// depType ("" = all), in row-id order, paged by afterID and bounded by limit
// (0 = default, capped).
func dependentRecords(r kv, targetID, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	if limit <= 0 {
		limit = defaultDependentRecordsLimit
	}
	if limit > maxDependentRecordsLimit {
		limit = maxDependentRecordsLimit
	}
	ins, err := incomingEdges(r, targetID)
	if err != nil {
		return nil, err
	}
	var out []*types.Dependency
	for _, d := range ins {
		if depType != "" && string(d.Type) != depType {
			continue
		}
		if afterID != "" && d.ID <= afterID {
			continue
		}
		out = append(out, normalizeDep(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// dependentRecordsForIssues returns the inbound edges of every target, keyed
// by target, de-duplicated by row id.
func dependentRecordsForIssues(r kv, targetIDs []string) (map[string][]*types.Dependency, error) {
	out := make(map[string][]*types.Dependency)
	seen := map[string]bool{}
	for _, id := range targetIDs {
		ins, err := incomingEdges(r, id)
		if err != nil {
			return nil, err
		}
		for _, d := range ins {
			if seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			out[id] = append(out[id], normalizeDep(d))
		}
	}
	return out, nil
}

// countDependentRecords counts the inbound edges of targetID (depType "" =
// all).
func countDependentRecords(r kv, targetID, depType string) (int, error) {
	ins, err := incomingEdges(r, targetID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range ins {
		if depType == "" || string(d.Type) == depType {
			n++
		}
	}
	return n, nil
}

// isBlocked returns the row's stored (transitive) flag and, when set, the
// live blockers behind its own blocking edges rendered as issueops.Blocker
// strings ("<id>" for blocks, "<id> (<type>)" otherwise), in (target, type)
// order. A missing row is not blocked.
func isBlocked(r kv, issueID string) (bool, []string, error) {
	rec, err := getRecord(r, issueID)
	if err != nil {
		if isNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	if !rec.Issue.IsBlocked {
		return false, nil, nil
	}
	outs, err := outgoingEdges(r, issueID)
	if err != nil {
		return false, nil, err
	}
	var names []string
	for _, d := range outs {
		switch d.Type {
		case types.DepBlocks, types.DepWaitsFor, types.DepConditionalBlocks:
		default:
			continue
		}
		target, err := getRecord(r, d.DependsOnID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return false, nil, err
		}
		if !open(target.Issue.Status) {
			continue
		}
		names = append(names, publicops.Blocker{ID: d.DependsOnID, Type: d.Type}.String())
	}
	return true, names, nil
}

// isBlockedBatch returns the stored flag of every id that has a row.
func isBlockedBatch(r kv, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		rec, err := getRecord(r, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out[id] = rec.Issue.IsBlocked
	}
	return out, nil
}

// schedulingGraph returns the adjacency of the scheduling edges (blocks,
// conditional-blocks, parent-child), or of the blocking edges only.
func schedulingGraph(r kv, includeParentChild bool) (map[string][]string, error) {
	all, err := allEdges(r)
	if err != nil {
		return nil, err
	}
	graph := map[string][]string{}
	for _, deps := range all {
		for _, d := range deps {
			switch d.Type {
			case types.DepBlocks, types.DepConditionalBlocks:
			case types.DepParentChild:
				if !includeParentChild {
					continue
				}
			default:
				continue
			}
			graph[d.IssueID] = append(graph[d.IssueID], d.DependsOnID)
		}
	}
	return graph, nil
}

// cycleThroughEdges reports a rendered cycle in the scheduling graph that
// traverses one of the given new edges, or "" when none does.
func cycleThroughEdges(r kv, edges [][2]string) (string, error) {
	graph, err := schedulingGraph(r, true)
	if err != nil {
		return "", err
	}
	return storageissueops.CycleThroughEdgesInGraph(graph, edges), nil
}

// --- issue-hydrating readers ------------------------------------------------------

// anchorEdges returns the far ends of issueID's edges (targets, or sources
// when inbound) with their types, one per far id, in index order.
func anchorEdges(r kv, issueID string, inbound bool) ([]*types.Dependency, error) {
	var edges []*types.Dependency
	var err error
	if inbound {
		edges, err = incomingEdges(r, issueID)
	} else {
		edges, err = outgoingEdges(r, issueID)
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := edges[:0:0]
	for _, d := range edges {
		far := d.DependsOnID
		if inbound {
			far = d.IssueID
		}
		if seen[far] {
			continue
		}
		seen[far] = true
		out = append(out, d)
	}
	return out, nil
}

func anchorIssues(r kv, issueID string, inbound bool) ([]*types.Issue, error) {
	edges, err := anchorEdges(r, issueID, inbound)
	if err != nil {
		return nil, err
	}
	var out []*types.Issue
	for _, d := range edges {
		far := d.DependsOnID
		if inbound {
			far = d.IssueID
		}
		issue, err := loadIssue(r, far)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, issue)
	}
	return out, nil
}

func anchorIssuesWithMetadata(r kv, issueID string, inbound bool) ([]*types.IssueWithDependencyMetadata, error) {
	edges, err := anchorEdges(r, issueID, inbound)
	if err != nil {
		return nil, err
	}
	var out []*types.IssueWithDependencyMetadata
	for _, d := range edges {
		far := d.DependsOnID
		if inbound {
			far = d.IssueID
		}
		issue, err := loadIssue(r, far)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, &types.IssueWithDependencyMetadata{Issue: *issue, DependencyType: d.Type})
	}
	return out, nil
}

// GetDependencies returns the issues issueID depends on.
func (s *Store) GetDependencies(ctx context.Context, issueID string) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		v, err := anchorIssues(r, issueID, false)
		out = v
		return err
	})
	return out, err
}

// GetDependents returns the issues that depend on issueID.
func (s *Store) GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		v, err := anchorIssues(r, issueID, true)
		out = v
		return err
	})
	return out, err
}

// GetDependenciesWithMetadata returns the issues issueID depends on with the
// edge type.
func (s *Store) GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	var out []*types.IssueWithDependencyMetadata
	err := s.read(func(r kv) error {
		v, err := anchorIssuesWithMetadata(r, issueID, false)
		out = v
		return err
	})
	return out, err
}

// GetDependentsWithMetadata returns the issues depending on issueID with the
// edge type.
func (s *Store) GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	var out []*types.IssueWithDependencyMetadata
	err := s.read(func(r kv) error {
		v, err := anchorIssuesWithMetadata(r, issueID, true)
		out = v
		return err
	})
	return out, err
}

// IterDependenciesWithMetadata streams GetDependenciesWithMetadata.
func (s *Store) IterDependenciesWithMetadata(ctx context.Context, issueID string) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	items, err := s.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(items), nil
}

// IterDependentsWithMetadata streams GetDependentsWithMetadata.
func (s *Store) IterDependentsWithMetadata(ctx context.Context, issueID string) (storage.Iter[types.IssueWithDependencyMetadata], error) {
	items, err := s.GetDependentsWithMetadata(ctx, issueID)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(items), nil
}

// GetDependencyTree walks the dependency graph from issueID
// (issueops.GetDependencyTreeInTx): relates-to edges are skipped, an issue
// already expanded appears again only as a Deduped stub, and a path never
// revisits itself. showAllPaths is accepted for the interface and, as on the
// SQL backends, does not change the walk.
func (s *Store) GetDependencyTree(ctx context.Context, issueID string, maxDepth int, showAllPaths bool, reverse bool) ([]*types.TreeNode, error) {
	_ = showAllPaths
	var out []*types.TreeNode
	err := s.read(func(r kv) error {
		v, err := buildDependencyTree(r, issueID, 0, maxDepth, reverse, map[string]*types.Issue{}, map[string]bool{}, "", "")
		out = v
		return err
	})
	return out, err
}

func buildDependencyTree(r kv, issueID string, depth, maxDepth int, reverse bool, expanded map[string]*types.Issue, onPath map[string]bool, parentID string, edge types.DependencyType) ([]*types.TreeNode, error) {
	if depth >= maxDepth || onPath[issueID] {
		return nil, nil
	}
	if seen, ok := expanded[issueID]; ok {
		return []*types.TreeNode{{Issue: *seen, Depth: depth, ParentID: parentID, EdgeFromParent: edge, Deduped: true}}, nil
	}
	issue, err := loadIssue(r, issueID)
	if err != nil {
		return nil, err
	}
	expanded[issueID] = issue
	onPath[issueID] = true
	defer delete(onPath, issueID)
	related, err := anchorIssuesWithMetadata(r, issueID, reverse)
	if err != nil {
		return nil, err
	}
	nodes := []*types.TreeNode{{Issue: *issue, Depth: depth, ParentID: parentID, EdgeFromParent: edge}}
	for _, rel := range related {
		if rel.DependencyType == types.DepRelatesTo {
			continue
		}
		children, err := buildDependencyTree(r, rel.ID, depth+1, maxDepth, reverse, expanded, onPath, issueID, rel.DependencyType)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, children...)
	}
	return nodes, nil
}

// CountDependents returns the number of distinct issues depending on issueID
// (every edge type).
func (s *Store) CountDependents(ctx context.Context, issueID string) (int64, error) {
	var n int64
	err := s.read(func(r kv) error {
		edges, err := anchorEdges(r, issueID, true)
		n = int64(len(edges))
		return err
	})
	return n, err
}

// CountDependencies returns the number of distinct issues issueID depends on
// (every edge type).
func (s *Store) CountDependencies(ctx context.Context, issueID string) (int64, error) {
	var n int64
	err := s.read(func(r kv) error {
		edges, err := anchorEdges(r, issueID, false)
		n = int64(len(edges))
		return err
	})
	return n, err
}

// CountDependentsByStatus counts the inbound edges of issueID whose source
// row is in status, across both planes and every edge type.
func (s *Store) CountDependentsByStatus(ctx context.Context, issueID string, status types.Status) (int64, error) {
	var n int64
	err := s.read(func(r kv) error {
		ins, err := incomingEdges(r, issueID)
		if err != nil {
			return err
		}
		for _, d := range ins {
			src, err := getRecord(r, d.IssueID)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			if src.Issue.Status == status {
				n++
			}
		}
		return nil
	})
	return n, err
}

// --- DependencyQueryStore ----------------------------------------------------------

// GetDependencyRecords returns the raw outgoing edges of issueID.
func (s *Store) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	var out []*types.Dependency
	err := s.read(func(r kv) error {
		v, err := dependencyRecords(r, issueID)
		out = v
		return err
	})
	return out, err
}

// GetDependencyRecordsForIssues returns the raw outgoing edges of every id,
// keyed by source; ids without edges are absent.
func (s *Store) GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error) {
	out := make(map[string][]*types.Dependency)
	err := s.read(func(r kv) error {
		for _, id := range issueIDs {
			if _, done := out[id]; done {
				continue
			}
			deps, err := dependencyRecords(r, id)
			if err != nil {
				return err
			}
			if len(deps) > 0 {
				out[id] = deps
			}
		}
		return nil
	})
	return out, err
}

// GetDependentRecords returns the raw inbound edges of targetID; see
// dependentRecords for the paging contract.
func (s *Store) GetDependentRecords(ctx context.Context, targetID string, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	var out []*types.Dependency
	err := s.read(func(r kv) error {
		v, err := dependentRecords(r, targetID, depType, limit, afterID)
		out = v
		return err
	})
	return out, err
}

// GetDependentRecordsForIssues returns the raw inbound edges of every target,
// keyed by target.
func (s *Store) GetDependentRecordsForIssues(ctx context.Context, targetIDs []string) (map[string][]*types.Dependency, error) {
	var out map[string][]*types.Dependency
	err := s.read(func(r kv) error {
		v, err := dependentRecordsForIssues(r, targetIDs)
		out = v
		return err
	})
	return out, err
}

// CountDependentRecords counts the inbound edges of targetID.
func (s *Store) CountDependentRecords(ctx context.Context, targetID string, depType string) (int, error) {
	var n int
	err := s.read(func(r kv) error {
		v, err := countDependentRecords(r, targetID, depType)
		n = v
		return err
	})
	return n, err
}

// GetAllDependencyRecords returns every edge grouped by source (a non-nil
// map).
func (s *Store) GetAllDependencyRecords(ctx context.Context) (map[string][]*types.Dependency, error) {
	out := make(map[string][]*types.Dependency)
	err := s.read(func(r kv) error {
		all, err := allEdges(r)
		if err != nil {
			return err
		}
		for src, deps := range all {
			for _, d := range deps {
				normalizeDep(d)
			}
			out[src] = deps
		}
		return nil
	})
	return out, err
}

// IterAllDependencyRecords streams every edge, grouped by source in id order.
func (s *Store) IterAllDependencyRecords(ctx context.Context) (storage.Iter[types.Dependency], error) {
	all, err := s.GetAllDependencyRecords(ctx)
	if err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(all))
	for src := range all {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	var flat []*types.Dependency
	for _, src := range sources {
		flat = append(flat, all[src]...)
	}
	return storage.NewSliceIter(flat), nil
}

// GetDependencyCounts returns, for every id, its outgoing and inbound blocks
// edge counts (blocks only, as the SQL counts query does).
func (s *Store) GetDependencyCounts(ctx context.Context, issueIDs []string) (map[string]*types.DependencyCounts, error) {
	out := make(map[string]*types.DependencyCounts, len(issueIDs))
	err := s.read(func(r kv) error {
		for _, id := range issueIDs {
			c := &types.DependencyCounts{}
			out[id] = c
			outs, err := outgoingEdges(r, id)
			if err != nil {
				return err
			}
			for _, d := range outs {
				if d.Type == types.DepBlocks {
					c.DependencyCount++
				}
			}
			ins, err := incomingEdges(r, id)
			if err != nil {
				return err
			}
			for _, d := range ins {
				if d.Type == types.DepBlocks {
					c.DependentCount++
				}
			}
		}
		return nil
	})
	return out, err
}

// GetBlockingInfoForIssues returns, for a set of ids, the open issues each
// is blocked by, the issues each blocks, and each one's parent
// (issueops.GetBlockingInfoForIssuesInTx: a closed end drops out).
func (s *Store) GetBlockingInfoForIssues(ctx context.Context, issueIDs []string) (blockedByMap map[string][]string, blocksMap map[string][]string, parentMap map[string]string, err error) {
	blockedByMap = make(map[string][]string)
	blocksMap = make(map[string][]string)
	parentMap = make(map[string]string)
	err = s.read(func(r kv) error {
		for _, id := range issueIDs {
			outs, err := outgoingEdges(r, id)
			if err != nil {
				return err
			}
			for _, d := range outs {
				if d.Type != types.DepBlocks && d.Type != types.DepParentChild {
					continue
				}
				if closed, err := recordClosed(r, d.DependsOnID); err != nil {
					return err
				} else if closed {
					continue
				}
				if d.Type == types.DepParentChild {
					parentMap[id] = d.DependsOnID
				} else {
					blockedByMap[id] = append(blockedByMap[id], d.DependsOnID)
				}
			}
			if closed, err := recordClosed(r, id); err != nil {
				return err
			} else if closed {
				continue
			}
			ins, err := incomingEdges(r, id)
			if err != nil {
				return err
			}
			for _, d := range ins {
				if d.Type == types.DepBlocks {
					blocksMap[id] = append(blocksMap[id], d.IssueID)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return blockedByMap, blocksMap, parentMap, nil
}

// recordClosed reports whether id has a row in status closed; a missing row
// is not closed, as the SQL status join leaves it unmatched.
func recordClosed(r kv, id string) (bool, error) {
	rec, err := getRecord(r, id)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return rec.Issue.Status == types.StatusClosed, nil
}

// IsBlocked returns the row's stored blocked flag and its live blockers.
func (s *Store) IsBlocked(ctx context.Context, issueID string) (bool, []string, error) {
	var blocked bool
	var blockers []string
	err := s.read(func(r kv) error {
		b, names, err := isBlocked(r, issueID)
		blocked, blockers = b, names
		return err
	})
	if err != nil {
		return false, nil, fmt.Errorf("failed to check blockers: %w", err)
	}
	return blocked, blockers, nil
}

// IsBlockedBatch returns the stored blocked flag of every id that has a row.
func (s *Store) IsBlockedBatch(ctx context.Context, ids []string) (map[string]bool, error) {
	var out map[string]bool
	err := s.read(func(r kv) error {
		v, err := isBlockedBatch(r, ids)
		out = v
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to batch-check blockers: %w", err)
	}
	return out, nil
}

// GetNewlyUnblockedByClose returns the open issues whose only open blocks
// targets would be closedIssueID once it closes.
func (s *Store) GetNewlyUnblockedByClose(ctx context.Context, closedIssueID string) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		ins, err := incomingEdges(r, closedIssueID)
		if err != nil {
			return err
		}
		candidates := map[string]bool{}
		for _, d := range ins {
			if d.Type == types.DepBlocks {
				candidates[d.IssueID] = true
			}
		}
		ids := make([]string, 0, len(candidates))
		for id := range candidates {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rec, err := getRecord(r, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			if !open(rec.Issue.Status) {
				continue
			}
			outs, err := outgoingEdges(r, id)
			if err != nil {
				return err
			}
			stillBlocked := false
			for _, d := range outs {
				if d.Type != types.DepBlocks || d.DependsOnID == closedIssueID {
					continue
				}
				target, err := getRecord(r, d.DependsOnID)
				if err != nil {
					if isNotFound(err) {
						continue
					}
					return err
				}
				if open(target.Issue.Status) {
					stillBlocked = true
					break
				}
			}
			if stillBlocked {
				continue
			}
			issue, err := loadIssue(r, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			out = append(out, issue)
		}
		return nil
	})
	return out, err
}

// DetectCycles finds cycles among blocks and conditional-blocks edges, each
// reported as the issues on its path.
func (s *Store) DetectCycles(ctx context.Context) ([][]*types.Issue, error) {
	var cycles [][]*types.Issue
	err := s.read(func(r kv) error {
		graph, err := schedulingGraph(r, false)
		if err != nil {
			return err
		}
		nodes := make([]string, 0, len(graph))
		for n := range graph {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		visited := map[string]bool{}
		onStack := map[string]bool{}
		var path []string
		var dfs func(node string) error
		dfs = func(node string) error {
			visited[node], onStack[node] = true, true
			path = append(path, node)
			for _, next := range graph[node] {
				if !visited[next] {
					if err := dfs(next); err != nil {
						return err
					}
					continue
				}
				if !onStack[next] {
					continue
				}
				start := -1
				for i, n := range path {
					if n == next {
						start = i
						break
					}
				}
				if start < 0 {
					continue
				}
				var cycle []*types.Issue
				for _, id := range path[start:] {
					issue, err := loadIssue(r, id)
					if err != nil {
						if isNotFound(err) {
							continue
						}
						return err
					}
					cycle = append(cycle, issue)
				}
				if len(cycle) > 0 {
					cycles = append(cycles, cycle)
				}
			}
			path = path[:len(path)-1]
			onStack[node] = false
			return nil
		}
		for _, n := range nodes {
			if !visited[n] {
				if err := dfs(n); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return cycles, err
}

// FindWispDependentsRecursive returns the transitive dependents of ids whose
// edges live on the wisps plane (edges whose source is a wisp), the seeds
// excluded. A nil input yields a nil map; no dependents yields an empty one.
func (s *Store) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	discovered := map[string]bool{}
	err := s.read(func(r kv) error {
		seen := map[string]bool{}
		queue := append([]string(nil), ids...)
		for _, id := range ids {
			seen[id] = true
		}
		for len(queue) > 0 {
			if len(seen) > maxWispDependentResults {
				return fmt.Errorf("wisp cascade traversal discovered over %d issues; aborting", maxWispDependentResults)
			}
			cur := queue[0]
			queue = queue[1:]
			ins, err := incomingEdges(r, cur)
			if err != nil {
				return err
			}
			for _, d := range ins {
				if seen[d.IssueID] {
					continue
				}
				src, err := getRecord(r, d.IssueID)
				if err != nil {
					if isNotFound(err) {
						continue
					}
					return err
				}
				if src.Plane != planeWisps {
					continue
				}
				seen[d.IssueID] = true
				discovered[d.IssueID] = true
				queue = append(queue, d.IssueID)
			}
		}
		return nil
	})
	if err != nil {
		return discovered, err
	}
	return discovered, nil
}
