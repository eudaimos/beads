package pebble

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/types"
)

// Statistics, stale issues, epic closure and molecule rollups: the
// issueops bodies (statistics.go, stale.go, epic_closure.go, molecule.go,
// compaction.go) over a scan. Where those read the issues table alone, only
// durable-plane rows count here.

// statistics is issueops.ScanIssueCountsInTx over the durable plane, plus
// the blocked count and the clamped ready count when withBlocked is set.
func statistics(r kv, withBlocked bool) (*types.Statistics, error) {
	stats := &types.Statistics{}
	blocked := 0
	err := scanRecords(r, func(rec *issueRecord) (bool, error) {
		if rec.Plane != planeIssues {
			return true, nil
		}
		i := &rec.Issue
		stats.TotalIssues++
		switch i.Status {
		case types.StatusOpen:
			stats.OpenIssues++
		case types.StatusInProgress:
			stats.InProgressIssues++
		case types.StatusClosed:
			stats.ClosedIssues++
		case types.StatusDeferred:
			stats.DeferredIssues++
		}
		if i.Pinned {
			stats.PinnedIssues++
		}
		if i.IssueType == types.TypeGate {
			stats.GateIssues++
		}
		if i.IsTemplate {
			stats.TemplateIssues++
		}
		if i.IsBlocked && open(i.Status) {
			blocked++
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get statistics: %w", err)
	}
	if withBlocked {
		stats.BlockedIssues = &blocked
		ready := stats.OpenIssues - blocked
		if ready < 0 {
			ready = 0
		}
		stats.ReadyIssues = &ready
	}
	return stats, nil
}

// GetStatistics returns the summary counts over the durable plane, with the
// blocked count and ReadyIssues = OpenIssues - BlockedIssues clamped at 0.
func (s *Store) GetStatistics(ctx context.Context) (*types.Statistics, error) {
	var out *types.Statistics
	err := s.read(func(r kv) error {
		v, err := statistics(r, true)
		out = v
		return err
	})
	return out, err
}

// GetStatisticsNoBlocked is GetStatistics without the blocked set:
// BlockedIssues and ReadyIssues stay nil.
func (s *Store) GetStatisticsNoBlocked(ctx context.Context) (*types.Statistics, error) {
	var out *types.Statistics
	err := s.read(func(r kv) error {
		v, err := statistics(r, false)
		out = v
		return err
	})
	return out, err
}

// staleIssues is issueops.GetStaleIssuesInTx over r: durable, non-ephemeral
// rows not updated since the cutoff, in the given status (open and
// in_progress by default), with no lease heartbeat since the cutoff, stalest
// first.
func staleIssues(r kv, filter types.StaleFilter) ([]*types.Issue, error) {
	cutoff := nowUTC().AddDate(0, 0, -filter.Days)
	var recs []*issueRecord
	err := scanRecords(r, func(rec *issueRecord) (bool, error) {
		i := &rec.Issue
		if rec.Plane != planeIssues || i.Ephemeral || !i.UpdatedAt.Before(cutoff) {
			return true, nil
		}
		if filter.Status != "" {
			if string(i.Status) != filter.Status {
				return true, nil
			}
		} else if i.Status != types.StatusOpen && i.Status != types.StatusInProgress {
			return true, nil
		}
		if !labelSetMatches(i.Labels, filter.Labels, filter.LabelsAny, filter.ExcludeLabels) {
			return true, nil
		}
		l, err := getLease(r, i.ID)
		if err != nil {
			return false, err
		}
		if l != nil && !l.HeartbeatAt.Before(cutoff) {
			return true, nil
		}
		recs = append(recs, rec)
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get stale issues: %w", err)
	}
	stableSortBy(recs, func(a, b *issueRecord) bool {
		if !a.Issue.UpdatedAt.Equal(b.Issue.UpdatedAt) {
			return a.Issue.UpdatedAt.Before(b.Issue.UpdatedAt)
		}
		return a.Issue.ID < b.Issue.ID
	})
	if filter.Limit > 0 && len(recs) > filter.Limit {
		recs = recs[:filter.Limit]
	}
	out := make([]*types.Issue, 0, len(recs))
	for _, rec := range recs {
		issue := rec.toIssue()
		if err := projectLease(r, issue); err != nil {
			return nil, fmt.Errorf("get stale issues: lease %s: %w", issue.ID, err)
		}
		out = append(out, issue)
	}
	return out, nil
}

// GetStaleIssues returns the issues not updated in filter.Days days, oldest
// update first.
func (s *Store) GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		v, err := staleIssues(r, filter)
		out = v
		return err
	})
	return out, err
}

// epicsEligibleForClosure is issueops.GetEpicsEligibleForClosureInTx over r:
// every open durable epic with at least one parent-child child, eligible
// when every child is closed for a completing reason.
func epicsEligibleForClosure(r kv) ([]*types.EpicStatus, error) {
	var epics []*issueRecord
	err := scanRecords(r, func(rec *issueRecord) (bool, error) {
		if rec.Plane == planeIssues && rec.Issue.IssueType == types.TypeEpic && rec.Issue.Status != types.StatusClosed {
			epics = append(epics, rec)
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get epics eligible for closure: %w", err)
	}
	var results []*types.EpicStatus
	for _, epic := range epics {
		children, err := childrenOf(r, epic.Issue.ID)
		if err != nil {
			return nil, fmt.Errorf("get epics eligible for closure: children of %s: %w", epic.Issue.ID, err)
		}
		if len(children) == 0 {
			continue
		}
		closed, completing := 0, 0
		for _, c := range children {
			child, err := getRecord(r, c)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, err
			}
			if child.Issue.Status != types.StatusClosed {
				continue
			}
			closed++
			if !types.IsNonCompletingClose(child.Issue.CloseReason) {
				completing++
			}
		}
		issue := epic.toIssue()
		if err := projectLease(r, issue); err != nil {
			return nil, err
		}
		results = append(results, &types.EpicStatus{
			Epic:             issue,
			TotalChildren:    len(children),
			ClosedChildren:   closed,
			EligibleForClose: len(children) > 0 && len(children) == completing,
		})
	}
	return results, nil
}

// GetEpicsEligibleForClosure returns the open epics that have children,
// flagging those whose children are all closed with a completing reason.
func (s *Store) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	var out []*types.EpicStatus
	err := s.read(func(r kv) error {
		v, err := epicsEligibleForClosure(r)
		out = v
		return err
	})
	return out, err
}

// moleculeChildren resolves a molecule the way the SQL path routes it: the
// row's own plane (the issues plane for a missing molecule) and the
// parent-child children stored in that plane's dependency table, which are
// the children living in the same plane.
func moleculeChildren(r kv, moleculeID string) (*issueRecord, []*issueRecord, error) {
	mol, err := getRecord(r, moleculeID)
	if err != nil && !isNotFound(err) {
		return nil, nil, err
	}
	plane := planeIssues
	if mol != nil {
		plane = mol.Plane
	}
	ids, err := childrenOf(r, moleculeID)
	if err != nil {
		return nil, nil, err
	}
	var children []*issueRecord
	for _, id := range ids {
		child, err := getRecord(r, id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		if child.Plane == plane {
			children = append(children, child)
		}
	}
	return mol, children, nil
}

// moleculeProgress is issueops.GetMoleculeProgressInTx over r.
func moleculeProgress(r kv, moleculeID string) (*types.MoleculeProgressStats, error) {
	stats := &types.MoleculeProgressStats{MoleculeID: moleculeID}
	mol, children, err := moleculeChildren(r, moleculeID)
	if err != nil {
		return nil, fmt.Errorf("get molecule progress: %w", err)
	}
	if mol != nil {
		stats.MoleculeTitle = mol.Issue.Title
	}
	for _, child := range children {
		stats.Total++
		switch child.Issue.Status {
		case types.StatusClosed:
			stats.Completed++
		case types.StatusInProgress:
			stats.InProgress++
			if stats.CurrentStepID == "" {
				stats.CurrentStepID = child.Issue.ID
			}
		}
	}
	return stats, nil
}

// GetMoleculeProgress returns the step counts of a molecule's direct
// children; a missing molecule answers a zeroed result, not an error.
func (s *Store) GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error) {
	var out *types.MoleculeProgressStats
	err := s.read(func(r kv) error {
		v, err := moleculeProgress(r, moleculeID)
		out = v
		return err
	})
	return out, err
}

// moleculeLastActivity is issueops.GetMoleculeLastActivityInTx over r: the
// newest child updated_at, overtaken by the newest child closed_at only when
// strictly later; a childless molecule answers its own updated_at.
func moleculeLastActivity(r kv, moleculeID string) (*types.MoleculeLastActivity, error) {
	mol, children, err := moleculeChildren(r, moleculeID)
	if err != nil {
		return nil, fmt.Errorf("get molecule last activity: %w", err)
	}
	if len(children) == 0 {
		if mol == nil {
			return nil, fmt.Errorf("molecule %s not found: %w", moleculeID, errNotFound("issue", moleculeID))
		}
		return &types.MoleculeLastActivity{
			MoleculeID:   moleculeID,
			LastActivity: mol.Issue.UpdatedAt,
			Source:       "molecule_updated",
		}, nil
	}
	result := &types.MoleculeLastActivity{MoleculeID: moleculeID, Source: "step_updated"}
	var lastClosed *types.Issue
	for _, child := range children {
		i := &child.Issue
		if result.SourceStepID == "" || i.UpdatedAt.After(result.LastActivity) {
			result.LastActivity = i.UpdatedAt
			result.SourceStepID = i.ID
		}
		if i.ClosedAt != nil && (lastClosed == nil || i.ClosedAt.After(*lastClosed.ClosedAt)) {
			lastClosed = i
		}
	}
	if lastClosed != nil && lastClosed.ClosedAt.After(result.LastActivity) {
		result.LastActivity = *lastClosed.ClosedAt
		result.Source = "step_closed"
		result.SourceStepID = lastClosed.ID
	}
	return result, nil
}

// GetMoleculeLastActivity returns the most recent activity on a molecule's
// steps (or on the molecule itself when it has none); a missing molecule is
// an error.
func (s *Store) GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error) {
	var out *types.MoleculeLastActivity
	err := s.read(func(r kv) error {
		v, err := moleculeLastActivity(r, moleculeID)
		out = v
		return err
	})
	return out, err
}
