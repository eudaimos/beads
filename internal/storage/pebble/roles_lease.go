package pebble

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// The lease family: the heartbeat that keeps a claim alive, the sweep that
// takes stale claims back, and the atomic take of ready work. Each is a thin
// door over the transaction bodies the flat verbs (HeartbeatIssue,
// ReclaimExpiredLeases, ClaimReadyIssue) already run; the role adds the
// contract's validation and refusal vocabulary.

// --- Heartbeater ------------------------------------------------------------

// Heartbeater serves the lease refresh behind `bd heartbeat`.
func (s *Store) Heartbeater() (publicops.Heartbeater, error) {
	return &heartbeater{s: s}, nil
}

type heartbeater struct{ s *Store }

var _ publicops.Heartbeater = (*heartbeater)(nil)

// Heartbeat refreshes the caller's lease, the in-process form of
// issueops.ExecuteHeartbeatInTx: a wisp or an absent row is refused before
// the lease is touched, and the renewal body decides between a refresh (the
// lease row alone), a re-arm (recorded as a mutation) and a refusal. A
// *LeaseConflictError refusal COMMITS, because it may have deleted the
// caller's orphaned lease row, and is reported after the commit.
func (h *heartbeater) Heartbeat(ctx context.Context, request publicops.HeartbeatRequest) (publicops.HeartbeatResult, error) {
	if err := storageissueops.ValidateHeartbeatRequest(request); err != nil {
		return publicops.HeartbeatResult{}, err
	}
	id := request.IssueID
	var result publicops.HeartbeatResult
	var refusal error
	err := h.s.run(ctx, func(t *txn) error {
		refusal = nil
		rec, err := getRecord(t.kv, id)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
			}
			return err
		}
		if rec.Plane == planeWisps {
			refusal = &publicops.LeaseConflictError{
				IssueID: id, Status: rec.Issue.Status, Ephemeral: true,
				Err: fmt.Errorf("%w: %s is ephemeral", publicops.ErrLeaseNotHeld, id),
			}
			return nil
		}
		renewal, refused, err := t.renewLease(rec, request.Actor)
		if err != nil {
			return err
		}
		if refused != nil {
			refusal = classifyLeaseRenewalRefusal(refused, request.Actor)
			return nil
		}
		result = publicops.HeartbeatResult{
			IssueID:        id,
			Holder:         request.Actor,
			HeartbeatAt:    renewal.HeartbeatAt,
			LeaseExpiresAt: renewal.ExpiresAt,
			Rearmed:        renewal.Rearmed,
		}
		return nil
	})
	if err != nil {
		return publicops.HeartbeatResult{}, err
	}
	if refusal != nil {
		return publicops.HeartbeatResult{}, refusal
	}
	return result, nil
}

// classifyLeaseRenewalRefusal turns the renewal body's refusal into the typed
// conflict the role promises, the way the SQL seam's unexported classifier
// does: a foreign holder names the Holder and wraps ErrAlreadyClaimed; every
// other state wraps ErrLeaseNotHeld.
func classifyLeaseRenewalRefusal(refusal *storageissueops.LeaseRenewalRefusal, actor string) error {
	id := refusal.IssueID
	status := types.Status(refusal.Status)
	if refusal.Assignee != "" && !storageissueops.ActorMatches(refusal.Assignee, actor) {
		return &publicops.LeaseConflictError{IssueID: id, Holder: refusal.Assignee, Status: status, Err: refusal.Err}
	}
	return &publicops.LeaseConflictError{
		IssueID: id, Status: status,
		Err: fmt.Errorf("%w: %s status %s", publicops.ErrLeaseNotHeld, id, refusal.Status),
	}
}

// --- LeaseReclaimer ---------------------------------------------------------

// LeaseReclaimer serves the stale-lease sweep behind `bd reclaim`.
func (s *Store) LeaseReclaimer() (publicops.LeaseReclaimer, error) {
	return &leaseReclaimer{s: s}, nil
}

type leaseReclaimer struct{ s *Store }

var _ publicops.LeaseReclaimer = (*leaseReclaimer)(nil)

// Reclaim validates the request and runs the sweep ReclaimExpiredLeases runs,
// in one transaction, answering an empty non-nil slice when nothing was
// stale.
func (l *leaseReclaimer) Reclaim(ctx context.Context, request publicops.ReclaimRequest) (publicops.ReclaimResult, error) {
	if err := storageissueops.ValidateReclaimRequest(request); err != nil {
		return publicops.ReclaimResult{}, err
	}
	reclaimed, err := l.s.ReclaimExpiredLeases(ctx, request.OlderThan, request.Filter, strings.TrimSpace(request.Actor))
	if err != nil {
		return publicops.ReclaimResult{}, fmt.Errorf("reclaim: %w", err)
	}
	if reclaimed == nil {
		reclaimed = []publicops.ReclaimedLease{}
	}
	return publicops.ReclaimResult{Reclaimed: reclaimed}, nil
}

// --- ReadyClaimer -----------------------------------------------------------

// ReadyClaimer serves the atomic take of ready work behind `bd ready --claim`.
func (s *Store) ReadyClaimer() (publicops.ReadyClaimer, error) {
	return &readyClaimer{s: s}, nil
}

type readyClaimer struct{ s *Store }

var _ publicops.ReadyClaimer = (*readyClaimer)(nil)

// ClaimNext runs selection, the compare-and-set and the hydration in one
// transaction. The predicate is the one Reader.Ready lists for the same
// request, through the same builder; expired dated defers need no advisory
// wake here because the candidate scan reads the defer date directly.
func (c *readyClaimer) ClaimNext(ctx context.Context, request publicops.ClaimNextRequest) (publicops.ClaimNextResult, error) {
	if err := storageissueops.ValidateClaimNextRequest(request); err != nil {
		return publicops.ClaimNextResult{}, err
	}
	filter, err := workapi.BuildReadyFilter(request.Filter)
	if err != nil {
		return publicops.ClaimNextResult{}, err
	}
	var result publicops.ClaimNextResult
	err = c.s.run(ctx, func(t *txn) error {
		result = publicops.ClaimNextResult{}
		rec, err := t.claimNext(filter, request.Actor)
		if err != nil || rec == nil {
			return err
		}
		claimed, err := withCounts(t.kv, rec, countsHydration{})
		if err != nil {
			return err
		}
		result.Claimed = claimed
		return nil
	})
	if err != nil {
		return publicops.ClaimNextResult{}, err
	}
	return result, nil
}
