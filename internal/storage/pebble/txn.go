package pebble

import "context"

// txn is one write transaction: the indexed batch every operation in it
// reads and writes, plus what the transaction has learned (ids whose blocked
// state must be recomputed before commit).
type txn struct {
	store *Store
	kv    kv
	ctx   context.Context

	// journal is the store's events-journal activation as read when the
	// transaction began (journal.go).
	journal bool

	// dirtyBlocked collects the ids whose neighborhood a write in this
	// transaction may have changed; recomputeBlocked settles them.
	dirtyBlocked map[string]bool

	// claimedIDs is every id a batch create has spoken for — the explicit
	// ids its later items name and the ids it has minted so far — so mintID
	// passes over them the way the SQL batch body's claimed set does.
	claimedIDs map[string]bool
}

func (t *txn) markBlockedDirty(ids ...string) {
	if t.dirtyBlocked == nil {
		t.dirtyBlocked = make(map[string]bool)
	}
	for _, id := range ids {
		if id != "" {
			t.dirtyBlocked[id] = true
		}
	}
}
