package pebble

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// Sweeper serves the bulk clearance of closed rows behind `bd purge` and
// `bd prune`.
func (s *Store) Sweeper() (publicops.Sweeper, error) {
	return &sweeper{s: s}, nil
}

type sweeper struct{ s *Store }

var _ publicops.Sweeper = (*sweeper)(nil)

// Sweep is issueops.SweepInTx over the key space: the candidate query, the
// pattern and closed_at rechecks, the retained-type partition, the two
// protections, the limit and the deletion all run in ONE transaction, so the
// set the result describes is the set it deleted. The narrowing runs in the
// order the role states, through the shared workapi bodies, so the skip
// counters are the SQL legs' own.
//
// VALIDATION HAPPENS BEFORE THE TRANSACTION, so a refused request reads
// nothing. A retaining sweep is refused with *ErrUnsupported: this store
// keeps no id tombstones, and the role forbids sweeping without them.
//
// A dry run runs the same body and stops before the delete, so a preview
// cannot fail where the real run would succeed. The store keeps no commit
// log, so the one history entry the role permits is not minted here.
func (w *sweeper) Sweep(ctx context.Context, req publicops.SweepRequest) (publicops.SweepResult, error) {
	if err := workapi.ValidateSweepRequest(req); err != nil {
		return publicops.SweepResult{}, err
	}
	if req.RetainIDs {
		return publicops.SweepResult{}, &storage.ErrUnsupported{Op: "Sweep with RetainIDs", Backend: BackendName}
	}
	var result publicops.SweepResult
	err := w.s.run(ctx, func(t *txn) error {
		result = publicops.SweepResult{}
		kept, selected, err := selectSweep(t.kv, req)
		if err != nil {
			return err
		}
		result = selected
		if len(kept) == 0 {
			return nil
		}
		ids := make([]string, len(kept))
		for i, issue := range kept {
			ids[i] = issue.ID
		}
		// cascade=false, force=true: a sweep deletes what it selected and
		// orphans dependents outside the selection, in both modes.
		deleted, err := t.deleteIssues(ids, false, true, req.DryRun, req.Actor)
		if err != nil {
			return err
		}
		result.Swept = deleted.DeletedCount
		result.Dependencies = deleted.DependenciesCount
		result.Labels = deleted.LabelsCount
		result.Events = deleted.EventsCount
		if req.DryRun {
			return nil
		}
		return t.settleBlocked()
	})
	if err != nil {
		return publicops.SweepResult{}, err
	}
	return result, nil
}

// selectSweep resolves the rows a sweep request reaches: the tier's closed
// candidates, narrowed in the role's order, with the skips counted along the
// way. It reads only, so a dry run and a real sweep select identically.
func selectSweep(r kv, req publicops.SweepRequest) ([]*types.Issue, publicops.SweepResult, error) {
	result := publicops.SweepResult{DryRun: req.DryRun, RetainIDs: req.RetainIDs}

	candidates, err := sweepCandidates(r, req)
	if err != nil {
		return nil, publicops.SweepResult{}, fmt.Errorf("listing sweep candidates: %w", err)
	}

	kept, skips := workapi.FilterSweepCandidates(candidates, req.IDPattern, req.ClosedBefore)
	result.Skipped = skips

	if !req.RetainIDs {
		value, err := getConfig(r, publicops.SettingKeyTypesRetained)
		if err != nil {
			return nil, publicops.SweepResult{}, fmt.Errorf("reading %s for the retained-type protection: %w", publicops.SettingKeyTypesRetained, err)
		}
		kept, result.Skipped.RetentionRequired = workapi.PartitionSweepRetainedTypes(kept, workapi.RetainedTypesFromSetting(value))
	}

	if req.ProtectLiveDependents {
		protected, err := sweepLiveDependents(r, kept)
		if err != nil {
			return nil, publicops.SweepResult{}, err
		}
		kept, result.Skipped.LiveDependent = workapi.PartitionSweepLiveDependents(kept, protected)
	}

	if req.ProtectReferenced {
		referenced, err := sweepReferenced(r, kept)
		if err != nil {
			return nil, publicops.SweepResult{}, err
		}
		var count int
		kept, count, result.ReferencedIDs = workapi.PartitionSweepReferenced(kept, referenced)
		result.Skipped.Referenced = count
	}

	kept, result.Remaining = workapi.LimitSweepCandidates(kept, req.Limit)
	return kept, result, nil
}

// sweepCandidates is the tier's candidate query: the shared filter over the
// merged scan for the two tiers, and the wisps plane alone for the plane
// sweep (issueops.SearchWispsPlaneInTx), where every closed row stored there
// is a candidate whatever its flags.
func sweepCandidates(r kv, req publicops.SweepRequest) ([]*types.Issue, error) {
	filter := workapi.BuildSweepCandidateFilter(req)
	if !workapi.SweepSearchesWispsPlaneOnly(req) {
		return searchIssues(r, "", filter)
	}
	recs, err := searchRecords(r, "", filter)
	if err != nil {
		return nil, err
	}
	var out []*types.Issue
	for _, rec := range recs {
		if rec.Plane == planeWisps {
			out = append(out, rec.toIssue())
		}
	}
	return out, nil
}

// sweepLiveDependents is issueops' sweepLiveDependentsInTx over the key
// space: which candidates a live row depends on through a protecting edge,
// directly or through other candidates. The custom statuses are read on the
// same view, and a failure to read them fails the sweep.
func sweepLiveDependents(r kv, candidates []*types.Issue) (map[string]bool, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := make([]string, len(candidates))
	for i, issue := range candidates {
		ids[i] = issue.ID
	}
	incoming, err := dependentRecordsForIssues(r, ids)
	if err != nil {
		return nil, fmt.Errorf("reading dependents for live-dependent protection: %w", err)
	}
	sources := workapi.SweepLiveDependentSources(incoming)
	if len(sources) == 0 {
		return nil, nil
	}
	custom, err := customStatuses(r)
	if err != nil {
		return nil, fmt.Errorf("reading custom statuses for live-dependent protection: %w", err)
	}
	liveRows, err := searchIssues(r, "", workapi.BuildSweepLiveDependentScanFilter(sources, custom))
	if err != nil {
		return nil, fmt.Errorf("reading dependent statuses for live-dependent protection: %w", err)
	}
	live := make(map[string]bool, len(liveRows))
	for _, issue := range liveRows {
		if issue != nil {
			live[issue.ID] = true
		}
	}
	return workapi.SweepLiveDependentTargets(incoming, live), nil
}

// sweepReferenced is issueops' sweepReferencedInTx over the key space: which
// candidates a not-done row cites in its description, its notes or a
// comment, at ASCII word boundaries.
func sweepReferenced(r kv, candidates []*types.Issue) (map[string]bool, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	candidateIDs := make(map[string]bool, len(candidates))
	for _, issue := range candidates {
		candidateIDs[issue.ID] = true
	}
	matcher := workapi.NewCandidateIDMatcher(candidateIDs)

	custom, err := customStatuses(r)
	if err != nil {
		return nil, fmt.Errorf("reading custom statuses for reference scan: %w", err)
	}
	notDone, err := searchIssues(r, "", workapi.BuildSweepReferenceScanFilter(custom))
	if err != nil {
		return nil, fmt.Errorf("scanning open beads for references: %w", err)
	}
	referenced := make(map[string]bool)
	for _, issue := range notDone {
		if issue == nil {
			continue
		}
		matcher.FindAll(issue.Description, referenced)
		matcher.FindAll(issue.Notes, referenced)
		comments, err := commentsOf(r, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("scanning open beads for references: %w", err)
		}
		for _, c := range comments {
			matcher.FindAll(c.Text, referenced)
		}
	}
	return referenced, nil
}
