package pebble

import (
	"context"
	"fmt"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// listWisps is DoltStore.ListWisps over r: the wisps-plane rows that the
// projected filter admits (issueops.WispFilterToIssueFilter pins
// Ephemeral=true and hides closed wisps unless asked), ordered priority ASC,
// created_at ASC, bounded by Limit alone.
func listWisps(r kv, filter types.WispFilter) ([]*types.Issue, error) {
	f := storageissueops.WispFilterToIssueFilter(filter)
	m, err := newIssueMatcher(r, "", f)
	if err != nil {
		return nil, err
	}
	var out []*types.Issue
	err = scanRecords(r, func(rec *issueRecord) (bool, error) {
		if rec.Plane != planeWisps {
			return true, nil
		}
		ok, err := m.match(rec)
		if err != nil || !ok {
			return err == nil, err
		}
		issue := rec.toIssue()
		if err := projectLease(r, issue); err != nil {
			return false, err
		}
		out = append(out, issue)
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list wisps: %w", err)
	}
	stableSortBy(out, func(a, b *types.Issue) bool {
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ListWisps returns the ephemeral issues matching filter, closed ones
// excluded unless IncludeClosed or an explicit Status asks for them.
func (s *Store) ListWisps(ctx context.Context, filter types.WispFilter) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		v, err := listWisps(r, filter)
		out = v
		return err
	})
	return out, err
}
