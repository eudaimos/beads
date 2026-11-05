package pebble

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi/storereader"
	"github.com/steveyegge/beads/internal/workapi/storestats"
	"github.com/steveyegge/beads/internal/workapi/storeworkspaceconfig"
	publicops "github.com/steveyegge/beads/issueops"
)

// The read-side roles below are the ones gascity's native store resolves.
// Three of them have a shared implementation over the flat storage surface
// and need nothing engine-specific; the two graph roles read the edge keys
// directly so a missing anchor is told apart from an anchor with no edges.

// IssueReader serves guarded issue queries over the flat storage methods.
func (s *Store) IssueReader() (publicops.Reader, error) {
	return storereader.New(s)
}

// WorkspaceConfig serves the settings role over the config keys.
func (s *Store) WorkspaceConfig() (publicops.WorkspaceConfig, error) {
	return storeworkspaceconfig.New(s)
}

// StatsReporter serves the summary and per-assignee statistics role.
func (s *Store) StatsReporter() (publicops.StatsReporter, error) {
	return storestats.New(s)
}

// EdgeReader serves the per-anchor outgoing edge read.
func (s *Store) EdgeReader() (publicops.EdgeReader, error) {
	return &edgeReader{s: s}, nil
}

type edgeReader struct{ s *Store }

var _ publicops.EdgeReader = (*edgeReader)(nil)

func (e *edgeReader) ReadEdges(ctx context.Context, request publicops.EdgeReadRequest) (publicops.EdgeReadResult, error) {
	if err := storageissueops.ValidateEdgeReadRequest(request); err != nil {
		return publicops.EdgeReadResult{}, err
	}
	anchors := storageissueops.EdgeReadAnchors(request.IDs)
	if len(anchors) == 0 {
		return publicops.EdgeReadResult{Anchors: []publicops.AnchorEdges{}}, nil
	}
	present := make(map[string]struct{}, len(anchors))
	edges := make(map[string][]*types.Dependency, len(anchors))
	err := e.s.read(func(r kv) error {
		for _, id := range anchors {
			ok, err := recordExists(r, id)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			present[id] = struct{}{}
			deps, err := dependencyRecords(r, id)
			if err != nil {
				return err
			}
			// The source-keyed read does not select the row id.
			for i, dep := range deps {
				clone := *dep
				clone.ID = ""
				deps[i] = &clone
			}
			edges[id] = deps
		}
		return nil
	})
	if err != nil {
		return publicops.EdgeReadResult{}, err
	}
	return storageissueops.FinishEdgeRead(anchors, present, edges, request.Types), nil
}

// IssueRelations serves the typed neighbor listing in either direction.
func (s *Store) IssueRelations() (publicops.Relations, error) {
	return &issueRelations{s: s}, nil
}

type issueRelations struct{ s *Store }

var _ publicops.Relations = (*issueRelations)(nil)

func (x *issueRelations) Related(ctx context.Context, request publicops.RelatedRequest) ([]*publicops.RelatedIssue, error) {
	if err := storageissueops.ValidateRelatedRequest(request); err != nil {
		return nil, err
	}
	var items []*types.IssueWithDependencyMetadata
	err := x.s.read(func(r kv) error {
		ok, err := recordExists(r, request.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: issue %s", storage.ErrNotFound, request.ID)
		}
		items, err = anchorIssuesWithMetadata(r, request.ID, request.Direction == publicops.RelationIn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return storageissueops.FinishRelatedPage(items, request.Types), nil
}
