package pebble

import (
	"context"
	"strings"

	"github.com/steveyegge/beads/internal/memoryapi"
	storagememoryops "github.com/steveyegge/beads/internal/storage/memoryops"
	"github.com/steveyegge/beads/memoryops"
)

// Memories serves the persistent-memory plane (`bd remember`, `bd recall`,
// `bd forget`, `bd memories`) over the config keys, under the one storage
// prefix the SQL bodies encode with (storagememoryops.StorageKey).
func (s *Store) Memories() (memoryops.Memories, error) {
	return &memories{s: s}, nil
}

type memories struct{ s *Store }

var _ memoryops.Memories = (*memories)(nil)

// Remember validates and derives BEFORE the transaction, so a refusal cannot
// be mistaken for a write that rolled back, then probes and writes inside
// one. Replaced is about the ROW: a memory stored as the empty string reports
// true, as Recall reports it found.
func (m *memories) Remember(ctx context.Context, req memoryops.RememberRequest) (memoryops.RememberResult, error) {
	key, err := memoryapi.ResolveKey(req.Key, req.Content)
	if err != nil {
		return memoryops.RememberResult{}, err
	}
	storageKey := storagememoryops.StorageKey(key)
	var replaced bool
	err = m.s.run(ctx, func(t *txn) error {
		_, exists, err := getString(t.kv, configKey(storageKey))
		if err != nil {
			return err
		}
		replaced = exists
		return setConfig(t.kv, storageKey, req.Content)
	})
	if err != nil {
		return memoryops.RememberResult{}, err
	}
	return memoryops.RememberResult{Key: key, Value: req.Content, Replaced: replaced}, nil
}

// Recall reads one memory and reports whether its row exists, so a stored
// empty value is distinct from no row.
func (m *memories) Recall(ctx context.Context, req memoryops.RecallRequest) (memoryops.RecallResult, error) {
	key, err := memoryapi.ValidateKey(req.Key)
	if err != nil {
		return memoryops.RecallResult{}, err
	}
	var (
		value string
		found bool
	)
	err = m.s.readCtx(ctx, func(r kv) error {
		value, found, err = getString(r, configKey(storagememoryops.StorageKey(key)))
		return err
	})
	if err != nil {
		return memoryops.RecallResult{}, err
	}
	return memoryops.RecallResult{Key: key, Value: value, Found: found}, nil
}

// Forget reads the value and deletes the row in ONE transaction, so the value
// it reports is the one it removed. The delete names the one encoded key:
// forgetting "a" cannot take "a-b" with it.
func (m *memories) Forget(ctx context.Context, req memoryops.ForgetRequest) (memoryops.ForgetResult, error) {
	key, err := memoryapi.ValidateKey(req.Key)
	if err != nil {
		return memoryops.ForgetResult{}, err
	}
	storageKey := configKey(storagememoryops.StorageKey(key))
	var (
		previous string
		found    bool
	)
	err = m.s.run(ctx, func(t *txn) error {
		previous, found, err = getString(t.kv, storageKey)
		if err != nil || !found {
			return err
		}
		return t.kv.del(storageKey)
	})
	if err != nil {
		return memoryops.ForgetResult{}, err
	}
	return memoryops.ForgetResult{Key: key, Value: previous, Found: found}, nil
}

// List reads the whole memory plane and narrows it by the search term, both
// through the shared bodies, so which rows are memories and how a term
// matches are decided once for every backend.
func (m *memories) List(ctx context.Context, req memoryops.ListRequest) (memoryops.ListResult, error) {
	all := map[string]string{}
	err := m.s.readCtx(ctx, func(r kv) error {
		return r.scan([]byte(nsConfig), func(k, v []byte) (bool, error) {
			all[strings.TrimPrefix(string(k), nsConfig)] = string(v)
			return true, nil
		})
	})
	if err != nil {
		return memoryops.ListResult{}, err
	}
	return memoryops.ListResult{
		Memories: memoryapi.FilterMemories(storagememoryops.MemoriesFromConfig(all), req.Search),
	}, nil
}
