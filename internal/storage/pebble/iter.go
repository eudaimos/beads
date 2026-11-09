package pebble

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// The iterators wrap the list reads: a scan already materializes the
// matching set in process, so streaming buys nothing here beyond the
// interface.

// IterIssues streams the issues SearchIssues would return for query and
// filter, in the same order.
func (s *Store) IterIssues(ctx context.Context, query string, filter types.IssueFilter) (storage.Iter[types.Issue], error) {
	issues, err := s.SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(issues), nil
}

// IterReadyWork streams the issues GetReadyWork would return for filter.
func (s *Store) IterReadyWork(ctx context.Context, filter types.WorkFilter) (storage.Iter[types.Issue], error) {
	issues, err := s.GetReadyWork(ctx, filter)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(issues), nil
}

// IterBlockedIssues streams the rows GetBlockedIssues would return.
func (s *Store) IterBlockedIssues(ctx context.Context, filter types.WorkFilter) (storage.Iter[types.BlockedIssue], error) {
	blocked, err := s.GetBlockedIssues(ctx, filter)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(blocked), nil
}

// IterWisps streams the issues ListWisps would return, in the same order.
func (s *Store) IterWisps(ctx context.Context, filter types.WispFilter) (storage.Iter[types.Issue], error) {
	wisps, err := s.ListWisps(ctx, filter)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(wisps), nil
}
