package pebble

import (
	"context"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// RunInTransaction runs fn against one write transaction: every method of
// the Transaction it yields reads and writes the same indexed batch, so fn
// reads its own writes and sees no concurrent writer, and the batch commits
// once when fn returns nil (after the blocked-state fixpoint) or is
// discarded when it returns an error. commitMsg is accepted for signature
// parity and ignored: this backend keeps no commit graph.
func (s *Store) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx storage.Transaction) error) error {
	_ = commitMsg
	return s.run(ctx, func(t *txn) error {
		if err := fn(&transaction{t: t}); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// transaction is storage.Transaction over one txn. Every write settles the
// blocked-state fixpoint before returning, so a read inside the transaction
// sees the flags its own writes imply (TransactionReadYourWrites).
type transaction struct{ t *txn }

func (x *transaction) settled(err error) error {
	if err != nil {
		return err
	}
	return x.t.settleBlocked()
}

var _ storage.Transaction = (*transaction)(nil)

func (x *transaction) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if _, err := x.t.createIssue(issue, actor, storage.BatchCreateOptions{SkipPrefixValidation: true}); err != nil {
		return err
	}
	return x.settled(x.t.persistCreateDependencies([]*types.Issue{issue}, actor, storage.BatchCreateOptions{}))
}

func (x *transaction) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	var accepted []*types.Issue
	for _, issue := range issues {
		written, err := x.t.createIssue(issue, actor, storage.BatchCreateOptions{})
		if err != nil {
			return err
		}
		if written {
			accepted = append(accepted, issue)
		}
	}
	return x.settled(x.t.persistCreateDependencies(accepted, actor, storage.BatchCreateOptions{}))
}

func (x *transaction) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	_, err := x.t.updateIssue(id, updates, actor, storage.UpdateIssueOptions{})
	return x.settled(err)
}

func (x *transaction) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	_, err := x.t.closeIssue(id, reason, actor, session, nil)
	return x.settled(err)
}

func (x *transaction) DeleteIssue(ctx context.Context, id string) error {
	return x.settled(x.t.deleteIssue(id))
}

func (x *transaction) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	return loadIssue(x.t.kv, id)
}

func (x *transaction) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	return searchIssues(x.t.kv, query, filter)
}

func (x *transaction) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	return searchIssueIDs(x.t.kv, query, filter)
}

func (x *transaction) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return x.settled(x.t.addDependency(dep, actor, storage.DependencyAddOptions{EmitEvent: true}))
}

func (x *transaction) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts storage.DependencyAddOptions) error {
	return x.settled(x.t.addDependency(dep, actor, opts))
}

func (x *transaction) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return x.settled(x.t.removeEdge(issueID, dependsOnID, actor, storage.DependencyRemoveOptions{EmitEvent: true}))
}

func (x *transaction) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts storage.DependencyRemoveOptions) error {
	return x.settled(x.t.removeEdge(issueID, dependsOnID, actor, opts))
}

func (x *transaction) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	return dependencyRecords(x.t.kv, issueID)
}

func (x *transaction) CycleThroughEdges(ctx context.Context, edges [][2]string) (string, error) {
	return cycleThroughEdges(x.t.kv, edges)
}

func (x *transaction) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return x.t.addLabel(issueID, label, actor)
}

func (x *transaction) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return x.t.removeLabel(issueID, label, actor)
}

func (x *transaction) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	rec, err := getRecord(x.t.kv, issueID)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return dedupeSorted(rec.Issue.Labels), nil
}

func (x *transaction) SetConfig(ctx context.Context, key, value string) error {
	return setConfig(x.t.kv, key, value)
}

func (x *transaction) GetConfig(ctx context.Context, key string) (string, error) {
	return getConfig(x.t.kv, key)
}

func (x *transaction) SetMetadata(ctx context.Context, key, value string) error {
	return x.t.kv.set(metadataKey(key), []byte(value))
}

func (x *transaction) GetMetadata(ctx context.Context, key string) (string, error) {
	v, _, err := getString(x.t.kv, metadataKey(key))
	return v, err
}

func (x *transaction) SetLocalMetadata(ctx context.Context, key, value string) error {
	return x.t.kv.set(localMetaKey(key), []byte(value))
}

func (x *transaction) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	v, _, err := getString(x.t.kv, localMetaKey(key))
	return v, err
}

func (x *transaction) AddComment(ctx context.Context, issueID, actor, comment string) error {
	_, err := x.t.addLiveComment(issueID, actor, comment)
	return err
}

func (x *transaction) ImportIssueComment(ctx context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error) {
	rec, err := getRecord(x.t.kv, issueID)
	if err != nil {
		return nil, err
	}
	if createdAt.IsZero() {
		createdAt, err = nextLiveCommentTime(x.t.kv, issueID, nowUTC())
		if err != nil {
			return nil, err
		}
	}
	return x.t.insertComment(rec, author, text, createdAt)
}

func (x *transaction) GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) {
	return commentsOf(x.t.kv, issueID)
}

func (x *transaction) GetIssueCommentsPage(ctx context.Context, issueID string, after storage.CommentPageCursor, limit int) ([]*types.Comment, error) {
	return commentsPage(x.t.kv, issueID, after, limit)
}

func (x *transaction) CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error) {
	return countIssuesByGroup(x.t.kv, filter, groupBy)
}

func (x *transaction) GetDependentRecords(ctx context.Context, targetID string, depType string, limit int, afterID string) ([]*types.Dependency, error) {
	return dependentRecords(x.t.kv, targetID, depType, limit, afterID)
}

func (x *transaction) GetDependentRecordsForIssues(ctx context.Context, targetIDs []string) (map[string][]*types.Dependency, error) {
	return dependentRecordsForIssues(x.t.kv, targetIDs)
}

func (x *transaction) CountDependentRecords(ctx context.Context, targetID string, depType string) (int, error) {
	return countDependentRecords(x.t.kv, targetID, depType)
}

func (x *transaction) IsBlocked(ctx context.Context, issueID string) (bool, []string, error) {
	return isBlocked(x.t.kv, issueID)
}

func (x *transaction) IsBlockedBatch(ctx context.Context, ids []string) (map[string]bool, error) {
	return isBlockedBatch(x.t.kv, ids)
}

func (x *transaction) EventsSince(ctx context.Context, cursor storage.EventCursor, issueID string, limit int) ([]*types.Event, error) {
	return eventsSince(x.t.kv, cursor, issueID, limit)
}
