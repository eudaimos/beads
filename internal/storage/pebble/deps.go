package pebble

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/rowid"
	"github.com/steveyegge/beads/internal/types"
)

// Edges are stored once under the source ("d/<src>/<dst>/<type>") and
// indexed under the target ("r/<dst>/<src>/<type>"). A pair of rows carries at
// most one edge per type, as the SQL unique key (issue_id, depends_on, type)
// enforces.

func getEdge(r kv, src, dst, typ string) (*types.Dependency, error) {
	data, ok, err := r.get(depKey(src, dst, typ))
	if err != nil || !ok {
		return nil, err
	}
	return decodeJSON[types.Dependency](data)
}

func putEdge(w kv, dep *types.Dependency) error {
	data, err := encodeJSON(dep)
	if err != nil {
		return err
	}
	if err := w.set(depKey(dep.IssueID, dep.DependsOnID, string(dep.Type)), data); err != nil {
		return err
	}
	return w.set(revDepKey(dep.DependsOnID, dep.IssueID, string(dep.Type)), nil)
}

func deleteEdge(w kv, src, dst, typ string) error {
	if err := w.del(depKey(src, dst, typ)); err != nil {
		return err
	}
	return w.del(revDepKey(dst, src, typ))
}

// outgoingEdges returns the edges whose source is id, in (target, type) order.
func outgoingEdges(r kv, id string) ([]*types.Dependency, error) {
	var out []*types.Dependency
	err := r.scan(depPrefix(id), func(_, v []byte) (bool, error) {
		dep, err := decodeJSON[types.Dependency](v)
		if err != nil {
			return false, err
		}
		out = append(out, dep)
		return true, nil
	})
	return out, err
}

// incomingEdges returns the edges whose target is id, in (source, type) order.
func incomingEdges(r kv, id string) ([]*types.Dependency, error) {
	var out []*types.Dependency
	err := r.scan(revDepPrefix(id), func(k, _ []byte) (bool, error) {
		parts := splitKey(nsRevDep, k)
		if len(parts) != 3 {
			return true, nil
		}
		dep, err := getEdge(r, parts[1], parts[0], parts[2])
		if err != nil {
			return false, err
		}
		if dep != nil {
			out = append(out, dep)
		}
		return true, nil
	})
	return out, err
}

// deleteEdgesOf removes every edge in either direction of id.
func deleteEdgesOf(w kv, id string) error {
	outs, err := outgoingEdges(w, id)
	if err != nil {
		return err
	}
	ins, err := incomingEdges(w, id)
	if err != nil {
		return err
	}
	for _, d := range outs {
		if err := deleteEdge(w, d.IssueID, d.DependsOnID, string(d.Type)); err != nil {
			return err
		}
	}
	for _, d := range ins {
		if err := deleteEdge(w, d.IssueID, d.DependsOnID, string(d.Type)); err != nil {
			return err
		}
	}
	return nil
}

// allEdges returns every edge in the store grouped by source.
func allEdges(r kv) (map[string][]*types.Dependency, error) {
	out := map[string][]*types.Dependency{}
	err := r.scan([]byte(nsDep), func(_, v []byte) (bool, error) {
		dep, err := decodeJSON[types.Dependency](v)
		if err != nil {
			return false, err
		}
		out[dep.IssueID] = append(out[dep.IssueID], dep)
		return true, nil
	})
	return out, err
}

// parentOf returns the id of the parent-child parent of id, or "".
func parentOf(r kv, id string) (string, error) {
	var parent string
	err := r.scan(depPrefix(id), func(k, _ []byte) (bool, error) {
		parts := splitKey(nsDep, k)
		if len(parts) == 3 && parts[2] == string(types.DepParentChild) {
			parent = parts[1]
			return false, nil
		}
		return true, nil
	})
	return parent, err
}

// childrenOf returns the ids with a parent-child edge onto id.
func childrenOf(r kv, id string) ([]string, error) {
	var out []string
	err := r.scan(revDepPrefix(id), func(k, _ []byte) (bool, error) {
		parts := splitKey(nsRevDep, k)
		if len(parts) == 3 && parts[2] == string(types.DepParentChild) {
			out = append(out, parts[1])
		}
		return true, nil
	})
	return out, err
}

// dependencyID derives the stable row id the SQL backends mint for an edge.
func dependencyID(dep *types.Dependency) string {
	digest := rowid.Digest(nullStrings(dep.IssueID, dep.DependsOnID, string(dep.Type)))
	return rowid.New("dependencies", 0, digest)
}

// addEdge validates and writes one edge inside t, recording the event when
// asked. It is the shared body of AddDependency(WithOptions) on the store and
// the transaction and of the DependencyEditor role.
func (t *txn) addEdge(dep *types.Dependency, actor string, opts storage.DependencyAddOptions) error {
	if dep == nil {
		return fmt.Errorf("%w: dependency must not be nil", storage.ErrValidation)
	}
	if dep.IssueID == "" || dep.DependsOnID == "" {
		return fmt.Errorf("%w: dependency needs both ends", storage.ErrValidation)
	}
	if dep.IssueID == dep.DependsOnID {
		return fmt.Errorf("%w: issue cannot depend on itself", storage.ErrValidation)
	}
	if dep.Type == "" {
		dep.Type = types.DepBlocks
	}
	if !dep.Type.IsValid() {
		return fmt.Errorf("%w: invalid dependency type %q", storage.ErrValidation, dep.Type)
	}
	src, err := getRecord(t.kv, dep.IssueID)
	if err != nil {
		return err
	}
	if _, err := getRecord(t.kv, dep.DependsOnID); err != nil {
		if isNotFound(err) && dep.Type == types.DepBlocks && strings.Contains(dep.DependsOnID, ":") {
			// External references ("external:...") are tolerated by the SQL
			// backends through the external dependency policy; the spike
			// keeps the edge without a target row.
		} else {
			return err
		}
	}
	if existing, err := getEdge(t.kv, dep.IssueID, dep.DependsOnID, string(dep.Type)); err != nil {
		return err
	} else if existing != nil {
		dep.ID, dep.CreatedAt, dep.CreatedBy = existing.ID, existing.CreatedAt, existing.CreatedBy
		return nil // idempotent
	}
	if !opts.SkipCycleCheck && dep.Type.AffectsReadyWork() {
		if cycle, err := wouldCycle(t.kv, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			return err
		} else if cycle {
			return fmt.Errorf("%w: adding dependency %s -> %s would create a cycle", storage.ErrValidation, dep.IssueID, dep.DependsOnID)
		}
	}
	if dep.CreatedAt.IsZero() {
		dep.CreatedAt = nowUTC()
	} else {
		dep.CreatedAt = dep.CreatedAt.UTC()
	}
	if dep.CreatedBy == "" {
		dep.CreatedBy = actor
	}
	if dep.ID == "" {
		dep.ID = dependencyID(dep)
	}
	if err := putEdge(t.kv, dep); err != nil {
		return err
	}
	if opts.EmitEvent {
		if err := t.recordEvent(src, types.EventDependencyAdded, actor, nil, strPtr(dep.DependsOnID), strPtr(fmt.Sprintf("Added dependency: %s -> %s (%s)", dep.IssueID, dep.DependsOnID, dep.Type))); err != nil {
			return err
		}
	}
	if dep.Type.AffectsReadyWork() {
		t.markBlockedDirty(dep.IssueID, dep.DependsOnID)
	}
	return nil
}

// removeEdge removes every edge from src to dst (any type, as the SQL
// RemoveDependency does) and returns whether one existed.
func (t *txn) removeEdge(src, dst, actor string, opts storage.DependencyRemoveOptions) (bool, error) {
	outs, err := outgoingEdges(t.kv, src)
	if err != nil {
		return false, err
	}
	removed := false
	for _, d := range outs {
		if d.DependsOnID != dst {
			continue
		}
		if err := deleteEdge(t.kv, d.IssueID, d.DependsOnID, string(d.Type)); err != nil {
			return false, err
		}
		removed = true
		if d.Type.AffectsReadyWork() {
			t.markBlockedDirty(src, dst)
		}
	}
	if removed && opts.EmitEvent {
		rec, err := getRecord(t.kv, src)
		if err == nil {
			if err := t.recordEvent(rec, types.EventDependencyRemoved, actor, strPtr(dst), nil, strPtr(fmt.Sprintf("Removed dependency: %s -> %s", src, dst))); err != nil {
				return false, err
			}
		}
	}
	return removed, nil
}

// wouldCycle reports whether an edge src -> dst of typ would close a cycle
// among edges of the same type: whether src is reachable from dst by
// following edges of that type (the SQL backends scope cycle detection per
// dependency type).
func wouldCycle(r kv, src, dst string, typ types.DependencyType) (bool, error) {
	seen := map[string]bool{dst: true}
	queue := []string{dst}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == src {
			return true, nil
		}
		outs, err := outgoingEdges(r, cur)
		if err != nil {
			return false, err
		}
		for _, d := range outs {
			if d.Type != typ || seen[d.DependsOnID] {
				continue
			}
			seen[d.DependsOnID] = true
			queue = append(queue, d.DependsOnID)
		}
	}
	return false, nil
}

func sortDeps(deps []*types.Dependency) {
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].IssueID != deps[j].IssueID {
			return deps[i].IssueID < deps[j].IssueID
		}
		if deps[i].DependsOnID != deps[j].DependsOnID {
			return deps[i].DependsOnID < deps[j].DependsOnID
		}
		return deps[i].Type < deps[j].Type
	})
}
