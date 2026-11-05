package pebble

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
)

// Metadata slots: per-issue key/value pairs kept in the row's metadata JSON
// (issueops.MergeMetadataInTx / DeleteMetadataInTx). Every write is a
// read-modify-write of the whole object inside one transaction and goes
// through the generic update, so it records an "updated" event and runs the
// configured metadata validation.

// metadataMap reads a row's metadata as a raw-value map (empty for none).
func metadataMap(r kv, issueID string) (map[string]json.RawMessage, error) {
	rec, err := getRecord(r, issueID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]json.RawMessage)
	raw := string(rec.Issue.Metadata)
	if raw != "" && raw != "null" {
		if err := json.Unmarshal(rec.Issue.Metadata, &m); err != nil {
			return nil, fmt.Errorf("parse metadata for %s: %w", issueID, err)
		}
	}
	return m, nil
}

// writeMetadata validates the merged object and writes it through the
// generic update, as issueops.writeMergedMetadataInTx does.
func (t *txn) writeMetadata(issueID string, m map[string]json.RawMessage, actor string) error {
	merged, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal metadata for %s: %w", issueID, err)
	}
	if err := storageissueops.ValidateMetadataIfConfigured(json.RawMessage(merged)); err != nil {
		return err
	}
	_, err = t.updateIssue(issueID, map[string]interface{}{"metadata": string(merged)}, actor, storage.UpdateIssueOptions{})
	return err
}

// MergeMetadata merges one key into the issue's metadata JSON as a raw JSON
// value, keeping every other key.
func (s *Store) MergeMetadata(ctx context.Context, issueID, key string, value json.RawMessage, actor string) error {
	if !json.Valid(value) {
		return fmt.Errorf("%w: metadata value for key %q on %s is not valid JSON", storage.ErrValidation, key, issueID)
	}
	return s.run(ctx, func(t *txn) error {
		m, err := metadataMap(t.kv, issueID)
		if err != nil {
			return err
		}
		m[key] = value
		if err := t.writeMetadata(issueID, m, actor); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}

// SlotSet stores value under key in the issue's metadata as a JSON string.
func (s *Store) SlotSet(ctx context.Context, issueID, key, value, actor string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshaling slot value for %s.%s: %w", issueID, key, err)
	}
	return s.MergeMetadata(ctx, issueID, key, raw, actor)
}

// SlotGet returns the value under key: a string as is, any other JSON value
// re-encoded. A missing issue, no metadata or an absent key is an error.
func (s *Store) SlotGet(ctx context.Context, issueID, key string) (string, error) {
	var out string
	err := s.read(func(r kv) error {
		rec, err := getRecord(r, issueID)
		if err != nil {
			return fmt.Errorf("getting issue %s: %w", issueID, err)
		}
		if len(rec.Issue.Metadata) == 0 {
			return fmt.Errorf("no slot %q on %s: no metadata", key, issueID)
		}
		metadata := make(map[string]interface{})
		if err := json.Unmarshal(rec.Issue.Metadata, &metadata); err != nil {
			return fmt.Errorf("parsing metadata for %s: %w", issueID, err)
		}
		val, ok := metadata[key]
		if !ok {
			return fmt.Errorf("no slot %q on %s: key not found", key, issueID)
		}
		if str, isString := val.(string); isString {
			out = str
			return nil
		}
		raw, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("marshaling slot value for %s.%s: %w", issueID, key, err)
		}
		out = string(raw)
		return nil
	})
	return out, err
}

// SlotClear removes key from the issue's metadata; an absent key is a
// silent no-op that writes nothing.
func (s *Store) SlotClear(ctx context.Context, issueID, key, actor string) error {
	return s.run(ctx, func(t *txn) error {
		m, err := metadataMap(t.kv, issueID)
		if err != nil {
			return err
		}
		if _, ok := m[key]; !ok {
			return nil
		}
		delete(m, key)
		if err := t.writeMetadata(issueID, m, actor); err != nil {
			return err
		}
		return t.settleBlocked()
	})
}
