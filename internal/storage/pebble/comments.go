package pebble

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/rowid"
	"github.com/steveyegge/beads/internal/types"
)

// commentsOf returns the row's comments oldest first (created_at, id).
func commentsOf(r kv, id string) ([]*types.Comment, error) {
	var out []*types.Comment
	err := r.scan(commentPrefix(id), func(_, v []byte) (bool, error) {
		c, err := decodeJSON[types.Comment](v)
		if err != nil {
			return false, err
		}
		out = append(out, c)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	stableSortBy(out, func(a, b *types.Comment) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return out, nil
}

// nextLiveCommentTime mirrors issueops.NextLiveCommentTime: a live comment is
// stamped at second granularity and never earlier than the row's newest
// comment, so a burst of comments keeps its order.
func nextLiveCommentTime(r kv, id string, now time.Time) (time.Time, error) {
	now = now.UTC().Truncate(time.Second)
	existing, err := commentsOf(r, id)
	if err != nil {
		return time.Time{}, err
	}
	if len(existing) == 0 {
		return now, nil
	}
	newest := existing[len(existing)-1].CreatedAt.UTC().Truncate(time.Second)
	if newest.Before(now) {
		return now, nil
	}
	return newest.Add(time.Second), nil
}

// insertComment writes one comment with the derived id the SQL backends
// mint (rowid over issue, author, text, created_at); an identical comment
// already stored is returned instead of duplicated.
func (t *txn) insertComment(rec *issueRecord, author, text string, createdAt time.Time) (*types.Comment, error) {
	createdAt = createdAt.UTC()
	existing, err := commentsOf(t.kv, rec.Issue.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range existing {
		if c.Author == author && c.Text == text && c.CreatedAt.Equal(createdAt) {
			return c, nil
		}
	}
	table := "comments"
	if rec.Plane == planeWisps {
		table = "wisp_comments"
	}
	digest := rowid.Digest([]sql.NullString{
		{String: rec.Issue.ID, Valid: true}, {String: author, Valid: true}, {String: text, Valid: true},
		{String: createdAt.Format(auxTimeLayout), Valid: true},
	})
	seq, err := nextSeq(t.kv, "comments")
	if err != nil {
		return nil, err
	}
	c := &types.Comment{ID: rowid.New(table, 0, digest), IssueID: rec.Issue.ID, Author: author, Text: text, CreatedAt: createdAt}
	data, err := encodeJSON(c)
	if err != nil {
		return nil, err
	}
	return c, t.kv.set(commentKey(rec.Issue.ID, createdAt.UnixNano(), seq), data)
}

// addLiveComment is the AddIssueComment / AddComment body: a comment stamped
// now plus the "commented" event and a row touch.
func (t *txn) addLiveComment(id, author, text string) (*types.Comment, error) {
	if err := types.CheckFieldLen("text", text); err != nil {
		return nil, err
	}
	rec, err := getRecord(t.kv, id)
	if err != nil {
		return nil, err
	}
	at, err := nextLiveCommentTime(t.kv, id, nowUTC())
	if err != nil {
		return nil, err
	}
	c, err := t.insertComment(rec, author, text, at)
	if err != nil {
		return nil, err
	}
	if err := t.recordEvent(rec, types.EventCommented, author, nil, nil, strPtr(text)); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	var out *types.Comment
	err := s.run(ctx, func(t *txn) error {
		c, err := t.addLiveComment(issueID, author, text)
		out = c
		return err
	})
	return out, err
}

func (s *Store) AddComment(ctx context.Context, issueID, actor, comment string) error {
	_, err := s.AddIssueComment(ctx, issueID, actor, comment)
	return err
}

func (s *Store) ImportIssueComment(ctx context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error) {
	var out *types.Comment
	err := s.run(ctx, func(t *txn) error {
		rec, err := getRecord(t.kv, issueID)
		if err != nil {
			return err
		}
		if createdAt.IsZero() {
			createdAt, err = nextLiveCommentTime(t.kv, issueID, nowUTC())
			if err != nil {
				return err
			}
		}
		c, err := t.insertComment(rec, author, text, createdAt)
		out = c
		return err
	})
	return out, err
}

func (s *Store) GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) {
	var out []*types.Comment
	err := s.read(func(r kv) error {
		v, err := commentsOf(r, issueID)
		out = v
		return err
	})
	return out, err
}

// commentsPage returns the comments strictly after the cursor, oldest first,
// at most limit (<= 0: all).
func commentsPage(r kv, issueID string, after storage.CommentPageCursor, limit int) ([]*types.Comment, error) {
	all, err := commentsOf(r, issueID)
	if err != nil {
		return nil, err
	}
	var out []*types.Comment
	for _, c := range all {
		if !after.CreatedAt.IsZero() || after.ID != "" {
			if c.CreatedAt.Before(after.CreatedAt) || (c.CreatedAt.Equal(after.CreatedAt) && c.ID <= after.ID) {
				continue
			}
		}
		out = append(out, c)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Store) GetIssueCommentsPage(ctx context.Context, issueID string, after storage.CommentPageCursor, limit int) ([]*types.Comment, error) {
	var out []*types.Comment
	err := s.read(func(r kv) error {
		v, err := commentsPage(r, issueID, after, limit)
		out = v
		return err
	})
	return out, err
}

func (s *Store) CountIssueComments(ctx context.Context, issueID string) (int64, error) {
	cs, err := s.GetIssueComments(ctx, issueID)
	return int64(len(cs)), err
}

func (s *Store) GetCommentCounts(ctx context.Context, issueIDs []string) (map[string]int, error) {
	out := map[string]int{}
	err := s.read(func(r kv) error {
		for _, id := range issueIDs {
			cs, err := commentsOf(r, id)
			if err != nil {
				return err
			}
			if len(cs) > 0 {
				out[id] = len(cs)
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) GetCommentsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Comment, error) {
	out := map[string][]*types.Comment{}
	err := s.read(func(r kv) error {
		for _, id := range issueIDs {
			cs, err := commentsOf(r, id)
			if err != nil {
				return err
			}
			if len(cs) > 0 {
				out[id] = cs
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) IterIssueComments(ctx context.Context, issueID string) (storage.Iter[types.Comment], error) {
	cs, err := s.GetIssueComments(ctx, issueID)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(cs), nil
}

var _ = fmt.Sprintf
