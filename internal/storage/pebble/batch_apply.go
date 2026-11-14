package pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// BatchApplier serves the ordered multi-item apply: creates, updates, closes
// and edge adds in one transaction, with request-local keys resolving to the
// ids the creates minted.
func (s *Store) BatchApplier() (publicops.BatchApplier, error) {
	return &batchApplier{s: s}, nil
}

type batchApplier struct{ s *Store }

var _ publicops.BatchApplier = (*batchApplier)(nil)

func (b *batchApplier) ApplyBatch(ctx context.Context, request publicops.ApplyBatchRequest) (publicops.ApplyBatchResult, error) {
	plan, err := storage.PlanApplyBatch(request)
	if err != nil {
		return publicops.ApplyBatchResult{}, err
	}
	run := &applyRun{
		plan:   plan,
		keys:   make(map[string]string, len(plan.KeyIndex)),
		planes: map[string]bool{},
		result: publicops.ApplyBatchResult{
			Keys:  make(map[string]string, len(plan.KeyIndex)),
			Items: make([]publicops.ItemResult, len(plan.Items)),
		},
	}
	if err := b.s.run(ctx, run.apply); err != nil {
		return publicops.ApplyBatchResult{}, err
	}
	return run.result, nil
}

// applyRun is one request in flight: the plan, the keys it has bound so far,
// the plane of every row it created, and the edges it landed for the end gate.
type applyRun struct {
	plan   storage.ApplyBatchPlan
	keys   map[string]string
	planes map[string]bool
	edges  []appliedEdge
	result publicops.ApplyBatchResult
}

type appliedEdge struct {
	index int
	edge  publicops.DependencyEdge
}

func (r *applyRun) apply(t *txn) error {
	for i := range r.plan.Items {
		if err := r.applyItem(t, i); err != nil {
			return err
		}
	}
	if err := r.spliceMetadataRefs(t); err != nil {
		return err
	}
	if err := r.endGate(t); err != nil {
		return err
	}
	return t.settleBlocked()
}

func (r *applyRun) applyItem(t *txn, index int) error {
	item := r.plan.Items[index]
	switch item.Kind {
	case publicops.ItemCreate:
		return r.applyCreate(t, index, item.Create)
	case publicops.ItemUpdate:
		return r.applyUpdate(t, index, item.Update)
	case publicops.ItemClose:
		return r.applyClose(t, index, item.Close)
	case publicops.ItemDepAdd:
		return r.applyDepAdd(t, index, item.DepAdd)
	}
	return fmt.Errorf("%w: apply batch item %d has unknown kind %q", storage.ErrValidation, index, item.Kind)
}

func (r *applyRun) applyCreate(t *txn, index int, item *publicops.CreateItem) error {
	itemErr := func(err error) error {
		return &publicops.ItemError{Index: index, Kind: publicops.ItemCreate, Key: item.Key, Err: err}
	}
	attempt := storageissueops.CloneCreateRequest(publicops.CreateRequest{
		Actor:           r.plan.Actor,
		Issue:           item.Issue,
		ForceIDPrefix:   r.plan.ForceIDPrefix,
		DefaultPriority: item.DefaultPriority,
	})
	if err := storageissueops.ValidatePublicCreateRequest(attempt); err != nil {
		return itemErr(err)
	}
	id, err := t.lifecycleCreate(attempt)
	if err != nil {
		return itemErr(err)
	}
	if err := t.settleBlocked(); err != nil {
		return err
	}
	created, err := loadIssue(t.kv, id)
	if err != nil {
		return err
	}
	if item.Key != "" {
		r.keys[item.Key] = id
		r.result.Keys[item.Key] = id
	}
	r.planes[id] = storageissueops.IsWisp(created)
	r.result.Items[index] = publicops.ItemResult{
		Kind:       publicops.ItemCreate,
		IssueID:    id,
		Changed:    true,
		RowVersion: created.RowVersion,
		Issue:      created,
	}
	return nil
}

func (r *applyRun) applyUpdate(t *txn, index int, item *publicops.UpdateItem) error {
	id, err := r.resolve(item.Target, index, "target")
	if err != nil {
		return err
	}
	itemErr := func(err error) error {
		return &publicops.ItemError{Index: index, Kind: publicops.ItemUpdate, Key: item.Target.Key, IssueID: id, Err: err}
	}
	attempt := storageissueops.CloneUpdateRequest(publicops.UpdateRequest{
		Actor:                 r.plan.Actor,
		IssueID:               id,
		Patch:                 item.Patch,
		ExpectedVersion:       item.ExpectedVersion,
		ExpectedStatus:        item.ExpectedStatus,
		ExpectedAssignee:      item.ExpectedAssignee,
		ForceClosePolicy:      item.ForceClosePolicy,
		ForceAssigneeTransfer: item.ForceAssigneeTransfer,
		ForceNotesOverwrite:   item.ForceNotesOverwrite,
	})
	if err := validateUpdateRequest(attempt); err != nil {
		return itemErr(err)
	}
	changed, err := t.lifecycleUpdate(attempt, true)
	if err != nil {
		return itemErr(err)
	}
	if err := t.settleBlocked(); err != nil {
		return err
	}
	updated, err := loadIssue(t.kv, id)
	if err != nil {
		return err
	}
	r.result.Items[index] = publicops.ItemResult{
		Kind:       publicops.ItemUpdate,
		IssueID:    id,
		Changed:    changed,
		RowVersion: updated.RowVersion,
		Issue:      updated,
	}
	return nil
}

func (r *applyRun) applyClose(t *txn, index int, item *publicops.CloseItem) error {
	id, err := r.resolve(item.Target, index, "target")
	if err != nil {
		return err
	}
	itemErr := func(err error) error {
		return &publicops.ItemError{Index: index, Kind: publicops.ItemClose, Key: item.Target.Key, IssueID: id, Err: err}
	}
	request := publicops.CloseRequest{
		Actor:           r.plan.Actor,
		IssueID:         id,
		Reason:          item.Reason,
		Session:         item.Session,
		Force:           item.Force,
		ExpectedVersion: item.ExpectedVersion,
	}
	if err := validateCloseRequest(request); err != nil {
		return itemErr(err)
	}
	unchanged, _, err := t.lifecycleClose(request)
	if err != nil {
		return itemErr(err)
	}
	if err := t.settleBlocked(); err != nil {
		return err
	}
	closed, err := loadIssue(t.kv, id)
	if err != nil {
		return err
	}
	r.result.Items[index] = publicops.ItemResult{
		Kind:       publicops.ItemClose,
		IssueID:    id,
		Changed:    !unchanged,
		RowVersion: closed.RowVersion,
		Issue:      closed,
	}
	return nil
}

func (r *applyRun) applyDepAdd(t *txn, index int, item *publicops.DepAddItem) error {
	source, err := r.resolve(item.Source, index, "source")
	if err != nil {
		return err
	}
	target, err := r.resolve(item.Target, index, "target")
	if err != nil {
		return err
	}
	itemErr := func(err error) error {
		return &publicops.ItemError{Index: index, Kind: publicops.ItemDepAdd, Key: item.Source.Key, IssueID: source, Err: err}
	}
	if source == target {
		return itemErr(fmt.Errorf("%w: %s", publicops.ErrSelfDependency, source))
	}
	if err := publicops.CheckDottedChildDependency(source, target, item.Type); err != nil {
		return itemErr(err)
	}
	sourceWisp, sourceMine := r.planes[source]
	targetWisp, targetMine := r.planes[target]
	if sourceMine && targetMine && sourceWisp != targetWisp {
		return itemErr(storageissueops.CrossPlaneBatchEdgeError(source, target))
	}
	metadata := item.Metadata
	if item.HasSpawner {
		metadata, err = storageissueops.StampWaitsForSpawnerID(item.Type, item.Metadata, target)
		if err != nil {
			return itemErr(fmt.Errorf("%w: %v", publicops.ErrValidation, err))
		}
	}
	existing, err := edgeBetween(t.kv, source, target)
	if err != nil {
		return err
	}
	dep := &types.Dependency{
		IssueID:     source,
		DependsOnID: target,
		Type:        item.Type,
		Metadata:    metadata,
		ThreadID:    item.ThreadID,
	}
	opts := storage.DependencyAddOptions{SkipCycleCheck: r.plan.SkipPerEdgeCycleCheck, EmitEvent: true}
	if err := t.addDependency(dep, r.plan.Actor, opts); err != nil {
		return itemErr(err)
	}
	if err := t.settleBlocked(); err != nil {
		return err
	}
	r.edges = append(r.edges, appliedEdge{
		index: index,
		edge:  publicops.DependencyEdge{IssueID: source, DependsOnID: target, Type: item.Type},
	})
	r.result.Items[index] = publicops.ItemResult{
		Kind:        publicops.ItemDepAdd,
		IssueID:     source,
		DependsOnID: target,
		Changed:     existing == nil,
	}
	return nil
}

// spliceMetadataRefs rewrites every create item's metadata_refs into the ids
// the request bound, once every create has landed.
func (r *applyRun) spliceMetadataRefs(t *txn) error {
	for index := range r.plan.Items {
		item := r.plan.Items[index]
		if item.Kind != publicops.ItemCreate || len(item.Create.MetadataRefs) == 0 {
			continue
		}
		set, err := r.resolveMetadataRefs(index, item.Create.MetadataRefs)
		if err != nil {
			return err
		}
		id := r.result.Items[index].IssueID
		attempt := publicops.UpdateRequest{
			Actor:   r.plan.Actor,
			IssueID: id,
			Patch:   publicops.IssuePatch{Metadata: publicops.MetadataPatch{Set: set}},
		}
		if _, err := t.lifecycleUpdate(attempt, false); err != nil {
			return &publicops.ItemError{Index: index, Kind: publicops.ItemCreate, Key: item.Create.Key, IssueID: id, Err: err}
		}
		if err := t.settleBlocked(); err != nil {
			return err
		}
		updated, err := loadIssue(t.kv, id)
		if err != nil {
			return err
		}
		r.result.Items[index].Issue = updated
		r.result.Items[index].RowVersion = updated.RowVersion
	}
	return nil
}

func (r *applyRun) resolveMetadataRefs(index int, refs map[string]publicops.Ref) (map[string]json.RawMessage, error) {
	set := make(map[string]json.RawMessage, len(refs))
	metaKeys := make([]string, 0, len(refs))
	for metaKey := range refs {
		metaKeys = append(metaKeys, metaKey)
	}
	sort.Strings(metaKeys)
	for _, metaKey := range metaKeys {
		ref := refs[metaKey]
		id := ref.ID
		if ref.Key != "" {
			resolved, ok := r.keys[ref.Key]
			if !ok {
				return nil, &publicops.RefError{Index: index, Member: "metadata_ref " + metaKey, Key: ref.Key}
			}
			id = resolved
		}
		encoded, err := json.Marshal(id)
		if err != nil {
			return nil, fmt.Errorf("%w: apply batch item %d: encoding metadata_ref %q: %v",
				storage.ErrValidation, index, metaKey, err)
		}
		set[metaKey] = encoded
	}
	return set, nil
}

// endGate runs the whole-request checks the per-edge probes cannot: a cycle
// through the edges the request added together, and a blocking edge across
// a hierarchy the request itself built.
func (r *applyRun) endGate(t *txn) error {
	if len(r.edges) == 0 {
		return nil
	}
	edges := make([]publicops.DependencyEdge, 0, len(r.edges))
	for _, applied := range r.edges {
		edges = append(edges, applied.edge)
	}
	if err := t.checkAddedEdgesForCycles(edges); err != nil {
		return err
	}
	for _, applied := range r.edges {
		dep := &types.Dependency{
			IssueID:     applied.edge.IssueID,
			DependsOnID: applied.edge.DependsOnID,
			Type:        applied.edge.Type,
		}
		if storageissueops.IsExternalDepTarget(dep.IssueID, dep.DependsOnID) {
			continue
		}
		if err := checkBlockingHierarchy(t.kv, dep); err != nil {
			return &publicops.ItemError{Index: applied.index, Kind: publicops.ItemDepAdd, IssueID: dep.IssueID, Err: err}
		}
	}
	return nil
}

func (r *applyRun) resolve(ref publicops.Ref, index int, member string) (string, error) {
	if ref.Key == "" {
		return ref.ID, nil
	}
	id, ok := r.keys[ref.Key]
	if !ok {
		return "", &publicops.RefError{Index: index, Member: member, Key: ref.Key}
	}
	return id, nil
}
