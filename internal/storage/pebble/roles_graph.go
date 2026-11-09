package pebble

import (
	"context"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// The three graph roles: the anchored recursive walk behind `bd dep tree`,
// the per-anchor edge cardinality behind `bd show`'s counts, and the
// blocking decoration a listing prints beside each row. Each is a read over
// ONE snapshot, which is the promise the SQL bodies keep with a read
// transaction: the root probe, the adjacency reads and the status lookups
// cannot straddle a commit here either.

// --- TreeWalker -------------------------------------------------------------

// TreeWalker serves the dependency-tree walk over the edge keys.
func (s *Store) TreeWalker() (publicops.TreeWalker, error) {
	return &treeWalker{s: s}, nil
}

type treeWalker struct{ s *Store }

var _ publicops.TreeWalker = (*treeWalker)(nil)

// WalkTree is issueops.WalkDependencyTreeInTx over the key space: validate,
// probe the root (an absent one is ErrNotFound rather than an empty tree),
// walk each requested direction through the same body GetDependencyTree
// runs, prune each half by status before the merge, and enforce the
// defensive cap on the nodes the caller would be handed.
func (w *treeWalker) WalkTree(ctx context.Context, req publicops.WalkTreeRequest) (publicops.TreeResult, error) {
	direction, err := storageissueops.ValidateWalkTreeRequest(req)
	if err != nil {
		return publicops.TreeResult{}, err
	}
	var result publicops.TreeResult
	err = w.s.readSnapshotCtx(ctx, func(r kv) error {
		if _, err := getRecord(r, req.RootID); err != nil {
			return err
		}
		nodes, err := walkTreeDirections(r, req.RootID, req.MaxDepth, direction, req.Status)
		if err != nil {
			return err
		}
		if nodes == nil {
			nodes = []*types.TreeNode{}
		}
		if err := storageissueops.EnforceMaxRowsCap(len(nodes), req.MaxRows, req.MaxRowsSource); err != nil {
			return err
		}
		result = publicops.TreeResult{Nodes: nodes}
		return nil
	})
	if err != nil {
		return publicops.TreeResult{}, err
	}
	return result, nil
}

// walkTreeDirections runs the one or two walks a direction asks for and
// prunes EACH HALF before merging them, for the reason the SQL body gives: a
// merged `both` list may carry one id twice with a different parent in each
// half, and a prune over the merged list would look one half's survivors up
// through the other half's parents.
func walkTreeDirections(r kv, rootID string, maxDepth int, direction publicops.TreeDirection, status publicops.Status) ([]*types.TreeNode, error) {
	walk := func(reverse bool) ([]*types.TreeNode, error) {
		return buildDependencyTree(r, rootID, 0, maxDepth, reverse, map[string]*types.Issue{}, map[string]bool{}, "", "")
	}
	if direction != publicops.TreeBoth {
		nodes, err := walk(direction == publicops.TreeUp)
		if err != nil {
			return nil, err
		}
		if status != "" {
			nodes = storageissueops.PruneTreeByStatus(nodes, status)
		}
		return nodes, nil
	}
	downTree, err := walk(false)
	if err != nil {
		return nil, err
	}
	upTree, err := walk(true)
	if err != nil {
		return nil, err
	}
	if status != "" {
		downTree, upTree = pruneBidirectionalByStatus(downTree, upTree, rootID, status)
	}
	return storageissueops.MergeBidirectionalTree(downTree, upTree, rootID), nil
}

// pruneBidirectionalByStatus prunes the two halves of a `both` walk and keeps
// the merge's root invariant: the merge takes the root from the DOWN half, so
// an up half that survived a prune the down half did not gets the down tree's
// own root node put back in front of it.
func pruneBidirectionalByStatus(downTree, upTree []*types.TreeNode, rootID string, status publicops.Status) ([]*types.TreeNode, []*types.TreeNode) {
	down := storageissueops.PruneTreeByStatus(downTree, status)
	up := storageissueops.PruneTreeByStatus(upTree, status)
	if len(up) == 0 {
		return down, up
	}
	for _, node := range down {
		if node != nil && node.ID == rootID {
			return down, up
		}
	}
	for _, node := range downTree {
		if node != nil && node.ID == rootID {
			clone := *node
			return append([]*types.TreeNode{&clone}, down...), up
		}
	}
	return down, up
}

// --- GraphCounter -----------------------------------------------------------

// GraphCounter serves the per-anchor edge count over the edge keys.
func (s *Store) GraphCounter() (publicops.GraphCounter, error) {
	return &graphCounter{s: s}, nil
}

type graphCounter struct{ s *Store }

var _ publicops.GraphCounter = (*graphCounter)(nil)

// CountEdges is issueops.ExecuteEdgeCount over the key space: the existence
// probe and the tally read one snapshot, and the answer is assembled by the
// shared FinishEdgeCount so the missing-anchor rule is the SQL legs' own.
//
// Every edge is stored once here, keyed by its source, so the "sum over the
// two planes" the role promises is the count of edge keys at the anchor's
// end. An outbound edge counts whatever its far end names, a dangling
// `external:` reference included; an inbound count narrowed by Status reads
// the dependent's own row, and an edge whose source row is gone drops out,
// as the SQL join leaves it unmatched.
func (c *graphCounter) CountEdges(ctx context.Context, req publicops.EdgeCountRequest) (publicops.EdgeCountResult, error) {
	if err := storageissueops.ValidateEdgeCountRequest(req); err != nil {
		return publicops.EdgeCountResult{}, err
	}
	anchors := storageissueops.EdgeReadAnchors(req.IDs)
	if len(anchors) == 0 {
		return publicops.EdgeCountResult{Anchors: []publicops.AnchorEdgeCount{}}, nil
	}
	var wantType map[publicops.DependencyType]bool
	if len(req.Types) > 0 {
		wantType = make(map[publicops.DependencyType]bool, len(req.Types))
		for _, t := range req.Types {
			wantType[t] = true
		}
	}
	var result publicops.EdgeCountResult
	err := c.s.readSnapshotCtx(ctx, func(r kv) error {
		present := make(map[string]struct{}, len(anchors))
		tallies := make(map[string]int64, len(anchors))
		for _, id := range anchors {
			ok, err := recordExists(r, id)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			present[id] = struct{}{}
			var edges []*types.Dependency
			if req.Direction == publicops.EdgeDirectionOut {
				edges, err = outgoingEdges(r, id)
			} else {
				edges, err = incomingEdges(r, id)
			}
			if err != nil {
				return err
			}
			for _, d := range edges {
				if wantType != nil && !wantType[d.Type] {
					continue
				}
				if req.Status != "" {
					src, err := getRecord(r, d.IssueID)
					if err != nil {
						if isNotFound(err) {
							continue
						}
						return err
					}
					if string(src.Issue.Status) != req.Status {
						continue
					}
				}
				tallies[id]++
			}
		}
		result = storageissueops.FinishEdgeCount(anchors, present, tallies)
		return nil
	})
	if err != nil {
		return publicops.EdgeCountResult{}, err
	}
	return result, nil
}

// --- BlockingAnnotator ------------------------------------------------------

// BlockingAnnotator serves the blocking decoration a listing prints beside
// each row, over the edge keys.
func (s *Store) BlockingAnnotator() (publicops.BlockingAnnotator, error) {
	return &blockingAnnotator{s: s}, nil
}

type blockingAnnotator struct{ s *Store }

var _ publicops.BlockingAnnotator = (*blockingAnnotator)(nil)

// AnnotateBlocking is issueops.ExecuteBlockingAnnotation over the key space:
// the outbound read, the inbound read and the status lookups that decide
// which edges are live all see one snapshot, and the shared
// FinishBlockingAnnotation pins the order and the collapse of repeats.
func (a *blockingAnnotator) AnnotateBlocking(ctx context.Context, req publicops.BlockingRequest) (publicops.BlockingResult, error) {
	if err := storageissueops.ValidateBlockingRequest(req); err != nil {
		return publicops.BlockingResult{}, err
	}
	anchors := storageissueops.EdgeReadAnchors(req.IDs)
	if len(anchors) == 0 {
		return publicops.BlockingResult{Items: []publicops.IssueBlocking{}}, nil
	}
	var result publicops.BlockingResult
	err := a.s.readSnapshotCtx(ctx, func(r kv) error {
		blockedBy, blocks, parent, err := blockingInfo(r, anchors)
		if err != nil {
			return err
		}
		result = storageissueops.FinishBlockingAnnotation(anchors, blockedBy, blocks, parent)
		if req.IncludeIsBlocked {
			flags, err := isBlockedBatch(r, anchors)
			if err != nil {
				return err
			}
			storageissueops.ApplyBlockedFlags(&result, flags)
		}
		return nil
	})
	if err != nil {
		return publicops.BlockingResult{}, err
	}
	return result, nil
}
