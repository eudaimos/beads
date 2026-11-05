package pebble

import (
	"fmt"
	"strconv"
	"strings"
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

func parseSeq(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimLeft(s, "0"), 10, 64)
	return n
}

// splitKey returns the components after the namespace of key.
func splitKey(ns string, key []byte) []string {
	return strings.Split(strings.TrimPrefix(string(key), ns), sep)
}
