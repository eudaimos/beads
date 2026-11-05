package pebble

import (
	"context"
	"database/sql"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/rowid"
	"github.com/steveyegge/beads/internal/types"
)

// auxTimeLayout is the second-granularity layout the SQL backends store
// event and comment times in; this backend truncates to the same grain so
// ordering and dedup behave the same.
const auxTimeLayout = "2006-01-02 15:04:05"

func strPtr(s string) *string { return &s }

func nullStrings(values ...string) []sql.NullString {
	out := make([]sql.NullString, len(values))
	for i, v := range values {
		out[i] = sql.NullString{String: v, Valid: true}
	}
	return out
}

func nullOf(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

// recordEvent appends one history event for rec's row, stamped now at second
// granularity, with an id derived from its content the way the SQL backends
// derive theirs (rowid): equal content in the same second gets the next
// ordinal, never a collision. A wisps-plane row stores "" for absent values,
// as the wisp_events table does (the audit pins that nullability asymmetry).
func (t *txn) recordEvent(rec *issueRecord, eventType types.EventType, actor string, oldValue, newValue, comment *string) error {
	return t.recordEventAt(rec, eventType, actor, oldValue, newValue, comment, nowUTC())
}

func (t *txn) recordEventAt(rec *issueRecord, eventType types.EventType, actor string, oldValue, newValue, comment *string, at time.Time) error {
	at = at.UTC().Truncate(time.Second)
	table := "events"
	if rec.Plane == planeWisps {
		table = "wisp_events"
		empty := ""
		if oldValue == nil {
			oldValue = &empty
		}
		if newValue == nil {
			newValue = &empty
		}
		if comment == nil {
			comment = &empty
		}
	}
	digest := rowid.Digest([]sql.NullString{
		{String: rec.Issue.ID, Valid: true}, {String: string(eventType), Valid: true}, {String: actor, Valid: true},
		nullOf(oldValue), nullOf(newValue), nullOf(comment), {String: at.Format(auxTimeLayout), Valid: true},
	})
	taken := map[string]bool{}
	err := t.kv.scan(eventPrefix(rec.Issue.ID), func(_, v []byte) (bool, error) {
		ev, err := decodeJSON[types.Event](v)
		if err != nil {
			return false, err
		}
		taken[ev.ID] = true
		return true, nil
	})
	if err != nil {
		return err
	}
	var id string
	for ordinal := 0; ; ordinal++ {
		id = rowid.New(table, ordinal, digest)
		if !taken[id] {
			break
		}
	}
	seq, err := nextSeq(t.kv, "events")
	if err != nil {
		return err
	}
	ev := &types.Event{
		ID: id, IssueID: rec.Issue.ID, EventType: eventType, Actor: actor,
		OldValue: oldValue, NewValue: newValue, Comment: comment, CreatedAt: at,
	}
	data, err := encodeJSON(ev)
	if err != nil {
		return err
	}
	if err := t.kv.set(eventKey(rec.Issue.ID, seq), data); err != nil {
		return err
	}
	return t.kv.set(globalEventKey(at.UnixNano(), seq), []byte(rec.Issue.ID+sep+seqString(seq)))
}

// eventsOf returns the row's events, newest first (the SQL readers order by
// created_at DESC, id DESC); limit <= 0 means all.
func eventsOf(r kv, id string, limit int) ([]*types.Event, error) {
	var out []*types.Event
	err := r.scan(eventPrefix(id), func(_, v []byte) (bool, error) {
		ev, err := decodeJSON[types.Event](v)
		if err != nil {
			return false, err
		}
		out = append(out, ev)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	// Scan order is insertion order (seq); reverse for newest first, with
	// created_at as the primary key so imported events sort by their stamp.
	sortEventsDesc(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sortEventsDesc(events []*types.Event) {
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	stableSortBy(events, func(a, b *types.Event) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
}

// eventsSince returns events strictly after the cursor across every row, in
// (created_at, id) order, at most limit (<= 0: all); issueID narrows to one
// row.
func eventsSince(r kv, cursor storage.EventCursor, issueID string, limit int) ([]*types.Event, error) {
	var out []*types.Event
	err := r.scan([]byte(nsGlobalEv), func(_, v []byte) (bool, error) {
		parts := splitKey("", v)
		if len(parts) != 2 {
			return true, nil
		}
		if issueID != "" && parts[0] != issueID {
			return true, nil
		}
		data, ok, err := r.get(eventKey(parts[0], parseSeq(parts[1])))
		if err != nil || !ok {
			return err == nil, err
		}
		ev, err := decodeJSON[types.Event](data)
		if err != nil {
			return false, err
		}
		if ev.CreatedAt.Before(cursor.CreatedAt) || (ev.CreatedAt.Equal(cursor.CreatedAt) && ev.ID <= cursor.ID) {
			return true, nil
		}
		out = append(out, ev)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	stableSortBy(out, func(a, b *types.Event) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- Store / Transaction surface ----------------------------------------------

func (s *Store) GetEvents(ctx context.Context, issueID string, limit int) ([]*types.Event, error) {
	var out []*types.Event
	err := s.read(func(r kv) error {
		v, err := eventsOf(r, issueID, limit)
		out = v
		return err
	})
	return out, err
}

func (s *Store) CountEvents(ctx context.Context, issueID string, limit int) (int64, error) {
	evs, err := s.GetEvents(ctx, issueID, limit)
	return int64(len(evs)), err
}

func (s *Store) GetAllEventsSince(ctx context.Context, since time.Time) ([]*types.Event, error) {
	var out []*types.Event
	err := s.read(func(r kv) error {
		v, err := eventsSince(r, storage.EventCursor{CreatedAt: since.Add(-time.Nanosecond)}, "", 0)
		out = v
		return err
	})
	return out, err
}

func (s *Store) EventsSince(ctx context.Context, cursor storage.EventCursor, issueID string, limit int) ([]*types.Event, error) {
	var out []*types.Event
	err := s.read(func(r kv) error {
		v, err := eventsSince(r, cursor, issueID, limit)
		out = v
		return err
	})
	return out, err
}

func (s *Store) IterEvents(ctx context.Context, issueID string, limit int) (storage.Iter[types.Event], error) {
	evs, err := s.GetEvents(ctx, issueID, limit)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(evs), nil
}

func (s *Store) IterAllEventsSince(ctx context.Context, since time.Time) (storage.Iter[types.Event], error) {
	evs, err := s.GetAllEventsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(evs), nil
}
