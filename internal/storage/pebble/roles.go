package pebble

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// The Lifecycle role is the guarded create/update/close/reopen surface Gas
// City's op journal writes through. The spike implements it over the same
// transaction bodies the raw methods use; the other roles answer
// ErrUnsupported through the generated shell.

// IssueLifecycle returns the guarded issue operations.
func (s *Store) IssueLifecycle() (publicops.Lifecycle, error) {
	return &lifecycle{s: s}, nil
}

type lifecycle struct{ s *Store }

// Create is the create-only guarded create: an occupied id is refused with
// ErrAlreadyExists, a missing id is minted (under the parent as a dotted
// child id when ParentID is set), inline dependencies and the waits-for edge
// are written, and the hydrated row comes back.
func (l *lifecycle) Create(ctx context.Context, request publicops.CreateRequest) (publicops.CreateResult, error) {
	attempt := storageissueops.CloneCreateRequest(request)
	if err := storageissueops.ValidatePublicCreateRequest(attempt); err != nil {
		return publicops.CreateResult{}, err
	}
	var result publicops.CreateResult
	err := l.s.run(ctx, func(t *txn) error {
		prefix, err := getConfig(t.kv, "issue_prefix")
		if err != nil {
			return err
		}
		allowed, err := getConfig(t.kv, "allowed_prefixes")
		if err != nil {
			return err
		}
		statuses, err := customStatuses(t.kv)
		if err != nil {
			return err
		}
		statusNames := make([]string, 0, len(statuses))
		for _, st := range statuses {
			statusNames = append(statusNames, st.Name)
		}
		typeNames, err := customTypes(t.kv)
		if err != nil {
			return err
		}
		attempt, err = storageissueops.PreparePublicCreateRequest(attempt, storageissueops.PublicCreateContext{
			IssuePrefix: prefix, AllowedPrefixes: allowed, CustomStatuses: statusNames, CustomTypes: typeNames,
		})
		if err != nil {
			return err
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
				return err
			}
			issue.ID = id
		}
		if issue.ID != "" && !attempt.ForceIDPrefix && prefix != "" {
			if err := storageissueops.ValidateIssueIDPrefix(issue.ID, prefix, allowed); err != nil {
				return err
			}
		}
		issue.Dependencies = storage.CreatePublicCreateDependencies("", attempt)
		if _, err := t.createIssue(issue, attempt.Actor, storage.BatchCreateOptions{CreateOnly: true, SkipPrefixValidation: attempt.ForceIDPrefix}); err != nil {
			return err
		}
		for _, dep := range issue.Dependencies {
			if dep.IssueID == "" {
				dep.IssueID = issue.ID
			}
			if dep.DependsOnID == "" {
				dep.DependsOnID = issue.ID
			}
		}
		if err := t.persistCreateDependencies([]*types.Issue{issue}, attempt.Actor, storage.BatchCreateOptions{}); err != nil {
			return err
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		hydrated, err := loadIssue(t.kv, issue.ID)
		if err != nil {
			return err
		}
		result.Issue = hydrated
		return nil
	})
	return result, err
}

// lifecycleNextChildID mints the next dotted child id under parent: one past
// the larger of the parent's counter and its existing direct children.
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

// Update applies a guarded patch: version, assignee and status fences, the
// field patch (with merge ops resolved against the stored row), the label
// patch, the parent patch and an optional claim, then returns the row.
func (l *lifecycle) Update(ctx context.Context, request publicops.UpdateRequest) (publicops.UpdateResult, error) {
	attempt := storageissueops.CloneUpdateRequest(request)
	if attempt.Actor == "" || attempt.IssueID == "" {
		return publicops.UpdateResult{}, fmt.Errorf("%w: update requires actor and issue ID", storage.ErrValidation)
	}
	if err := storageissueops.ValidateUpdateRequest(attempt); err != nil {
		return publicops.UpdateResult{}, err
	}
	if err := storageissueops.ValidateMetadataPatch(attempt.Patch.Metadata); err != nil {
		return publicops.UpdateResult{}, err
	}
	var result publicops.UpdateResult
	err := l.s.run(ctx, func(t *txn) error {
		rec, err := getRecord(t.kv, attempt.IssueID)
		if err != nil {
			return err
		}
		if attempt.IssuePlaneOnly && rec.Plane == planeWisps {
			return errNotFound("issue", attempt.IssueID)
		}
		before := rec.toIssue()
		if attempt.ExpectedVersion != nil && *attempt.ExpectedVersion != rec.RowVersion {
			return fmt.Errorf("%w: issue %s is at version %d, expected %d", storage.ErrVersionMismatch, attempt.IssueID, rec.RowVersion, *attempt.ExpectedVersion)
		}
		if attempt.ExpectedAssignee != nil && *attempt.ExpectedAssignee != before.Assignee {
			return fmt.Errorf("%w: issue %s is assigned to %q, expected %q", storage.ErrAssigneeMismatch, attempt.IssueID, before.Assignee, *attempt.ExpectedAssignee)
		}
		if attempt.ExpectedStatus != nil && *attempt.ExpectedStatus != before.Status {
			return fmt.Errorf("%w: issue %s is %q, expected %q", storage.ErrStatusMismatch, attempt.IssueID, before.Status, *attempt.ExpectedStatus)
		}
		if err := storageissueops.AuthorizeTemplateUpdate(before, attempt); err != nil {
			return err
		}
		if err := storageissueops.AuthorizeNotesOverwrite(before, attempt); err != nil {
			return err
		}
		changed := false
		updates := storageissueops.UpdateFields(attempt.Patch)
		if attempt.Claim {
			updates["assignee"] = attempt.Actor
			updates["status"] = string(types.StatusInProgress)
		}
		metadata, metadataChanged, err := storageissueops.ApplyMetadataPatch(before.Metadata, attempt.Patch.Metadata)
		if err != nil {
			return err
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
				return err
			}
			changed = changed || didChange
		}
		for _, label := range attempt.Patch.Labels.Add {
			if err := t.addLabel(attempt.IssueID, label, attempt.Actor); err != nil {
				return err
			}
			changed = true
		}
		for _, label := range attempt.Patch.Labels.Remove {
			if err := t.removeLabel(attempt.IssueID, label, attempt.Actor); err != nil {
				return err
			}
			changed = true
		}
		if attempt.Patch.Labels.Replace.Set {
			current, _ := getRecord(t.kv, attempt.IssueID)
			want := map[string]bool{}
			for _, l := range attempt.Patch.Labels.Replace.Value {
				want[l] = true
			}
			if current != nil {
				for _, l := range current.Issue.Labels {
					if !want[l] {
						if err := t.removeLabel(attempt.IssueID, l, attempt.Actor); err != nil {
							return err
						}
						changed = true
					}
				}
			}
			for l := range want {
				if err := t.addLabel(attempt.IssueID, l, attempt.Actor); err != nil {
					return err
				}
				changed = true
			}
		}
		if attempt.Patch.ParentID.Set {
			if moved, err := t.setParent(attempt.IssueID, attempt.Patch.ParentID.Value, attempt.Actor); err != nil {
				return err
			} else if moved {
				changed = true
			}
		}
		if attempt.Patch.Persistence.Set {
			return l.s.unsupported("Lifecycle.Update.Persistence")
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

// setParent replaces the row's parent-child edge ("" removes it).
func (t *txn) setParent(id, parent, actor string) (bool, error) {
	current, err := parentOf(t.kv, id)
	if err != nil {
		return false, err
	}
	if current == parent {
		return false, nil
	}
	if current != "" {
		if err := deleteEdge(t.kv, id, current, string(types.DepParentChild)); err != nil {
			return false, err
		}
		t.markBlockedDirty(id, current)
	}
	if parent != "" {
		dep := &types.Dependency{IssueID: id, DependsOnID: parent, Type: types.DepParentChild}
		if err := t.addEdge(dep, actor, storage.DependencyAddOptions{}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Close closes the row with the request's reason and session; Force bypasses
// the close policy. An already-closed row reports Changed=false.
func (l *lifecycle) Close(ctx context.Context, request publicops.CloseRequest) (publicops.CloseResult, error) {
	if request.Actor == "" || request.IssueID == "" {
		return publicops.CloseResult{}, fmt.Errorf("%w: close requires actor and issue ID", storage.ErrValidation)
	}
	var result publicops.CloseResult
	err := l.s.run(ctx, func(t *txn) error {
		children, err := childrenOf(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		openChildren := 0
		for _, c := range children {
			if child, err := getRecord(t.kv, c); err == nil && open(child.Issue.Status) {
				openChildren++
			}
		}
		if !request.Force && openChildren > 0 && closePolicyRefusesOpenChildren(t.kv) {
			return fmt.Errorf("%w: %s has %d open children", storage.ErrCloseOpenChildren, request.IssueID, openChildren)
		}
		unchanged, err := t.closeIssue(request.IssueID, request.Reason, request.Actor, request.Session, request.ExpectedVersion)
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
		return nil
	})
	return result, err
}

// Reopen returns a done-category row to open; a row that is not done
// reports Changed=false.
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
