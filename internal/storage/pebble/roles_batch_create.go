package pebble

import (
	"context"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// BatchCreator serves the homogeneous create-many role `bd create --file` and
// the markdown importer call: N items, one transaction, the created rows back
// in request order.
func (s *Store) BatchCreator() (publicops.BatchCreator, error) {
	return &batchCreator{s: s}, nil
}

type batchCreator struct{ s *Store }

var _ publicops.BatchCreator = (*batchCreator)(nil)

// CreateBatch creates every item in ONE transaction and commits them together,
// or refuses the whole request: the first item that refuses returns, and the
// batch it was in is rolled back with nothing created.
//
// THE ITEMS LAND IN ORDER through the same per-item body Lifecycle.Create and
// the batch applier run (lifecycleCreate), each item's edges written as the
// item is, so an edge naming an EARLIER item of the batch resolves and one
// naming a later item is refused as absent — the unit-of-work body's shape,
// which is the one the contract promises. The cross-plane rule is checked
// before anything is written, because item-by-item creation would otherwise
// accept an edge between the planes once its target is an ordinary row.
//
// The explicit ids the batch names are claimed up front, so a minted id can
// never collide with one a later item spells for itself.
func (b *batchCreator) CreateBatch(ctx context.Context, request publicops.CreateBatchRequest) (publicops.CreateBatchResult, error) {
	snapshot := storageissueops.CloneCreateBatchRequest(request)
	if err := storageissueops.ValidateCreateBatchRequest(snapshot); err != nil {
		return publicops.CreateBatchResult{}, err
	}
	var result publicops.CreateBatchResult
	err := b.s.run(ctx, func(t *txn) error {
		attempt := storageissueops.CloneCreateBatchRequest(snapshot)
		if err := storageissueops.ValidateCreateBatchPlanes(attempt, infraTypes(t.kv)); err != nil {
			return err
		}
		t.claimedIDs = make(map[string]bool, len(attempt.Items))
		for _, item := range attempt.Items {
			if item.Issue.ID != "" {
				t.claimedIDs[item.Issue.ID] = true
			}
		}
		ids := make([]string, len(attempt.Items))
		for i, item := range attempt.Items {
			single := storageissueops.CloneCreateRequest(storageissueops.CreateBatchItemRequest(attempt, item))
			if err := storageissueops.ValidatePublicCreateRequest(single); err != nil {
				return storageissueops.CreateBatchItemError(i, err)
			}
			id, err := t.lifecycleCreate(single)
			if err != nil {
				return storageissueops.CreateBatchItemError(i, err)
			}
			t.claimedIDs[id] = true
			ids[i] = id
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		result = publicops.CreateBatchResult{Issues: make([]*types.Issue, len(ids))}
		for i, id := range ids {
			hydrated, err := loadIssue(t.kv, id)
			if err != nil {
				return err
			}
			result.Issues[i] = hydrated
		}
		return nil
	})
	if err != nil {
		return publicops.CreateBatchResult{}, err
	}
	return result, nil
}
