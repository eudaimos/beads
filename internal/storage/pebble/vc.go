package pebble

import "context"

// The commit family is answered as a no-op, the way the http backend answers
// it: every write here is durable when its transaction commits, so there is
// no pending work for a Dolt-style commit to land, and bd's auto-commit after
// each verb must not fail on a store with nothing to commit. The rest of the
// version-control surface (branches, merges, remotes, history, as-of) stays
// unsupported through the generated shell.

func (s *Store) Commit(context.Context, string) error                { return nil }
func (s *Store) CommitWithConfig(context.Context, string) error      { return nil }
func (s *Store) CommitMergeResolution(context.Context, string) error { return nil }
func (s *Store) CommitConfigOnly(context.Context, string) error      { return nil }
func (s *Store) CommitConfigUserKVOnly(context.Context, string) error {
	return nil
}
func (s *Store) CommitAll(context.Context, string) (bool, error)     { return false, nil }
func (s *Store) CommitPending(context.Context, string) (bool, error) { return false, nil }
