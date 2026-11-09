package pebble

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/internal/workapi/storecounter"
	publicops "github.com/steveyegge/beads/issueops"
)

// The roles below are the ones the bd CLI reaches on its ordinary verbs
// beyond the set gascity's native store calls: count, dep add's cycle report
// and close.

// Counter serves the scoped counts role over the flat surface.
func (s *Store) Counter() (publicops.Counter, error) {
	return storecounter.New(s)
}

// CycleDetector serves the cycle report over the edge keys.
func (s *Store) CycleDetector() (publicops.CycleDetector, error) {
	return &cycleDetector{s: s}, nil
}

type cycleDetector struct{ s *Store }

var _ publicops.CycleDetector = (*cycleDetector)(nil)

func (c *cycleDetector) DetectCycles(ctx context.Context, request publicops.DetectCyclesRequest) (publicops.CycleReport, error) {
	var report publicops.CycleReport
	err := c.s.read(func(r kv) error {
		hydrate := func(id string) *types.Issue {
			issue, err := loadIssue(r, id)
			if err != nil {
				return nil
			}
			return issue
		}
		all, err := allEdges(r)
		if err != nil {
			return err
		}
		if request.IncludeTracks {
			graph := map[string][]storageissueops.MixedCycleEdge{}
			for _, deps := range all {
				for _, d := range deps {
					switch d.Type {
					case types.DepBlocks, types.DepConditionalBlocks:
						graph[d.IssueID] = append(graph[d.IssueID], storageissueops.MixedCycleEdge{To: d.DependsOnID, Scheduling: true})
					case types.DepTracks:
						graph[d.IssueID] = append(graph[d.IssueID], storageissueops.MixedCycleEdge{To: d.DependsOnID})
					}
				}
			}
			paths, err := storageissueops.CanonicalMixedCyclePaths(graph)
			if err != nil {
				return err
			}
			report = publicops.CycleReport{Cycles: storageissueops.BuildCycles(paths, hydrate)}
			return nil
		}
		graph, err := schedulingGraph(r, false)
		if err != nil {
			return err
		}
		report = publicops.CycleReport{Cycles: storageissueops.BuildCycles(storageissueops.CanonicalCyclePaths(graph), hydrate)}
		return nil
	})
	if err != nil {
		return publicops.CycleReport{}, err
	}
	return report, nil
}

// BatchCloser serves the multi-issue close with its optional claim-next.
func (s *Store) BatchCloser() (publicops.BatchCloser, error) {
	return &batchCloser{s: s}, nil
}

// BatchCloserWithPolicy serves the same close under a caller-supplied blocker
// policy (storage.PolicyBatchCloserSource).
func (s *Store) BatchCloserWithPolicy(policy storage.BatchClosePolicy) (publicops.BatchCloser, error) {
	return &batchCloser{s: s, policy: policy}, nil
}

var _ storage.PolicyBatchCloserSource = (*Store)(nil)

type batchCloser struct {
	s      *Store
	policy storage.BatchClosePolicy
}

var _ publicops.BatchCloser = (*batchCloser)(nil)

func (b *batchCloser) CloseBatch(ctx context.Context, request publicops.CloseBatchRequest) (publicops.CloseBatchResult, error) {
	if err := storageissueops.ValidateCloseBatchRequest(request); err != nil {
		return publicops.CloseBatchResult{}, err
	}
	var claimFilter *types.WorkFilter
	if request.ClaimNext != nil {
		filter, err := workapi.BuildReadyFilter(*request.ClaimNext)
		if err != nil {
			return publicops.CloseBatchResult{}, err
		}
		claimFilter = &filter
	}
	result := publicops.CloseBatchResult{Outcomes: make([]publicops.CloseOutcome, len(request.Items))}
	err := b.s.run(ctx, func(t *txn) error {
		landed := 0
		for i, item := range request.Items {
			if err := b.policy.CheckClose(item.IssueID, request.Force); err != nil {
				rec, readErr := getRecord(t.kv, item.IssueID)
				if readErr != nil && !isNotFound(readErr) {
					result.Outcomes[i] = publicops.CloseOutcome{IssueID: item.IssueID, Err: readErr}
					continue
				}
				if readErr == nil && rec.Issue.Status != types.StatusClosed {
					result.Outcomes[i] = publicops.CloseOutcome{IssueID: item.IssueID, Err: err}
					continue
				}
			}
			closeRequest := publicops.CloseRequest{
				Actor:   request.Actor,
				IssueID: item.IssueID,
				Reason:  item.Reason,
				Session: request.Session,
				Force:   request.Force,
			}
			if err := validateCloseRequest(closeRequest); err != nil {
				result.Outcomes[i] = publicops.CloseOutcome{IssueID: item.IssueID, Err: err}
				continue
			}
			unchanged, openChildren, err := t.lifecycleClose(closeRequest)
			if err != nil {
				result.Outcomes[i] = publicops.CloseOutcome{IssueID: item.IssueID, Err: err}
				continue
			}
			if err := t.settleBlocked(); err != nil {
				return err
			}
			hydrated, err := loadIssue(t.kv, item.IssueID)
			if err != nil {
				return err
			}
			if !unchanged {
				landed++
			}
			result.Outcomes[i] = publicops.CloseOutcome{
				IssueID:      item.IssueID,
				Issue:        hydrated,
				Changed:      !unchanged,
				OpenChildren: openChildren,
			}
		}
		if request.AutoCloseMolecule {
			closedAt := make([]int, 0, len(result.Outcomes))
			stepIDs := make([]string, 0, len(result.Outcomes))
			for i, outcome := range result.Outcomes {
				if outcome.Err == nil {
					closedAt = append(closedAt, i)
					stepIDs = append(stepIDs, outcome.IssueID)
				}
			}
			autos, err := t.closeCompletedMolecules(stepIDs, request.Actor, request.Session)
			if err != nil {
				return err
			}
			for j, i := range closedAt {
				result.Outcomes[i].AutoClosedMolecule, result.Outcomes[i].MoleculeAutoCloseRefusal = autos[j].Root, autos[j].Refusal
			}
		}
		if claimFilter != nil && landed > 0 {
			rec, err := t.claimNext(b.policy.FilterClaim(*claimFilter), request.Actor)
			if err != nil {
				return err
			}
			if rec != nil {
				hydrated, err := withCounts(t.kv, rec, countsHydration{})
				if err != nil {
					return err
				}
				result.ClaimedNext = hydrated
			}
		}
		return nil
	})
	if err != nil {
		return publicops.CloseBatchResult{}, err
	}
	return result, nil
}

// claimNext claims the front of the ready set the filter admits for actor and
// returns the claimed row, or nil when the set is empty.
func (t *txn) claimNext(filter types.WorkFilter, actor string) (*issueRecord, error) {
	claimFilter := filter
	claimFilter.Status = types.StatusOpen
	claimFilter.Statuses = nil
	claimFilter.Unassigned = true
	claimFilter.Assignee = nil
	claimFilter.Limit = 0
	claimFilter.Offset = 0
	claimFilter.MaxRows = 0
	claimFilter.MaxRowsSource = ""
	candidates, err := claimCandidates(t.kv, claimFilter)
	if err != nil {
		return nil, err
	}
	for _, rec := range candidates {
		if err := t.claimIssue(rec.Issue.ID, actor); err != nil {
			if errors.Is(err, storage.ErrAlreadyClaimed) || errors.Is(err, storage.ErrNotClaimable) {
				continue
			}
			return nil, err
		}
		if err := t.settleBlocked(); err != nil {
			return nil, err
		}
		return getRecord(t.kv, rec.Issue.ID)
	}
	return nil, nil
}

// closeCompletedMolecules closes every molecule root whose last step is among
// stepIDs, the way the SQL path does after a close: one answer per step, the
// root it closed or the refusal that kept it open.
func (t *txn) closeCompletedMolecules(stepIDs []string, actor, session string) ([]storageissueops.MoleculeAutoClose, error) {
	reader := publicops.MoleculeReaderOver(kvMoleculeReader{r: t.kv})
	roots, err := publicops.AutoClosingMoleculeRoots(t.ctx, reader, stepIDs)
	if err != nil {
		return nil, err
	}
	autos := make([]storageissueops.MoleculeAutoClose, len(stepIDs))
	decided := make(map[string]bool, len(roots))
	for i, stepID := range stepIDs {
		root := roots[stepID]
		if root == nil || decided[root.ID] {
			continue
		}
		decided[root.ID] = true
		complete, err := publicops.MoleculeStepsClosed(t.ctx, reader, root.ID)
		if err != nil {
			return nil, err
		}
		if !complete {
			continue
		}
		request := publicops.MoleculeRootClose(root, actor, session)
		unchanged, _, err := t.lifecycleClose(request)
		if publicops.IsMoleculeAutoCloseRefusal(err) {
			autos[i] = storageissueops.RefusedMoleculeAutoClose(err)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("auto-closing molecule %s: %w", root.ID, err)
		}
		if unchanged {
			continue
		}
		if err := t.settleBlocked(); err != nil {
			return nil, err
		}
		closed, err := loadIssue(t.kv, root.ID)
		if err != nil {
			return nil, err
		}
		autos[i] = storageissueops.MoleculeAutoClose{Root: closed}
	}
	return autos, nil
}
