package pebble

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Bulk issue operations: batch delete, source-repo purge, id rename,
// promotion out of the wisps plane, and child-id reservation.

const maxCascadeResults = 10000

// --- delete ----------------------------------------------------------------------

// deletionSet is issueops.DeletionSet: the ids a delete removes, in both
// planes, after cascade expansion. Only ids with a row are listed.
type deletionSet struct {
	all     []string
	regular []string
	wisps   []string
}

// resolveDeletionSet mirrors issueops.ResolveDeletionSetInTx: the named ids,
// plus under cascade every parent-child descendant of each, transitively.
func resolveDeletionSet(r kv, ids []string, cascade bool) (deletionSet, error) {
	all := append([]string(nil), ids...)
	if cascade {
		closure := map[string]bool{}
		queue := append([]string(nil), ids...)
		for _, id := range ids {
			closure[id] = true
		}
		for len(queue) > 0 {
			if len(closure) > maxCascadeResults {
				return deletionSet{}, fmt.Errorf("cascade traversal discovered over %d issues; aborting to prevent runaway deletion", maxCascadeResults)
			}
			cur := queue[0]
			queue = queue[1:]
			children, err := childrenOf(r, cur)
			if err != nil {
				return deletionSet{}, fmt.Errorf("expand cascade: %w", err)
			}
			for _, c := range children {
				if !closure[c] {
					closure[c] = true
					queue = append(queue, c)
				}
			}
		}
		all = all[:0]
		for id := range closure {
			all = append(all, id)
		}
		sort.Strings(all)
	}
	var set deletionSet
	seen := map[string]bool{}
	for _, id := range all {
		if seen[id] {
			continue
		}
		seen[id] = true
		rec, err := getRecord(r, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return deletionSet{}, err
		}
		set.all = append(set.all, id)
		if rec.Plane == planeWisps {
			set.wisps = append(set.wisps, id)
		} else {
			set.regular = append(set.regular, id)
		}
	}
	return set, nil
}

// externalDependents mirrors issueops.ExternalDependentsBySourceInTx: for
// each id, the sources of its inbound edges that the request (idSet) does
// not name, in ascending order.
func externalDependents(r kv, ids []string, idSet map[string]bool) (map[string][]string, error) {
	out := map[string][]string{}
	for _, id := range ids {
		ins, err := incomingEdges(r, id)
		if err != nil {
			return nil, fmt.Errorf("get dependents: %w", err)
		}
		seen := map[string]bool{}
		for _, d := range ins {
			if idSet[d.IssueID] || seen[d.IssueID] {
				continue
			}
			seen[d.IssueID] = true
			out[id] = append(out[id], d.IssueID)
		}
		sort.Strings(out[id])
	}
	return out, nil
}

func orphanedIDs(external map[string][]string) []string {
	set := map[string]bool{}
	for _, deps := range external {
		for _, id := range deps {
			set[id] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// deleteIssues is the body of DeleteIssues (issueops.DeleteIssuesInTx). The
// result it returns beside a guard refusal carries the dependents that
// refused it.
func (t *txn) deleteIssues(ids []string, cascade, force, dryRun bool) (*types.DeleteIssuesResult, error) {
	set, err := resolveDeletionSet(t.kv, ids, cascade)
	if err != nil {
		return nil, err
	}
	var orphaned []string
	if !cascade {
		// The storage seam's guard is durable-only: a request's wisps are
		// deleted beside its issues without it (dolt/issues.go deleteIssues).
		idSet := make(map[string]bool, len(ids))
		for _, id := range ids {
			idSet[id] = true
		}
		external, err := externalDependents(t.kv, set.regular, idSet)
		if err != nil {
			return nil, err
		}
		if !force {
			for _, id := range set.regular {
				if deps := external[id]; len(deps) > 0 {
					return &types.DeleteIssuesResult{OrphanedIssues: deps},
						&publicops.DependentsOutsideRequestError{IssueID: id, Dependents: deps}
				}
			}
		} else {
			orphaned = orphanedIDs(external)
		}
	} else {
		idSet := make(map[string]bool, len(set.all))
		for _, id := range set.all {
			idSet[id] = true
		}
		external, err := externalDependents(t.kv, set.all, idSet)
		if err != nil {
			return nil, err
		}
		orphaned = orphanedIDs(external)
	}

	result := &types.DeleteIssuesResult{DeletedCount: len(set.all), OrphanedIssues: orphaned}
	if len(set.all) == 0 {
		return result, nil
	}
	deleting := make(map[string]bool, len(set.all))
	for _, id := range set.all {
		deleting[id] = true
	}
	for _, id := range set.all {
		rec, err := getRecord(t.kv, id)
		if err != nil {
			return nil, err
		}
		outs, err := outgoingEdges(t.kv, id)
		if err != nil {
			return nil, fmt.Errorf("count dependencies: %w", err)
		}
		result.DependenciesCount += len(outs)
		ins, err := incomingEdges(t.kv, id)
		if err != nil {
			return nil, fmt.Errorf("count inbound dependencies: %w", err)
		}
		for _, d := range ins {
			if !deleting[d.IssueID] {
				result.DependenciesCount++
			}
		}
		result.LabelsCount += len(rec.Issue.Labels)
		events, err := eventsOf(t.kv, id, 0)
		if err != nil {
			return nil, fmt.Errorf("count events: %w", err)
		}
		result.EventsCount += len(events)
	}
	if dryRun {
		return result, nil
	}
	// The storage surface's bulk delete carries no actor (the SQL seam's
	// DeleteIssuesInTx passes ""), and every edge the set touches is journaled
	// before any row goes.
	if err := t.journalEdgeRemovalsOf(set.all, ""); err != nil {
		return nil, err
	}
	for _, id := range set.all {
		if err := t.deleteIssue(id, ""); err != nil {
			return nil, fmt.Errorf("delete %s: %w", id, err)
		}
	}
	return result, nil
}

// DeleteIssues deletes ids: without cascade or force a row with a dependent
// outside the request is refused (DependentsOutsideRequestError, the result
// naming the dependents); force deletes it and reports them as orphaned;
// cascade also removes every parent-child descendant. dryRun only counts.
func (s *Store) DeleteIssues(ctx context.Context, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	if len(ids) == 0 {
		return &types.DeleteIssuesResult{}, nil
	}
	var result *types.DeleteIssuesResult
	err := s.run(ctx, func(t *txn) error {
		r, err := t.deleteIssues(ids, cascade, force, dryRun)
		result = r
		if err != nil || dryRun {
			return err
		}
		return t.settleBlocked()
	})
	return result, err
}

// DeleteIssuesBySourceRepo deletes every durable issue whose source_repo is
// sourceRepo ("" matches rows created without one); wisps are untouched.
func (s *Store) DeleteIssuesBySourceRepo(ctx context.Context, sourceRepo string) (int, error) {
	count := 0
	err := s.run(ctx, func(t *txn) error {
		var ids []string
		if err := scanRecords(t.kv, func(rec *issueRecord) (bool, error) {
			if rec.Plane != planeWisps && rec.SourceRepo == sourceRepo {
				ids = append(ids, rec.Issue.ID)
			}
			return true, nil
		}); err != nil {
			return fmt.Errorf("query issues: %w", err)
		}
		if err := t.journalEdgeRemovalsOf(ids, ""); err != nil {
			return err
		}
		for _, id := range ids {
			if err := t.deleteIssue(id, ""); err != nil {
				return fmt.Errorf("delete %s: %w", id, err)
			}
			count++
		}
		return t.settleBlocked()
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// --- rename ----------------------------------------------------------------------

// UpdateIssueID renames oldID to newID (issueops.UpdateIssueIDInTx): the row
// moves with issue's title, description, design, acceptance criteria and
// notes (every other column is kept), and every edge, label, comment, event,
// lease and child counter follows it; a "renamed" event is recorded under
// the new id. An occupied newID is refused.
func (s *Store) UpdateIssueID(ctx context.Context, oldID, newID string, issue *types.Issue, actor string) error {
	if issue == nil {
		return fmt.Errorf("%w: issue must not be nil", storage.ErrValidation)
	}
	if newID == "" {
		return fmt.Errorf("%w: new issue id must not be empty", storage.ErrValidation)
	}
	if err := validateID(newID); err != nil {
		return err
	}
	return s.run(ctx, func(t *txn) error {
		if err := t.renameIssue(oldID, newID, issue, actor); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

func (t *txn) renameIssue(oldID, newID string, issue *types.Issue, actor string) error {
	w := t.kv
	old, err := getRecord(w, oldID)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("issue not found: %s: %w", oldID, err)
		}
		return err
	}
	if oldID == newID {
		return fmt.Errorf("%w: issue %s already has that id", storage.ErrValidation, oldID)
	}
	if taken, err := recordExists(w, newID); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: cannot rename %s to %s", storage.ErrAlreadyExists, oldID, newID)
	}

	// The journal sees a rename as the SQL seam records it: every edge of
	// the old row removed, the old row deleted, the new row created (keeping
	// the row's arrival stamp), and every edge added back under the new id.
	if err := t.journalEdgeRemovalsOf([]string{oldID}, actor); err != nil {
		return err
	}

	// The row and its indexes.
	rec := *old
	rec.Issue.ID = newID
	rec.Issue.Title = issue.Title
	rec.Issue.Description = issue.Description
	rec.Issue.Design = issue.Design
	rec.Issue.AcceptanceCriteria = issue.AcceptanceCriteria
	rec.Issue.Notes = issue.Notes
	rec.Issue.UpdatedAt = nowUTC()
	rec.Issue.Labels = append([]string(nil), old.Issue.Labels...)
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := w.del(issueKey(oldID)); err != nil {
		return err
	}
	if err := dropRowIndexes(w, old); err != nil {
		return err
	}
	if err := putRecord(w, nil, &rec); err != nil {
		return fmt.Errorf("update issue ID: %w", err)
	}

	// Edges in both directions, with their derived ids recomputed.
	outs, err := outgoingEdges(w, oldID)
	if err != nil {
		return err
	}
	ins, err := incomingEdges(w, oldID)
	if err != nil {
		return err
	}
	for _, d := range outs {
		if err := deleteEdge(w, d.IssueID, d.DependsOnID, string(d.Type)); err != nil {
			return err
		}
		d.IssueID = newID
		d.ID = dependencyID(d)
		if err := putEdge(w, d); err != nil {
			return err
		}
	}
	for _, d := range ins {
		if err := deleteEdge(w, d.IssueID, d.DependsOnID, string(d.Type)); err != nil {
			return err
		}
		d.DependsOnID = newID
		d.ID = dependencyID(d)
		if err := putEdge(w, d); err != nil {
			return err
		}
		t.markBlockedDirty(d.IssueID)
	}

	// Comments and events keep their ids and stamps under the new key.
	if err := rekeyRows(w, nsComment, oldID, newID, func(v []byte) ([]byte, error) {
		c, err := decodeJSON[types.Comment](v)
		if err != nil {
			return nil, err
		}
		c.IssueID = newID
		return encodeJSON(c)
	}); err != nil {
		return fmt.Errorf("rename comments: %w", err)
	}
	if err := rekeyRows(w, nsEvent, oldID, newID, func(v []byte) ([]byte, error) {
		ev, err := decodeJSON[types.Event](v)
		if err != nil {
			return nil, err
		}
		ev.IssueID = newID
		return encodeJSON(ev)
	}); err != nil {
		return fmt.Errorf("rename events: %w", err)
	}
	if err := rekeyGlobalEvents(w, oldID, newID); err != nil {
		return fmt.Errorf("rename event index: %w", err)
	}

	// A live lease and the child counter follow the issue.
	if l, err := getLease(w, oldID); err != nil {
		return err
	} else if l != nil {
		if err := putLease(w, newID, l); err != nil {
			return fmt.Errorf("rename lease row: %w", err)
		}
		if err := w.del(leaseKey(oldID)); err != nil {
			return err
		}
	}
	if n, err := readSeq(w, "child"+sep+oldID); err != nil {
		return err
	} else if n > 0 {
		if err := w.set(seqKey("child"+sep+newID), []byte(fmt.Sprint(n))); err != nil {
			return err
		}
		if err := w.del(seqKey("child" + sep + oldID)); err != nil {
			return err
		}
	}

	if err := t.recordEvent(&rec, types.EventType("renamed"), actor, strPtr(oldID), strPtr(newID), nil); err != nil {
		return err
	}
	if err := t.journalDelete(oldID, actor); err != nil {
		return err
	}
	if err := t.journalIssue(storageissueops.EventCreate, newID, actor); err != nil {
		return err
	}
	if t.journal {
		outs, err := outgoingEdges(w, newID)
		if err != nil {
			return err
		}
		ins, err := incomingEdges(w, newID)
		if err != nil {
			return err
		}
		for _, d := range append(outs, ins...) {
			if err := t.journalDep(storageissueops.EventDepAdd, d, actor); err != nil {
				return err
			}
		}
	}
	t.markBlockedDirty(newID)
	return nil
}

// rekeyRows moves every "<ns><oldID><sep>..." row to the same key under
// newID, rewriting its value with fn. Keys are collected before any write.
func rekeyRows(w kv, ns, oldID, newID string, fn func(v []byte) ([]byte, error)) error {
	type row struct{ key, value []byte }
	var rows []row
	prefix := []byte(ns + oldID + sep)
	if err := w.scan(prefix, func(k, v []byte) (bool, error) {
		rows = append(rows, row{append([]byte(nil), k...), append([]byte(nil), v...)})
		return true, nil
	}); err != nil {
		return err
	}
	for _, r := range rows {
		value, err := fn(r.value)
		if err != nil {
			return err
		}
		rest := strings.TrimPrefix(string(r.key), string(prefix))
		if err := w.set([]byte(ns+newID+sep+rest), value); err != nil {
			return err
		}
		if err := w.del(r.key); err != nil {
			return err
		}
	}
	return nil
}

// rekeyGlobalEvents rewrites the events-by-time index entries that point at
// oldID's events.
func rekeyGlobalEvents(w kv, oldID, newID string) error {
	type row struct{ key, value []byte }
	var rows []row
	prefix := oldID + sep
	if err := w.scan([]byte(nsGlobalEv), func(k, v []byte) (bool, error) {
		if strings.HasPrefix(string(v), prefix) {
			rows = append(rows, row{append([]byte(nil), k...), append([]byte(nil), v...)})
		}
		return true, nil
	}); err != nil {
		return err
	}
	for _, r := range rows {
		rest := strings.TrimPrefix(string(r.value), prefix)
		if err := w.set(r.key, []byte(newID+sep+rest)); err != nil {
			return err
		}
	}
	return nil
}

// --- promote ---------------------------------------------------------------------

// PromoteFromEphemeral moves a wisps-plane row onto the issues plane
// (issueops.PromoteFromEphemeralInTx): both wisp-plane flags clear, an
// explicit ephemeral storage class clears, and the row keeps its id, labels,
// comments, events and edges in both directions. A row that is not a wisp is
// refused.
func (s *Store) PromoteFromEphemeral(ctx context.Context, id string, actor string) error {
	return s.run(ctx, func(t *txn) error {
		rec, err := getRecord(t.kv, id)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("wisp %s not found: %w", id, err)
			}
			return err
		}
		if rec.Plane != planeWisps {
			return fmt.Errorf("%w: wisp %s not found", storage.ErrNotFound, id)
		}
		old := *rec
		rec.Plane = planeIssues
		rec.Issue.Ephemeral = false
		rec.Issue.NoHistory = false
		if rec.Issue.StorageClass == types.StorageClassEphemeral {
			rec.Issue.StorageClass = ""
		}
		// The arrival stamp is defined on the wisp plane only (journal.go).
		rec.Issue.ArrivalSeq = 0
		rec.Issue.UpdatedAt = nowUTC()
		rec.ContentHash = rec.Issue.ComputeContentHash()
		rec.RowVersion++
		if err := putRecord(t.kv, &old, rec); err != nil {
			return fmt.Errorf("promote wisp to issues: %w", err)
		}
		if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
			return err
		}
		t.markBlockedDirty(id)
		return t.settleBlocked()
	})
}

// --- child ids -------------------------------------------------------------------

// GetNextChildID reserves the next dotted child id of parentID
// (issueops.GetNextChildIDTx): one past the greater of the parent's counter
// and its highest existing direct child, with the counter advanced to it.
// The parent must exist.
func (s *Store) GetNextChildID(ctx context.Context, parentID string) (string, error) {
	var childID string
	err := s.run(ctx, func(t *txn) error {
		if _, err := getRecord(t.kv, parentID); err != nil {
			return fmt.Errorf("get next child ID: %w", err)
		}
		name := "child" + sep + parentID
		last, err := readSeq(t.kv, name)
		if err != nil {
			return fmt.Errorf("get next child ID: read counter: %w", err)
		}
		lastChild := int(last)
		if err := t.kv.scan([]byte(nsIssue+parentID+"."), func(k, _ []byte) (bool, error) {
			id := strings.TrimPrefix(string(k), nsIssue)
			if parent, n, ok := storageissueops.ParseHierarchicalID(id); ok && parent == parentID && n > lastChild {
				lastChild = n
			}
			return true, nil
		}); err != nil {
			return fmt.Errorf("get next child ID: query existing children: %w", err)
		}
		next := lastChild + 1
		if err := t.kv.set(seqKey(name), []byte(fmt.Sprint(next))); err != nil {
			return fmt.Errorf("get next child ID: update counter: %w", err)
		}
		childID = fmt.Sprintf("%s.%d", parentID, next)
		return nil
	})
	return childID, err
}
