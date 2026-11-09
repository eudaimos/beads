package pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/journalops"
)

// The durable events journal (journalops): one record per committed bead
// mutation, in commit order, written in the mutation's own transaction. The
// SQL backends keep it in the clone-local bd_events_journal table with its seq
// drawn from the bd_events_seq counter row (issueops/journal.go); here it is
// the J/ key range with its seq drawn from the "journal" counter, and the
// same-transaction guarantee is the batch: a record lands with the mutation it
// describes or not at all.
//
// Activation is per store instance (storage.EventsJournalConfigurer) and is
// read once per transaction, so a switch thrown mid-transaction takes effect
// at the next one. Emission is unconditional once on — never gated by
// EmitEvent — for the reason the SQL seam gives: suppressing audit noise must
// not punch a hole in a consumer's cursor.
//
// Each emission site below mirrors the op the SQL seam records for the same
// verb (create, update, close, delete, dep_add, dep_remove, comment), and the
// derived blocked-state maintenance journals an update for every row whose
// stored flag flips (settleBlocked), as recordBlockedJournalChanges does.
//
// A wisp-plane create record stamps the row's arrival_seq with its own seq
// (the first create record journaled for an unstamped row, and only that one),
// so a consumer can recover create order after the records are pruned; a row
// that leaves the wisp plane loses the stamp, and the issues plane is never
// stamped. Only this file writes the field.

var (
	_ storage.EventsJournalConfigurer = (*Store)(nil)
	_ storage.EventsJournalAccessor   = (*Store)(nil)
	_ storage.EventsJournalCursor     = (*Store)(nil)
)

// journalCounter is the seq counter's name under s/.
const journalCounter = "journal"

// journalRow is the stored record. TS is the insert time, stamped inside the
// committing transaction; it is a label and not the order (journalops.Row).
type journalRow struct {
	Seq         int64     `json:"seq"`
	TS          time.Time `json:"ts"`
	Op          string    `json:"op"`
	IssueID     string    `json:"issue_id"`
	Actor       string    `json:"actor"`
	IssueJSON   string    `json:"issue_json,omitempty"`
	DepJSON     string    `json:"dep_json,omitempty"`
	CommentJSON string    `json:"comment_json,omitempty"`
}

func (r *journalRow) toRow() storage.EventsJournalRow {
	return storage.EventsJournalRow{
		Seq: r.Seq, TS: r.TS.UTC().Format(time.RFC3339Nano), Op: r.Op, IssueID: r.IssueID, Actor: r.Actor,
		IssueJSON: r.IssueJSON, DepJSON: r.DepJSON, CommentJSON: r.CommentJSON,
	}
}

// SetEventsJournalEnabled activates the journal for this store instance only.
func (s *Store) SetEventsJournalEnabled(enabled bool) {
	s.journal.Store(enabled)
}

// --- emission -----------------------------------------------------------------

// journalIssue records op for id with the row's post-mutation snapshot. A
// create of an unstamped wisp-plane row stamps its arrival_seq with the
// record's seq and the snapshot carries the stamp. A no-op when the journal is
// off; a missing row is an error, because the mutation and the journal then
// disagree.
func (t *txn) journalIssue(op storageissueops.EventOp, id, actor string) error {
	if !t.journal {
		return nil
	}
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return fmt.Errorf("journal: snapshot %s for %s: %w", op, id, err)
	}
	seq, err := nextSeq(t.kv, journalCounter)
	if err != nil {
		return err
	}
	if op == storageissueops.EventCreate && rec.Plane == planeWisps && rec.Issue.ArrivalSeq == 0 {
		rec.Issue.ArrivalSeq = seq
		data, err := encodeRecord(rec)
		if err != nil {
			return err
		}
		if err := t.kv.set(issueKey(id), data); err != nil {
			return fmt.Errorf("journal: stamp arrival_seq %d on %s: %w", seq, id, err)
		}
	}
	snapshot, err := journalSnapshot(t.kv, rec)
	if err != nil {
		return err
	}
	return t.writeJournalRow(seq, op, id, actor, snapshot, nil, nil)
}

// journalDelete records a delete for id with no snapshot: the row is gone.
func (t *txn) journalDelete(id, actor string) error {
	if !t.journal {
		return nil
	}
	seq, err := nextSeq(t.kv, journalCounter)
	if err != nil {
		return err
	}
	return t.writeJournalRow(seq, storageissueops.EventDelete, id, actor, nil, nil, nil)
}

// journalDep records an edge add or remove under its source, with the
// source's snapshot when the source still exists (a cascade may have taken
// it) and the edge's kind, target and metadata.
func (t *txn) journalDep(op storageissueops.EventOp, dep *types.Dependency, actor string) error {
	if !t.journal {
		return nil
	}
	var snapshot *types.Issue
	rec, err := getRecord(t.kv, dep.IssueID)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("journal: snapshot %s for %s: %w", op, dep.IssueID, err)
	}
	if err == nil {
		if snapshot, err = journalSnapshot(t.kv, rec); err != nil {
			return err
		}
	}
	metadata := dep.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	seq, err := nextSeq(t.kv, journalCounter)
	if err != nil {
		return err
	}
	edge := &storageissueops.EventDep{Kind: string(dep.Type), Target: dep.DependsOnID, Metadata: metadata}
	return t.writeJournalRow(seq, op, dep.IssueID, actor, snapshot, edge, nil)
}

// journalEdgeRemovalsOf records one dep_remove for every edge touching ids,
// in (source, target, kind) order, before the rows or edges go
// (issueops.RecordDependencyRemovalsForIssuesInTx).
func (t *txn) journalEdgeRemovalsOf(ids []string, actor string) error {
	if !t.journal || len(ids) == 0 {
		return nil
	}
	byKey := map[string]*types.Dependency{}
	for _, id := range ids {
		outs, err := outgoingEdges(t.kv, id)
		if err != nil {
			return err
		}
		ins, err := incomingEdges(t.kv, id)
		if err != nil {
			return err
		}
		for _, d := range append(outs, ins...) {
			byKey[d.IssueID+sep+d.DependsOnID+sep+string(d.Type)] = d
		}
	}
	edges := make([]*types.Dependency, 0, len(byKey))
	for _, d := range byKey {
		edges = append(edges, d)
	}
	stableSortBy(edges, func(a, b *types.Dependency) bool {
		if a.IssueID != b.IssueID {
			return a.IssueID < b.IssueID
		}
		if a.DependsOnID != b.DependsOnID {
			return a.DependsOnID < b.DependsOnID
		}
		return a.Type < b.Type
	})
	for _, d := range edges {
		if err := t.journalDep(storageissueops.EventDepRemove, d, actor); err != nil {
			return err
		}
	}
	return nil
}

// journalComment records a structured or audit comment on id; the record's
// actor is the comment's author.
func (t *txn) journalComment(id string, comment *storageissueops.EventComment) error {
	if !t.journal {
		return nil
	}
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return fmt.Errorf("journal: snapshot comment for %s: %w", id, err)
	}
	snapshot, err := journalSnapshot(t.kv, rec)
	if err != nil {
		return err
	}
	seq, err := nextSeq(t.kv, journalCounter)
	if err != nil {
		return err
	}
	return t.writeJournalRow(seq, storageissueops.EventCommentWrite, id, comment.Author, snapshot, nil, comment)
}

// journalSnapshot is the row as the SQL seam snapshots it: the stored row
// with its labels and lease projection, carrying the stored is_blocked
// (ordinary hydration drops it; a replayed dependency delta needs it).
func journalSnapshot(r kv, rec *issueRecord) (*types.Issue, error) {
	issue := rec.toIssue()
	if err := projectLease(r, issue); err != nil {
		return nil, err
	}
	issue.IsBlocked = rec.Issue.IsBlocked
	return issue, nil
}

func (t *txn) writeJournalRow(seq int64, op storageissueops.EventOp, id, actor string, issue *types.Issue, dep *storageissueops.EventDep, comment *storageissueops.EventComment) error {
	row := journalRow{Seq: seq, TS: nowUTC(), Op: string(op), IssueID: id, Actor: actor}
	if issue != nil {
		b, err := json.Marshal(issue)
		if err != nil {
			return fmt.Errorf("journal: marshal issue %s: %w", id, err)
		}
		row.IssueJSON = string(b)
	}
	if dep != nil {
		b, err := json.Marshal(dep)
		if err != nil {
			return fmt.Errorf("journal: marshal dep for %s: %w", id, err)
		}
		row.DepJSON = string(b)
	}
	if comment != nil {
		b, err := json.Marshal(comment)
		if err != nil {
			return fmt.Errorf("journal: marshal comment for %s: %w", id, err)
		}
		row.CommentJSON = string(b)
	}
	data, err := encodeJSON(&row)
	if err != nil {
		return err
	}
	return t.kv.set(journalKey(seq), data)
}

// --- reads ----------------------------------------------------------------------

// readJournalRows returns the records with seq greater than since, seq
// ascending, at most limit (0: all).
func readJournalRows(r kv, since int64, limit int) ([]storage.EventsJournalRow, error) {
	if since >= math.MaxInt64 {
		return nil, nil
	}
	start := since + 1
	if start < 1 {
		start = 1
	}
	var out []storage.EventsJournalRow
	err := scanRange(r, journalKey(start), prefixEnd([]byte(nsJournal)), func(_, v []byte) (bool, error) {
		row, err := decodeJSON[journalRow](v)
		if err != nil {
			return false, err
		}
		out = append(out, row.toRow())
		return limit <= 0 || len(out) < limit, nil
	})
	return out, err
}

// journalHead is the highest seq ever assigned; prune never touches it.
func journalHead(r kv) (int64, error) {
	return readSeq(r, journalCounter)
}

// ReadEventsJournal returns the records after since on journalops' terms,
// with the typed truncation when since sits below the retained window.
func (s *Store) ReadEventsJournal(ctx context.Context, since int64, limit int) ([]storage.EventsJournalRow, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	var out []storage.EventsJournalRow
	err := s.read(func(r kv) error {
		rows, err := readJournalRows(r, since, limit)
		if err != nil {
			return err
		}
		if err := storageissueops.ComputeEventsTruncation(since, rows, func() (int64, error) {
			return journalHead(r)
		}); err != nil {
			return err
		}
		out = rows
		return nil
	})
	return out, err
}

// ReadEventsJournalPage is ReadEventsJournal plus the head, read from one
// snapshot so the pair cannot straddle a commit (journalops.Journal).
func (s *Store) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (storage.EventsJournalPage, error) {
	if err := ctxErr(ctx); err != nil {
		return storage.EventsJournalPage{}, err
	}
	var page storage.EventsJournalPage
	err := s.readSnapshot(func(r kv) error {
		rows, err := readJournalRows(r, since, limit)
		if err != nil {
			return err
		}
		head, err := journalHead(r)
		if err != nil {
			return err
		}
		if err := storageissueops.ComputeEventsTruncation(since, rows, func() (int64, error) {
			return head, nil
		}); err != nil {
			return err
		}
		page = storage.EventsJournalPage{Rows: rows, Head: head}
		return nil
	})
	return page, err
}

// PruneEventsJournal deletes the records below before, honoring the
// retain-days and retain-rows floors (0 disables one), and reports how many
// it removed. The bound is resolved by issueops.ComputeEventsPruneWhere, so a
// floor means here what it means on the SQL backends, and the delete is the
// same strict prefix.
func (s *Store) PruneEventsJournal(ctx context.Context, before int64, retainDays, retainRows int) (int64, error) {
	var n int64
	err := s.run(ctx, func(t *txn) error {
		readRowsCeil := func() (int64, bool, error) {
			// The seq of the (retainRows+1)-th newest record, or none when
			// the journal holds retainRows or fewer.
			seqs, err := journalSeqsDesc(t.kv, retainRows+1)
			if err != nil {
				return 0, false, err
			}
			if len(seqs) <= retainRows {
				return 0, false, nil
			}
			return seqs[retainRows], true, nil
		}
		readDaysFloor := func(cutoff time.Time) (int64, bool, error) {
			// The lowest seq whose ts is at or after the cutoff.
			var floor int64
			found := false
			err := scanRange(t.kv, journalKey(1), prefixEnd([]byte(nsJournal)), func(_, v []byte) (bool, error) {
				row, err := decodeJSON[journalRow](v)
				if err != nil {
					return false, err
				}
				if !row.TS.Before(cutoff) {
					floor, found = row.Seq, true
					return false, nil
				}
				return true, nil
			})
			return floor, found, err
		}
		_, args, skip, err := storageissueops.ComputeEventsPruneWhere(before, retainDays, retainRows, nowUTC(), readRowsCeil, readDaysFloor)
		if err != nil {
			return err
		}
		if skip {
			return nil
		}
		bound, _ := args[0].(int64)
		var keys [][]byte
		err = scanRange(t.kv, journalKey(1), journalKey(bound), func(k, _ []byte) (bool, error) {
			keys = append(keys, append([]byte(nil), k...))
			return true, nil
		})
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := t.kv.del(k); err != nil {
				return err
			}
		}
		n = int64(len(keys))
		return nil
	})
	return n, err
}

// journalSeqsDesc returns the newest n record seqs, newest first.
func journalSeqsDesc(r kv, n int) ([]int64, error) {
	it, err := r.iter(journalKey(1), prefixEnd([]byte(nsJournal)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = it.Close() }()
	var out []int64
	for ok := it.Last(); ok && len(out) < n; ok = it.Prev() {
		parts := splitKey(nsJournal, it.Key())
		if len(parts) != 1 {
			continue
		}
		out = append(out, parseSeq(parts[0]))
	}
	return out, it.Error()
}

// journalCursor is a compile-time check that the store is reached as a
// journalops.Journal by type assertion, the way bd serve and gascity reach it.
var _ journalops.Journal = (*Store)(nil)
