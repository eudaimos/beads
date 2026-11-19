package pebble

import (
	"context"

	publicops "github.com/steveyegge/beads/issueops"
)

// BatchGetter serves the many-ids read behind gc's ready veto and the CLI's
// id resolution, over one snapshot.
func (s *Store) BatchGetter() (publicops.BatchGetter, error) {
	return &batchGetter{s: s}, nil
}

type batchGetter struct{ s *Store }

var _ publicops.BatchGetter = (*batchGetter)(nil)

// GetMany is issueops.ExecuteGetMany over the key space, through the same
// getMany the molecule helpers read with: the existence check and the
// hydration share one snapshot, so an id cannot be reported missing by a
// probe that raced a create a later read then found.
func (g *batchGetter) GetMany(ctx context.Context, request publicops.GetManyRequest) (publicops.GetManyResult, error) {
	var result publicops.GetManyResult
	err := g.s.readSnapshotCtx(ctx, func(r kv) error {
		var err error
		result, err = getMany(r, request)
		return err
	})
	if err != nil {
		return publicops.GetManyResult{}, err
	}
	return result, nil
}
