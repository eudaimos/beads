// Package pebble is a Phase 0 spike of a pure-Go key-value storage backend
// for beads, built on CockroachDB's Pebble (github.com/cockroachdb/pebble/v2)
// behind the existing backend seam (backend.Register, storage.DoltStorage).
//
// Why: gascity measured that the cost of a small beads write on Dolt is the
// number of SQL round trips the issueops write path makes to maintain its
// invariants (blocked-state fixpoint, events, journal), not Dolt's commit
// graph. This backend keeps every invariant in process over an indexed graph,
// so a create or an edge change touches the affected neighborhood with a few
// point reads and one atomic batch write.
//
// # Scope of the spike
//
// The portable raw surface the conformance suite exercises (backend/conformance
// RunAll): issue CRUD, search, counts, dependencies and readiness, blocked
// state, labels, comments, events, config and metadata, slots, statistics,
// stale issues, iterators, transactions, claim/lease/heartbeat/reclaim,
// promote and rekey, batch create, and the five issueops roles RunAll reaches
// (Lifecycle, Reader, BatchCreator, DependencyEditor, Commenter). Everything
// else — version control, history, remotes, sync, federation, compaction, the
// remaining roles — answers *storage.ErrUnsupported through the generated
// shell in unsupported_gen.go and is declared through RunUnsupportedContract.
//
// # Key layout
//
// Every key is a short ASCII namespace ("i/", "d/", ...) followed by its
// components joined by a NUL byte (shown as "/" below). No component carries
// a NUL, so a prefix scan over "i/" is the issues table and a prefix scan over
// "r/<id>/" is the reverse-edge index of one row.
//
//	i/<id>                      issueRecord (JSON): the row, both planes, with
//	                            is_blocked, row_version and content_hash
//	x/<external_ref>            id (external_ref index)
//	l/<label>/<id>              "" (label index; labels also live in the row)
//	d/<src>/<dst>/<type>        types.Dependency (JSON), the forward edge
//	r/<dst>/<src>/<type>        "" (reverse edge index)
//	e/<id>/<seq>                types.Event (JSON), per-issue history
//	g/<nanos>/<seq>             "<id>/<seq>" (global events-by-time index)
//	c/<id>/<nanos>/<seq>        types.Comment (JSON)
//	k/<key>                     config (issue_prefix, status.custom, ...)
//	m/<key>                     metadata
//	n/<key>                     local metadata (node-local, never synced)
//	t/<repo_path>               repo mtime record (JSON)
//	s/<name>                    counters (events, comments, child ids)
//	L/<id>                      lease (JSON): holder, granted, expires, heartbeat
//
// # Concurrency and transactions
//
// A Store is a single-process store: one *pebble.DB, one writer at a time.
// Every mutation runs inside a write transaction (txn): an indexed Pebble
// batch under the store's write mutex, so reads inside the transaction see
// its own writes, and the batch commits with fsync at the end or is discarded
// on error. RunInTransaction hands the caller a storage.Transaction over the
// same batch. Reads outside a transaction go straight to the DB.
//
// # Blocked state
//
// is_blocked is derived state, stored on the row, and recomputed in process
// to the same rule the SQL union in internal/storage/issueops applies
// (blocked_consistency.go, shouldBeBlockedIDsUnionCoreSQL): a row is blocked
// when it has a blocks or conditional-blocks edge onto an open target, a
// parent-child edge onto an open, blocked parent whose blockedness is not
// explained by its own subtree, or a waits-for edge whose spawner still gates
// it. After every write that can change the answer, the affected
// neighborhood (the row, its dependents, and their parent-child descendants)
// is recomputed to a fixpoint. See blocked.go.
package pebble
