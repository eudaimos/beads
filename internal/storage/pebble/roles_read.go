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
	var result publicops.EdgeReadResult
	err := e.s.read(func(r kv) error {
		var err error
		result, err = readEdges(r, request)
		return err
	})
	if err != nil {
		return publicops.EdgeReadResult{}, err
	}
	return result, nil
}

// readEdges is the edge reader's body over one read view.
func readEdges(r kv, request publicops.EdgeReadRequest) (publicops.EdgeReadResult, error) {
	if err := storageissueops.ValidateEdgeReadRequest(request); err != nil {
		return publicops.EdgeReadResult{}, err
	}
	anchors := storageissueops.EdgeReadAnchors(request.IDs)
	if len(anchors) == 0 {
		return publicops.EdgeReadResult{Anchors: []publicops.AnchorEdges{}}, nil
	}
	present := make(map[string]struct{}, len(anchors))
	edges := make(map[string][]*types.Dependency, len(anchors))
	for _, id := range anchors {
		ok, err := recordExists(r, id)
		if err != nil {
			return publicops.EdgeReadResult{}, err
		}
		if !ok {
			continue
		}
		present[id] = struct{}{}
		deps, err := dependencyRecords(r, id)
		if err != nil {
			return publicops.EdgeReadResult{}, err
		}
		// The source-keyed read does not select the row id.
		for i, dep := range deps {
			clone := *dep
			clone.ID = ""
			deps[i] = &clone
		}
		edges[id] = deps
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
	var items []*publicops.RelatedIssue
	err := x.s.read(func(r kv) error {
		var err error
		items, err = related(r, request)
		return err
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// related is the relations role's body over one read view.
func related(r kv, request publicops.RelatedRequest) ([]*publicops.RelatedIssue, error) {
	if err := storageissueops.ValidateRelatedRequest(request); err != nil {
		return nil, err
	}
	ok, err := recordExists(r, request.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, request.ID)
	}
	items, err := anchorIssuesWithMetadata(r, request.ID, request.Direction == publicops.RelationIn)
	if err != nil {
		return nil, err
	}
	return storageissueops.FinishRelatedPage(items, request.Types), nil
}

// getMany is the batch get the molecule helpers read through.
func getMany(r kv, request publicops.GetManyRequest) (publicops.GetManyResult, error) {
	if err := storageissueops.ValidateGetManyRequest(request); err != nil {
		return publicops.GetManyResult{}, err
	}
	ids := storageissueops.DedupeGetManyIDs(request.IDs)
	if len(ids) == 0 {
		return publicops.GetManyResult{Issues: []*publicops.Issue{}, Missing: []string{}}, nil
	}
	found := make(map[string]*publicops.Issue, len(ids))
	for _, id := range ids {
		issue, err := loadIssue(r, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return publicops.GetManyResult{}, fmt.Errorf("get many: %w", err)
		}
		found[id] = issue
	}
	return storageissueops.FinishGetMany(ids, found), nil
}

// kvMoleculeReader serves the three reads the molecule helpers in the public
// issueops package need, over one read view.
type kvMoleculeReader struct{ r kv }

func (m kvMoleculeReader) GetMany(_ context.Context, request publicops.GetManyRequest) (publicops.GetManyResult, error) {
	return getMany(m.r, request)
}

func (m kvMoleculeReader) Related(_ context.Context, request publicops.RelatedRequest) ([]*publicops.RelatedIssue, error) {
	return related(m.r, request)
}

func (m kvMoleculeReader) ReadEdges(_ context.Context, request publicops.EdgeReadRequest) (publicops.EdgeReadResult, error) {
	return readEdges(m.r, request)
}
