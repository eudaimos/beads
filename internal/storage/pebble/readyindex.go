package pebble

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/steveyegge/beads/internal/types"
)

// Derived indexes over the rows, maintained by putRecord and dropped by
// dropRowIndexes, so the ready predicate and the blocked list never scan the
// issues namespace:
//
//	R/<status>/<priority>/<created>/<id>   ready candidate: open status, not
//	                                       pinned, not effectively blocked.
//	                                       Value: the cheap filter fields.
//	B/<id>                                 open row whose stored flag is set
//	F/<defer_until>/<id>                   row with a defer_until
//
// The R key order is the ready sort order inside one (status, priority)
// group: created_at, then id. A ready page is a merge of the groups the
// filter admits, each read as far as the page needs; the hybrid policy reads
// the recent half of every group first (by priority) and the rest after (by
// age). Rows the index cannot decide (labels, metadata) are loaded on demand.

// indexVersion is stamped at z/indexes; a store without it (or an older one)
// is rebuilt on open.
const indexVersion = "1"

// readyEntry is one candidate-index entry: the sort fields from its key and
// the cheap filter fields from its value, plus the row once it is loaded.
type readyEntry struct {
	status   types.Status
	priority int
	created  int64
	id       string

	plane     plane
	typ       types.IssueType
	ephemeral bool
	deferAt   int64 // unixNanos of defer_until, 0 when unset
	assignee  string

	rec *issueRecord
}

// readyCandidate reports whether a row belongs in the R index: the clauses
// of the ready predicate that do not depend on the filter.
func readyCandidate(rec *issueRecord) bool {
	return open(rec.Issue.Status) && !rec.Issue.Pinned && !rec.EffectiveBlocked
}

func readyIndexKey(rec *issueRecord) []byte {
	if !readyCandidate(rec) {
		return nil
	}
	return readyKey(rec.Issue.Status, rec.Issue.Priority, unixNanos(rec.Issue.CreatedAt), rec.Issue.ID)
}

// readyIndexValue packs the filter fields a candidate is screened on without
// loading the row. The assignee is last so a NUL in it cannot shift fields.
func readyIndexValue(rec *issueRecord) []byte {
	i := &rec.Issue
	eph, deferAt := "", ""
	if i.Ephemeral {
		eph = "1"
	}
	if i.DeferUntil != nil {
		deferAt = strconv.FormatInt(unixNanos(*i.DeferUntil), 10)
	}
	return []byte(strings.Join([]string{string(rec.Plane), string(i.IssueType), eph, deferAt, i.Assignee}, sep))
}

func parseReadyEntry(key, value []byte) (*readyEntry, bool) {
	parts := splitKey(nsReady, key)
	if len(parts) != 4 {
		return nil, false
	}
	e := &readyEntry{status: types.Status(parts[0]), priority: parsePriority(parts[1]), id: parts[3]}
	e.created, _ = strconv.ParseInt(strings.TrimLeft(parts[2], "0"), 10, 64)
	fields := strings.SplitN(string(value), sep, 5)
	if len(fields) != 5 {
		return nil, false
	}
	e.plane = plane(fields[0])
	e.typ = types.IssueType(fields[1])
	e.ephemeral = fields[2] == "1"
	if fields[3] != "" {
		e.deferAt, _ = strconv.ParseInt(fields[3], 10, 64)
	}
	e.assignee = fields[4]
	return e, true
}

// entryFromRecord builds the entry the index would hold for rec, for the
// full-scan path to share the matcher.
func entryFromRecord(rec *issueRecord) *readyEntry {
	e := &readyEntry{
		status: rec.Issue.Status, priority: rec.Issue.Priority, created: unixNanos(rec.Issue.CreatedAt), id: rec.Issue.ID,
		plane: rec.Plane, typ: rec.Issue.IssueType, ephemeral: rec.Issue.Ephemeral, assignee: rec.Issue.Assignee, rec: rec,
	}
	if rec.Issue.DeferUntil != nil {
		e.deferAt = unixNanos(*rec.Issue.DeferUntil)
	}
	return e
}

func blockedIndexKey(rec *issueRecord) []byte {
	if !rec.Issue.IsBlocked || !open(rec.Issue.Status) {
		return nil
	}
	return blockedKey(rec.Issue.ID)
}

func deferIndexKey(rec *issueRecord) []byte {
	if rec.Issue.DeferUntil == nil {
		return nil
	}
	return deferKey(unixNanos(*rec.Issue.DeferUntil), rec.Issue.ID)
}

// putDerivedIndexes moves the derived entries of a row from old (nil for a
// new row) to rec. The R value is rewritten whenever the row is, since the
// fields it packs can change under the same key.
func putDerivedIndexes(w kv, old, rec *issueRecord) error {
	var oldReady, oldBlocked, oldDefer []byte
	if old != nil {
		oldReady, oldBlocked, oldDefer = readyIndexKey(old), blockedIndexKey(old), deferIndexKey(old)
	}
	newReady := readyIndexKey(rec)
	if oldReady != nil && !bytes.Equal(oldReady, newReady) {
		if err := w.del(oldReady); err != nil {
			return err
		}
	}
	if newReady != nil {
		if err := w.set(newReady, readyIndexValue(rec)); err != nil {
			return err
		}
	}
	for _, k := range []struct{ old, cur []byte }{{oldBlocked, blockedIndexKey(rec)}, {oldDefer, deferIndexKey(rec)}} {
		if k.old != nil && !bytes.Equal(k.old, k.cur) {
			if err := w.del(k.old); err != nil {
				return err
			}
		}
		if k.cur != nil && !bytes.Equal(k.old, k.cur) {
			if err := w.set(k.cur, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func dropDerivedIndexes(w kv, rec *issueRecord) error {
	for _, k := range [][]byte{readyIndexKey(rec), blockedIndexKey(rec), deferIndexKey(rec)} {
		if k == nil {
			continue
		}
		if err := w.del(k); err != nil {
			return err
		}
	}
	return nil
}

// ensureIndexes rebuilds the derived indexes when the store predates them.
func (s *Store) ensureIndexes(ctx context.Context) error {
	v, ok, err := dbKV{db: s.db}.get(internalKey("indexes"))
	if err != nil {
		return err
	}
	if ok && string(v) == indexVersion {
		return nil
	}
	return s.run(ctx, func(t *txn) error {
		if err := rebuildIndexes(dbKV{db: s.db}, t.kv); err != nil {
			return err
		}
		return t.kv.set(internalKey("indexes"), []byte(indexVersion))
	})
}

// rebuildIndexes drops and rewrites every derived entry, reading the rows
// from r and writing to w (a batch, which must not see its own writes while
// the rows are scanned).
func rebuildIndexes(r, w kv) error {
	for _, ns := range []string{nsReady, nsBlocked, nsDefer} {
		var keys [][]byte
		err := r.scan([]byte(ns), func(k, _ []byte) (bool, error) {
			keys = append(keys, append([]byte(nil), k...))
			return true, nil
		})
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := w.del(k); err != nil {
				return err
			}
		}
	}
	return scanRecords(r, func(rec *issueRecord) (bool, error) {
		return true, putRecord(w, nil, rec)
	})
}

// --- the indexed ready scan ---------------------------------------------------

// readyStream walks one key range of the R index and yields the entries the
// matcher admits, in key order.
type readyStream struct {
	it   *pebble.Iterator
	m    *readyMatcher
	head *readyEntry
	err  error
}

func newReadyStream(r kv, m *readyMatcher, lo, hi []byte) (*readyStream, error) {
	it, err := r.iter(lo, hi)
	if err != nil {
		return nil, err
	}
	it.First()
	return &readyStream{it: it, m: m}, nil
}

// peek returns the next admitted entry without consuming it, or nil at the
// end (or on error, left in s.err).
func (s *readyStream) peek() *readyEntry {
	for s.head == nil && s.err == nil && s.it.Valid() {
		e, ok := parseReadyEntry(s.it.Key(), s.it.Value())
		if ok {
			admit, err := s.m.matchIndexed(e)
			if err != nil {
				s.err = err
				return nil
			}
			if admit {
				s.head = e
				return e
			}
		}
		s.it.Next()
	}
	if s.err == nil {
		s.err = s.it.Error()
	}
	return s.head
}

func (s *readyStream) pop() {
	s.head = nil
	s.it.Next()
}

func (s *readyStream) close() error {
	err := s.it.Close()
	if s.err != nil {
		return s.err
	}
	return err
}

func closeStreams(streams []*readyStream, err *error) {
	for _, s := range streams {
		if cerr := s.close(); cerr != nil && *err == nil {
			*err = cerr
		}
	}
}

// mergeStreams visits the union of the streams in less order until visit
// returns false.
func mergeStreams(streams []*readyStream, less func(a, b *readyEntry) bool, visit func(e *readyEntry) (bool, error)) error {
	for {
		var best *readyStream
		for _, s := range streams {
			e := s.peek()
			if s.err != nil {
				return s.err
			}
			if e == nil {
				continue
			}
			if best == nil || less(e, best.head) {
				best = s
			}
		}
		if best == nil {
			return nil
		}
		more, err := visit(best.head)
		if err != nil || !more {
			return err
		}
		best.pop()
	}
}

func lessByPriority(a, b *readyEntry) bool {
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	return lessByCreated(a, b)
}

func lessByCreated(a, b *readyEntry) bool {
	if a.created != b.created {
		return a.created < b.created
	}
	return a.id < b.id
}

// priorityGroups lists the priorities present in one status partition, in
// order, with one seek per group.
func priorityGroups(r kv, status types.Status) ([]int, error) {
	prefix := readyStatusPrefix(status)
	it, err := r.iter(prefix, prefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	var out []int
	for ok := it.First(); ok; {
		parts := splitKey(nsReady, it.Key())
		if len(parts) != 4 {
			ok = it.Next()
			continue
		}
		p := parsePriority(parts[1])
		out = append(out, p)
		ok = it.SeekGE(prefixEnd(readyGroupPrefix(status, p)))
	}
	if err := it.Error(); err != nil {
		_ = it.Close()
		return nil, err
	}
	return out, it.Close()
}

// readyPage is the result of a ready evaluation: the first limit admitted
// rows in sort order (every row when limit is 0, none when it is negative)
// and, when counted, the size of the whole admitted set.
type readyPage struct {
	recs  []*issueRecord
	total int
}

// collect drains streams in less order into page, loading rows for the first
// limit entries and only counting the rest; it stops early when the page is
// full and no count is wanted.
func collect(r kv, streams []*readyStream, less func(a, b *readyEntry) bool, page *readyPage, limit int, wantTotal bool) error {
	return mergeStreams(streams, less, func(e *readyEntry) (bool, error) {
		page.total++
		if limit < 0 || (limit > 0 && len(page.recs) >= limit) {
			return wantTotal, nil
		}
		if e.rec == nil {
			rec, err := getRecord(r, e.id)
			if err != nil {
				return false, err
			}
			e.rec = rec
		}
		page.recs = append(page.recs, e.rec)
		return true, nil
	})
}

// readyScanIndexed evaluates filter over the R index.
func readyScanIndexed(r kv, m *readyMatcher, limit int, wantTotal bool) (page readyPage, err error) {
	statuses := m.statusSet()
	type group struct {
		status   types.Status
		priority int
	}
	var groups []group
	for _, st := range statuses {
		ps, err := priorityGroups(r, st)
		if err != nil {
			return page, err
		}
		for _, p := range ps {
			groups = append(groups, group{st, p})
		}
	}
	openGroups := func(lo, hi func(g group) []byte) ([]*readyStream, error) {
		streams := make([]*readyStream, 0, len(groups))
		for _, g := range groups {
			s, err := newReadyStream(r, m, lo(g), hi(g))
			if err != nil {
				closeStreams(streams, &err)
				return nil, err
			}
			streams = append(streams, s)
		}
		return streams, nil
	}
	groupStart := func(g group) []byte { return readyGroupPrefix(g.status, g.priority) }
	groupEnd := func(g group) []byte { return prefixEnd(readyGroupPrefix(g.status, g.priority)) }

	var streams, older []*readyStream
	defer func() {
		closeStreams(streams, &err)
		closeStreams(older, &err)
	}()
	switch m.filter.SortPolicy {
	case types.SortPolicyOldest:
		if streams, err = openGroups(groupStart, groupEnd); err != nil {
			return page, err
		}
		err = collect(r, streams, lessByCreated, &page, limit, wantTotal)
	case types.SortPolicyHybrid, "":
		cutoff := nanosString(unixNanos(m.now.Add(-48 * time.Hour)))
		at := func(g group) []byte { return append(readyGroupPrefix(g.status, g.priority), cutoff...) }
		if streams, err = openGroups(at, groupEnd); err != nil {
			return page, err
		}
		if err = collect(r, streams, lessByPriority, &page, limit, wantTotal); err != nil {
			return page, err
		}
		if limit > 0 && len(page.recs) >= limit && !wantTotal {
			return page, nil
		}
		if older, err = openGroups(groupStart, at); err != nil {
			return page, err
		}
		err = collect(r, older, lessByCreated, &page, limit, wantTotal)
	default:
		// Priority order, which is also readyLess's order for any policy
		// it does not know. A status partition is already in (priority,
		// created, id) order, so one stream per status suffices.
		for _, st := range statuses {
			prefix := readyStatusPrefix(st)
			var s *readyStream
			if s, err = newReadyStream(r, m, prefix, prefixEnd(prefix)); err != nil {
				return page, err
			}
			streams = append(streams, s)
		}
		err = collect(r, streams, lessByPriority, &page, limit, wantTotal)
	}
	return page, err
}
