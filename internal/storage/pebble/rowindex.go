package pebble

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// The row-order indexes and the statistics counters, maintained beside the
// ready index (readyindex.go) by putDerivedIndexes and dropped by
// dropDerivedIndexes, so the list page, the stale read, the epic-closure
// read and the statistics never scan the issues namespace:
//
//	O/<plane>/<priority>/<created>/<id>   every row. One (plane, priority)
//	                                      group is in (created, id) order, so
//	                                      the default list sort (priority,
//	                                      created DESC, id) and the created
//	                                      sort are both bounded merges of the
//	                                      groups (orderindex.go).
//	U/<status>/<updated>/<id>             durable, non-ephemeral rows: the
//	                                      stale read's population, in its
//	                                      order, so a cutoff is a key bound.
//	T/<type>/<id>                         durable rows not closed, so the
//	                                      open epics are one prefix.
//	S/<counter>                           the statistics counts over the
//	                                      durable plane, as decimal text.
//
// The counters are the one derived structure that is not a set of keys: a
// row contributes to a fixed list of them (statsContributions), and a write
// applies the difference between the old row's list and the new one's, so
// every write path that goes through putRecord keeps them exact. They are
// rebuilt with the indexes.

func orderKey(p plane, priority int, createdNanos int64, id string) []byte {
	return []byte(nsOrder + string(p) + sep + priorityString(priority) + sep + nanosString(createdNanos) + sep + id)
}
func orderPlanePrefix(p plane) []byte { return []byte(nsOrder + string(p) + sep) }
func orderGroupPrefix(p plane, priority int) []byte {
	return []byte(nsOrder + string(p) + sep + priorityString(priority) + sep)
}

func updatedKey(status types.Status, updatedNanos int64, id string) []byte {
	return []byte(nsUpdated + string(status) + sep + nanosString(updatedNanos) + sep + id)
}
func updatedStatusPrefix(status types.Status) []byte { return []byte(nsUpdated + string(status) + sep) }

func typeKey(t types.IssueType, id string) []byte { return []byte(nsType + string(t) + sep + id) }
func typePrefix(t types.IssueType) []byte         { return []byte(nsType + string(t) + sep) }

func statsKey(name string) []byte { return []byte(nsStats + name) }

func orderIndexKey(rec *issueRecord) []byte {
	return orderKey(rec.Plane, rec.Issue.Priority, unixNanos(rec.Issue.CreatedAt), rec.Issue.ID)
}

// updatedIndexKey is nil for the rows the stale read never returns: wisps
// and ephemeral rows (issueops.GetStaleIssuesInTx reads the issues table
// and excludes ephemeral = 1).
func updatedIndexKey(rec *issueRecord) []byte {
	if rec.Plane != planeIssues || rec.Issue.Ephemeral {
		return nil
	}
	return updatedKey(rec.Issue.Status, unixNanos(rec.Issue.UpdatedAt), rec.Issue.ID)
}

// typeIndexKey is nil for wisps and for closed rows: the type index answers
// "the open rows of this type" (the epic-closure read), not a census.
func typeIndexKey(rec *issueRecord) []byte {
	if rec.Plane != planeIssues || rec.Issue.Status == types.StatusClosed {
		return nil
	}
	return typeKey(rec.Issue.IssueType, rec.Issue.ID)
}

// The statistics counters. Only durable-plane rows count, as
// issueops.ScanIssueCountsInTx reads the issues table alone.
const (
	statTotal    = "total"
	statStatus   = "status" + sep // + status
	statPinned   = "pinned"
	statGate     = "gate"
	statTemplate = "template"
	statBlocked  = "blocked"
)

// statsContributions lists the counters rec adds one to.
func statsContributions(rec *issueRecord) []string {
	if rec == nil || rec.Plane != planeIssues {
		return nil
	}
	i := &rec.Issue
	out := []string{statTotal, statStatus + string(i.Status)}
	if i.Pinned {
		out = append(out, statPinned)
	}
	if i.IssueType == types.TypeGate {
		out = append(out, statGate)
	}
	if i.IsTemplate {
		out = append(out, statTemplate)
	}
	if i.IsBlocked && open(i.Status) {
		out = append(out, statBlocked)
	}
	return out
}

// applyStatsDelta moves the counters from old's contributions to rec's.
func applyStatsDelta(w kv, old, rec *issueRecord) error {
	before, after := statsContributions(old), statsContributions(rec)
	for _, name := range after {
		if !containsString(before, name) {
			if err := addStat(w, name, 1); err != nil {
				return err
			}
		}
	}
	for _, name := range before {
		if !containsString(after, name) {
			if err := addStat(w, name, -1); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsString(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func addStat(w kv, name string, delta int64) error {
	n, err := readStat(w, name)
	if err != nil {
		return err
	}
	n += delta
	if n < 0 {
		n = 0
	}
	return w.set(statsKey(name), []byte(strconv.FormatInt(n, 10)))
}

func readStat(r kv, name string) (int64, error) {
	data, ok, err := r.get(statsKey(name))
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// rowIndexKeys lists the row-order keys a row owns, nil entries for the
// indexes it is not in, in a fixed order so old and new can be diffed
// position by position.
func rowIndexKeys(rec *issueRecord) [][]byte {
	if rec == nil {
		return make([][]byte, 3)
	}
	return [][]byte{orderIndexKey(rec), updatedIndexKey(rec), typeIndexKey(rec)}
}

// putRowIndexes moves the row-order entries and the counters from old (nil
// for a new row) to rec.
func putRowIndexes(w kv, old, rec *issueRecord) error {
	newKeys := rowIndexKeys(rec)
	for i, oldKey := range rowIndexKeys(old) {
		newKey := newKeys[i]
		if oldKey != nil && !bytes.Equal(oldKey, newKey) {
			if err := w.del(oldKey); err != nil {
				return err
			}
		}
		if newKey != nil && !bytes.Equal(oldKey, newKey) {
			if err := w.set(newKey, nil); err != nil {
				return err
			}
		}
	}
	return applyStatsDelta(w, old, rec)
}

func dropRowIndexEntries(w kv, rec *issueRecord) error {
	for _, k := range rowIndexKeys(rec) {
		if k == nil {
			continue
		}
		if err := w.del(k); err != nil {
			return err
		}
	}
	return applyStatsDelta(w, rec, nil)
}
