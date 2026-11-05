package pebble

import (
	"encoding/json"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// Blocked state, computed in process to the SQL backends' rule
// (issueops/blocked_consistency.go, shouldBeBlockedIDsUnionCoreSQL). A row X
// should be blocked when any of these holds and X itself is open:
//
//  1. X has a blocks or conditional-blocks edge onto a target that is open.
//  2. X has a parent-child edge onto a parent P that is open and blocked, and
//     P's blockedness is not solely explained by P's own subtree: at least one
//     of P's blocking reasons (rule 1 and 3 targets) lies outside the
//     parent-child descendants of P within subtreeWalkDepth levels. A parent
//     blocked only by its own children (the "close gate on the epic" idiom)
//     does not darken those children.
//  3. X has a waits-for edge onto a spawner S, and S still gates X: S has at
//     least one open parent-child child, and not (the edge's gate is
//     "any-children" and S has at least one closed child).
//
// A closed or pinned row is never blocked. The stored flag is recomputed to
// a fixpoint over the neighborhood a write can affect: the touched rows,
// everything that depends on them (reverse edges of ready-affecting types),
// and the parent-child descendants of all of those, repeated until no flag
// changes.

const subtreeWalkDepth = 4

// shouldBeBlocked evaluates the rule for one row.
func shouldBeBlocked(r kv, rec *issueRecord) (bool, error) {
	if !open(rec.Issue.Status) {
		return false, nil
	}
	outs, err := outgoingEdges(r, rec.Issue.ID)
	if err != nil {
		return false, err
	}
	for _, d := range outs {
		switch d.Type {
		case types.DepBlocks, types.DepConditionalBlocks:
			if blocked, err := targetOpen(r, d.DependsOnID); err != nil || blocked {
				return blocked, err
			}
		case types.DepParentChild:
			parent, err := getRecord(r, d.DependsOnID)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return false, err
			}
			if !parent.Issue.IsBlocked || !open(parent.Issue.Status) {
				continue
			}
			explained, err := explainedBySubtree(r, parent)
			if err != nil {
				return false, err
			}
			if !explained {
				return true, nil
			}
		case types.DepWaitsFor:
			gated, err := waitsForGates(r, d)
			if err != nil || gated {
				return gated, err
			}
		}
	}
	return false, nil
}

func targetOpen(r kv, id string) (bool, error) {
	target, err := getRecord(r, id)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return open(target.Issue.Status), nil
}

// blockingReasons returns the ids a row's rule-1 and rule-3 blockedness
// comes from: open blocks/conditional-blocks targets, and gating spawners.
func blockingReasons(r kv, rec *issueRecord) ([]string, error) {
	outs, err := outgoingEdges(r, rec.Issue.ID)
	if err != nil {
		return nil, err
	}
	var reasons []string
	for _, d := range outs {
		switch d.Type {
		case types.DepBlocks, types.DepConditionalBlocks:
			if isOpen, err := targetOpen(r, d.DependsOnID); err != nil {
				return nil, err
			} else if isOpen {
				reasons = append(reasons, d.DependsOnID)
			}
		case types.DepWaitsFor:
			if gated, err := waitsForGates(r, d); err != nil {
				return nil, err
			} else if gated {
				reasons = append(reasons, d.DependsOnID)
			}
		}
	}
	return reasons, nil
}

// explainedBySubtree reports whether every blocking reason of parent is one
// of its own parent-child descendants within subtreeWalkDepth levels. A
// parent with no reason of its own (blocked only through its own parent) is
// not explained: its blockedness is inherited and propagates on.
func explainedBySubtree(r kv, parent *issueRecord) (bool, error) {
	reasons, err := blockingReasons(r, parent)
	if err != nil {
		return false, err
	}
	if len(reasons) == 0 {
		return false, nil
	}
	for _, reason := range reasons {
		inside := false
		cur := reason
		for level := 0; level < subtreeWalkDepth; level++ {
			p, err := parentOf(r, cur)
			if err != nil {
				return false, err
			}
			if p == "" {
				break
			}
			if p == parent.Issue.ID {
				inside = true
				break
			}
			cur = p
		}
		if !inside {
			return false, nil
		}
	}
	return true, nil
}

// waitsForGates evaluates rule 3 for one waits-for edge.
func waitsForGates(r kv, d *types.Dependency) (bool, error) {
	children, err := childrenOf(r, d.DependsOnID)
	if err != nil {
		return false, err
	}
	openChild, closedChild := false, false
	for _, c := range children {
		child, err := getRecord(r, c)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return false, err
		}
		if open(child.Issue.Status) {
			openChild = true
		}
		if child.Issue.Status == types.StatusClosed {
			closedChild = true
		}
	}
	if !openChild {
		return false, nil
	}
	if gateMode(d.Metadata) == "any-children" && closedChild {
		return false, nil
	}
	return true, nil
}

// gateMode reads the "gate" key of a waits-for edge's metadata JSON; absent
// (legacy "{}" rows) means the all-children default.
func gateMode(metadata string) string {
	if strings.TrimSpace(metadata) == "" {
		return "all-children"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return "all-children"
	}
	if g, ok := m["gate"].(string); ok && g != "" {
		return g
	}
	return "all-children"
}

// affectedNeighborhood expands seeds to every row whose flag can change
// when the seeds' state did: the seeds, the rows depending on them through
// ready-affecting edges (transitively), and parent-child descendants.
func affectedNeighborhood(r kv, seeds map[string]bool) ([]string, error) {
	seen := map[string]bool{}
	var queue, out []string
	for id := range seeds {
		seen[id] = true
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		ins, err := incomingEdges(r, id)
		if err != nil {
			return nil, err
		}
		for _, d := range ins {
			if !d.Type.AffectsReadyWork() || seen[d.IssueID] {
				continue
			}
			seen[d.IssueID] = true
			queue = append(queue, d.IssueID)
		}
		// A spawner's children changing state re-gates every waiter on the
		// spawner (rule 3), so a child's parent is affected too, and through
		// it the waiters: walk the parent-child edge upward once.
		if parent, err := parentOf(r, id); err != nil {
			return nil, err
		} else if parent != "" && !seen[parent] {
			seen[parent] = true
			queue = append(queue, parent)
		}
	}
	return out, nil
}

// settleBlocked recomputes the flag over the transaction's dirty
// neighborhood to a fixpoint. It is called once, before commit.
func (t *txn) settleBlocked() error {
	if len(t.dirtyBlocked) == 0 {
		return nil
	}
	ids, err := affectedNeighborhood(t.kv, t.dirtyBlocked)
	if err != nil {
		return err
	}
	t.dirtyBlocked = nil
	for pass := 0; pass < 64; pass++ {
		changed := false
		for _, id := range ids {
			rec, err := getRecord(t.kv, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			want, err := shouldBeBlocked(t.kv, rec)
			if err != nil {
				return err
			}
			// A row whose flag is right but whose effective flag is not
			// (a stale flag that an edge add has since made real) is
			// rewritten too, so the ready index follows.
			if want == rec.Issue.IsBlocked && want == rec.EffectiveBlocked {
				continue
			}
			old := *rec
			rec.Issue.IsBlocked = want
			if err := putRecord(t.kv, &old, rec); err != nil {
				return err
			}
			changed = true
		}
		if !changed {
			return nil
		}
	}
	return nil
}

// recomputeAll recomputes every row's flag to a fixpoint (doctor / repair).
func recomputeAll(w kv) (int, error) {
	var ids []string
	if err := scanRecords(w, func(rec *issueRecord) (bool, error) {
		ids = append(ids, rec.Issue.ID)
		return true, nil
	}); err != nil {
		return 0, err
	}
	total := 0
	for pass := 0; pass < 64; pass++ {
		changed := 0
		for _, id := range ids {
			rec, err := getRecord(w, id)
			if err != nil {
				return total, err
			}
			want, err := shouldBeBlocked(w, rec)
			if err != nil {
				return total, err
			}
			if want == rec.Issue.IsBlocked && want == rec.EffectiveBlocked {
				continue
			}
			old := *rec
			rec.Issue.IsBlocked = want
			if err := putRecord(w, &old, rec); err != nil {
				return total, err
			}
			changed++
		}
		total += changed
		if changed == 0 {
			break
		}
	}
	return total, nil
}

// blockers lists the open targets of a row's blocking edges and the gating
// spawners, for IsBlocked's description list.
func blockers(r kv, rec *issueRecord) ([]string, error) {
	return blockingReasons(r, rec)
}
