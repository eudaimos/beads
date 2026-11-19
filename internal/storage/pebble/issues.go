package pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/idgen"
	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// --- create -----------------------------------------------------------------------

// createIssue is the shared body of every create path. With opts.CreateOnly
// an occupied id is refused (ErrAlreadyExists); otherwise the raw create is
// an upsert, as on the SQL backends: an existing row takes the snapshot's
// content and keeps its blocked flag and row version (+1). The returned
// flags report whether a row was written and whether it was new.
func (t *txn) createIssue(issue *types.Issue, actor string, opts storage.BatchCreateOptions) (written bool, err error) {
	if issue == nil {
		return false, fmt.Errorf("issue must not be nil")
	}
	if err := validateID(issue.ID); err != nil {
		return false, err
	}
	infra := infraTypes(t.kv)
	wisp := isWispPlane(issue, infra)
	if wisp && !issue.NoHistory {
		issue.Ephemeral = true
	}
	statuses, err := customStatuses(t.kv)
	if err != nil {
		return false, err
	}
	statusNames := make([]string, 0, len(statuses))
	for _, st := range statuses {
		statusNames = append(statusNames, st.Name)
	}
	typeNames, err := customTypes(t.kv)
	if err != nil {
		return false, err
	}
	if err := storageissueops.PrepareIssueForInsert(issue, statusNames, typeNames); err != nil {
		return false, err
	}
	if issue.ID == "" {
		id, err := t.mintID(issue, actor)
		if err != nil {
			return false, err
		}
		issue.ID = id
	} else if !opts.SkipPrefixValidation && !opts.CreateOnly {
		// The raw create accepts any id; prefix validation is a role concern.
	}
	old, err := getRecord(t.kv, issue.ID)
	if err != nil && !isNotFound(err) {
		return false, err
	}
	if old != nil {
		if opts.ConflictSkip {
			return false, nil
		}
		if opts.CreateOnly {
			return false, fmt.Errorf("%w: %s", storage.ErrAlreadyExists, issue.ID)
		}
		if opts.RejectStaleUpserts && old.Issue.UpdatedAt.After(issue.UpdatedAt) {
			if opts.OnStaleRejected != nil {
				opts.OnStaleRejected(issue.ID)
			}
			return false, nil
		}
	}
	keepStored := opts.RejectStaleUpserts && old != nil && !issue.UpdatedAt.After(old.Issue.UpdatedAt)
	plane := planeIssues
	if wisp {
		plane = planeWisps
	}
	rec := &issueRecord{Issue: *issue, Plane: plane, ContentHash: issue.ContentHash, SourceRepo: issue.SourceRepo, IDPrefix: issue.IDPrefix}
	rec.Issue.Labels = dedupeSorted(issue.Labels)
	if keepStored {
		// Equal timestamps keep every stored column (the SQL upsert's
		// IF(VALUES(updated_at) > stored.updated_at) guard); only the
		// snapshot's labels merge in.
		merged := *old
		merged.Issue.Labels = dedupeSorted(append(append([]string(nil), old.Issue.Labels...), issue.Labels...))
		rec = &merged
	}
	if old != nil {
		rec.Issue.IsBlocked = old.Issue.IsBlocked
		// The stamp is storage's own (journal.go): an upsert keeps the one
		// the row carries, and PrepareIssueForInsert dropped any the
		// caller supplied.
		rec.Issue.ArrivalSeq = old.Issue.ArrivalSeq
		rec.RowVersion = old.RowVersion + 1
		rec.Plane = old.Plane
	} else {
		rec.Issue.IsBlocked = false
		rec.RowVersion = 1
	}
	if err := putRecord(t.kv, old, rec); err != nil {
		return false, err
	}
	isNew := old == nil
	if isNew {
		if err := t.recordEvent(rec, types.EventCreated, actor, strPtr(""), strPtr(""), nil); err != nil {
			return false, err
		}
		if parent, n, ok := storageissueops.ParseHierarchicalID(issue.ID); ok && n > 0 {
			if err := t.raiseChildCounter(parent, n); err != nil {
				return false, err
			}
		}
	}
	// The journal records every landed write as a create, an upsert of an
	// existing row included (the SQL seam's finishCreateIssueInTx), and the
	// create record hands the row's arrival stamp back onto the caller's
	// struct.
	if err := t.journalIssue(storageissueops.EventCreate, issue.ID, actor); err != nil {
		return false, err
	}
	if t.journal {
		stamped, err := getRecord(t.kv, issue.ID)
		if err != nil {
			return false, err
		}
		issue.ArrivalSeq = stamped.Issue.ArrivalSeq
	}
	for _, c := range issue.Comments {
		if c == nil {
			continue
		}
		at := c.CreatedAt
		if at.IsZero() {
			at, err = nextLiveCommentTime(t.kv, issue.ID, nowUTC())
			if err != nil {
				return false, err
			}
		}
		stored, err := t.insertComment(rec, c.Author, c.Text, at)
		if err != nil {
			return false, err
		}
		c.ID, c.CreatedAt = stored.ID, stored.CreatedAt
		if err := t.journalComment(issue.ID, &storageissueops.EventComment{
			ID: stored.ID, Author: stored.Author, Text: stored.Text, CreatedAt: stored.CreatedAt,
			Source: storageissueops.CommentSourceStructured,
		}); err != nil {
			return false, err
		}
	}
	issue.RowVersion = rec.RowVersion
	issue.Labels = rec.Issue.Labels
	t.markBlockedDirty(issue.ID)
	return true, nil
}

// persistCreateDependencies writes the inline dependencies a create carries,
// after every row of the batch exists.
func (t *txn) persistCreateDependencies(issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	for _, issue := range issues {
		for _, dep := range issue.Dependencies {
			if dep == nil {
				continue
			}
			d := *dep
			if d.IssueID == "" {
				d.IssueID = issue.ID
			}
			err := t.addEdge(&d, actor, storage.DependencyAddOptions{SkipCycleCheck: false})
			if err != nil {
				// A missing target is skipped silently, as the SQL batch path
				// does; other refusals are skipped only when the options say so.
				if isNotFound(err) || opts.SkipDependencyValidationErrors {
					if opts.OnSkippedDependency != nil {
						opts.OnSkippedDependency(d.IssueID, d.DependsOnID, err.Error())
					}
					continue
				}
				return err
			}
			dep.ID, dep.CreatedAt, dep.CreatedBy = d.ID, d.CreatedAt, d.CreatedBy
		}
	}
	return nil
}

// mintID generates an id for a create without one, the way the SQL backends
// do (issueops.assignCreateIssueIDInTx and GenerateIssueIDInTable): the
// prefix is the issue's override, else the configured prefix extended by the
// issue's sub-prefix or, for a wisp, by "-wisp"; a durable row under
// issue_id_mode=counter takes the prefix's next counter value; anything else
// takes a content hash at the adaptive length, grown until it is free.
func (t *txn) mintID(issue *types.Issue, actor string) (string, error) {
	configPrefix, err := getConfig(t.kv, "issue_prefix")
	if err != nil {
		return "", err
	}
	wisp := storageissueops.IsWisp(issue)
	prefix := configPrefix
	switch {
	case issue.PrefixOverride != "":
		prefix = issue.PrefixOverride
	case issue.IDPrefix != "":
		prefix = configPrefix + "-" + issue.IDPrefix
	case wisp:
		prefix = configPrefix + "-wisp"
	}
	if prefix == "" {
		return "", fmt.Errorf("%w: issue_prefix is not configured and the issue has no id", storage.ErrNotInitialized)
	}
	if !wisp {
		mode, err := getConfig(t.kv, "issue_id_mode")
		if err != nil {
			return "", err
		}
		if mode == "counter" {
			return t.nextCounterID(prefix)
		}
	}
	baseLength, err := adaptiveIDLength(t.kv, prefix)
	if err != nil {
		return "", err
	}
	const maxLength = 8
	if baseLength > maxLength {
		baseLength = maxLength
	}
	for length := baseLength; length <= maxLength; length++ {
		for nonce := 0; nonce < 10; nonce++ {
			candidate := idgen.GenerateHashID(prefix, issue.Title, issue.Description, actor, issue.CreatedAt, length, nonce)
			if t.claimedIDs[candidate] {
				continue
			}
			if ok, err := recordExists(t.kv, candidate); err != nil {
				return "", err
			} else if !ok {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("failed to generate unique ID after trying lengths %d-%d with 10 nonces each", baseLength, maxLength)
}

// maxCounterIDSkips bounds how many occupied or claimed counter values one
// create walks past (issueops.maxRetiredCounterSkips).
const maxCounterIDSkips = 1000

// nextCounterID is issueops.NextCounterIDTx over the key space: the
// prefix's counter, seeded on first use from the highest numeric suffix a
// top-level row under the prefix already carries, advanced past the values
// the batch has claimed or a pinned row occupies. The counter only ever
// advances, so a skipped value is never offered again.
func (t *txn) nextCounterID(prefix string) (string, error) {
	name := "counter" + sep + prefix
	if _, seeded, err := t.kv.get(seqKey(name)); err != nil {
		return "", err
	} else if !seeded {
		high, err := highestCounterSuffix(t.kv, prefix)
		if err != nil {
			return "", err
		}
		if high > 0 {
			if err := t.kv.set(seqKey(name), []byte(strconv.Itoa(high))); err != nil {
				return "", err
			}
		}
	}
	for skips := 0; ; skips++ {
		n, err := nextSeq(t.kv, name)
		if err != nil {
			return "", err
		}
		candidate := fmt.Sprintf("%s-%d", prefix, n)
		if !t.claimedIDs[candidate] {
			taken, err := recordExists(t.kv, candidate)
			if err != nil {
				return "", err
			}
			if !taken {
				return candidate, nil
			}
		}
		if skips >= maxCounterIDSkips {
			return "", fmt.Errorf("issue counter for prefix %q walked past %d occupied or claimed ids without finding a free one", prefix, maxCounterIDSkips)
		}
	}
}

// topLevelIDs calls fn with the suffix of every row id of the form
// <prefix>-<suffix> whose suffix carries no dot (a child id), reading keys
// only, until fn returns false.
func topLevelIDs(r kv, prefix string, fn func(suffix string) bool) error {
	lead := nsIssue + prefix + "-"
	return r.scan([]byte(lead), func(k, _ []byte) (bool, error) {
		suffix := strings.TrimPrefix(string(k), lead)
		if strings.Contains(suffix, ".") {
			return true, nil
		}
		return fn(suffix), nil
	})
}

// highestCounterSuffix is issueops.SeedCounterFromExistingIssuesTx's scan:
// the largest numeric suffix among the top-level rows under prefix.
func highestCounterSuffix(r kv, prefix string) (int, error) {
	high := 0
	err := topLevelIDs(r, prefix, func(suffix string) bool {
		if n, err := strconv.Atoi(suffix); err == nil && n > high {
			high = n
		}
		return true
	})
	return high, err
}

// adaptiveIDLength is issueops.GetAdaptiveIDLengthTx over the key space:
// the shortest hash length whose birthday-collision probability over the
// top-level rows under prefix stays under the configured bound. The count
// stops once it passes the largest threshold the bound can move.
func adaptiveIDLength(r kv, prefix string) (int, error) {
	cfg := storageissueops.DefaultAdaptiveConfig()
	if v, err := getConfig(r, "max_collision_prob"); err != nil {
		return 0, err
	} else if v != "" {
		if prob, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxCollisionProbability = prob
		}
	}
	if v, err := getConfig(r, "min_hash_length"); err != nil {
		return 0, err
	} else if v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MinLength = n
		}
	}
	if v, err := getConfig(r, "max_hash_length"); err != nil {
		return 0, err
	} else if v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxLength = n
		}
	}
	// Past this many rows every length up to the maximum is already chosen,
	// so the count can stop.
	const countCeiling = 1 << 20
	count := 0
	err := topLevelIDs(r, prefix, func(string) bool {
		count++
		return count < countCeiling
	})
	if err != nil {
		return 0, err
	}
	return storageissueops.ComputeAdaptiveLength(count, cfg), nil
}

// raiseChildCounter records that child number n exists under parent. A
// parent with no row gets no counter (ReconcileSkipsMissingParentCounter).
func (t *txn) raiseChildCounter(parent string, n int) error {
	if ok, err := recordExists(t.kv, parent); err != nil || !ok {
		return err
	}
	name := "child" + sep + parent
	cur, err := readSeq(t.kv, name)
	if err != nil {
		return err
	}
	if int64(n) <= cur {
		return nil
	}
	return t.kv.set(seqKey(name), []byte(strconv.Itoa(n)))
}

func dedupeSorted(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	stableSortBy(out, func(a, b string) bool { return a < b })
	return out
}

func (s *Store) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return s.run(ctx, func(t *txn) error {
		if _, err := t.createIssue(issue, actor, storage.BatchCreateOptions{SkipPrefixValidation: true}); err != nil {
			return err
		}
		if err := t.persistCreateDependencies([]*types.Issue{issue}, actor, storage.BatchCreateOptions{}); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

func (s *Store) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	return s.CreateIssuesWithFullOptions(ctx, issues, actor, storage.BatchCreateOptions{})
}

func (s *Store) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	if len(issues) == 0 {
		return nil
	}
	if !opts.SkipDependencyValidationErrors {
		if err := validateMixedBucketDependencies(issues); err != nil {
			return err
		}
	}
	return s.run(ctx, func(t *txn) error {
		var accepted []*types.Issue
		for _, issue := range issues {
			written, err := t.createIssue(issue, actor, opts)
			if err != nil {
				return err
			}
			if written {
				accepted = append(accepted, issue)
			}
		}
		if err := t.persistCreateDependencies(accepted, actor, opts); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// validateMixedBucketDependencies mirrors the SQL batch rule: a batch that
// mixes durable and wisp rows may not carry an inline edge across the two.
func validateMixedBucketDependencies(issues []*types.Issue) error {
	wispByID := map[string]bool{}
	hasRegular, hasWisp := false, false
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		w := storageissueops.IsWisp(issue)
		hasWisp = hasWisp || w
		hasRegular = hasRegular || !w
		if issue.ID != "" {
			wispByID[issue.ID] = w
		}
	}
	if !hasRegular || !hasWisp {
		return nil
	}
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		for _, dep := range issue.Dependencies {
			if dep == nil {
				continue
			}
			src, srcWisp := issue.ID, storageissueops.IsWisp(issue)
			if dep.IssueID != "" {
				src = dep.IssueID
				if w, ok := wispByID[src]; ok {
					srcWisp = w
				}
			}
			dstWisp, ok := wispByID[dep.DependsOnID]
			if !ok || srcWisp == dstWisp {
				continue
			}
			return fmt.Errorf("mixed regular/wisp CreateIssues batch cannot include cross-bucket dependency %s -> %s; create the issues first, then add the in-batch dependency after both issues exist", src, dep.DependsOnID)
		}
	}
	return nil
}

// --- read --------------------------------------------------------------------------

// loadIssue returns a detached issue with its lease and dependency records.
func loadIssue(r kv, id string) (*types.Issue, error) {
	rec, err := getRecord(r, id)
	if err != nil {
		return nil, err
	}
	issue := rec.toIssue()
	if err := projectLease(r, issue); err != nil {
		return nil, err
	}
	deps, err := outgoingEdges(r, id)
	if err != nil {
		return nil, err
	}
	issue.Dependencies = deps
	return issue, nil
}

func (s *Store) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	var out *types.Issue
	err := s.read(func(r kv) error {
		v, err := loadIssue(r, id)
		out = v
		return err
	})
	return out, err
}

func (s *Store) GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	var out []*types.Issue
	err := s.read(func(r kv) error {
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			issue, err := loadIssue(r, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			out = append(out, issue)
		}
		return nil
	})
	return out, err
}

func (s *Store) GetIssueByExternalRef(ctx context.Context, externalRef string) (*types.Issue, error) {
	var out *types.Issue
	err := s.read(func(r kv) error {
		id, ok, err := getString(r, extRefKey(externalRef))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: external_ref %s", storage.ErrNotFound, externalRef)
		}
		v, err := loadIssue(r, id)
		out = v
		return err
	})
	return out, err
}

// --- update -----------------------------------------------------------------------

// updateIssue applies an updates map to a row the way updateIssueInTx does:
// no-op fields are discarded, closed_at / started_at / lease follow the
// status change, the event type is derived, and the row version is minted.
func (t *txn) updateIssue(id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) (bool, error) {
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return false, err
	}
	oldIssue := rec.toIssue()
	if opts.ExpectedVersion != nil && *opts.ExpectedVersion != rec.RowVersion {
		return false, fmt.Errorf("%w: issue %s is at version %d, expected %d", storage.ErrVersionMismatch, id, rec.RowVersion, *opts.ExpectedVersion)
	}
	if opts.ExpectedAssignee != nil && *opts.ExpectedAssignee != oldIssue.Assignee {
		return false, fmt.Errorf("%w: issue %s is assigned to %q, expected %q", storage.ErrAssigneeMismatch, id, oldIssue.Assignee, *opts.ExpectedAssignee)
	}
	if opts.ExpectedStatus != nil && *opts.ExpectedStatus != string(oldIssue.Status) {
		return false, fmt.Errorf("%w: issue %s is %q, expected %q", storage.ErrStatusMismatch, id, oldIssue.Status, *opts.ExpectedStatus)
	}
	updates = cloneUpdates(updates)
	forceClosePolicy := storageissueops.PopForceClosePolicy(updates)
	updates, err = storageissueops.ResolveMergeOps(oldIssue, updates)
	if err != nil {
		return false, err
	}
	if err := storageissueops.ValidateClosedAtCoherence(oldIssue, updates); err != nil {
		return false, err
	}
	updates, err = storageissueops.DiscardNoopIssueUpdates(oldIssue, updates)
	if err != nil {
		return false, err
	}
	if len(updates) == 0 {
		return false, nil
	}
	for key := range updates {
		if !storageissueops.IsAllowedUpdateField(key) {
			return false, fmt.Errorf("invalid field for update: %s", key)
		}
	}
	if raw, ok := updates["status"]; ok && !forceClosePolicy {
		if cat, err := statusCategory(t.kv, types.Status(statusString(raw))); err != nil {
			return false, err
		} else if cat == types.CategoryDone && open(oldIssue.Status) {
			if _, err := enforceClosePolicy(t.kv, id, false, false); err != nil {
				return false, err
			}
		}
	}
	old := *rec
	now := nowUTC()
	newIssue := rec.Issue
	for key, value := range updates {
		if err := applyUpdateField(&newIssue, key, value); err != nil {
			return false, err
		}
	}
	if rawStatus, ok := updates["status"]; ok {
		newStatus := statusString(rawStatus)
		if oldIssue.Pinned && newStatus != string(types.StatusPinned) {
			if _, explicit := updates["pinned"]; !explicit {
				newIssue.Pinned = false
			}
		}
		if newStatus == string(types.StatusClosed) {
			if _, explicit := updates["closed_at"]; !explicit {
				newIssue.ClosedAt = &now
			}
			if _, explicit := updates["close_reason"]; !explicit {
				newIssue.CloseReason = ""
			}
			if _, explicit := updates["closed_by_session"]; !explicit {
				newIssue.ClosedBySession = ""
			}
		} else if oldIssue.Status == types.StatusClosed {
			if _, explicit := updates["closed_at"]; !explicit {
				newIssue.ClosedAt = nil
			}
			if _, explicit := updates["close_reason"]; !explicit {
				newIssue.CloseReason = ""
			}
			if _, explicit := updates["closed_by_session"]; !explicit {
				newIssue.ClosedBySession = ""
			}
		}
		if _, explicit := updates["started_at"]; !explicit && newStatus == string(types.StatusInProgress) && oldIssue.StartedAt == nil {
			newIssue.StartedAt = &now
		}
	}
	newIssue.UpdatedAt = now
	rec.Issue = newIssue
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := putRecord(t.kv, &old, rec); err != nil {
		return false, err
	}
	if storageissueops.ManageLeaseOnUpdate(oldIssue, updates) {
		if err := t.kv.del(leaseKey(id)); err != nil {
			return false, err
		}
	}
	oldData, _ := json.Marshal(oldIssue)
	newData, _ := json.Marshal(updates)
	eventType := storageissueops.DetermineEventType(oldIssue, updates)
	if err := t.recordEvent(rec, eventType, actor, strPtr(string(oldData)), strPtr(string(newData)), nil); err != nil {
		return false, err
	}
	if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
		return false, err
	}
	if _, ok := updates["status"]; ok {
		t.markBlockedDirty(id)
	}
	return true, nil
}

func cloneUpdates(updates map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		out[k] = v
	}
	return out
}

func statusString(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case types.Status:
		return string(s)
	case *string:
		if s != nil {
			return *s
		}
	}
	return ""
}

func asString(v interface{}) (string, bool) {
	switch s := v.(type) {
	case nil:
		return "", true
	case string:
		return s, true
	case *string:
		if s == nil {
			return "", true
		}
		return *s, true
	case types.Status:
		return string(s), true
	case types.IssueType:
		return string(s), true
	case types.MolType:
		return string(s), true
	case types.WispType:
		return string(s), true
	case fmt.Stringer:
		return s.String(), true
	}
	return "", false
}

func asTime(v interface{}) (*time.Time, bool) {
	switch x := v.(type) {
	case nil:
		return nil, true
	case time.Time:
		u := x.UTC()
		return &u, true
	case *time.Time:
		if x == nil {
			return nil, true
		}
		u := x.UTC()
		return &u, true
	case string:
		if x == "" {
			return nil, true
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, auxTimeLayout} {
			if parsed, err := time.Parse(layout, x); err == nil {
				u := parsed.UTC()
				return &u, true
			}
		}
	}
	return nil, false
}

func asInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case int32:
		return int(x), true
	case float64:
		return int(x), true
	case *int:
		if x == nil {
			return 0, true
		}
		return *x, true
	case string:
		n, err := strconv.Atoi(x)
		return n, err == nil
	}
	return 0, false
}

func asBool(v interface{}) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case *bool:
		if x == nil {
			return false, true
		}
		return *x, true
	case int:
		return x != 0, true
	case int64:
		return x != 0, true
	case string:
		b, err := strconv.ParseBool(x)
		return b, err == nil
	}
	return false, false
}

// applyUpdateField sets one updates-map field on the issue, with the value
// coercions the SQL path gets from the driver for free.
func applyUpdateField(issue *types.Issue, key string, value interface{}) error {
	bad := func() error { return fmt.Errorf("%w: invalid value for %s: %v", storage.ErrValidation, key, value) }
	str := func(dst *string) error {
		s, ok := asString(value)
		if !ok {
			return bad()
		}
		*dst = s
		return nil
	}
	tm := func(dst **time.Time) error {
		t, ok := asTime(value)
		if !ok {
			return bad()
		}
		*dst = t
		return nil
	}
	switch key {
	case "title":
		return str(&issue.Title)
	case "description":
		return str(&issue.Description)
	case "design":
		return str(&issue.Design)
	case "acceptance_criteria":
		return str(&issue.AcceptanceCriteria)
	case "notes":
		return str(&issue.Notes)
	case "spec_id":
		return str(&issue.SpecID)
	case "status":
		s, ok := asString(value)
		if !ok {
			return bad()
		}
		issue.Status = types.Status(s)
	case "priority":
		n, ok := asInt(value)
		if !ok {
			return bad()
		}
		issue.Priority = n
	case "issue_type":
		s, ok := asString(value)
		if !ok {
			return bad()
		}
		issue.IssueType = types.IssueType(s)
	case "assignee":
		return str(&issue.Assignee)
	case "owner":
		return str(&issue.Owner)
	case "estimated_minutes":
		if value == nil {
			issue.EstimatedMinutes = nil
			return nil
		}
		if p, isPtr := value.(*int); isPtr && p == nil {
			issue.EstimatedMinutes = nil
			return nil
		}
		n, ok := asInt(value)
		if !ok {
			return bad()
		}
		issue.EstimatedMinutes = &n
	case "closed_at":
		return tm(&issue.ClosedAt)
	case "close_reason":
		return str(&issue.CloseReason)
	case "closed_by_session":
		return str(&issue.ClosedBySession)
	case "started_at":
		return tm(&issue.StartedAt)
	case "due_at":
		return tm(&issue.DueAt)
	case "defer_until":
		return tm(&issue.DeferUntil)
	case "external_ref":
		if value == nil {
			issue.ExternalRef = nil
			return nil
		}
		s, ok := asString(value)
		if !ok {
			return bad()
		}
		if s == "" {
			issue.ExternalRef = nil
		} else {
			issue.ExternalRef = &s
		}
	case "source_repo":
		return str(&issue.SourceRepo)
	case "sender":
		return str(&issue.Sender)
	case "metadata":
		normalized, err := storage.NormalizeMetadataValue(value)
		if err != nil {
			return fmt.Errorf("invalid metadata: %w", err)
		}
		if normalized == "" {
			issue.Metadata = nil
		} else {
			issue.Metadata = json.RawMessage(normalized)
		}
	case "pinned":
		b, ok := asBool(value)
		if !ok {
			return bad()
		}
		issue.Pinned = b
	case "mol_type":
		s, ok := asString(value)
		if !ok {
			return bad()
		}
		issue.MolType = types.MolType(s)
	case "wisp_type":
		s, ok := asString(value)
		if !ok {
			return bad()
		}
		issue.WispType = types.WispType(s)
	case "no_history":
		b, ok := asBool(value)
		if !ok {
			return bad()
		}
		issue.NoHistory = b
	case "wisp":
		b, ok := asBool(value)
		if !ok {
			return bad()
		}
		issue.Ephemeral = b
	case "await_id":
		return str(&issue.AwaitID)
	case "waiters":
		switch w := value.(type) {
		case []string:
			issue.Waiters = append([]string{}, w...)
		case nil:
			issue.Waiters = nil
		case string:
			var parsed []string
			if err := json.Unmarshal([]byte(w), &parsed); err != nil {
				return bad()
			}
			issue.Waiters = parsed
		default:
			return bad()
		}
	case "event_actor":
		return str(&issue.Actor)
	case "event_category":
		return str(&issue.EventKind)
	case "event_payload":
		return str(&issue.Payload)
	case "event_target":
		return str(&issue.Target)
	default:
		return fmt.Errorf("invalid field for update: %s", key)
	}
	return nil
}

func (s *Store) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return s.UpdateIssueChecked(ctx, id, updates, actor, storage.UpdateIssueOptions{})
}

func (s *Store) UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts storage.UpdateIssueOptions) error {
	return s.run(ctx, func(t *txn) error {
		if _, err := t.updateIssue(id, updates, actor, opts); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

func (s *Store) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	if !types.IssueType(issueType).IsValid() {
		custom, err := s.GetCustomTypes(ctx)
		if err != nil {
			return err
		}
		known := false
		for _, c := range custom {
			if c == issueType {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("%w: invalid issue type %q", storage.ErrValidation, issueType)
		}
	}
	return s.UpdateIssue(ctx, id, map[string]interface{}{"issue_type": issueType}, actor)
}

// --- close / reopen -----------------------------------------------------------

// closeIssue is closeIssueInTx: an already-closed row is a no-op
// (unchanged=true), otherwise the row closes with reason and session, its
// lease goes, and the event is recorded.
func (t *txn) closeIssue(id, reason, actor, session string, expected *int64) (unchanged bool, err error) {
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return false, err
	}
	if expected != nil && *expected != rec.RowVersion {
		return false, fmt.Errorf("%w: issue %s is at version %d, expected %d", storage.ErrVersionMismatch, id, rec.RowVersion, *expected)
	}
	if rec.Issue.Status == types.StatusClosed {
		return true, nil
	}
	old := *rec
	now := nowUTC()
	rec.Issue.Status = types.StatusClosed
	rec.Issue.ClosedAt = &now
	rec.Issue.UpdatedAt = now
	rec.Issue.CloseReason = reason
	rec.Issue.ClosedBySession = session
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := putRecord(t.kv, &old, rec); err != nil {
		return false, err
	}
	if err := t.kv.del(leaseKey(id)); err != nil {
		return false, err
	}
	if err := t.recordEvent(rec, types.EventClosed, actor, strPtr(""), strPtr(reason), nil); err != nil {
		return false, err
	}
	if err := t.journalIssue(storageissueops.EventClose, id, actor); err != nil {
		return false, err
	}
	t.markBlockedDirty(id)
	return false, nil
}

func (s *Store) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	_, err := s.CloseIssueChecked(ctx, id, actor, storage.CloseIssueOptions{Reason: reason, Session: session})
	return err
}

func (s *Store) CloseIssueChecked(ctx context.Context, id string, actor string, opts storage.CloseIssueOptions) (storage.CloseIssueResult, error) {
	var result storage.CloseIssueResult
	err := s.run(ctx, func(t *txn) error {
		if !opts.Force {
			children, err := childrenOf(t.kv, id)
			if err != nil {
				return err
			}
			openChildren := 0
			for _, c := range children {
				if child, err := getRecord(t.kv, c); err == nil && open(child.Issue.Status) {
					openChildren++
				}
			}
			if openChildren > 0 && closePolicyRefusesOpenChildren(t.kv) {
				return fmt.Errorf("%w: %s has %d open children", storage.ErrCloseOpenChildren, id, openChildren)
			}
		}
		unchanged, err := t.closeIssue(id, opts.Reason, actor, opts.Session, opts.ExpectedVersion)
		if err != nil {
			return err
		}
		result.Unchanged = unchanged
		return t.settleBlocked()
	})
	return result, err
}

// closePolicyRefusesOpenChildren reads the close policy; the spike honors
// only the explicit "strict" setting, the SQL backends' default being to
// allow a close over open children.
func closePolicyRefusesOpenChildren(r kv) bool {
	v, _ := getConfig(r, "close.policy")
	return strings.EqualFold(strings.TrimSpace(v), "strict")
}

// reopenIssue is reopenIssueInTx: a row whose status is in the done
// category returns to open, its close fields and defer_until clear, and the
// event (plus a comment carrying the reason) is recorded.
func (t *txn) reopenIssue(id, reason, actor string) (changed bool, err error) {
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return false, err
	}
	if cat, err := statusCategory(t.kv, rec.Issue.Status); err != nil {
		return false, err
	} else if cat != types.CategoryDone {
		return false, nil
	}
	old := *rec
	now := nowUTC()
	rec.Issue.Status = types.StatusOpen
	rec.Issue.ClosedAt = nil
	rec.Issue.CloseReason = ""
	rec.Issue.ClosedBySession = ""
	rec.Issue.DeferUntil = nil
	rec.Issue.UpdatedAt = now
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := putRecord(t.kv, &old, rec); err != nil {
		return false, err
	}
	if err := t.kv.del(leaseKey(id)); err != nil {
		return false, err
	}
	if err := t.recordEvent(rec, types.EventReopened, actor, strPtr(""), strPtr(reason), nil); err != nil {
		return false, err
	}
	if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
		return false, err
	}
	if reason != "" {
		if _, err := t.addLiveComment(id, actor, reason); err != nil {
			return false, err
		}
	}
	t.markBlockedDirty(id)
	return true, nil
}

// statusCategory mirrors issueops.ReopenCategoryInTx.
func statusCategory(r kv, status types.Status) (types.StatusCategory, error) {
	if status.IsValid() {
		return types.BuiltInStatusCategory(status), nil
	}
	customs, err := customStatuses(r)
	if err != nil {
		return types.CategoryUnspecified, err
	}
	for _, c := range customs {
		if c.Name == string(status) {
			return c.Category, nil
		}
	}
	return types.CategoryUnspecified, nil
}

func (s *Store) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	return s.run(ctx, func(t *txn) error {
		if _, err := t.reopenIssue(id, reason, actor); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// --- unclaim ---------------------------------------------------------------------

// unclaim releases a claim the way the SQL path does: a closed row refuses,
// an unassigned row refuses, a foreign holder refuses unless forced (or
// unless the caller named that holder and it matches), and the release
// clears the assignee, the started_at stamp and the lease, then records an
// "unclaimed" event carrying the row before and the fields it reset.
func (t *txn) unclaim(id, actor string, expectedAssignee *string, force bool) error {
	if expectedAssignee != nil && *expectedAssignee == "" {
		return fmt.Errorf("conditional unclaim of %s: expected assignee must not be empty (use UnclaimIssue for an unconditional release)", id)
	}
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return fmt.Errorf("failed to get issue for unclaim: %w", err)
	}
	oldIssue := rec.toIssue()
	if oldIssue.Status == types.StatusClosed {
		return fmt.Errorf("cannot unclaim closed issue %s", id)
	}
	if expectedAssignee != nil {
		if !storageissueops.ActorMatches(oldIssue.Assignee, *expectedAssignee) {
			return fmt.Errorf("%w: %s is held by %q, expected %q", storage.ErrAssigneeMismatch, id, oldIssue.Assignee, *expectedAssignee)
		}
	} else {
		if oldIssue.Assignee == "" {
			return fmt.Errorf("issue %s is not assigned", id)
		}
		if !force && !storageissueops.ActorMatches(oldIssue.Assignee, actor) {
			return fmt.Errorf("%w: %s is held by %s; coordinate with the holder — pass --force only if their claim is abandoned (crashed agent, expired lease)",
				publicops.ErrNotOwner, id, oldIssue.Assignee)
		}
	}
	if oldIssue.Status != types.StatusOpen && oldIssue.Status != types.StatusInProgress {
		return fmt.Errorf("failed to unclaim issue %s: no matching row", id)
	}
	old := *rec
	now := nowUTC()
	rec.Issue.Assignee = ""
	rec.Issue.Status = types.StatusOpen
	rec.Issue.StartedAt = nil
	rec.Issue.UpdatedAt = now
	rec.ContentHash = rec.Issue.ComputeContentHash()
	rec.RowVersion++
	if err := putRecord(t.kv, &old, rec); err != nil {
		return fmt.Errorf("failed to unclaim issue: %w", err)
	}
	if err := t.kv.del(leaseKey(id)); err != nil {
		return err
	}
	oldData, _ := json.Marshal(oldIssue)
	newData, _ := json.Marshal(map[string]interface{}{"assignee": "", "status": "open"})
	if err := t.recordEvent(rec, types.EventType("unclaimed"), actor, strPtr(string(oldData)), strPtr(string(newData)), nil); err != nil {
		return fmt.Errorf("failed to record unclaim event: %w", err)
	}
	if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
		return err
	}
	t.markBlockedDirty(id)
	return nil
}

func (s *Store) UnclaimIssue(ctx context.Context, id string, actor string, force bool) error {
	return s.run(ctx, func(t *txn) error {
		if err := t.unclaim(id, actor, nil, force); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

func (s *Store) UnclaimIssueIfAssignee(ctx context.Context, id string, actor string, expectedAssignee string) error {
	return s.run(ctx, func(t *txn) error {
		if err := t.unclaim(id, actor, &expectedAssignee, false); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// --- delete ----------------------------------------------------------------------

// deleteIssue removes one row with everything it owns. actor is the identity
// whose request removed it, "" for the system surfaces with no request
// behind them; the journal's edge-removal and delete records carry it.
func (t *txn) deleteIssue(id, actor string) error {
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return err
	}
	// Edges are journaled before the row goes, while their source snapshots
	// can still be read.
	if err := t.journalEdgeRemovalsOf([]string{id}, actor); err != nil {
		return err
	}
	ins, err := incomingEdges(t.kv, id)
	if err != nil {
		return err
	}
	for _, d := range ins {
		t.markBlockedDirty(d.IssueID)
	}
	if parent, err := parentOf(t.kv, id); err != nil {
		return err
	} else if parent != "" {
		t.markBlockedDirty(parent)
	}
	if err := deleteRecord(t.kv, rec); err != nil {
		return err
	}
	return t.journalDelete(id, actor)
}

func (s *Store) DeleteIssue(ctx context.Context, id string) error {
	return s.run(ctx, func(t *txn) error {
		// The actorless storage surface: its records carry no actor.
		if err := t.deleteIssue(id, ""); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}
