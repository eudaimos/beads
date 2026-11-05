package pebble

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Commenter serves the add-comment role: the write side of `bd comment`, and
// the role issueops.Promote and gascity's arrival hooks bind through.
func (s *Store) Commenter() (publicops.Commenter, error) {
	return &commenter{s: s}, nil
}

type commenter struct{ s *Store }

var _ publicops.Commenter = (*commenter)(nil)

// AddComment validates outside the transaction and appends the row through
// the live-comment body AddIssueComment shares, so the stamp rule (one
// second past the thread's newest comment, never behind the clock), the
// "commented" event and the journal record are the ones every other comment
// write lands.
//
// The anchor is resolved INSIDE the transaction the insert runs in, as the
// SQL seam's resolveCommentPlaneInTx does, and a missing one is the typed
// ErrNotFound the contract classifies with errors.Is. There is one key space
// for both planes here, so the issue-then-wisp fallback the SQL body runs is a
// single point read; the row's own plane routes the comment's table.
func (c *commenter) AddComment(ctx context.Context, request publicops.AddCommentRequest) (publicops.AddCommentResult, error) {
	if err := storageissueops.ValidateAddCommentRequest(request); err != nil {
		return publicops.AddCommentResult{}, err
	}
	var out *types.Comment
	err := c.s.run(ctx, func(t *txn) error {
		exists, err := recordExists(t.kv, request.IssueID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: issue %s", storage.ErrNotFound, request.IssueID)
		}
		comment, err := t.addLiveComment(request.IssueID, request.Author, request.Text)
		if err != nil {
			return err
		}
		out = comment
		return nil
	})
	if err != nil {
		return publicops.AddCommentResult{}, err
	}
	return publicops.AddCommentResult{Comment: out}, nil
}
