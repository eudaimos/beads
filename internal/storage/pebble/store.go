package pebble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/backends"
)

// BackendName is the name this backend registers under.
const BackendName = "pebble"

// DataDirName is the directory under .beads that holds the Pebble store.
const DataDirName = "pebble"

func init() {
	backends.Register(BackendName, backends.Backend{
		Open:         openBackend,
		OpenReadOnly: openBackend,
		Create:       createBackend,
		// The store lives under the .beads directory itself; there is no
		// database file for workspace discovery to look for.
		WorkspaceIsBeadsDir: true,
	})
}

func openBackend(ctx context.Context, beadsDir string) (storage.DoltStorage, error) {
	return Open(ctx, filepath.Join(beadsDir, DataDirName))
}

// createBackend provisions the store directory for bd init: an open creates
// it, and closing leaves an empty store behind.
func createBackend(ctx context.Context, beadsDir string) error {
	s, err := Open(ctx, filepath.Join(beadsDir, DataDirName))
	if err != nil {
		return err
	}
	return s.Close()
}

// Store is the Pebble-backed storage.DoltStorage. See the package doc.
type Store struct {
	unsupportedDoltStorage

	db       *pebble.DB
	dir      string
	readOnly bool

	// write serializes transactions: Pebble batches are not isolated from
	// one another, so one writer at a time is what makes a transaction's
	// read-your-writes view and its blocked-state recompute sound.
	write sync.Mutex

	// journal is the events-journal activation for this instance
	// (storage.EventsJournalConfigurer); see journal.go.
	journal atomic.Bool

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

var _ storage.DoltStorage = (*Store)(nil)

// Open opens (creating if needed) the Pebble store at dir.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("pebble: create %s: %w", dir, err)
	}
	return openWith(ctx, dir, &pebble.Options{})
}

// OpenInMemory opens a store that lives in process memory only, for tests
// and measurements.
func OpenInMemory(ctx context.Context) (*Store, error) {
	return openWith(ctx, "", &pebble.Options{FS: vfs.NewMem()})
}

func openWith(ctx context.Context, dir string, opts *pebble.Options) (*Store, error) {
	opts.Logger = silentLogger{}
	db, err := pebble.Open(dir, opts)
	if err != nil {
		return nil, fmt.Errorf("pebble: open %s: %w", dir, err)
	}
	s := &Store{db: db, dir: dir}
	if err := s.ensureIndexes(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pebble: open %s: %w", dir, err)
	}
	return s, nil
}

// silentLogger keeps Pebble's own event log out of test output.
type silentLogger struct{}

func (silentLogger) Infof(string, ...interface{})      {}
func (silentLogger) Errorf(string, ...interface{})     {}
func (silentLogger) Fatalf(f string, a ...interface{}) { panic(fmt.Sprintf(f, a...)) }

// Close releases the store. It is idempotent.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// IsClosed reports whether Close has been called, the way the SQL stores
// report it for the callers that sweep closed handles.
func (s *Store) IsClosed() bool { return s.closed.Load() }

// Dir is the directory the store was opened at ("" in memory).
func (s *Store) Dir() string { return s.dir }

func (s *Store) backendName() string { return BackendName }

func (s *Store) unsupported(op string) error {
	return &storage.ErrUnsupported{Op: op, Backend: BackendName}
}

// read runs fn against the committed database.
func (s *Store) read(fn func(r kv) error) error {
	return fn(dbKV{db: s.db})
}

// readCtx is read for a caller whose context may already be done: a
// canceled read reports the cancellation instead of answering as if nothing
// were wrong, the way the SQL backends' drivers do.
func (s *Store) readCtx(ctx context.Context, fn func(r kv) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.read(fn)
}

// readSnapshot runs fn against one consistent snapshot of the committed
// database, for a read whose parts must not straddle a commit.
func (s *Store) readSnapshot(fn func(r kv) error) error {
	snap := s.db.NewSnapshot()
	defer func() { _ = snap.Close() }()
	return fn(snapKV{snap: snap})
}

// run executes fn inside a write transaction: an indexed batch under the
// write mutex, committed with fsync when fn returns nil and discarded
// otherwise. Nested calls are not supported; a transaction's own methods take
// the batch they already hold.
func (s *Store) run(ctx context.Context, fn func(t *txn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.write.Lock()
	defer s.write.Unlock()
	b := s.db.NewIndexedBatch()
	t := &txn{store: s, kv: batchKV{b: b}, ctx: ctx, journal: s.journal.Load()}
	if err := fn(t); err != nil {
		_ = b.Close()
		return err
	}
	if err := b.Commit(pebble.Sync); err != nil {
		_ = b.Close()
		return fmt.Errorf("pebble: commit: %w", err)
	}
	return b.Close()
}

// errNotFound wraps storage.ErrNotFound the way the SQL backends do.
func errNotFound(what, id string) error {
	return fmt.Errorf("%w: %s %s", storage.ErrNotFound, what, id)
}

func isNotFound(err error) bool { return errors.Is(err, storage.ErrNotFound) }
