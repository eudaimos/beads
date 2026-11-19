package pebble

import (
	"strconv"
	"strings"

	"github.com/cockroachdb/pebble/v2"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// The indexed list page. A search whose sort the O index serves (the default
// priority sort and the created sort) and whose page is bounded walks the
// index in sort order, loads each candidate row, screens it with the same
// matcher the scan uses, and stops when the page is full. A candidate costs
// a point read, so the walk is budgeted: a filter sparse enough to exhaust
// the budget is finished by the scan. An explicit id set or a label set
// names its rows outright; a search with no bound or another sort key takes
// the scan.

// orderEntry is one O-index key, parsed.
type orderEntry struct {
	plane    plane
	priority int
	created  int64
	id       string
}

func parseOrderEntry(key []byte) (orderEntry, bool) {
	parts := splitKey(nsOrder, key)
	if len(parts) != 4 {
		return orderEntry{}, false
	}
	created, _ := strconv.ParseInt(strings.TrimLeft(parts[2], "0"), 10, 64)
	return orderEntry{plane: plane(parts[0]), priority: parsePriority(parts[1]), created: created, id: parts[3]}, true
}

// orderStream yields the entries of one (plane, priority) group in
// (created ASC, id ASC) order, or in (created DESC, id ASC) order when
// desc is set: the key order is (created, id) both ascending, so the
// descending walk buffers each run of equal timestamps and emits it id-first.
type orderStream struct {
	it   *pebble.Iterator
	desc bool
	run  []orderEntry
	head *orderEntry
	err  error
}

func newOrderStream(r kv, lo, hi []byte, desc bool) (*orderStream, error) {
	it, err := r.iter(lo, hi)
	if err != nil {
		return nil, err
	}
	if desc {
		it.Last()
	} else {
		it.First()
	}
	return &orderStream{it: it, desc: desc}, nil
}

// peek returns the next entry without consuming it, or nil at the end or on
// an error (left in s.err).
func (s *orderStream) peek() *orderEntry {
	if s.head != nil || s.err != nil {
		return s.head
	}
	if !s.desc {
		for s.it.Valid() {
			e, ok := parseOrderEntry(s.it.Key())
			if ok {
				s.head = &e
				return s.head
			}
			s.it.Next()
		}
		s.err = s.it.Error()
		return nil
	}
	if len(s.run) == 0 {
		// Gather the run of entries sharing the newest remaining timestamp,
		// walking backwards, then order the run by id.
		for s.it.Valid() {
			e, ok := parseOrderEntry(s.it.Key())
			if !ok {
				s.it.Prev()
				continue
			}
			if len(s.run) > 0 && e.created != s.run[0].created {
				break
			}
			s.run = append(s.run, e)
			s.it.Prev()
		}
		if err := s.it.Error(); err != nil {
			s.err = err
			return nil
		}
		if len(s.run) == 0 {
			return nil
		}
		stableSortBy(s.run, func(a, b orderEntry) bool { return a.id < b.id })
	}
	s.head = &s.run[0]
	return s.head
}

func (s *orderStream) pop() {
	s.head = nil
	if s.desc {
		s.run = s.run[1:]
		return
	}
	s.it.Next()
}

func (s *orderStream) close() error {
	err := s.it.Close()
	if s.err != nil {
		return s.err
	}
	return err
}

// orderGroups lists the (plane, priority) groups present, one seek per
// group.
func orderGroups(r kv, planes []plane) ([]struct {
	plane    plane
	priority int
}, error) {
	type group = struct {
		plane    plane
		priority int
	}
	var out []group
	for _, p := range planes {
		prefix := orderPlanePrefix(p)
		it, err := r.iter(prefix, prefixEnd(prefix))
		if err != nil {
			return nil, err
		}
		for ok := it.First(); ok; {
			e, parsed := parseOrderEntry(it.Key())
			if !parsed {
				ok = it.Next()
				continue
			}
			out = append(out, group{p, e.priority})
			ok = it.SeekGE(prefixEnd(orderGroupPrefix(p, e.priority)))
		}
		if err := it.Error(); err != nil {
			_ = it.Close()
			return nil, err
		}
		if err := it.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// orderIndexServes reports whether the O index yields filter's sort order:
// the default priority sort (priority, created DESC, id) and the created
// sort (created, id), either direction.
func orderIndexServes(sortBy string) bool {
	switch sortBy {
	case "", "priority", "created":
		return true
	}
	return false
}

// orderLess is sqlbuild.Less restricted to the fields the O key carries, for
// the sorts orderIndexServes admits.
func orderLess(sortBy string, sortDesc bool) func(a, b *orderEntry) bool {
	if sortBy == "created" {
		return func(a, b *orderEntry) bool {
			if a.created != b.created {
				if sortDesc {
					return a.created < b.created
				}
				return a.created > b.created
			}
			return a.id < b.id
		}
	}
	return func(a, b *orderEntry) bool {
		if a.priority != b.priority {
			if sortDesc {
				return a.priority > b.priority
			}
			return a.priority < b.priority
		}
		if a.created != b.created {
			return a.created > b.created
		}
		return a.id < b.id
	}
}

// searchPageNeed is the number of rows a bounded search has to collect in
// sort order before boundSearch can cut the page exactly as the scan path
// does: the smaller positive of the effective leg limit and Limit, or 0
// when the search is unbounded.
func searchPageNeed(filter types.IssueFilter) int {
	need := storageissueops.EffectiveSearchLimit(filter.Limit, filter.MaxRows)
	if filter.Limit > 0 && (need == 0 || filter.Limit < need) {
		need = filter.Limit
	}
	if need < 0 {
		return 0
	}
	return need
}

// searchStrategy is how a search reaches its rows.
type searchStrategy int

const (
	// searchByScan walks every row, sorts the matches and cuts the page.
	searchByScan searchStrategy = iota
	// searchByIDs reads the rows an explicit id set names.
	searchByIDs
	// searchByLabel reads the rows one label of the filter's AND set, or
	// the union of its ANY set, names in the label index.
	searchByLabel
	// searchByOrder walks the O index in sort order and stops when the
	// page is full, falling back to the scan when the filter proves sparse.
	searchByOrder
)

// chooseSearchStrategy picks the cheapest route the filter admits. An
// explicit id set and a label set each name their rows outright. The O
// index serves a bounded page in an order it carries, without a keyset
// position (a scan is cheaper than the walk it would need). Everything else
// is a scan.
func chooseSearchStrategy(filter types.IssueFilter) searchStrategy {
	switch {
	case len(filter.IDs) > 0:
		return searchByIDs
	case len(sqlbuild.CompactNonEmptyStrings(filter.Labels)) > 0 || len(sqlbuild.CompactNonEmptyStrings(filter.LabelsAny)) > 0:
		return searchByLabel
	case searchPageNeed(filter) > 0 && orderIndexServes(filter.SortBy) && filter.AfterCreatedAt == nil:
		return searchByOrder
	}
	return searchByScan
}

// orderWalkBudget bounds the candidates an ordered walk examines before a
// page is full. A candidate costs a point read, several times a scanned
// row, so a walk that has examined this many without filling the page is a
// sparse filter, and the scan finishes it faster. A dense filter fills a
// page of fifty within a few hundred. Tests lower it to force the fallback.
var orderWalkBudget = 2048

// orderWalkWarmup is how many candidates a walk examines before it projects,
// from the match rate so far, whether the page will fill inside the budget,
// and gives up early when it will not; so a sparse filter costs a warm-up,
// not the whole budget, before the scan takes over.
const orderWalkWarmup = 128

// walkWillOverrun reports whether, at the observed match rate, filling the
// remaining rows of the page would take more candidates than the budget
// leaves.
func walkWillOverrun(examined, matched, need, budget int) bool {
	if examined < orderWalkWarmup {
		return false
	}
	remaining := need - matched
	// Candidates per match so far, counting one phantom match so a run with
	// none is finite.
	perMatch := examined / (matched + 1)
	return remaining*perMatch > budget-examined
}

// searchRecordsOrdered returns the first need rows the query and filter
// admit, in filter's sort order, read through the O index; complete is false
// when the walk ran out of budget first, and recs is then partial.
func searchRecordsOrdered(r kv, query string, filter types.IssueFilter, need int) (recs []*issueRecord, complete bool, err error) {
	m, err := newIssueMatcher(r, query, filter)
	if err != nil {
		return nil, false, err
	}
	planes := []plane{planeIssues, planeWisps}
	if filter.SkipWisps {
		planes = planes[:1]
	}
	groups, err := orderGroups(r, planes)
	if err != nil {
		return nil, false, err
	}
	// Inside one group the default sort wants created DESC; the created
	// sort wants created in its own direction.
	desc := filter.SortBy != "created" || !filter.SortDesc
	streams := make([]*orderStream, 0, len(groups))
	defer func() {
		for _, s := range streams {
			if cerr := s.close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	}()
	for _, g := range groups {
		prefix := orderGroupPrefix(g.plane, g.priority)
		s, err := newOrderStream(r, prefix, prefixEnd(prefix), desc)
		if err != nil {
			return nil, false, err
		}
		streams = append(streams, s)
	}
	less := orderLess(filter.SortBy, filter.SortDesc)
	budget := orderWalkBudget
	if b := 16 * need; b > budget {
		budget = b
	}
	examined := 0
	for len(recs) < need {
		if examined >= budget || walkWillOverrun(examined, len(recs), need, budget) {
			return recs, false, nil
		}
		var best *orderStream
		for _, s := range streams {
			e := s.peek()
			if s.err != nil {
				return nil, false, s.err
			}
			if e != nil && (best == nil || less(e, best.head)) {
				best = s
			}
		}
		if best == nil {
			break
		}
		examined++
		rec, err := getRecord(r, best.head.id)
		best.pop()
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, false, err
		}
		ok, err := m.match(rec)
		if err != nil {
			return nil, false, err
		}
		if ok {
			recs = append(recs, rec)
		}
	}
	return recs, true, nil
}

// searchRecordsByIDs returns the rows the filter's id set names that the
// query and filter admit, in no particular order.
func searchRecordsByIDs(r kv, query string, filter types.IssueFilter) ([]*issueRecord, error) {
	m, err := newIssueMatcher(r, query, filter)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(filter.IDs))
	var out []*issueRecord
	for _, id := range filter.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		rec, err := getRecord(r, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if filter.SkipWisps && rec.Plane == planeWisps {
			continue
		}
		ok, err := m.match(rec)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// searchRecordsByLabel returns the rows the query and filter admit among
// the rows that carry one label of the filter's AND set (any one narrows the
// candidates; the matcher checks the rest) or, when the AND set is empty,
// any label of its ANY set, in no particular order.
func searchRecordsByLabel(r kv, query string, filter types.IssueFilter) ([]*issueRecord, error) {
	m, err := newIssueMatcher(r, query, filter)
	if err != nil {
		return nil, err
	}
	candidates := sqlbuild.CompactNonEmptyStrings(filter.Labels)
	if len(candidates) > 0 {
		candidates = candidates[:1]
	} else {
		candidates = sqlbuild.CompactNonEmptyStrings(filter.LabelsAny)
	}
	seen := map[string]bool{}
	var out []*issueRecord
	for _, label := range candidates {
		err := r.scan(labelPrefix(label), func(k, _ []byte) (bool, error) {
			parts := splitKey(nsLabel, k)
			if len(parts) != 2 || seen[parts[1]] {
				return true, nil
			}
			seen[parts[1]] = true
			rec, err := getRecord(r, parts[1])
			if err != nil {
				if isNotFound(err) {
					return true, nil
				}
				return false, err
			}
			if filter.SkipWisps && rec.Plane == planeWisps {
				return true, nil
			}
			ok, err := m.match(rec)
			if err != nil {
				return false, err
			}
			if ok {
				out = append(out, rec)
			}
			return true, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sortRecords orders rows by the shared comparator over their stored fields,
// which are every field the comparator reads (sqlbuild.sortFields), so a
// page can be cut before any row is hydrated.
func sortRecords(recs []*issueRecord, sortBy string, sortDesc bool) {
	stableSortBy(recs, func(a, b *issueRecord) bool { return sqlbuild.Less(&a.Issue, &b.Issue, sortBy, sortDesc) })
}

// searchPage returns the rows of the page a search delivers, in order and
// bounded, not yet hydrated, by the strategy chooseSearchStrategy picks. An
// ordered walk that runs out of budget is finished by the scan.
func searchPage(r kv, query string, filter types.IssueFilter) ([]*issueRecord, error) {
	var recs []*issueRecord
	var err error
	sorted := false
	switch chooseSearchStrategy(filter) {
	case searchByIDs:
		recs, err = searchRecordsByIDs(r, query, filter)
	case searchByLabel:
		recs, err = searchRecordsByLabel(r, query, filter)
	case searchByOrder:
		var complete bool
		recs, complete, err = searchRecordsOrdered(r, query, filter, searchPageNeed(filter))
		sorted = complete
		if err == nil && !complete {
			recs, err = searchRecords(r, query, filter)
		}
	default:
		recs, err = searchRecords(r, query, filter)
	}
	if err != nil {
		return nil, err
	}
	if !sorted {
		sortRecords(recs, filter.SortBy, filter.SortDesc)
	}
	return boundSearch(recs, filter)
}
