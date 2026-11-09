package pebble

import (
	"context"
	"sort"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

func (t *txn) addLabel(issueID, label, actor string) error {
	if err := types.CheckFieldLen("label", label); err != nil {
		return err
	}
	rec, err := getRecord(t.kv, issueID)
	if err != nil {
		return err
	}
	old := *rec
	had := false
	for _, l := range rec.Issue.Labels {
		if l == label {
			had = true
			break
		}
	}
	if !had {
		// A label write is not a write to the issue row: the SQL backends
		// insert into the labels table and leave row_lock and updated_at
		// alone, so a caller's expected_version survives a label-only edit.
		// The labels ride in the record here, and the version must not.
		rec.Issue.Labels = append(append([]string(nil), rec.Issue.Labels...), label)
		sort.Strings(rec.Issue.Labels)
		if err := putRecord(t.kv, &old, rec); err != nil {
			return err
		}
	}
	// The SQL backends record the event whether or not the row changed
	// (the audit's LabelEventNonIdempotent case pins that).
	if err := t.recordEvent(rec, types.EventLabelAdded, actor, nil, nil, strPtr("Added label: "+label)); err != nil {
		return err
	}
	return t.journalIssue(storageissueops.EventUpdate, issueID, actor)
}

func (t *txn) removeLabel(issueID, label, actor string) error {
	rec, err := getRecord(t.kv, issueID)
	if err != nil {
		return err
	}
	old := *rec
	kept := rec.Issue.Labels[:0:0]
	removed := false
	for _, l := range rec.Issue.Labels {
		if l == label {
			removed = true
			continue
		}
		kept = append(kept, l)
	}
	if removed {
		rec.Issue.Labels = kept
		if err := putRecord(t.kv, &old, rec); err != nil {
			return err
		}
	}
	if err := t.recordEvent(rec, types.EventLabelRemoved, actor, nil, nil, strPtr("Removed label: "+label)); err != nil {
		return err
	}
	return t.journalIssue(storageissueops.EventUpdate, issueID, actor)
}

func (s *Store) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return s.run(ctx, func(t *txn) error { return t.addLabel(issueID, label, actor) })
}

func (s *Store) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return s.run(ctx, func(t *txn) error { return t.removeLabel(issueID, label, actor) })
}

func (s *Store) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	var out []string
	err := s.read(func(r kv) error {
		rec, err := getRecord(r, issueID)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		out = append([]string(nil), rec.Issue.Labels...)
		sort.Strings(out)
		return nil
	})
	return out, err
}

func (s *Store) GetLabelsForIssues(ctx context.Context, issueIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	err := s.read(func(r kv) error {
		for _, id := range issueIDs {
			rec, err := getRecord(r, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			if len(rec.Issue.Labels) > 0 {
				labels := append([]string(nil), rec.Issue.Labels...)
				sort.Strings(labels)
				out[id] = labels
			}
		}
		return nil
	})
	return out, err
}

// labelledIDs returns the ids under a label, in id order.
func labelledIDs(r kv, label string) ([]string, error) {
	var out []string
	err := r.scan(labelPrefix(label), func(k, _ []byte) (bool, error) {
		parts := splitKey(nsLabel, k)
		if len(parts) == 2 {
			out = append(out, parts[1])
		}
		return true, nil
	})
	return out, err
}

func (s *Store) GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error) {
	var out []*types.Issue
	err := s.read(func(r kv) error {
		ids, err := labelledIDs(r, label)
		if err != nil {
			return err
		}
		for _, id := range ids {
			rec, err := getRecord(r, id)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return err
			}
			out = append(out, rec.toIssue())
		}
		return nil
	})
	return out, err
}

func (s *Store) RenameLabel(ctx context.Context, oldLabel, newLabel, actor string) (renamed, merged int, ids []string, err error) {
	err = s.run(ctx, func(t *txn) error {
		members, err := labelledIDs(t.kv, oldLabel)
		if err != nil {
			return err
		}
		for _, id := range members {
			rec, err := getRecord(t.kv, id)
			if err != nil {
				return err
			}
			old := *rec
			hasNew := false
			kept := rec.Issue.Labels[:0:0]
			for _, l := range rec.Issue.Labels {
				if l == newLabel {
					hasNew = true
				}
				if l != oldLabel {
					kept = append(kept, l)
				}
			}
			if hasNew {
				merged++
			} else {
				kept = append(kept, newLabel)
				renamed++
			}
			sort.Strings(kept)
			rec.Issue.Labels = kept
			if err := putRecord(t.kv, &old, rec); err != nil {
				return err
			}
			if err := t.recordEvent(rec, types.EventLabelRenamed, actor, strPtr(oldLabel), strPtr(newLabel), nil); err != nil {
				return err
			}
			if err := t.journalIssue(storageissueops.EventUpdate, id, actor); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return renamed, merged, ids, err
}
