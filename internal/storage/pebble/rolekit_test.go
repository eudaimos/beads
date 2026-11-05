package pebble

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// roleKit is the pebble counterpart of the Dolt role fixture kit: the hooks
// every role contract shares, over one fresh in-memory store.
//
// The contracts read the backend through a scalar SQL hook because the SQL
// backends expose their tables that way. This store has no tables, so the
// hook answers the handful of query shapes the contracts use by projecting
// the key space into rows (see queryScalar). History hooks are nil: the
// store keeps no commit log, and the cases that count one skip.
type roleKit struct {
	store         *Store
	IssuePrefix   string
	CreateIssue   func(context.Context, *types.Issue, string) error
	CreateWisp    func(context.Context, *types.Issue, string) error
	AddDependency func(context.Context, *types.Dependency, string) error
	SetConfig     func(context.Context, string, string) error
	QueryScalar   func(context.Context, string, []any, ...any) error
	CommitPending func(context.Context) error
	Vocabulary    *conformance.WorkspaceVocabularyReader
}

func newRoleKit(t *testing.T, prefix string) (roleKit, context.Context) {
	t.Helper()
	s, err := OpenInMemory(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetConfig(context.Background(), "issue_prefix", "test"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	kit := roleKit{
		store:       s,
		IssuePrefix: prefix,
		CreateIssue: s.CreateIssue,
		CreateWisp:  s.CreateIssue,
		AddDependency: func(ctx context.Context, dep *types.Dependency, actor string) error {
			return s.AddDependencyWithOptions(ctx, dep, actor, storage.DependencyAddOptions{EmitEvent: true})
		},
		SetConfig:     s.SetConfig,
		CommitPending: func(context.Context) error { return nil },
		Vocabulary: &conformance.WorkspaceVocabularyReader{
			CustomStatuses: s.GetCustomStatusesDetailed,
			CustomTypes:    s.GetCustomTypes,
			InfraTypes: func(ctx context.Context) (map[string]bool, error) {
				return s.GetInfraTypes(ctx), nil
			},
		},
	}
	kit.QueryScalar = kit.queryScalar
	return kit, ctx
}

var (
	execUpdateLeaseRe = regexp.MustCompile(`(?i)^UPDATE leases SET (.+) WHERE issue_id = \?$`)
	execDeleteLeaseRe = regexp.MustCompile(`(?i)^DELETE FROM leases WHERE issue_id = \?$`)
	execInsertLeaseRe = regexp.MustCompile(`(?i)^INSERT INTO leases \(issue_id, holder, granted_at, lease_expires_at, heartbeat_at, granted_node\) VALUES \(\?, \?, \?, \?, \?, ''\)$`)
	execInsertLabelRe = regexp.MustCompile(`(?i)^INSERT INTO labels \(issue_id, label\) VALUES \(\?, \?\)$`)
)

// exec answers the handful of raw write statements the lease contracts use
// to manufacture a state the roles cannot produce (an aged or orphaned lease
// row, a label planted past the roles), the way queryScalar answers their
// reads: by the statement's shape, against the key space.
func (k roleKit) exec(ctx context.Context, statements []conformance.SQLStatement) error {
	for _, stmt := range statements {
		query := strings.Join(strings.Fields(stmt.Query), " ")
		if err := k.store.run(ctx, func(t *txn) error {
			return k.execOne(t, query, stmt.Args)
		}); err != nil {
			return fmt.Errorf("%s: %w", stmt.Query, err)
		}
	}
	return nil
}

func (k roleKit) execOne(t *txn, query string, args []any) error {
	asTime := func(v any) (time.Time, error) {
		switch x := v.(type) {
		case time.Time:
			return x.UTC(), nil
		case string:
			return time.ParseInLocation(sqlTimeLayout, x, time.UTC)
		}
		return time.Time{}, fmt.Errorf("role fixture: %v is not a time", v)
	}
	switch {
	case execUpdateLeaseRe.MatchString(query):
		assignments := splitTop(execUpdateLeaseRe.FindStringSubmatch(query)[1], ',')
		if len(args) != len(assignments)+1 {
			return fmt.Errorf("role fixture: %d args for %d assignments", len(args), len(assignments))
		}
		id, ok := args[len(args)-1].(string)
		if !ok {
			return fmt.Errorf("role fixture: lease issue_id %v is not a string", args[len(args)-1])
		}
		l, err := getLease(t.kv, id)
		if err != nil {
			return err
		}
		if l == nil {
			return nil
		}
		for i, assignment := range assignments {
			col := strings.TrimSpace(strings.SplitN(assignment, "=", 2)[0])
			switch col {
			case "lease_expires_at":
				if l.ExpiresAt, err = asTime(args[i]); err != nil {
					return err
				}
			case "heartbeat_at":
				if l.HeartbeatAt, err = asTime(args[i]); err != nil {
					return err
				}
			case "granted_at":
				if l.GrantedAt, err = asTime(args[i]); err != nil {
					return err
				}
			case "holder":
				l.Holder = stringify(args[i])
			case "granted_node":
				l.Node = stringify(args[i])
			default:
				return fmt.Errorf("role fixture: no lease column %q", col)
			}
		}
		return putLease(t.kv, id, l)
	case execDeleteLeaseRe.MatchString(query):
		return t.kv.del(leaseKey(stringify(args[0])))
	case execInsertLeaseRe.MatchString(query):
		if len(args) != 5 {
			return fmt.Errorf("role fixture: lease insert wants 5 args, got %d", len(args))
		}
		granted, err := asTime(args[2])
		if err != nil {
			return err
		}
		expires, err := asTime(args[3])
		if err != nil {
			return err
		}
		beat, err := asTime(args[4])
		if err != nil {
			return err
		}
		return putLease(t.kv, stringify(args[0]), &lease{
			Holder: stringify(args[1]), GrantedAt: granted, ExpiresAt: expires, HeartbeatAt: beat,
		})
	case execInsertLabelRe.MatchString(query):
		rec, err := getRecord(t.kv, stringify(args[0]))
		if err != nil {
			return err
		}
		old := *rec
		rec.Issue.Labels = dedupeSorted(append(append([]string(nil), rec.Issue.Labels...), stringify(args[1])))
		return putRecord(t.kv, &old, rec)
	}
	return fmt.Errorf("role fixture: unsupported statement %q", query)
}

// sqlRow is one projected table row: column name to value, nil for NULL.
type sqlRow map[string]any

// tableRows projects one of the SQL backends' tables out of the key space.
func (k roleKit) tableRows(r kv, table string) ([]sqlRow, error) {
	var rows []sqlRow
	switch table {
	case "issues", "wisps":
		want := planeIssues
		if table == "wisps" {
			want = planeWisps
		}
		err := scanRecords(r, func(rec *issueRecord) (bool, error) {
			if rec.Plane == want {
				rows = append(rows, issueRow(rec))
			}
			return true, nil
		})
		return rows, err
	case "labels", "wisp_labels":
		want := planeIssues
		if table == "wisp_labels" {
			want = planeWisps
		}
		err := scanRecords(r, func(rec *issueRecord) (bool, error) {
			if rec.Plane != want {
				return true, nil
			}
			for _, label := range rec.Issue.Labels {
				rows = append(rows, sqlRow{"issue_id": rec.Issue.ID, "label": label})
			}
			return true, nil
		})
		return rows, err
	case "dependencies", "wisp_dependencies":
		want := planeIssues
		if table == "wisp_dependencies" {
			want = planeWisps
		}
		err := scanRecords(r, func(rec *issueRecord) (bool, error) {
			if rec.Plane != want {
				return true, nil
			}
			outs, err := outgoingEdges(r, rec.Issue.ID)
			if err != nil {
				return false, err
			}
			for _, dep := range outs {
				row, err := depRow(r, dep)
				if err != nil {
					return false, err
				}
				rows = append(rows, row)
			}
			return true, nil
		})
		return rows, err
	case "events", "wisp_events":
		want := planeIssues
		if table == "wisp_events" {
			want = planeWisps
		}
		err := scanRecords(r, func(rec *issueRecord) (bool, error) {
			if rec.Plane != want {
				return true, nil
			}
			events, err := eventsOf(r, rec.Issue.ID, 0)
			if err != nil {
				return false, err
			}
			for _, ev := range events {
				rows = append(rows, sqlRow{
					"issue_id": ev.IssueID, "event_type": string(ev.EventType), "actor": ev.Actor,
					"old_value": strOrNil(ev.OldValue), "new_value": strOrNil(ev.NewValue), "comment": strOrNil(ev.Comment),
					"created_at": ev.CreatedAt,
				})
			}
			return true, nil
		})
		return rows, err
	case "comments", "wisp_comments":
		want := planeIssues
		if table == "wisp_comments" {
			want = planeWisps
		}
		err := scanRecords(r, func(rec *issueRecord) (bool, error) {
			if rec.Plane != want {
				return true, nil
			}
			comments, err := commentsOf(r, rec.Issue.ID)
			if err != nil {
				return false, err
			}
			for _, c := range comments {
				rows = append(rows, sqlRow{
					"id": c.ID, "issue_id": c.IssueID, "author": c.Author, "text": c.Text, "created_at": c.CreatedAt,
				})
			}
			return true, nil
		})
		return rows, err
	case "bd_events_journal":
		err := scanRange(r, journalKey(1), prefixEnd([]byte(nsJournal)), func(_, v []byte) (bool, error) {
			row, err := decodeJSON[journalRow](v)
			if err != nil {
				return false, err
			}
			rows = append(rows, sqlRow{"seq": row.Seq, "issue_id": row.IssueID, "op": row.Op, "actor": row.Actor, "ts": row.TS})
			return true, nil
		})
		return rows, err
	case "issue_versions", "wisp_versions":
		return nil, nil
	case "config":
		err := r.scan([]byte(nsConfig), func(key, value []byte) (bool, error) {
			rows = append(rows, sqlRow{"key": strings.TrimPrefix(string(key), nsConfig), "value": string(value)})
			return true, nil
		})
		return rows, err
	case "custom_statuses":
		data, ok, err := r.get(projectionKey(projCustomStatuses))
		if err != nil || !ok {
			return nil, err
		}
		parsed, err := decodeJSON[[]types.CustomStatus](data)
		if err != nil {
			return nil, err
		}
		for _, st := range *parsed {
			rows = append(rows, sqlRow{"name": st.Name, "category": string(st.Category)})
		}
		return rows, nil
	case "custom_types":
		data, ok, err := r.get(projectionKey(projCustomTypes))
		if err != nil || !ok {
			return nil, err
		}
		parsed, err := decodeJSON[[]string](data)
		if err != nil {
			return nil, err
		}
		for _, name := range *parsed {
			rows = append(rows, sqlRow{"name": name})
		}
		return rows, nil
	case "leases":
		err := r.scan([]byte(nsLease), func(key, value []byte) (bool, error) {
			l, err := decodeJSON[lease](value)
			if err != nil {
				return false, err
			}
			rows = append(rows, sqlRow{
				"issue_id": strings.TrimPrefix(string(key), nsLease), "holder": l.Holder,
				"granted_at": l.GrantedAt, "lease_expires_at": l.ExpiresAt, "heartbeat_at": l.HeartbeatAt,
				"node": l.Node, "granted_node": l.Node,
			})
			return true, nil
		})
		return rows, err
	}
	return nil, fmt.Errorf("role fixture: no projection for table %q", table)
}

func issueRow(rec *issueRecord) sqlRow {
	i := &rec.Issue
	return sqlRow{
		"id": i.ID, "row_lock": rec.RowVersion, "current_revision": rec.RowVersion, "status": string(i.Status), "assignee": i.Assignee,
		"issue_type": string(i.IssueType), "title": i.Title, "description": i.Description, "notes": i.Notes,
		"design": i.Design, "acceptance_criteria": i.AcceptanceCriteria, "created_by": i.CreatedBy,
		"owner": i.Owner, "spec_id": i.SpecID, "close_reason": i.CloseReason, "closed_by_session": i.ClosedBySession,
		"metadata": nilIfEmpty(string(i.Metadata)), "external_ref": strOrNil(i.ExternalRef),
		"started_at": timeOrNil(i.StartedAt), "updated_at": i.UpdatedAt, "created_at": i.CreatedAt,
		"closed_at": timeOrNil(i.ClosedAt), "due_at": timeOrNil(i.DueAt), "defer_until": timeOrNil(i.DeferUntil),
		"priority": int64(i.Priority), "pinned": i.Pinned, "ephemeral": i.Ephemeral, "is_blocked": i.IsBlocked,
		"await_type": nilIfEmpty(i.AwaitType), "await_id": nilIfEmpty(i.AwaitID), "timeout_ns": int64(i.Timeout),
		"waiters":      waitersColumn(i.Waiters),
		"content_hash": nilIfEmpty(rec.ContentHash), "estimated_minutes": intOrNil(i.EstimatedMinutes),
		"sender": nilIfEmpty(i.Sender), "wisp_type": nilIfEmpty(string(i.WispType)), "is_template": i.IsTemplate,
		"mol_type": nilIfEmpty(string(i.MolType)), "work_type": nilIfEmpty(string(i.WorkType)),
		"source_system": nilIfEmpty(i.SourceSystem), "source_repo": nilIfEmpty(rec.SourceRepo),
	}
}

func intOrNil(v *int) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

func depRow(r kv, dep *types.Dependency) (sqlRow, error) {
	row := sqlRow{
		"issue_id": dep.IssueID, "depends_on_id": dep.DependsOnID, "type": string(dep.Type),
		"metadata": nilIfEmpty(dep.Metadata), "thread_id": nilIfEmpty(dep.ThreadID), "created_by": dep.CreatedBy,
		"depends_on_issue_id": nil, "depends_on_wisp_id": nil, "depends_on_external": nil,
	}
	target, err := getRecord(r, dep.DependsOnID)
	switch {
	case err == nil && target.Plane == planeWisps:
		row["depends_on_wisp_id"] = dep.DependsOnID
	case err == nil:
		row["depends_on_issue_id"] = dep.DependsOnID
	case isNotFound(err):
		row["depends_on_external"] = dep.DependsOnID
	default:
		return nil, err
	}
	return row, nil
}

func waitersColumn(waiters []string) any {
	if waiters == nil {
		return nil
	}
	data, err := json.Marshal(waiters)
	if err != nil {
		return nil
	}
	return string(data)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func timeOrNil(p *time.Time) any {
	if p == nil {
		return nil
	}
	return *p
}

var (
	selectRe = regexp.MustCompile(`(?is)^\s*SELECT\s+(.+?)\s+(FROM\s+.+?)(?:\s+WHERE\s+(.+?))?\s*$`)
	// A trailing ORDER BY, LIMIT and OFFSET, each optional, in that order.
	orderLimitRe = regexp.MustCompile(`(?is)^(.*?)(?:\s+ORDER\s+BY\s+(.+?))?(?:\s+LIMIT\s+(\d+))?(?:\s+OFFSET\s+(\d+))?\s*$`)
	isNullRe     = regexp.MustCompile(`(?is)^(.+?)\s+IS\s+(NOT\s+)?NULL$`)
	inRe         = regexp.MustCompile(`(?is)^(.+?)\s+IN\s*\((.*)\)$`)
	eqRe         = regexp.MustCompile(`(?s)^(.+?)\s*(=|<>|!=)\s*(\?|'(?:[^']|'')*'|-?\d+)$`)
	joinRe       = regexp.MustCompile(`(?i)^FROM (\w+) (\w+) JOIN (\w+) (\w+) ON (\w+)\.(\w+) = (\w+)\.(\w+)$`)
	likeRe       = regexp.MustCompile(`(?is)^(.+?)\s+LIKE\s+(\?|'(?:[^']|'')*')$`)
	// A comparison against a bound argument: col > ?, col <= ?.
	cmpArgRe = regexp.MustCompile(`(?s)^(.+?)\s*(>=|<=|>|<)\s*\?$`)
	// BETWEEN's own AND is rewritten to ~AND~ before the top-level AND split.
	betweenRe     = regexp.MustCompile(`(?i)BETWEEN\s+(\?|'[^']*')\s+AND\s+(\?|'[^']*')`)
	betweenPredRe = regexp.MustCompile(`(?is)^(.+?)\s+BETWEEN\s+(\?|'[^']*')\s+~AND~\s+(\?|'[^']*')$`)
)

// queryScalar answers the contracts' scalar reads over the projected tables:
// SELECT <exprs> FROM <table> [WHERE <pred> AND ...], where an expression is
// COUNT(*), a column, COALESCE, CAST(... AS CHAR), LOWER, IS [NOT] NULL or a
// comparison, and a predicate is `expr = ?`, `expr = 'lit'`, `col IN (...)`
// or `expr IS [NOT] NULL`. It scans the first matching row like database/sql.
func (k roleKit) queryScalar(ctx context.Context, query string, args []any, dest ...any) error {
	query = strings.Join(strings.Fields(query), " ")
	query = betweenRe.ReplaceAllString(query, "BETWEEN $1 ~AND~ $2")
	query, order, limit, offset, err := splitOrderLimit(query)
	if err != nil {
		return err
	}
	if parts, ok := scalarSum(query); ok {
		if len(dest) != 1 {
			return fmt.Errorf("role fixture: a summed select scans one dest, got %d", len(dest))
		}
		var total int64
		for _, part := range parts {
			n := strings.Count(part, "?")
			if n > len(args) {
				return fmt.Errorf("role fixture: summed select %q wants more args than given", query)
			}
			var got int64
			if err := k.queryScalar(ctx, part, args[:n], &got); err != nil {
				return err
			}
			args = args[n:]
			total += got
		}
		return assignScalar(dest[0], total)
	}
	m := selectRe.FindStringSubmatch(query)
	if m == nil {
		return fmt.Errorf("role fixture: unsupported query %q", query)
	}
	exprs := splitTop(m[1], ',')
	from := m[2]
	argc := 0
	nextArg := func() (any, error) {
		if argc >= len(args) {
			return nil, fmt.Errorf("role fixture: query %q wants more than %d args", query, len(args))
		}
		v := args[argc]
		argc++
		return v, nil
	}
	var matchers []func(sqlRow) (bool, error)
	if strings.TrimSpace(m[3]) != "" {
		for _, pred := range splitTopAnd(m[3]) {
			mf, err := compilePredicate(pred, nextArg)
			if err != nil {
				return err
			}
			matchers = append(matchers, mf)
		}
	}
	var rows []sqlRow
	if err := k.store.read(func(r kv) error {
		var err error
		rows, err = k.fromRows(r, from)
		return err
	}); err != nil {
		return err
	}
	var matched []sqlRow
	for _, row := range rows {
		ok := true
		for _, mf := range matchers {
			hit, err := mf(row)
			if err != nil {
				return err
			}
			if !hit {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, row)
		}
	}
	if len(order) > 0 {
		var sortErr error
		sort.SliceStable(matched, func(i, j int) bool {
			for _, term := range order {
				cmp, err := compareValues(matched[i][term.col], matched[j][term.col])
				if err != nil {
					sortErr = err
					return false
				}
				if cmp == 0 {
					continue
				}
				if term.desc {
					return cmp > 0
				}
				return cmp < 0
			}
			return false
		})
		if sortErr != nil {
			return sortErr
		}
	}
	if offset > 0 {
		if offset > len(matched) {
			offset = len(matched)
		}
		matched = matched[offset:]
	}
	if limit >= 0 && limit < len(matched) {
		matched = matched[:limit]
	}
	if col, fallback, ok := maxAggregate(exprs); ok {
		if len(dest) != 1 {
			return fmt.Errorf("role fixture: MAX into %d dests", len(dest))
		}
		var best any
		for _, row := range matched {
			v := row[col]
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			cmp, err := compareValues(v, best)
			if err != nil {
				return err
			}
			if cmp > 0 {
				best = v
			}
		}
		if best == nil {
			best = fallback
		}
		return assignScalar(dest[0], best)
	}
	if len(exprs) == 1 && strings.EqualFold(strings.TrimSpace(exprs[0]), "COUNT(*)") {
		if len(dest) != 1 {
			return fmt.Errorf("role fixture: COUNT(*) into %d dests", len(dest))
		}
		return assignScalar(dest[0], int64(len(matched)))
	}
	if len(matched) == 0 {
		return sql.ErrNoRows
	}
	if len(exprs) != len(dest) {
		return fmt.Errorf("role fixture: %d columns into %d dests for %q", len(exprs), len(dest), query)
	}
	for i, e := range exprs {
		v, err := evalExpr(e, matched[0])
		if err != nil {
			return err
		}
		if err := assignScalar(dest[i], v); err != nil {
			return err
		}
	}
	return nil
}

// maxAggregate recognizes MAX(col) and COALESCE(MAX(col), n), answering the
// column and the value an empty match yields (nil without a COALESCE).
func maxAggregate(exprs []string) (string, any, bool) {
	if len(exprs) != 1 {
		return "", nil, false
	}
	expr := strings.TrimSpace(exprs[0])
	upper := strings.ToUpper(expr)
	var fallback any
	if strings.HasPrefix(upper, "COALESCE(") && strings.HasSuffix(expr, ")") {
		args := splitTop(expr[len("COALESCE("):len(expr)-1], ',')
		if len(args) != 2 {
			return "", nil, false
		}
		expr = strings.TrimSpace(args[0])
		upper = strings.ToUpper(expr)
		if n, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64); err == nil {
			fallback = n
		} else {
			fallback = literal(args[1])
		}
	}
	if !strings.HasPrefix(upper, "MAX(") || !strings.HasSuffix(expr, ")") {
		return "", nil, false
	}
	return strings.Trim(strings.TrimSpace(expr[len("MAX("):len(expr)-1]), "`"), fallback, true
}

// orderTerm is one ORDER BY column.
type orderTerm struct {
	col  string
	desc bool
}

// splitOrderLimit strips a trailing ORDER BY / LIMIT / OFFSET from a query
// and returns them: the terms, the limit (-1 for none) and the offset.
func splitOrderLimit(query string) (string, []orderTerm, int, int, error) {
	m := orderLimitRe.FindStringSubmatch(query)
	if m == nil {
		return query, nil, -1, 0, nil
	}
	var order []orderTerm
	for _, term := range splitTop(m[2], ',') {
		fields := strings.Fields(term)
		if len(fields) == 0 {
			continue
		}
		t := orderTerm{col: strings.Trim(fields[0], "`")}
		if len(fields) > 1 && strings.EqualFold(fields[1], "DESC") {
			t.desc = true
		}
		order = append(order, t)
	}
	limit, offset := -1, 0
	if m[3] != "" {
		n, err := strconv.Atoi(m[3])
		if err != nil {
			return "", nil, 0, 0, err
		}
		limit = n
	}
	if m[4] != "" {
		n, err := strconv.Atoi(m[4])
		if err != nil {
			return "", nil, 0, 0, err
		}
		offset = n
	}
	return strings.TrimSpace(m[1]), order, limit, offset, nil
}

// scalarSum recognizes SELECT (SELECT ...) + (SELECT ...) and returns the
// parenthesized selects to sum.
func scalarSum(query string) ([]string, bool) {
	rest := strings.TrimSpace(query)
	if !strings.HasPrefix(strings.ToUpper(rest), "SELECT ") {
		return nil, false
	}
	rest = strings.TrimSpace(rest[len("SELECT "):])
	if !strings.HasPrefix(rest, "(") {
		return nil, false
	}
	parts := splitTop(rest, '+')
	if len(parts) < 2 {
		return nil, false
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "(") || !strings.HasSuffix(part, ")") {
			return nil, false
		}
		out = append(out, strings.TrimSpace(part[1:len(part)-1]))
	}
	return out, true
}

func compilePredicate(pred string, nextArg func() (any, error)) (func(sqlRow) (bool, error), error) {
	pred = strings.TrimSpace(pred)
	for strings.HasPrefix(pred, "(") && strings.HasSuffix(pred, ")") && len(splitTop(pred[1:len(pred)-1], ')')) >= 1 && balanced(pred[1:len(pred)-1]) {
		pred = strings.TrimSpace(pred[1 : len(pred)-1])
	}
	if alts := splitTopOr(pred); len(alts) > 1 {
		var matchers []func(sqlRow) (bool, error)
		for _, alt := range alts {
			mf, err := compilePredicate(alt, nextArg)
			if err != nil {
				return nil, err
			}
			matchers = append(matchers, mf)
		}
		return func(row sqlRow) (bool, error) {
			for _, mf := range matchers {
				ok, err := mf(row)
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		}, nil
	}
	if ands := splitTopAnd(pred); len(ands) > 1 {
		var matchers []func(sqlRow) (bool, error)
		for _, part := range ands {
			mf, err := compilePredicate(part, nextArg)
			if err != nil {
				return nil, err
			}
			matchers = append(matchers, mf)
		}
		return func(row sqlRow) (bool, error) {
			for _, mf := range matchers {
				ok, err := mf(row)
				if err != nil || !ok {
					return false, err
				}
			}
			return true, nil
		}, nil
	}
	if m := likeRe.FindStringSubmatch(pred); m != nil {
		col := m[1]
		var pattern string
		if m[2] == "?" {
			v, err := nextArg()
			if err != nil {
				return nil, err
			}
			pattern = stringify(v)
		} else {
			pattern = stringify(literal(m[2]))
		}
		re := regexp.MustCompile("^" + strings.NewReplacer("%", ".*", "_", ".").Replace(regexp.QuoteMeta(pattern)) + "$")
		return func(row sqlRow) (bool, error) {
			v, err := evalExpr(col, row)
			if err != nil {
				return false, err
			}
			return v != nil && re.MatchString(stringify(v)), nil
		}, nil
	}
	if m := betweenPredRe.FindStringSubmatch(pred); m != nil {
		expr := m[1]
		bound := func(token string) (any, error) {
			if token == "?" {
				return nextArg()
			}
			return literal(token), nil
		}
		lo, err := bound(m[2])
		if err != nil {
			return nil, err
		}
		hi, err := bound(m[3])
		if err != nil {
			return nil, err
		}
		return func(row sqlRow) (bool, error) {
			v, err := evalExpr(expr, row)
			if err != nil || v == nil {
				return false, err
			}
			below, err := compareValues(v, lo)
			if err != nil {
				return false, err
			}
			above, err := compareValues(v, hi)
			if err != nil {
				return false, err
			}
			return below >= 0 && above <= 0, nil
		}, nil
	}
	if m := isNullRe.FindStringSubmatch(pred); m != nil {
		expr, not := m[1], strings.TrimSpace(m[2]) != ""
		return func(row sqlRow) (bool, error) {
			v, err := evalExpr(expr, row)
			if err != nil {
				return false, err
			}
			return (v == nil) != not, nil
		}, nil
	}
	if m := inRe.FindStringSubmatch(pred); m != nil {
		col := m[1]
		var vals []any
		for _, item := range splitTop(m[2], ',') {
			item = strings.TrimSpace(item)
			if item == "?" {
				v, err := nextArg()
				if err != nil {
					return nil, err
				}
				vals = append(vals, v)
				continue
			}
			vals = append(vals, literal(item))
		}
		return func(row sqlRow) (bool, error) {
			v, err := evalExpr(col, row)
			if err != nil {
				return false, err
			}
			for _, want := range vals {
				if valuesEqual(v, want) {
					return true, nil
				}
			}
			return false, nil
		}, nil
	}
	if m := eqRe.FindStringSubmatch(pred); m != nil {
		left, negate := m[1], m[2] != "="
		var want any
		if m[3] == "?" {
			v, err := nextArg()
			if err != nil {
				return nil, err
			}
			want = v
		} else {
			want = literal(m[3])
		}
		return func(row sqlRow) (bool, error) {
			v, err := evalExpr(left, row)
			if err != nil {
				return false, err
			}
			return valuesEqual(v, want) != negate, nil
		}, nil
	}
	if m := cmpArgRe.FindStringSubmatch(pred); m != nil {
		expr, op := m[1], m[2]
		want, err := nextArg()
		if err != nil {
			return nil, err
		}
		return func(row sqlRow) (bool, error) {
			v, err := evalExpr(expr, row)
			if err != nil || v == nil {
				return false, err
			}
			cmp, err := compareValues(v, want)
			if err != nil {
				return false, err
			}
			switch op {
			case ">=":
				return cmp >= 0, nil
			case "<=":
				return cmp <= 0, nil
			case ">":
				return cmp > 0, nil
			}
			return cmp < 0, nil
		}, nil
	}
	return nil, fmt.Errorf("role fixture: unsupported predicate %q", pred)
}

func evalExpr(expr string, row sqlRow) (any, error) {
	expr = strings.TrimSpace(expr)
	if m := isNullRe.FindStringSubmatch(expr); m != nil {
		v, err := evalExpr(m[1], row)
		if err != nil {
			return nil, err
		}
		return (v == nil) != (strings.TrimSpace(m[2]) != ""), nil
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		if left, right, ok := splitTopOnce(expr, op); ok {
			lv, err := evalExpr(left, row)
			if err != nil {
				return nil, err
			}
			rv, err := evalExpr(right, row)
			if err != nil {
				return nil, err
			}
			cmp, err := compareValues(lv, rv)
			if err != nil {
				return nil, err
			}
			switch op {
			case ">=":
				return cmp >= 0, nil
			case "<=":
				return cmp <= 0, nil
			case ">":
				return cmp > 0, nil
			default:
				return cmp < 0, nil
			}
		}
	}
	upper := strings.ToUpper(expr)
	switch {
	case strings.HasPrefix(upper, "COALESCE(") && strings.HasSuffix(expr, ")"):
		for _, arg := range splitTop(expr[len("COALESCE("):len(expr)-1], ',') {
			v, err := evalExpr(arg, row)
			if err != nil {
				return nil, err
			}
			if v != nil {
				return v, nil
			}
		}
		return nil, nil
	case strings.HasPrefix(upper, "CONCAT(") && strings.HasSuffix(expr, ")"):
		var out strings.Builder
		for _, arg := range splitTop(expr[len("CONCAT("):len(expr)-1], ',') {
			v, err := evalExpr(arg, row)
			if err != nil {
				return nil, err
			}
			if v == nil {
				// MySQL CONCAT is NULL when any argument is.
				return nil, nil
			}
			out.WriteString(stringify(v))
		}
		return out.String(), nil
	case strings.HasPrefix(upper, "CONCAT_WS(") && strings.HasSuffix(expr, ")"):
		args := splitTop(expr[len("CONCAT_WS("):len(expr)-1], ',')
		if len(args) == 0 {
			return nil, fmt.Errorf("role fixture: CONCAT_WS needs a separator")
		}
		sep := stringify(literal(args[0]))
		var parts []string
		for _, arg := range args[1:] {
			v, err := evalExpr(arg, row)
			if err != nil {
				return nil, err
			}
			if v == nil {
				// MySQL CONCAT_WS skips NULLs.
				continue
			}
			parts = append(parts, stringify(v))
		}
		return strings.Join(parts, sep), nil
	case strings.HasPrefix(upper, "CAST(") && strings.HasSuffix(expr, ")"):
		inner := expr[len("CAST(") : len(expr)-1]
		if idx := strings.LastIndex(strings.ToUpper(inner), " AS "); idx >= 0 {
			inner = inner[:idx]
		}
		v, err := evalExpr(inner, row)
		if err != nil || v == nil {
			return nil, err
		}
		return stringify(v), nil
	case strings.HasPrefix(upper, "LOWER(") && strings.HasSuffix(expr, ")"):
		v, err := evalExpr(expr[len("LOWER("):len(expr)-1], row)
		if err != nil || v == nil {
			return nil, err
		}
		return strings.ToLower(stringify(v)), nil
	case strings.HasPrefix(expr, "'"):
		return literal(expr), nil
	}
	if n, err := strconv.ParseInt(expr, 10, 64); err == nil {
		return n, nil
	}
	col := strings.Trim(expr, "`")
	v, ok := row[col]
	if !ok {
		return nil, fmt.Errorf("role fixture: unknown column %q", col)
	}
	return v, nil
}

func literal(s string) any {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2 {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}

// splitTop splits s on sep outside parentheses and quotes.
func splitTop(s string, sep rune) []string {
	var out []string
	depth, quoted := 0, false
	start := 0
	for i, c := range s {
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// balanced reports whether every parenthesis in s closes inside s.
func balanced(s string) bool {
	depth, quoted := 0, false
	for _, c := range s {
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// splitTopOr splits a predicate on top-level OR.
func splitTopOr(s string) []string {
	return splitTopWord(s, " OR ")
}

func splitTopWord(s, word string) []string {
	var out []string
	depth, quoted := 0, false
	start := 0
	upper := strings.ToUpper(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && i > 0 && strings.HasPrefix(upper[i:], word):
			out = append(out, s[start:i])
			start = i + len(word)
			i += len(word) - 1
		}
	}
	return append(out, s[start:])
}

// fromRows resolves a FROM clause: one table, or one table joined to a second
// on a column pair, with every column also reachable under its alias.
func (k roleKit) fromRows(r kv, from string) ([]sqlRow, error) {
	if m := joinRe.FindStringSubmatch(strings.TrimSpace(from)); m != nil {
		left, leftAlias, right, rightAlias := strings.ToLower(m[1]), m[2], strings.ToLower(m[3]), m[4]
		onLeftAlias, onLeftCol, onRightAlias, onRightCol := m[5], m[6], m[7], m[8]
		leftRows, err := k.tableRows(r, left)
		if err != nil {
			return nil, err
		}
		rightRows, err := k.tableRows(r, right)
		if err != nil {
			return nil, err
		}
		leftCol, rightCol := onLeftCol, onRightCol
		if onLeftAlias == rightAlias && onRightAlias == leftAlias {
			leftCol, rightCol = onRightCol, onLeftCol
		}
		var out []sqlRow
		for _, l := range leftRows {
			for _, rr := range rightRows {
				if !valuesEqual(l[leftCol], rr[rightCol]) {
					continue
				}
				joined := sqlRow{}
				for col, v := range l {
					joined[leftAlias+"."+col] = v
				}
				for col, v := range rr {
					joined[rightAlias+"."+col] = v
				}
				out = append(out, joined)
			}
		}
		return out, nil
	}
	fields := strings.Fields(from)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
		return nil, fmt.Errorf("role fixture: unsupported FROM clause %q", from)
	}
	rows, err := k.tableRows(r, strings.ToLower(fields[1]))
	if err != nil {
		return nil, err
	}
	if len(fields) == 3 {
		alias := fields[2]
		for _, row := range rows {
			for col, v := range row {
				row[alias+"."+col] = v
			}
		}
	}
	return rows, nil
}

// splitTopAnd splits a WHERE clause on top-level AND.
func splitTopAnd(s string) []string {
	return splitTopWord(s, " AND ")
}

// splitTopOnce splits expr on the first top-level occurrence of op.
func splitTopOnce(expr, op string) (string, string, bool) {
	depth, quoted := 0, false
	for i := 0; i+len(op) <= len(expr); i++ {
		c := expr[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && expr[i:i+len(op)] == op:
			// A bare ">" must not steal a ">=".
			if op == ">" || op == "<" {
				if i+1 < len(expr) && expr[i+1] == '=' {
					continue
				}
			}
			return expr[:i], expr[i+len(op):], true
		}
	}
	return "", "", false
}

const sqlTimeLayout = "2006-01-02 15:04:05"

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case time.Time:
		return x.UTC().Format(sqlTimeLayout)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	}
	return fmt.Sprint(v)
}

func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if ai, aok := toInt64(a); aok {
		if bi, bok := toInt64(b); bok {
			return ai == bi
		}
	}
	return stringify(a) == stringify(b)
}

func compareValues(a, b any) (int, error) {
	at, aok := a.(time.Time)
	bt, bok := b.(time.Time)
	if aok && bok {
		return at.Compare(bt), nil
	}
	ai, aok := toInt64(a)
	bi, bok := toInt64(b)
	if aok && bok {
		switch {
		case ai < bi:
			return -1, nil
		case ai > bi:
			return 1, nil
		}
		return 0, nil
	}
	return strings.Compare(stringify(a), stringify(b)), nil
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case float64:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func assignScalar(dest any, v any) error {
	switch d := dest.(type) {
	case *int:
		n, ok := toInt64(v)
		if !ok {
			return fmt.Errorf("role fixture: cannot scan %T into *int", v)
		}
		*d = int(n)
	case *int64:
		n, ok := toInt64(v)
		if !ok {
			return fmt.Errorf("role fixture: cannot scan %T into *int64", v)
		}
		*d = n
	case *string:
		if v == nil {
			return errors.New("role fixture: converting NULL to string is unsupported")
		}
		*d = stringify(v)
	case *bool:
		switch x := v.(type) {
		case bool:
			*d = x
		default:
			n, ok := toInt64(v)
			if !ok {
				return fmt.Errorf("role fixture: cannot scan %T into *bool", v)
			}
			*d = n != 0
		}
	case *float64:
		n, ok := toInt64(v)
		if !ok {
			return fmt.Errorf("role fixture: cannot scan %T into *float64", v)
		}
		*d = float64(n)
	case *time.Time:
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("role fixture: cannot scan %T into *time.Time", v)
		}
		*d = t
	case sql.Scanner:
		if v == nil {
			return d.Scan(nil)
		}
		if _, isTime := v.(time.Time); isTime {
			return d.Scan(stringify(v))
		}
		return d.Scan(v)
	default:
		return fmt.Errorf("role fixture: unsupported scan destination %T", dest)
	}
	return nil
}
