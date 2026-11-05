package pebble

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// --- row access ---------------------------------------------------------------

func getRecord(r kv, id string) (*issueRecord, error) {
	data, ok, err := r.get(issueKey(id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNotFound("issue", id)
	}
	return decodeRecord(data)
}

func recordExists(r kv, id string) (bool, error) {
	_, ok, err := r.get(issueKey(id))
	return ok, err
}

// putRecord writes rec and maintains the secondary indexes against old (nil
// for a new row).
func putRecord(w kv, old, rec *issueRecord) error {
	rec.Issue.ContentHash = rec.ContentHash
	data, err := encodeRecord(rec)
	if err != nil {
		return err
	}
	if err := w.set(issueKey(rec.Issue.ID), data); err != nil {
		return err
	}
	// external_ref index
	oldRef, newRef := "", ""
	if old != nil && old.Issue.ExternalRef != nil {
		oldRef = *old.Issue.ExternalRef
	}
	if rec.Issue.ExternalRef != nil {
		newRef = *rec.Issue.ExternalRef
	}
	if oldRef != newRef {
		if oldRef != "" {
			if err := w.del(extRefKey(oldRef)); err != nil {
				return err
			}
		}
		if newRef != "" {
			if err := w.set(extRefKey(newRef), []byte(rec.Issue.ID)); err != nil {
				return err
			}
		}
	}
	// label index
	oldLabels := map[string]bool{}
	if old != nil {
		for _, l := range old.Issue.Labels {
			oldLabels[l] = true
		}
	}
	newLabels := map[string]bool{}
	for _, l := range rec.Issue.Labels {
		newLabels[l] = true
		if !oldLabels[l] {
			if err := w.set(labelKey(l, rec.Issue.ID), nil); err != nil {
				return err
			}
		}
	}
	for l := range oldLabels {
		if !newLabels[l] {
			if err := w.del(labelKey(l, rec.Issue.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteRecord removes the row and every index entry and child row it owns:
// labels, comments, events, its lease, its child counter, and every edge in
// either direction.
func deleteRecord(w kv, rec *issueRecord) error {
	id := rec.Issue.ID
	if err := w.del(issueKey(id)); err != nil {
		return err
	}
	if rec.Issue.ExternalRef != nil && *rec.Issue.ExternalRef != "" {
		if err := w.del(extRefKey(*rec.Issue.ExternalRef)); err != nil {
			return err
		}
	}
	for _, l := range rec.Issue.Labels {
		if err := w.del(labelKey(l, id)); err != nil {
			return err
		}
	}
	for _, prefix := range [][]byte{commentPrefix(id), eventPrefix(id)} {
		if err := deletePrefix(w, prefix); err != nil {
			return err
		}
	}
	if err := w.del(leaseKey(id)); err != nil {
		return err
	}
	if err := w.del(seqKey("child" + sep + id)); err != nil {
		return err
	}
	return deleteEdgesOf(w, id)
}

// deletePrefix deletes every key under prefix. Keys are collected first so the
// iterator never sees its own deletions.
func deletePrefix(w kv, prefix []byte) error {
	var keys [][]byte
	err := w.scan(prefix, func(k, _ []byte) (bool, error) {
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
	return nil
}

// scanRecords calls fn for every row, in id order, until fn returns false.
func scanRecords(r kv, fn func(rec *issueRecord) (bool, error)) error {
	return r.scan([]byte(nsIssue), func(_, v []byte) (bool, error) {
		rec, err := decodeRecord(v)
		if err != nil {
			return false, err
		}
		return fn(rec)
	})
}

// --- counters -------------------------------------------------------------------

func nextSeq(w kv, name string) (int64, error) {
	key := seqKey(name)
	data, ok, err := w.get(key)
	if err != nil {
		return 0, err
	}
	var n int64
	if ok {
		n, _ = strconv.ParseInt(string(data), 10, 64)
	}
	n++
	return n, w.set(key, []byte(strconv.FormatInt(n, 10)))
}

func readSeq(r kv, name string) (int64, error) {
	data, ok, err := r.get(seqKey(name))
	if err != nil || !ok {
		return 0, err
	}
	n, _ := strconv.ParseInt(string(data), 10, 64)
	return n, nil
}

// --- leases -----------------------------------------------------------------------

func getLease(r kv, id string) (*lease, error) {
	data, ok, err := r.get(leaseKey(id))
	if err != nil || !ok {
		return nil, err
	}
	return decodeJSON[lease](data)
}

func putLease(w kv, id string, l *lease) error {
	data, err := encodeJSON(l)
	if err != nil {
		return err
	}
	return w.set(leaseKey(id), data)
}

// projectLease copies the lease, when there is one, onto the issue's lease
// fields the way the SQL readers join the leases table in.
func projectLease(r kv, issue *types.Issue) error {
	l, err := getLease(r, issue.ID)
	if err != nil || l == nil {
		return err
	}
	exp, hb := l.ExpiresAt, l.HeartbeatAt
	issue.LeaseExpiresAt = &exp
	issue.HeartbeatAt = &hb
	issue.LeaseGrantedNode = l.Node
	return nil
}

// --- ids ------------------------------------------------------------------------------

// validateID refuses the characters this backend's keys reserve.
func validateID(id string) error {
	if strings.Contains(id, sep) {
		return fmt.Errorf("%w: issue id must not contain a NUL byte", storage.ErrValidation)
	}
	return nil
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
