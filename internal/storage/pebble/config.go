package pebble

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// Normalized projections of two config keys. The SQL backends keep
// custom_statuses and custom_types tables that SetConfig refreshes and
// DeleteConfig leaves alone (the conformance audit pins that asymmetry), and
// readers prefer the table over the raw key.
const (
	projCustomStatuses = "custom_statuses"
	projCustomTypes    = "custom_types"
)

func projectionKey(name string) []byte { return []byte("N/" + name) }

// --- config -----------------------------------------------------------------

func getString(r kv, key []byte) (string, bool, error) {
	v, ok, err := r.get(key)
	if err != nil || !ok {
		return "", false, err
	}
	return string(v), true, nil
}

func (s *Store) SetConfig(ctx context.Context, key, value string) error {
	return s.run(ctx, func(t *txn) error { return setConfig(t.kv, key, value) })
}

func setConfig(w kv, key, value string) error {
	if key == "" {
		return fmt.Errorf("%w: config key must not be empty", storage.ErrValidation)
	}
	if err := syncProjection(w, key, value); err != nil {
		return err
	}
	return w.set(configKey(key), []byte(value))
}

// syncProjection mirrors issueops.SyncConfigTables: an invalid status.custom
// value refuses the write, and the parsed sets are stored normalized.
func syncProjection(w kv, key, value string) error {
	switch key {
	case "status.custom":
		var parsed []types.CustomStatus
		if value != "" {
			var err error
			parsed, err = types.ParseCustomStatusConfig(value)
			if err != nil {
				return fmt.Errorf("syncing custom_statuses table: invalid status.custom value: %w", err)
			}
		}
		data, err := encodeJSON(parsed)
		if err != nil {
			return err
		}
		return w.set(projectionKey(projCustomStatuses), data)
	case "types.custom":
		var names []string
		if value != "" {
			names = storageissueops.ParseTypesConfigValue(value)
		}
		data, err := encodeJSON(names)
		if err != nil {
			return err
		}
		return w.set(projectionKey(projCustomTypes), data)
	}
	return nil
}

func (s *Store) GetConfig(ctx context.Context, key string) (string, error) {
	var out string
	err := s.readCtx(ctx, func(r kv) error {
		v, _, err := getString(r, configKey(key))
		out = v
		return err
	})
	return out, err
}

func getConfig(r kv, key string) (string, error) {
	v, _, err := getString(r, configKey(key))
	return v, err
}

func (s *Store) GetAllConfig(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	err := s.readCtx(ctx, func(r kv) error {
		return r.scan([]byte(nsConfig), func(k, v []byte) (bool, error) {
			out[strings.TrimPrefix(string(k), nsConfig)] = string(v)
			return true, nil
		})
	})
	return out, err
}

func (s *Store) DeleteConfig(ctx context.Context, key string) error {
	return s.run(ctx, func(t *txn) error { return t.kv.del(configKey(key)) })
}

// --- metadata ---------------------------------------------------------------

func (s *Store) SetMetadata(ctx context.Context, key, value string) error {
	return s.run(ctx, func(t *txn) error { return t.kv.set(metadataKey(key), []byte(value)) })
}

func (s *Store) GetMetadata(ctx context.Context, key string) (string, error) {
	var out string
	err := s.readCtx(ctx, func(r kv) error {
		v, _, err := getString(r, metadataKey(key))
		out = v
		return err
	})
	return out, err
}

func (s *Store) SetLocalMetadata(ctx context.Context, key, value string) error {
	return s.run(ctx, func(t *txn) error { return t.kv.set(localMetaKey(key), []byte(value)) })
}

func (s *Store) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	var out string
	err := s.readCtx(ctx, func(r kv) error {
		v, _, err := getString(r, localMetaKey(key))
		out = v
		return err
	})
	return out, err
}

// --- vocabulary -------------------------------------------------------------

func customStatuses(r kv) ([]types.CustomStatus, error) {
	if data, ok, err := r.get(projectionKey(projCustomStatuses)); err != nil {
		return nil, err
	} else if ok {
		parsed, err := decodeJSON[[]types.CustomStatus](data)
		if err != nil {
			return nil, err
		}
		if len(*parsed) > 0 {
			out := append([]types.CustomStatus(nil), (*parsed)...)
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out, nil
		}
	}
	value, err := getConfig(r, "status.custom")
	if err != nil {
		return nil, err
	}
	if value != "" {
		parsed, err := types.ParseCustomStatusConfig(value)
		if err != nil {
			return nil, nil
		}
		return parsed, nil
	}
	if yamlStatuses := config.GetCustomStatusesFromYAML(); len(yamlStatuses) > 0 {
		return storageissueops.ParseStatusFallback(yamlStatuses), nil
	}
	return nil, nil
}

func customTypes(r kv) ([]string, error) {
	var fromTable []string
	if data, ok, err := r.get(projectionKey(projCustomTypes)); err != nil {
		return nil, err
	} else if ok {
		parsed, err := decodeJSON[[]string](data)
		if err != nil {
			return nil, err
		}
		fromTable = append([]string(nil), (*parsed)...)
		sort.Strings(fromTable)
	}
	var configValue string
	if len(fromTable) == 0 {
		v, err := getConfig(r, "types.custom")
		if err != nil {
			return nil, err
		}
		configValue = v
	}
	return storageissueops.ComposeCustomTypes(fromTable, configValue, config.GetCustomTypesFromYAML()), nil
}

func (s *Store) GetCustomStatuses(ctx context.Context) ([]string, error) {
	detailed, err := s.GetCustomStatusesDetailed(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(detailed))
	for _, st := range detailed {
		out = append(out, st.Name)
	}
	return out, nil
}

func (s *Store) GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error) {
	var out []types.CustomStatus
	err := s.read(func(r kv) error {
		v, err := customStatuses(r)
		out = v
		return err
	})
	return out, err
}

func (s *Store) GetCustomTypes(ctx context.Context) ([]string, error) {
	var out []string
	err := s.read(func(r kv) error {
		v, err := customTypes(r)
		out = v
		return err
	})
	return out, err
}

// infraTypes mirrors issueops.ResolveInfraTypesInTx: the types.infra config
// key, else the YAML setting, else the built-in default set.
func infraTypes(r kv) map[string]bool {
	var typeList []string
	if value, err := getConfig(r, "types.infra"); err == nil && value != "" {
		typeList = storageissueops.ParseCommaSeparatedList(value)
	}
	if len(typeList) == 0 {
		if yamlTypes := config.GetInfraTypesFromYAML(); len(yamlTypes) > 0 {
			typeList = yamlTypes
		}
	}
	if len(typeList) == 0 {
		typeList = domain.DefaultInfraTypes()
	}
	out := make(map[string]bool, len(typeList))
	for _, t := range typeList {
		out[t] = true
	}
	return out
}

func (s *Store) GetInfraTypes(ctx context.Context) map[string]bool {
	var out map[string]bool
	_ = s.read(func(r kv) error { out = infraTypes(r); return nil })
	return out
}

func (s *Store) IsInfraTypeCtx(ctx context.Context, t types.IssueType) bool {
	return s.GetInfraTypes(ctx)[string(t)]
}

// --- repo mtime -------------------------------------------------------------

func (s *Store) GetRepoMtime(ctx context.Context, repoPath string) (int64, error) {
	var out int64
	err := s.read(func(r kv) error {
		data, ok, err := r.get(repoMtimeKey(repoPath))
		if err != nil || !ok {
			return err
		}
		rec, err := decodeJSON[repoMtime](data)
		if err != nil {
			return err
		}
		out = rec.MtimeNs
		return nil
	})
	return out, err
}

func (s *Store) SetRepoMtime(ctx context.Context, repoPath, jsonlPath string, mtimeNs int64) error {
	data, err := encodeJSON(repoMtime{JSONLPath: jsonlPath, MtimeNs: mtimeNs})
	if err != nil {
		return err
	}
	return s.run(ctx, func(t *txn) error { return t.kv.set(repoMtimeKey(repoPath), data) })
}

// ClearRepoMtime deletes the record under the ABSOLUTE form of repoPath. The
// SQL backends resolve the path before their DELETE while Set and Get use it
// verbatim, and the conformance audit pins that asymmetry.
func (s *Store) ClearRepoMtime(ctx context.Context, repoPath string) error {
	if abs, err := filepath.Abs(repoPath); err == nil {
		repoPath = abs
	}
	return s.run(ctx, func(t *txn) error { return t.kv.del(repoMtimeKey(repoPath)) })
}
