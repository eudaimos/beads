package pebble

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// The write-side roles below each run as one store transaction: validate
// outside, write through the transaction helpers the flat methods already
// use, settle blocked state, and read back the row the caller gets.

// IssueClaimer serves the single-issue claim.
func (s *Store) IssueClaimer() (publicops.Claimer, error) {
	return &issueClaimer{s: s}, nil
}

type issueClaimer struct{ s *Store }

var _ publicops.Claimer = (*issueClaimer)(nil)

func (c *issueClaimer) Claim(ctx context.Context, request publicops.ClaimRequest) (publicops.ClaimResult, error) {
	if request.Actor == "" || request.IssueID == "" {
		return publicops.ClaimResult{}, fmt.Errorf("%w: claim requires actor and issue ID", storage.ErrValidation)
	}
	var result publicops.ClaimResult
	err := c.s.run(ctx, func(t *txn) error {
		rec, err := getRecord(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		if rec.Plane == planeWisps {
			return fmt.Errorf("%w: issue %s", storage.ErrNotFound, request.IssueID)
		}
		before := rec.Issue
		if err := t.claimIssue(request.IssueID, request.Actor); err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		issue, err := loadIssue(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		issue.Labels, issue.Dependencies, issue.Comments = nil, nil, nil
		result = publicops.ClaimResult{
			Issue:   issue,
			Changed: before.Status != types.StatusInProgress || !storageissueops.ActorMatches(before.Assignee, request.Actor),
		}
		return nil
	})
	if err != nil {
		return publicops.ClaimResult{}, err
	}
	return result, nil
}

// Releaser serves the claim release.
func (s *Store) Releaser() (publicops.Releaser, error) {
	return &releaser{s: s}, nil
}

type releaser struct{ s *Store }

var _ publicops.Releaser = (*releaser)(nil)

func (r *releaser) Release(ctx context.Context, request publicops.ReleaseRequest) (publicops.ReleaseResult, error) {
	if err := workapi.ValidateReleaseRequest(request); err != nil {
		return publicops.ReleaseResult{}, err
	}
	var result publicops.ReleaseResult
	err := r.s.run(ctx, func(t *txn) error {
		rec, err := getRecord(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		status := rec.Issue.Status
		if status != types.StatusOpen && status != types.StatusInProgress {
			return fmt.Errorf("%w: %s has status %q, which is neither %q nor %q",
				publicops.ErrNotReleasable, request.IssueID, status, types.StatusOpen, types.StatusInProgress)
		}
		if request.ExpectedAssignee == nil && rec.Issue.Assignee == "" {
			return fmt.Errorf("%w: %s has no assignee to release", publicops.ErrNotClaimed, request.IssueID)
		}
		if err := t.unclaim(request.IssueID, request.Actor, request.ExpectedAssignee, request.Force); err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		after, err := loadIssue(t.kv, request.IssueID)
		if err != nil {
			return fmt.Errorf("release %s: read back the released row: %w", request.IssueID, err)
		}
		result = publicops.ReleaseResult{Issue: after, Changed: true}
		return nil
	})
	if err != nil {
		return publicops.ReleaseResult{}, err
	}
	return result, nil
}

// MetadataCAS serves the per-key compare-and-set on issue metadata.
func (s *Store) MetadataCAS() (publicops.MetadataCAS, error) {
	return &metadataCAS{s: s}, nil
}

type metadataCAS struct{ s *Store }

var _ publicops.MetadataCAS = (*metadataCAS)(nil)

func (m *metadataCAS) CompareAndSetKey(ctx context.Context, request publicops.CompareAndSetKeyRequest) (publicops.CompareAndSetKeyResult, error) {
	plan, err := storage.PlanCompareAndSetKey(request)
	if err != nil {
		return publicops.CompareAndSetKeyResult{}, err
	}
	var result publicops.CompareAndSetKeyResult
	err = m.s.run(ctx, func(t *txn) error {
		metadata, err := metadataMap(t.kv, plan.IssueID)
		if err != nil {
			return err
		}
		current, err := canonicalMetadataKey(metadata, plan.Key)
		if err != nil {
			return fmt.Errorf("metadata key %q on %s: %w", plan.Key, plan.IssueID, err)
		}
		if !sameCanonicalValue(current, plan.Expected) {
			result = publicops.CompareAndSetKeyResult{Swapped: false, Current: current}
			return nil
		}
		if sameCanonicalValue(current, plan.Value) {
			result = publicops.CompareAndSetKeyResult{Swapped: true, Current: current}
			return nil
		}
		if plan.Value == nil {
			delete(metadata, plan.Key)
		} else {
			metadata[plan.Key] = *plan.Value
		}
		if err := t.writeMetadata(plan.IssueID, metadata, plan.Actor); err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		stored, err := metadataMap(t.kv, plan.IssueID)
		if err != nil {
			return err
		}
		landed, err := canonicalMetadataKey(stored, plan.Key)
		if err != nil {
			return fmt.Errorf("metadata key %q on %s after the swap: %w", plan.Key, plan.IssueID, err)
		}
		result = publicops.CompareAndSetKeyResult{Swapped: true, Current: landed}
		return nil
	})
	if err != nil {
		return publicops.CompareAndSetKeyResult{}, err
	}
	return result, nil
}

// canonicalMetadataKey reads one key in its canonical JSON form, nil when the
// key is absent (a stored null is a non-nil "null").
func canonicalMetadataKey(metadata map[string]json.RawMessage, key string) (*json.RawMessage, error) {
	stored, ok := metadata[key]
	if !ok {
		return nil, nil
	}
	canonical, err := storage.CanonicalMetadataValue(stored)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func sameCanonicalValue(a, b *json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return bytes.Equal(*a, *b)
}

// Deleter serves the guarded multi-issue delete.
func (s *Store) Deleter() (publicops.Deleter, error) {
	return &deleter{s: s}, nil
}

type deleter struct{ s *Store }

var _ publicops.Deleter = (*deleter)(nil)

func (d *deleter) Delete(ctx context.Context, request publicops.DeleteRequest) (publicops.DeleteResult, error) {
	if err := workapi.ValidateDeleteRequest(request); err != nil {
		return publicops.DeleteResult{}, err
	}
	request.IDs = workapi.NormalizeDeleteIDs(request.IDs)
	var result publicops.DeleteResult
	err := d.s.run(ctx, func(t *txn) error {
		var err error
		result, err = t.deleteForRole(request)
		if err != nil || request.DryRun {
			return err
		}
		return t.settleBlocked()
	})
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	return result, nil
}

// deleteForRole is the role's delete: every named id must exist, the version
// guard reads the first id, dependents outside the request refuse unless
// forced or cascaded, and the neighbors of the deleted rows get their text
// references rewritten.
func (t *txn) deleteForRole(req publicops.DeleteRequest) (publicops.DeleteResult, error) {
	ids := req.IDs
	result := publicops.DeleteResult{DryRun: req.DryRun}
	var missing []string
	for _, id := range ids {
		ok, err := recordExists(t.kv, id)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete: resolve ids: %w", err)
		}
		if !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return publicops.DeleteResult{}, &publicops.NotFoundError{IDs: missing}
	}
	if req.ExpectedVersion != nil {
		rec, err := getRecord(t.kv, ids[0])
		if err != nil {
			return publicops.DeleteResult{}, err
		}
		if rec.RowVersion != *req.ExpectedVersion {
			return publicops.DeleteResult{}, &publicops.VersionMismatchError{Expected: *req.ExpectedVersion, Current: rec.RowVersion}
		}
	}
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	if !req.Cascade {
		external, err := externalDependents(t.kv, ids, idSet)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete: check dependents: %w", err)
		}
		if !req.Force {
			for _, id := range ids {
				if deps := external[id]; len(deps) > 0 {
					return publicops.DeleteResult{}, &publicops.DependentsOutsideRequestError{IssueID: id, Dependents: deps}
				}
			}
		} else {
			result.Orphaned = orphanedIDs(external)
		}
	}
	set, err := resolveDeletionSet(t.kv, ids, req.Cascade)
	if err != nil {
		return publicops.DeleteResult{}, fmt.Errorf("delete: %w", err)
	}
	deleting := make(map[string]bool, len(set.all))
	for _, id := range set.all {
		deleting[id] = true
	}
	if req.Cascade {
		external, err := externalDependents(t.kv, set.all, deleting)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete: check dependents: %w", err)
		}
		result.Orphaned = orphanedIDs(external)
	}
	neighbors, err := deleteNeighbors(t.kv, set.all, deleting)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	for _, id := range set.all {
		rec, err := getRecord(t.kv, id)
		if err != nil {
			return publicops.DeleteResult{}, err
		}
		outs, err := outgoingEdges(t.kv, id)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("count dependencies: %w", err)
		}
		result.Dependencies += len(outs)
		ins, err := incomingEdges(t.kv, id)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("count inbound dependencies: %w", err)
		}
		for _, d := range ins {
			if !deleting[d.IssueID] {
				result.Dependencies++
			}
		}
		result.Labels += len(rec.Issue.Labels)
		events, err := eventsOf(t.kv, id, 0)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("count events: %w", err)
		}
		result.Events += len(events)
	}
	result.Deleted = len(set.all)
	result.DeletedIDs = workapi.SortedDeleteIDs(deleting)
	if req.DryRun {
		return result, nil
	}
	for _, id := range set.all {
		if err := t.deleteIssue(id); err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete %s: %w", id, err)
		}
	}
	rewritten, err := t.rewriteDeletedReferences(set.all, neighbors, req.Actor)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	result.ReferencesUpdated = rewritten
	return result, nil
}

// deleteNeighbors hydrates every row that shares an edge with the deletion
// set and survives it, so their text can be rewritten after the delete.
func deleteNeighbors(r kv, ids []string, deleting map[string]bool) ([]*issueRecord, error) {
	neighborIDs := map[string]bool{}
	for _, id := range ids {
		outs, err := outgoingEdges(r, id)
		if err != nil {
			return nil, fmt.Errorf("query neighbors: %w", err)
		}
		ins, err := incomingEdges(r, id)
		if err != nil {
			return nil, fmt.Errorf("query neighbors: %w", err)
		}
		for _, d := range append(outs, ins...) {
			for _, candidate := range [2]string{d.IssueID, d.DependsOnID} {
				if candidate != "" && !deleting[candidate] {
					neighborIDs[candidate] = true
				}
			}
		}
	}
	var out []*issueRecord
	for _, id := range workapi.SortedDeleteIDs(neighborIDs) {
		rec, err := getRecord(r, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("hydrate neighbors: %w", err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// rewriteDeletedReferences replaces mentions of each deleted id in the long
// text fields of the surviving neighbors and reports how many rows changed.
func (t *txn) rewriteDeletedReferences(deletedIDs []string, neighbors []*issueRecord, actor string) (int, error) {
	if len(neighbors) == 0 {
		return 0, nil
	}
	touched := map[string]bool{}
	for _, id := range deletedIDs {
		re := storageissueops.DeletedReferencePattern(id)
		replacement := `$1[deleted:` + id + `]$3`
		for _, neighbor := range neighbors {
			updates := map[string]interface{}{}
			for _, field := range []struct {
				column string
				value  *string
			}{
				{"description", &neighbor.Issue.Description},
				{"notes", &neighbor.Issue.Notes},
				{"design", &neighbor.Issue.Design},
				{"acceptance_criteria", &neighbor.Issue.AcceptanceCriteria},
			} {
				if *field.value == "" || !re.MatchString(*field.value) {
					continue
				}
				rewritten := re.ReplaceAllString(*field.value, replacement)
				updates[field.column] = rewritten
				*field.value = rewritten
			}
			if len(updates) == 0 {
				continue
			}
			if _, err := t.updateIssue(neighbor.Issue.ID, updates, actor, storage.UpdateIssueOptions{}); err != nil {
				return 0, fmt.Errorf("rewrite references in %s: %w", neighbor.Issue.ID, err)
			}
			touched[neighbor.Issue.ID] = true
		}
	}
	return len(touched), nil
}

// DependencyEditor serves the multi-edge add and the single-edge remove.
func (s *Store) DependencyEditor() (publicops.DependencyEditor, error) {
	return &dependencyEditor{s: s}, nil
}

type dependencyEditor struct{ s *Store }

var _ publicops.DependencyEditor = (*dependencyEditor)(nil)

func (e *dependencyEditor) AddDependencies(ctx context.Context, request publicops.AddDependenciesRequest) (publicops.AddDependenciesResult, error) {
	if err := storageissueops.ValidateAddDependenciesRequest(request); err != nil {
		return publicops.AddDependenciesResult{}, err
	}
	err := e.s.run(ctx, func(t *txn) error {
		for phase := 0; phase < 2; phase++ {
			parentPhase := phase == 0
			for _, edge := range request.Edges {
				if (edge.Type == types.DepParentChild) != parentPhase {
					continue
				}
				dep := &types.Dependency{IssueID: edge.IssueID, DependsOnID: edge.DependsOnID, Type: edge.Type}
				opts := storage.DependencyAddOptions{SkipCycleCheck: request.SkipPerEdgeCycleCheck, EmitEvent: true}
				if err := t.addDependency(dep, request.Actor, opts); err != nil {
					return err
				}
			}
		}
		if err := t.checkAddedEdgesForCycles(request.Edges); err != nil {
			return err
		}
		return t.settleBlocked()
	})
	if err != nil {
		return publicops.AddDependenciesResult{}, err
	}
	added := make([]publicops.DependencyEdge, len(request.Edges))
	copy(added, request.Edges)
	return publicops.AddDependenciesResult{Added: added}, nil
}

// checkAddedEdgesForCycles is the end gate over every scheduling edge a
// request landed: the per-edge probe can be skipped, this cannot.
func (t *txn) checkAddedEdgesForCycles(edges []publicops.DependencyEdge) error {
	var pairs [][2]string
	for _, edge := range edges {
		if !types.IsSchedulingEdge(edge.Type) {
			continue
		}
		pairs = append(pairs, [2]string{edge.IssueID, edge.DependsOnID})
	}
	if len(pairs) == 0 {
		return nil
	}
	path, err := cycleThroughEdges(t.kv, pairs)
	if err != nil {
		return fmt.Errorf("final cycle check failed (no edges added): %w", err)
	}
	if path != "" {
		return domain.NewCycleError("dependency cycle would be created: %s (no edges added; run 'bd dep cycles' for analysis)", path)
	}
	return nil
}

func (e *dependencyEditor) RemoveDependency(ctx context.Context, request publicops.RemoveDependencyRequest) (publicops.RemoveDependencyResult, error) {
	if err := storageissueops.ValidateRemoveDependencyRequest(request); err != nil {
		return publicops.RemoveDependencyResult{}, err
	}
	removed := false
	err := e.s.run(ctx, func(t *txn) error {
		existing, err := edgeBetween(t.kv, request.IssueID, request.DependsOnID)
		if err != nil {
			return err
		}
		if existing == nil {
			return nil
		}
		if err := t.removeEdge(request.IssueID, request.DependsOnID, request.Actor, storage.DependencyRemoveOptions{EmitEvent: true}); err != nil {
			return err
		}
		removed = true
		return t.settleBlocked()
	})
	if err != nil {
		return publicops.RemoveDependencyResult{}, err
	}
	return publicops.RemoveDependencyResult{Removed: removed}, nil
}
