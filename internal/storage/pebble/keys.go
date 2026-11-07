package pebble

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// sep joins key components. Ids never carry a NUL byte, and neither do
// labels, external refs or config keys, so a component boundary is unambiguous
// and a prefix scan over "<ns><id><sep>" is exactly one row's entries.
const sep = "\x00"

// Key namespaces; see the package doc for the layout.
const (
	nsIssue     = "i/"
	nsExtRef    = "x/"
	nsLabel     = "l/"
	nsDep       = "d/"
	nsRevDep    = "r/"
	nsEvent     = "e/"
	nsGlobalEv  = "g/"
	nsComment   = "c/"
	nsConfig    = "k/"
	nsMetadata  = "m/"
	nsLocalMeta = "n/"
	nsRepoMtime = "t/"
	nsSeq       = "s/"
	nsLease     = "L/"
	nsReady     = "R/"
	nsBlocked   = "B/"
	nsDefer     = "F/"
	nsInternal  = "z/"
	nsJournal   = "J/"
	nsOrder     = "O/"
	nsUpdated   = "U/"
	nsType      = "T/"
	nsStats     = "S/"
)

func issueKey(id string) []byte          { return []byte(nsIssue + id) }
func extRefKey(ref string) []byte        { return []byte(nsExtRef + ref) }
func labelKey(label, id string) []byte   { return []byte(nsLabel + label + sep + id) }
func labelPrefix(label string) []byte    { return []byte(nsLabel + label + sep) }
func depKey(src, dst, typ string) []byte { return []byte(nsDep + src + sep + dst + sep + typ) }
func depPrefix(src string) []byte        { return []byte(nsDep + src + sep) }
func revDepKey(dst, src, typ string) []byte {
	return []byte(nsRevDep + dst + sep + src + sep + typ)
}
func revDepPrefix(dst string) []byte { return []byte(nsRevDep + dst + sep) }
func eventKey(id string, seq int64) []byte {
	return []byte(nsEvent + id + sep + seqString(seq))
}
func eventPrefix(id string) []byte { return []byte(nsEvent + id + sep) }
func globalEventKey(nanos, seq int64) []byte {
	return []byte(nsGlobalEv + nanosString(nanos) + sep + seqString(seq))
}
func commentKey(id string, nanos, seq int64) []byte {
	return []byte(nsComment + id + sep + nanosString(nanos) + sep + seqString(seq))
}
func commentPrefix(id string) []byte  { return []byte(nsComment + id + sep) }
func configKey(key string) []byte     { return []byte(nsConfig + key) }
func metadataKey(key string) []byte   { return []byte(nsMetadata + key) }
func localMetaKey(key string) []byte  { return []byte(nsLocalMeta + key) }
func repoMtimeKey(path string) []byte { return []byte(nsRepoMtime + path) }
func seqKey(name string) []byte       { return []byte(nsSeq + name) }
func leaseKey(id string) []byte       { return []byte(nsLease + id) }
func internalKey(name string) []byte  { return []byte(nsInternal + name) }

// J/<seq> for every events-journal record (journal.go).
func journalKey(seq int64) []byte { return []byte(nsJournal + seqString(seq)) }

// Ready-candidate index: R/<status>/<priority>/<created>/<id> for every open,
// unpinned, not effectively blocked row, so a ready page is a bounded scan in
// sort order. See readyindex.go.
func readyKey(status types.Status, priority int, createdNanos int64, id string) []byte {
	return []byte(nsReady + string(status) + sep + priorityString(priority) + sep + nanosString(createdNanos) + sep + id)
}
func readyStatusPrefix(status types.Status) []byte { return []byte(nsReady + string(status) + sep) }
func readyGroupPrefix(status types.Status, priority int) []byte {
	return []byte(nsReady + string(status) + sep + priorityString(priority) + sep)
}

// B/<id> for every open row whose stored flag is set (the blocked list).
func blockedKey(id string) []byte { return []byte(nsBlocked + id) }

// F/<defer_until>/<id> for every row with a defer_until (the deferred-parent
// set of the ready predicate).
func deferKey(deferNanos int64, id string) []byte {
	return []byte(nsDefer + nanosString(deferNanos) + sep + id)
}
func deferFrom(nanos int64) []byte { return []byte(nsDefer + nanosString(nanos)) }

// priorityString renders a priority so that byte order is numeric order,
// with an offset so the P0..P4 range and any negative value sort correctly.
const priorityOffset = 10000

func priorityString(p int) string {
	p += priorityOffset
	if p < 0 {
		p = 0
	}
	if p > 99999 {
		p = 99999
	}
	return fmt.Sprintf("%05d", p)
}

func parsePriority(s string) int {
	n, _ := strconv.Atoi(s)
	return n - priorityOffset
}

// seqString renders a counter so that byte order is numeric order.
func seqString(seq int64) string { return fmt.Sprintf("%016d", seq) }

// nanosString renders a Unix-nanosecond timestamp so that byte order is time
// order; negative values (before 1970) are not expected and are clamped.
func nanosString(nanos int64) string {
	if nanos < 0 {
		nanos = 0
	}
	return fmt.Sprintf("%020d", nanos)
}

// unixNanos is t.UnixNano clamped to the int64 range instead of overflowing,
// so a far-future or pre-1678 stamp still sorts at the right end.
func unixNanos(t time.Time) int64 {
	switch {
	case t.Before(minNanosTime):
		return 0
	case t.After(maxNanosTime):
		return math.MaxInt64
	}
	return t.UnixNano()
}

var (
	minNanosTime = time.Unix(0, 0).UTC()
	maxNanosTime = time.Unix(0, math.MaxInt64).UTC()
)

// prefixEnd returns the first key after every key with the given prefix, or
// nil when there is none (a prefix of all 0xff bytes).
func prefixEnd(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xff {
			upper[i]++
			return upper[:i+1]
		}
	}
	return nil
}

func parseSeq(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimLeft(s, "0"), 10, 64)
	return n
}

// splitKey returns the components after the namespace of key.
func splitKey(ns string, key []byte) []string {
	return strings.Split(strings.TrimPrefix(string(key), ns), sep)
}
