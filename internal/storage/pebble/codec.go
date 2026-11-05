package pebble

import (
	"encoding/json"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// plane names the table a row would live in on the SQL backends: the
// durable issues table or the ephemeral wisps table. The distinction still
// matters here because callers observe it (ListWisps, IterWisps, wisp
// routing of promote and rekey, the EphemeralTier filter).
type plane string

const (
	planeIssues plane = "issues"
	planeWisps  plane = "wisps"
)

// issueRecord is the stored form of a row: the public issue plus the fields
// types.Issue keeps off its JSON (content hash, row version, source repo) and
// the plane. Relations (dependencies, comments) are stored under their own
// keys and stripped here; labels stay on the row and are also indexed.
type issueRecord struct {
	Issue       types.Issue `json:"issue"`
	Plane       plane       `json:"plane"`
	ContentHash string      `json:"content_hash,omitempty"`
	RowVersion  int64       `json:"row_version"`
	SourceRepo  string      `json:"source_repo,omitempty"`
	IDPrefix    string      `json:"id_prefix,omitempty"`

	// EffectiveBlocked is the flag the ready predicate honors: is_blocked
	// on a row that carries a ready-affecting edge of its own. A set flag on
	// a row without one is stale and does not hide the row (GH#3268,
	// sqlbuild.OwnReadyAffectingEdgeSQL). putRecord derives it, and the
	// ready-candidate index is keyed on it.
	EffectiveBlocked bool `json:"eff_blocked,omitempty"`
	// WaitersEmpty records a waiters list that was set to the empty list, which
	// the issue's own encoding cannot tell from an unset one; the SQL column
	// holds "[]" for the former and NULL for the latter.
	WaitersEmpty bool `json:"waiters_empty,omitempty"`
}

func encodeRecord(r *issueRecord) ([]byte, error) {
	clone := *r
	clone.Issue.Dependencies = nil
	clone.Issue.Comments = nil
	clone.WaitersEmpty = clone.Issue.Waiters != nil && len(clone.Issue.Waiters) == 0
	return json.Marshal(&clone)
}

func decodeRecord(data []byte) (*issueRecord, error) {
	var r issueRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	r.Issue.ContentHash = r.ContentHash
	r.Issue.RowVersion = r.RowVersion
	r.Issue.SourceRepo = r.SourceRepo
	r.Issue.IDPrefix = r.IDPrefix
	if r.WaitersEmpty && r.Issue.Waiters == nil {
		r.Issue.Waiters = []string{}
	}
	return &r, nil
}

// toIssue returns a detached copy of the row for a caller.
func (r *issueRecord) toIssue() *types.Issue {
	i := r.Issue
	i.ContentHash = r.ContentHash
	i.RowVersion = r.RowVersion
	i.SourceRepo = r.SourceRepo
	i.IDPrefix = r.IDPrefix
	if r.Issue.Labels != nil {
		i.Labels = append([]string(nil), r.Issue.Labels...)
	}
	return &i
}

// isWispPlane mirrors the SQL backends' routing rule: ephemeral, no-history,
// wisp-typed and infra-typed rows live in the wisps table.
func isWispPlane(i *types.Issue, infra map[string]bool) bool {
	if i.WispPlaneOverride != nil {
		return *i.WispPlaneOverride
	}
	return i.Ephemeral || i.NoHistory || i.WispType != "" || infra[string(i.IssueType)]
}

// open reports whether a status counts as active for blocking and readiness:
// anything but closed and pinned.
func open(status types.Status) bool {
	return status != types.StatusClosed && status != types.StatusPinned
}

func encodeJSON(v any) ([]byte, error) { return json.Marshal(v) }

func decodeJSON[T any](data []byte) (*T, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// lease is the node-local claim record a claimed issue carries.
type lease struct {
	Holder      string    `json:"holder"`
	GrantedAt   time.Time `json:"granted_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	Node        string    `json:"node,omitempty"`
}

// repoMtime is the per-repository JSONL mtime record (AdvancedQueryStore).
type repoMtime struct {
	JSONLPath string `json:"jsonl_path"`
	MtimeNs   int64  `json:"mtime_ns"`
}

func nowUTC() time.Time { return time.Now().UTC() }
