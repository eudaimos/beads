package pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Claim, lease, heartbeat and reclaim: the in-process form of
// issueops.ClaimIssueInTx, RenewLeaseInTx and ReclaimExpiredLeasesInTx. A
// lease is a node-local record under "L/<id>" that exists exactly while its
// issue is a live claim (in_progress, held by the lease's holder); every path
// that ends a claim deletes it. Wisps are never leased.

// --- claim ------------------------------------------------------------------

// claimableStatuses mirrors issueops.ClaimableSourceStatusesInTx: open plus
// every custom status of category active.
func claimableStatuses(r kv) ([]string, error) {
	out := []string{string(types.StatusOpen)}
	customs, err := customStatuses(r)
	if err != nil {
		return nil, err
	}
	for _, c := range customs {
		if c.Category == types.CategoryActive {
			out = append(out, c.Name)
		}
	}
	return out, nil
}

// claimPools mirrors issueops.ClaimPoolAliasesInTx: the claim.pools aliases
// any actor may claim from.
func claimPools(r kv) ([]string, error) {
	raw, err := getConfig(r, "claim.pools")
	if err != nil {
		return nil, err
	}
	return storageissueops.ParseClaimPools(raw), nil
}

// claimIssue is the body of ClaimIssue (issueops.claimIssueInTx): a
// compare-and-set that takes the row when it is in a claimable status and its
// assignee slot is free, the actor's own, or a pool alias. A re-claim of an
// in_progress row by its holder is an idempotent success that writes nothing.
// Every refusal is a *issueops.ClaimConflictError carrying the state that
// refused it and wrapping ErrAlreadyClaimed or ErrNotClaimable.
func (t *txn) claimIssue(id, actor string) error {
	if err := types.CheckFieldLen("actor", actor); err != nil {
		return err
	}
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return fmt.Errorf("failed to get issue for claim: %w", err)
	}
	oldIssue := rec.toIssue()
	if err := projectLease(t.kv, oldIssue); err != nil {
		return err
	}
	statuses, err := claimableStatuses(t.kv)
	if err != nil {
		return fmt.Errorf("failed to resolve claimable statuses: %w", err)
	}
	pools, err := claimPools(t.kv)
	if err != nil {
		return fmt.Errorf("failed to resolve claim pools: %w", err)
	}
	assignee, status := rec.Issue.Assignee, rec.Issue.Status
	assigneeOK := assignee == "" || storageissueops.ActorMatches(assignee, actor) || slices.Contains(pools, assignee)
	statusOK := slices.Contains(statuses, string(status))

	if !assigneeOK || !statusOK {
		if storageissueops.ActorMatches(assignee, actor) && status == types.StatusInProgress {
			return nil // idempotent re-claim
		}
		refusal := fmt.Errorf("%w%s%s", storage.ErrNotClaimable, storage.NotClaimableStatusFragment, status)
		if assignee != "" && !storageissueops.ActorMatches(assignee, actor) {
			switch {
			case slices.Contains(pools, assignee):
				// A pool-held row refused for its status: the status is the reason.
			case status == types.StatusOpen:
				refusal = fmt.Errorf("%w: already assigned to %q — coordinate with the holder; if their claim is abandoned (crashed agent), lease expiry will surface it for bd reclaim", storage.ErrAlreadyClaimed, assignee)
			default:
				refusal = fmt.Errorf("%w%s%s", storage.ErrAlreadyClaimed, storage.ClaimedByFragment, assignee)
			}
		}
		return &publicops.ClaimConflictError{IssueID: id, Assignee: assignee, Status: status, Err: refusal}
	}

	now := nowUTC()
	old := *rec
	rec.Issue.Assignee = actor
	rec.Issue.Status = types.StatusInProgress
	rec.Issue.UpdatedAt = now
	if rec.Issue.StartedAt == nil {
		rec.Issue.StartedAt = &now
	}
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := putRecord(t.kv, &old, rec); err != nil {
		return fmt.Errorf("failed to claim issue: %w", err)
	}
	if rec.Plane != planeWisps {
		if err := grantLease(t.ctx, t.kv, id, actor, now); err != nil {
			return err
		}
	}
	oldData, _ := json.Marshal(oldIssue)
	newData, _ := json.Marshal(map[string]interface{}{"assignee": actor, "status": "in_progress"})
	if err := t.recordEvent(rec, types.EventClaimed, actor, strPtr(string(oldData)), strPtr(string(newData)), nil); err != nil {
		return fmt.Errorf("failed to record claim event: %w", err)
	}
	if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
		return err
	}
	t.markBlockedDirty(id)
	return nil
}

// grantLease mirrors issueops.UpsertLeaseInTx: a fresh lease for holder with
// the context's TTL, stamped with this node's identity.
func grantLease(ctx context.Context, w kv, id, holder string, now time.Time) error {
	// The lease columns are DATETIME(0) on the SQL backends; the stamps are
	// kept at the same precision so a heartbeat reports the stored value.
	now = now.UTC().Truncate(time.Second)
	l := &lease{
		Holder:      holder,
		GrantedAt:   now,
		ExpiresAt:   now.Add(storageissueops.LeaseTTL(ctx)),
		HeartbeatAt: now,
		Node:        storageissueops.NodeID(ctx),
	}
	if err := putLease(w, id, l); err != nil {
		return fmt.Errorf("upsert lease for %s: %w", id, err)
	}
	return nil
}

// ClaimIssue atomically claims an issue for actor: assignee, in_progress,
// started_at (once) and a fresh lease. See claimIssue for the refusals.
func (s *Store) ClaimIssue(ctx context.Context, id string, actor string) error {
	return s.run(ctx, func(t *txn) error {
		if err := t.claimIssue(id, actor); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// ClaimReadyIssue claims the first ready issue matching filter, in the same
// transaction that computes readiness, skipping rows a racing claim took. It
// returns nil when nothing matching is claimable.
func (s *Store) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error) {
	var claimed *types.Issue
	err := s.run(ctx, func(t *txn) error {
		rec, err := t.claimNext(filter, actor)
		if err != nil || rec == nil {
			return err
		}
		got, err := loadIssue(t.kv, rec.Issue.ID)
		if err != nil {
			return fmt.Errorf("get claimed issue: %w", err)
		}
		claimed = got
		return nil
	})
	return claimed, err
}

func claimCandidates(r kv, filter types.WorkFilter) ([]*issueRecord, error) {
	excludedTypes := map[types.IssueType]bool{}
	if filter.Type == "" {
		for _, t := range sqlbuild.ReadyWorkExcludeTypes(filter.ExcludeTypes) {
			excludedTypes[t] = true
		}
	}
	excludedIDs := map[string]bool{}
	for _, id := range filter.ExcludeIDs {
		excludedIDs[id] = true
	}
	var labelRe *regexp.Regexp
	if filter.LabelRegex != "" {
		re, err := regexp.Compile(filter.LabelRegex)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid label regex %q: %v", storage.ErrValidation, filter.LabelRegex, err)
		}
		labelRe = re
	}
	var descendants map[string]bool
	if filter.ParentID != nil {
		d, err := parentChildDescendants(r, *filter.ParentID)
		if err != nil {
			return nil, err
		}
		descendants = d
	}
	now := nowUTC()
	var out []*issueRecord
	err := scanRecords(r, func(rec *issueRecord) (bool, error) {
		i := &rec.Issue
		switch {
		case filter.Status != "":
			if i.Status != filter.Status {
				return true, nil
			}
		case len(filter.Statuses) > 0:
			if !slices.Contains(filter.Statuses, i.Status) {
				return true, nil
			}
		default:
			if i.Status != types.StatusOpen && i.Status != types.StatusInProgress {
				return true, nil
			}
		}
		if i.Pinned || i.IsBlocked || excludedIDs[i.ID] {
			return true, nil
		}
		if !filter.IncludeEphemeral && i.Ephemeral {
			return true, nil
		}
		if filter.Priority != nil && i.Priority != *filter.Priority {
			return true, nil
		}
		if filter.Type != "" {
			if string(i.IssueType) != filter.Type {
				return true, nil
			}
		} else if excludedTypes[i.IssueType] {
			return true, nil
		}
		if filter.Unassigned {
			if i.Assignee != "" {
				return true, nil
			}
		} else if filter.Assignee != nil && i.Assignee != *filter.Assignee {
			return true, nil
		}
		if !filter.IncludeDeferred && i.DeferUntil != nil && i.DeferUntil.After(now) {
			return true, nil
		}
		if !labelsMatch(i.Labels, filter.Labels, filter.LabelsAny, filter.ExcludeLabels) {
			return true, nil
		}
		if filter.LabelPattern != "" && !anyLabel(i.Labels, func(l string) bool {
			ok, _ := path.Match(filter.LabelPattern, l)
			return ok
		}) {
			return true, nil
		}
		if labelRe != nil && !anyLabel(i.Labels, labelRe.MatchString) {
			return true, nil
		}
		if filter.ParentID != nil && !descendants[i.ID] {
			return true, nil
		}
		if filter.MoleculeID != "" {
			child, err := isDirectChild(r, i.ID, filter.MoleculeID)
			if err != nil {
				return false, err
			}
			if !child {
				return true, nil
			}
		}
		if filter.HasMetadataKey != "" || len(filter.MetadataFields) > 0 {
			if !claimMetadataMatches(i.Metadata, filter.HasMetadataKey, filter.MetadataFields) {
				return true, nil
			}
		}
		if filter.MolType != nil && i.MolType != *filter.MolType {
			return true, nil
		}
		if filter.WispType != nil && i.WispType != *filter.WispType {
			return true, nil
		}
		out = append(out, rec)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	stableSortBy(out, func(a, b *issueRecord) bool {
		if a.Issue.Priority != b.Issue.Priority {
			return a.Issue.Priority < b.Issue.Priority
		}
		if !a.Issue.CreatedAt.Equal(b.Issue.CreatedAt) {
			return a.Issue.CreatedAt.Before(b.Issue.CreatedAt)
		}
		return a.Issue.ID < b.Issue.ID
	})
	return out, nil
}

// labelsMatch applies the three label sets: every label of all, at least one
// of any (when given), and none of exclude.
func labelsMatch(labels, all, any, exclude []string) bool {
	has := map[string]bool{}
	for _, l := range labels {
		has[l] = true
	}
	for _, l := range all {
		if l != "" && !has[l] {
			return false
		}
	}
	if len(any) > 0 {
		matched := false
		for _, l := range any {
			if has[l] {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, l := range exclude {
		if has[l] {
			return false
		}
	}
	return true
}

func anyLabel(labels []string, pred func(string) bool) bool {
	for _, l := range labels {
		if pred(l) {
			return true
		}
	}
	return false
}

// claimMetadataMatches evaluates the HasMetadataKey / MetadataFields predicates
// over the row's top-level metadata keys.
func claimMetadataMatches(raw json.RawMessage, hasKey string, fields map[string]string) bool {
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return false
		}
	}
	if hasKey != "" {
		if v, ok := m[hasKey]; !ok || v == nil {
			return false
		}
	}
	for k, want := range fields {
		v, ok := m[k]
		if !ok {
			return false
		}
		var got string
		switch x := v.(type) {
		case string:
			got = x
		default:
			b, _ := json.Marshal(x)
			got = string(b)
		}
		if got != want {
			return false
		}
	}
	return true
}

// parentChildDescendants returns every transitive parent-child descendant
// of parentID, plus the rows whose dotted id sits under it and carry no
// parent-child edge of their own (sqlbuild.readyParentClause).
func parentChildDescendants(r kv, parentID string) (map[string]bool, error) {
	out := map[string]bool{}
	queue := []string{parentID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		children, err := childrenOf(r, cur)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			if !out[c] {
				out[c] = true
				queue = append(queue, c)
			}
		}
	}
	err := r.scan([]byte(nsIssue+parentID+"."), func(k, _ []byte) (bool, error) {
		id := strings.TrimPrefix(string(k), nsIssue)
		if out[id] {
			return true, nil
		}
		p, err := parentOf(r, id)
		if err != nil {
			return false, err
		}
		if p == "" {
			out[id] = true
		}
		return true, nil
	})
	return out, err
}

// isDirectChild reports whether id is a direct child of parentID: by
// parent-child edge, or by dotted id with no parent-child edge.
func isDirectChild(r kv, id, parentID string) (bool, error) {
	p, err := parentOf(r, id)
	if err != nil {
		return false, err
	}
	if p != "" {
		return p == parentID, nil
	}
	return strings.HasPrefix(id, parentID+"."), nil
}

// --- heartbeat --------------------------------------------------------------

// HeartbeatIssue refreshes the lease actor holds on an in_progress issue
// (issueops.RenewLeaseInTx). A heartbeat on a wisp is ErrNotClaimable. A
// refusal that read the row commits the orphan-lease cleanup it may carry and
// is returned as a *issueops.LeaseRenewalRefusal.
func (s *Store) HeartbeatIssue(ctx context.Context, id, actor string) error {
	var refusal *storageissueops.LeaseRenewalRefusal
	err := s.run(ctx, func(t *txn) error {
		rec, err := getRecord(t.kv, id)
		if err != nil {
			if isNotFound(err) {
				return &storageissueops.LeaseRenewalRefusal{
					IssueID: id, ReadErr: err,
					Err: fmt.Errorf("%w: %s", storage.ErrNotClaimable, id),
				}
			}
			return err
		}
		if rec.Plane == planeWisps {
			return fmt.Errorf("%w: %s is ephemeral", storage.ErrNotClaimable, id)
		}
		_, refusal, err = t.renewLease(rec, actor)
		return err
	})
	if err != nil {
		return err
	}
	if refusal != nil {
		return refusal
	}
	return nil
}

// leaseRenewal reports what a renewal wrote: the stored stamps, and whether
// it granted a fresh lease (a re-arm) rather than refreshing the caller's.
type leaseRenewal struct {
	HeartbeatAt time.Time
	ExpiresAt   time.Time
	Rearmed     bool
}

// renewLease is the body of HeartbeatIssue and the Heartbeater role for a
// durable row. It returns the renewal it wrote, or a refusal to be reported
// after commit (the orphan delete it may carry is kept), or an error that
// discards the transaction.
func (t *txn) renewLease(rec *issueRecord, actor string) (leaseRenewal, *storageissueops.LeaseRenewalRefusal, error) {
	id := rec.Issue.ID
	// Lease stamps are whole seconds (the SQL columns are DATETIME(0)); the
	// row's own updated_at keeps the store's precision.
	wall := nowUTC()
	now := wall.Truncate(time.Second)
	ttl := storageissueops.LeaseTTL(t.ctx)
	node := storageissueops.NodeID(t.ctx)
	l, err := getLease(t.kv, id)
	if err != nil {
		return leaseRenewal{}, nil, fmt.Errorf("read lease for %s: %w", id, err)
	}
	refreshed := leaseRenewal{HeartbeatAt: now, ExpiresAt: now.Add(ttl)}
	live := rec.Issue.Status == types.StatusInProgress && rec.Issue.Assignee == actor
	if l != nil && l.Holder == actor && live {
		// Fast path: the caller's own row on a live claim.
		l.ExpiresAt, l.HeartbeatAt = now.Add(ttl), now
		if l.Node == "" {
			l.Node = node
		}
		return refreshed, nil, putLease(t.kv, id, l)
	}
	assignee, status := rec.Issue.Assignee, string(rec.Issue.Status)
	refused := func(reason error) (leaseRenewal, *storageissueops.LeaseRenewalRefusal, error) {
		refusal := &storageissueops.LeaseRenewalRefusal{IssueID: id, Found: true, Assignee: assignee, Status: status, Err: reason}
		if l != nil && storageissueops.ActorMatches(l.Holder, actor) {
			// The caller's own lease on a claim it no longer holds is an
			// orphan; drop it rather than keep it alive.
			if err := t.kv.del(leaseKey(id)); err != nil {
				return leaseRenewal{}, nil, fmt.Errorf("delete orphaned lease for %s: %w", id, err)
			}
			refusal.OrphanDeleted = true
		}
		return leaseRenewal{}, refusal, nil
	}
	if assignee != "" && !storageissueops.ActorMatches(assignee, actor) {
		return refused(fmt.Errorf("%w by %s", storage.ErrAlreadyClaimed, assignee))
	}
	if storageissueops.ActorMatches(assignee, actor) && rec.Issue.Status == types.StatusInProgress {
		if l != nil && storageissueops.ActorMatches(l.Holder, actor) {
			// A refresh the fast path missed (holder spelled differently):
			// refresh in place under the caller's spelling, keeping grant
			// provenance.
			l.Holder = actor
			l.ExpiresAt, l.HeartbeatAt = now.Add(ttl), now
			if l.Node == "" {
				l.Node = node
			}
			return refreshed, nil, putLease(t.kv, id, l)
		}
		// Re-arm: a live claim with no lease (hand-doled through a generic
		// update). This is a mutation: the row's version moves and the
		// re-arm is recorded.
		old := *rec
		rec.Issue.UpdatedAt = wall
		rec.RowVersion++
		if err := putRecord(t.kv, &old, rec); err != nil {
			return leaseRenewal{}, nil, fmt.Errorf("remint row version for %s: %w", id, err)
		}
		if err := grantLease(t.ctx, t.kv, id, actor, now); err != nil {
			return leaseRenewal{}, nil, err
		}
		if err := t.recordEvent(rec, types.EventLeaseRearmed, actor, strPtr(""), strPtr(actor), nil); err != nil {
			return leaseRenewal{}, nil, fmt.Errorf("record lease re-arm event for %s: %w", id, err)
		}
		if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
			return leaseRenewal{}, nil, err
		}
		refreshed.Rearmed = true
		return refreshed, nil, nil
	}
	return refused(fmt.Errorf("%w: %s status %s", storage.ErrNotClaimable, id, status))
}

// --- reclaim ----------------------------------------------------------------

// ReclaimExpiredLeases reverts every in_progress durable issue whose lease
// expired before now-olderThan back to open and unassigned, within the scope
// filter selects (issueops.ReclaimExpiredLeasesInTx). A lease another named
// replica granted is left alone unless filter.AnyReplica.
func (s *Store) ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error) {
	cutoff := nowUTC().Add(-olderThan)
	var reclaimed []types.ReclaimedLease
	err := s.run(ctx, func(t *txn) error {
		stale, err := staleLeases(t.kv, cutoff, filter, storageissueops.NodeID(t.ctx))
		if err != nil {
			return err
		}
		for _, id := range stale {
			rec, err := getRecord(t.kv, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			if rec.Issue.Status != types.StatusInProgress {
				continue
			}
			previous := rec.Issue.Assignee
			if err := t.kv.del(leaseKey(id)); err != nil {
				return fmt.Errorf("reclaim %s: %w", id, err)
			}
			old := *rec
			now := nowUTC()
			rec.Issue.Status = types.StatusOpen
			rec.Issue.Assignee = ""
			rec.Issue.StartedAt = nil
			rec.Issue.UpdatedAt = now
			rec.ContentHash = rec.Issue.ComputeContentHash()
			rec.RowVersion++
			if err := putRecord(t.kv, &old, rec); err != nil {
				return fmt.Errorf("reclaim %s: %w", id, err)
			}
			if err := t.recordEvent(rec, types.EventLeaseReclaimed, actor, strPtr(previous), strPtr(""), nil); err != nil {
				return fmt.Errorf("record reclaim event for %s: %w", id, err)
			}
			if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
				return err
			}
			t.markBlockedDirty(id)
			reclaimed = append(reclaimed, types.ReclaimedLease{ID: id, PreviousOwner: previous, Revision: types.RevisionToken(rec.RowVersion)})
		}
		return t.settleBlocked()
	})
	if err != nil {
		return nil, err
	}
	return reclaimed, nil
}

// staleLeases returns, in id order, the ids whose lease expired strictly
// before cutoff, whose row is an in_progress durable issue, that the replica
// guard admits, and that the scope filter selects
// (sqlbuild.BuildReclaimScopeClauses: a populated field with no usable value
// selects nothing).
func staleLeases(r kv, cutoff time.Time, filter types.ReclaimFilter, localNode string) ([]string, error) {
	degenerate := func(raw, compacted []string) bool { return len(raw) > 0 && len(compacted) == 0 }
	ids := sqlbuild.CompactNonEmptyStrings(filter.IDs)
	assignees := sqlbuild.CompactNonEmptyStrings(filter.Assignees)
	labels := sqlbuild.CompactNonEmptyStrings(filter.Labels)
	any := sqlbuild.CompactNonEmptyStrings(filter.LabelsAny)
	exclude := sqlbuild.CompactNonEmptyStrings(filter.ExcludeLabels)
	if degenerate(filter.IDs, ids) || degenerate(filter.Assignees, assignees) ||
		degenerate(filter.Labels, labels) || degenerate(filter.LabelsAny, any) {
		return nil, nil
	}
	var out []string
	err := r.scan([]byte(nsLease), func(k, v []byte) (bool, error) {
		id := strings.TrimPrefix(string(k), nsLease)
		l, err := decodeJSON[lease](v)
		if err != nil {
			return false, fmt.Errorf("decode lease %s: %w", id, err)
		}
		if !l.ExpiresAt.Before(cutoff) {
			return true, nil
		}
		if !filter.AnyReplica && localNode != "" && l.Node != "" && l.Node != localNode {
			return true, nil
		}
		rec, err := getRecord(r, id)
		if err != nil {
			if isNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if rec.Plane == planeWisps || rec.Issue.Status != types.StatusInProgress {
			return true, nil
		}
		if len(ids) > 0 && !slices.Contains(ids, id) {
			return true, nil
		}
		if len(assignees) > 0 && !slices.Contains(assignees, rec.Issue.Assignee) {
			return true, nil
		}
		if !labelsMatch(rec.Issue.Labels, labels, any, exclude) {
			return true, nil
		}
		out = append(out, id)
		return true, nil
	})
	return out, err
}
