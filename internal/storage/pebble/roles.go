package pebble

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// IssueLifecycle serves the create/update/close/reopen role over the store.
//
// Each verb validates outside the transaction, writes through the
// transaction-level helper the batch applier shares, settles blocked state,
// and hydrates the row it touched.
func (s *Store) IssueLifecycle() (publicops.Lifecycle, error) {
	return &lifecycle{s: s}, nil
}

type lifecycle struct{ s *Store }

var _ publicops.Lifecycle = (*lifecycle)(nil)

func (l *lifecycle) Create(ctx context.Context, request publicops.CreateRequest) (publicops.CreateResult, error) {
	attempt := storageissueops.CloneCreateRequest(request)
	if err := storageissueops.ValidatePublicCreateRequest(attempt); err != nil {
		return publicops.CreateResult{}, err
	}
	var result publicops.CreateResult
	err := l.s.run(ctx, func(t *txn) error {
		id, err := t.lifecycleCreate(attempt)
		if err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		hydrated, err := loadIssue(t.kv, id)
		if err != nil {
			return err
		}
		result.Issue = hydrated
		return nil
	})
	return result, err
}

// lifecycleCreate lands one cloned, validated create request and returns the
// id it minted or honored. The caller settles blocked state and hydrates.
func (t *txn) lifecycleCreate(attempt publicops.CreateRequest) (string, error) {
	prefix, err := getConfig(t.kv, "issue_prefix")
	if err != nil {
		return "", err
	}
	allowed, err := getConfig(t.kv, "allowed_prefixes")
	if err != nil {
		return "", err
	}
	statuses, err := customStatuses(t.kv)
	if err != nil {
		return "", err
	}
	statusNames := make([]string, 0, len(statuses))
	for _, st := range statuses {
		statusNames = append(statusNames, st.Name)
	}
	typeNames, err := customTypes(t.kv)
	if err != nil {
		return "", err
	}
	attempt, err = storageissueops.PreparePublicCreateRequest(attempt, storageissueops.PublicCreateContext{
		IssuePrefix: prefix, AllowedPrefixes: allowed, CustomStatuses: statusNames, CustomTypes: typeNames,
	})
	if err != nil {
		return "", err
	}
	issue := attempt.Issue
	if attempt.InheritLabelsFromParent && attempt.ParentID != "" {
		if parent, err := getRecord(t.kv, attempt.ParentID); err == nil {
			issue.Labels = append(issue.Labels, parent.Issue.Labels...)
		}
	}
	if issue.ID == "" && attempt.ParentID != "" {
		id, err := lifecycleNextChildID(t.kv, attempt.ParentID)
		if err != nil {
			return "", err
		}
		issue.ID = id
	}
	if issue.ID != "" && !attempt.ForceIDPrefix && prefix != "" {
		if err := storageissueops.ValidateIssueIDPrefix(issue.ID, prefix, allowed); err != nil {
			return "", err
		}
	}
	issue.Dependencies = storage.CreatePublicCreateDependencies("", attempt)
	if _, err := t.createIssue(issue, attempt.Actor, storage.BatchCreateOptions{CreateOnly: true, SkipPrefixValidation: attempt.ForceIDPrefix}); err != nil {
		return "", storageissueops.ClassifyPublicCreateError(err)
	}
	// Unlike the bulk create, the role refuses a dependency whose target is
	// missing rather than skipping it.
	for _, dep := range issue.Dependencies {
		if dep.IssueID == "" {
			dep.IssueID = issue.ID
		}
		if dep.DependsOnID == "" {
			dep.DependsOnID = issue.ID
		}
		if err := t.addDependency(dep, attempt.Actor, storage.DependencyAddOptions{}); err != nil {
			return "", storageissueops.ClassifyPublicCreateError(err)
		}
	}
	return issue.ID, nil
}

// lifecycleNextChildID mints the next dotted child id under parent, keeping
// the per-parent counter ahead of any child that already exists.
func lifecycleNextChildID(w kv, parent string) (string, error) {
	if _, err := getRecord(w, parent); err != nil {
		return "", err
	}
	name := "child" + sep + parent
	last, err := readSeq(w, name)
	if err != nil {
		return "", err
	}
	prefix := parent + "."
	err = scanRecords(w, func(rec *issueRecord) (bool, error) {
		id := rec.Issue.ID
		if !strings.HasPrefix(id, prefix) || strings.Contains(id[len(prefix):], ".") {
			return true, nil
		}
		if _, n, ok := storageissueops.ParseHierarchicalID(id); ok && int64(n) > last {
			last = int64(n)
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	next := last + 1
	if err := w.set(seqKey(name), []byte(fmt.Sprint(next))); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%d", parent, next), nil
}

// validateUpdateRequest is the pre-transaction screen every update shares.
func validateUpdateRequest(attempt publicops.UpdateRequest) error {
	if attempt.Actor == "" || attempt.IssueID == "" {
		return fmt.Errorf("%w: update requires actor and issue ID", storage.ErrValidation)
	}
	if err := storageissueops.ValidateUpdateRequest(attempt); err != nil {
		return err
	}
	return storageissueops.ValidateMetadataPatch(attempt.Patch.Metadata)
}

func (l *lifecycle) Update(ctx context.Context, request publicops.UpdateRequest) (publicops.UpdateResult, error) {
	attempt := storageissueops.CloneUpdateRequest(request)
	if err := validateUpdateRequest(attempt); err != nil {
		return publicops.UpdateResult{}, err
	}
	var result publicops.UpdateResult
	err := l.s.run(ctx, func(t *txn) error {
		changed, err := t.lifecycleUpdate(attempt, true)
		if err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		hydrated, err := loadIssue(t.kv, attempt.IssueID)
		if err != nil {
			return err
		}
		result.Issue, result.Changed = hydrated, changed
		return nil
	})
	return result, err
}

// lifecycleUpdate applies one cloned, validated update request the way the
// SQL path does: the fences, the authorizations, the claim first, then the
// field patch, the label set, the parent set and the persistence move, each
// reporting whether it changed the row. guardTemplate false skips the
// template authorization, which the batch applier's metadata splice needs
// because it writes a template row it created in the same request.
func (t *txn) lifecycleUpdate(attempt publicops.UpdateRequest, guardTemplate bool) (bool, error) {
	rec, err := getRecord(t.kv, attempt.IssueID)
	if err != nil {
		return false, err
	}
	if attempt.IssuePlaneOnly && rec.Plane == planeWisps {
		return false, errNotFound("issue", attempt.IssueID)
	}
	before := rec.toIssue()
	if attempt.ExpectedVersion != nil && *attempt.ExpectedVersion != rec.RowVersion {
		return false, &publicops.VersionMismatchError{Expected: *attempt.ExpectedVersion, Current: rec.RowVersion}
	}
	if attempt.ExpectedAssignee != nil && !storageissueops.ActorMatches(before.Assignee, *attempt.ExpectedAssignee) {
		return false, fmt.Errorf("%w: %s is held by %q, expected %q", storage.ErrAssigneeMismatch, attempt.IssueID, before.Assignee, *attempt.ExpectedAssignee)
	}
	if attempt.ExpectedStatus != nil && *attempt.ExpectedStatus != before.Status {
		return false, fmt.Errorf("%w: %s has status %q, expected %q", storage.ErrStatusMismatch, attempt.IssueID, before.Status, *attempt.ExpectedStatus)
	}
	if guardTemplate {
		if err := storageissueops.AuthorizeTemplateUpdate(before, attempt); err != nil {
			return false, err
		}
	}
	if err := storageissueops.AuthorizeAssigneeTransferWithPools(before, attempt, nil); err != nil {
		pools, poolErr := claimPools(t.kv)
		if poolErr != nil {
			return false, poolErr
		}
		if err := storageissueops.AuthorizeAssigneeTransferWithPools(before, attempt, pools); err != nil {
			return false, err
		}
	}
	if err := storageissueops.AuthorizeNotesOverwrite(before, attempt); err != nil {
		return false, err
	}
	changed := false
	if attempt.Claim {
		if err := t.claimIssue(attempt.IssueID, attempt.Actor); err != nil {
			return false, err
		}
		if before.Status != types.StatusInProgress || !storageissueops.ActorMatches(before.Assignee, attempt.Actor) {
			changed = true
		}
	}
	current := before
	if attempt.Claim {
		claimed, err := getRecord(t.kv, attempt.IssueID)
		if err != nil {
			return false, err
		}
		current = claimed.toIssue()
	}
	updates := storageissueops.UpdateFields(attempt.Patch)
	metadata, metadataChanged, err := storageissueops.ApplyMetadataPatch(current.Metadata, attempt.Patch.Metadata)
	if err != nil {
		return false, err
	}
	if metadataChanged {
		updates["metadata"] = metadata
	}
	if attempt.ForceClosePolicy && attempt.Patch.Status.Set {
		updates[storageissueops.OpForceClosePolicy] = true
	}
	if len(updates) > 0 {
		didChange, err := t.updateIssue(attempt.IssueID, updates, attempt.Actor, storage.UpdateIssueOptions{})
		if err != nil {
			return false, err
		}
		changed = changed || didChange
	}
	labelsChanged, err := t.applyLabelPatch(current, attempt.Patch.Labels, attempt.Actor)
	if err != nil {
		return false, err
	}
	changed = changed || labelsChanged
	if attempt.Patch.ParentID.Set {
		moved, err := t.applyParentPatch(attempt.IssueID, attempt.Patch.ParentID.Value, attempt.Actor)
		if err != nil {
			return false, err
		}
		changed = changed || moved
	}
	if attempt.Patch.Persistence.Set {
		moved, err := t.movePersistence(attempt.IssueID, attempt.Patch.Persistence.Value, attempt.Actor)
		if err != nil {
			return false, err
		}
		changed = changed || moved
	}
	return changed, nil
}

// applyLabelPatch brings the row's label set to what the patch asks for and
// reports whether that was a change; a label already held or already absent
// is not touched and writes no event.
func (t *txn) applyLabelPatch(current *types.Issue, patch publicops.LabelPatch, actor string) (bool, error) {
	if !patch.Replace.Set && len(patch.Add) == 0 && len(patch.Remove) == 0 {
		return false, nil
	}
	existing := map[string]bool{}
	for _, label := range current.Labels {
		existing[label] = true
	}
	target := map[string]bool{}
	if !patch.Replace.Set {
		for label := range existing {
			target[label] = true
		}
	}
	for _, label := range patch.Replace.Value {
		if label != "" {
			target[label] = true
		}
	}
	for _, label := range patch.Add {
		if label != "" {
			target[label] = true
		}
	}
	for _, label := range patch.Remove {
		delete(target, label)
	}
	changed := false
	for _, label := range sortedKeys(existing) {
		if !target[label] {
			if err := t.removeLabel(current.ID, label, actor); err != nil {
				return false, err
			}
			changed = true
		}
	}
	for _, label := range sortedKeys(target) {
		if !existing[label] {
			if err := t.addLabel(current.ID, label, actor); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

// applyParentPatch replaces every parent edge of id with the one named (or
// none when parent is empty) and reports whether the parent set changed.
func (t *txn) applyParentPatch(id, parent, actor string) (bool, error) {
	outs, err := outgoingEdges(t.kv, id)
	if err != nil {
		return false, err
	}
	existing := map[string]bool{}
	for _, d := range outs {
		if d.Type == types.DepParentChild {
			existing[d.DependsOnID] = true
		}
	}
	target := map[string]bool{}
	if parent != "" {
		target[parent] = true
	}
	changed := false
	for _, old := range sortedKeys(existing) {
		if target[old] {
			continue
		}
		if err := deleteEdge(t.kv, id, old, string(types.DepParentChild)); err != nil {
			return false, err
		}
		t.markBlockedDirty(id, old)
		changed = true
	}
	if parent != "" && !existing[parent] {
		dep := &types.Dependency{IssueID: id, DependsOnID: parent, Type: types.DepParentChild}
		if err := t.addEdge(dep, actor, storage.DependencyAddOptions{}); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// setParent keeps the single-parent spelling the older callers use.
func (t *txn) setParent(id, parent, actor string) (bool, error) {
	return t.applyParentPatch(id, parent, actor)
}

// movePersistence moves one row to the persistence mode asked for: the plane
// and the ephemeral, no-history and storage-class fields follow the mode, and
// a row already in that state is a no-op.
func (t *txn) movePersistence(id string, mode types.PersistenceMode, actor string) (bool, error) {
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return false, err
	}
	current := rec.toIssue()
	ephemeral, noHistory, storageClass, err := types.NormalizePersistenceMode(*current, mode)
	if err != nil {
		if errors.Is(err, types.ErrUnversionedWispMove) {
			return false, fmt.Errorf("%w: %w", storage.ErrValidation, err)
		}
		return false, err
	}
	sourceWisp := rec.Plane == planeWisps
	targetWisp := ephemeral || noHistory
	if sourceWisp == targetWisp && current.Ephemeral == ephemeral && current.NoHistory == noHistory &&
		current.StorageClass.Normalize() == storageClass.Normalize() {
		return false, nil
	}
	old := *rec
	rec.Issue.Ephemeral, rec.Issue.NoHistory, rec.Issue.StorageClass = ephemeral, noHistory, storageClass.Normalize()
	rec.Plane = planeIssues
	if targetWisp {
		rec.Plane = planeWisps
	}
	if sourceWisp != targetWisp {
		// A row moved between planes is never stored with a stamp
		// (PrepareIssueForInsert); the journal's create seam alone writes one.
		rec.Issue.ArrivalSeq = 0
	}
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := putRecord(t.kv, &old, rec); err != nil {
		return false, fmt.Errorf("move issue persistence: %w", err)
	}
	if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
		return false, err
	}
	t.markBlockedDirty(id)
	return true, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func validateCloseRequest(request publicops.CloseRequest) error {
	if request.Actor == "" || request.IssueID == "" {
		return fmt.Errorf("%w: close requires actor and issue ID", storage.ErrValidation)
	}
	return nil
}

func (l *lifecycle) Close(ctx context.Context, request publicops.CloseRequest) (publicops.CloseResult, error) {
	if err := validateCloseRequest(request); err != nil {
		return publicops.CloseResult{}, err
	}
	var result publicops.CloseResult
	err := l.s.run(ctx, func(t *txn) error {
		unchanged, openChildren, err := t.lifecycleClose(request)
		if err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		hydrated, err := loadIssue(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		result.Issue, result.Changed, result.OpenChildren = hydrated, !unchanged, openChildren
		if request.AutoCloseMolecule {
			autos, err := t.closeCompletedMolecules([]string{request.IssueID}, request.Actor, request.Session)
			if err != nil {
				return err
			}
			result.AutoClosedMolecule, result.MoleculeAutoCloseRefusal = autos[0].Root, autos[0].Refusal
		}
		return nil
	})
	return result, err
}

// lifecycleClose closes one row under the close guards and the close
// policy the SQL path applies: a version guard, the closable screen (template,
// pinned, status, holder), then unless forced a refusal for open children and
// for live blockers. It reports whether the row was already closed and how
// many open children it has.
func (t *txn) lifecycleClose(request publicops.CloseRequest) (unchanged bool, openChildren int, err error) {
	rec, err := getRecord(t.kv, request.IssueID)
	if err != nil {
		return false, 0, err
	}
	if request.ExpectedVersion != nil && *request.ExpectedVersion != rec.RowVersion {
		return false, 0, &publicops.VersionMismatchError{Expected: *request.ExpectedVersion, Current: rec.RowVersion}
	}
	closed := rec.Issue.Status == types.StatusClosed
	if !closed {
		if err := storageissueops.CheckClosable(request.IssueID, rec.toIssue(), request.Actor, request.Force); err != nil {
			return false, 0, err
		}
	}
	openChildren, err = enforceClosePolicy(t.kv, request.IssueID, request.Force, closed)
	if err != nil {
		return false, 0, err
	}
	unchanged, err = t.closeIssue(request.IssueID, request.Reason, request.Actor, request.Session, nil)
	if err != nil {
		return false, 0, err
	}
	return unchanged, openChildren, nil
}

// enforceClosePolicy applies the close policy the SQL path applies to a row
// about to leave the open categories: unless forced, open children in either
// plane refuse, and so do live blockers of a row flagged blocked. It returns
// the open-children count.
func enforceClosePolicy(r kv, id string, force, closed bool) (int, error) {
	children, err := childrenOf(r, id)
	if err != nil {
		return 0, err
	}
	openChildren := 0
	for _, c := range children {
		if child, err := getRecord(r, c); err == nil && child.Issue.Status != types.StatusClosed {
			openChildren++
		}
	}
	if !force && openChildren > 0 {
		return 0, &publicops.CloseOpenChildrenError{IssueID: id, OpenChildren: openChildren}
	}
	if !force && !closed {
		rec, err := getRecord(r, id)
		if err != nil {
			return 0, err
		}
		if rec.Issue.IsBlocked {
			blockers, err := liveBlockers(r, id)
			if err != nil {
				return 0, err
			}
			if len(blockers) > 0 {
				return 0, &publicops.BlockedError{IssueID: id, Blockers: blockers, Err: publicops.ErrCloseBlocked}
			}
		}
	}
	return openChildren, nil
}

// liveBlockers lists the scheduling edges of id whose target is still open.
func liveBlockers(r kv, id string) ([]publicops.Blocker, error) {
	outs, err := outgoingEdges(r, id)
	if err != nil {
		return nil, err
	}
	var blockers []publicops.Blocker
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
			return nil, err
		}
		if !open(target.Issue.Status) {
			continue
		}
		blockers = append(blockers, publicops.Blocker{ID: d.DependsOnID, Type: d.Type})
	}
	return blockers, nil
}

func (l *lifecycle) Reopen(ctx context.Context, request publicops.ReopenRequest) (publicops.ReopenResult, error) {
	if request.Actor == "" || request.IssueID == "" {
		return publicops.ReopenResult{}, fmt.Errorf("%w: reopen requires actor and issue ID", storage.ErrValidation)
	}
	var result publicops.ReopenResult
	err := l.s.run(ctx, func(t *txn) error {
		if request.ExpectedVersion != nil {
			rec, err := getRecord(t.kv, request.IssueID)
			if err != nil {
				return err
			}
			if *request.ExpectedVersion != rec.RowVersion {
				return fmt.Errorf("%w: issue %s is at version %d, expected %d", storage.ErrVersionMismatch, request.IssueID, rec.RowVersion, *request.ExpectedVersion)
			}
		}
		changed, err := t.reopenIssue(request.IssueID, request.Reason, request.Actor)
		if err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		hydrated, err := loadIssue(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		result.Issue, result.Changed = hydrated, changed
		return nil
	})
	return result, err
}
