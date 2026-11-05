package pebble

import (
	"errors"
	"io"

	"github.com/cockroachdb/pebble/v2"
)

// kv is the read/write surface every operation in this package is written
// against: the DB itself outside a transaction, and an indexed batch inside
// one, so the same code reads its own uncommitted writes in a transaction.
type kv interface {
	get(key []byte) ([]byte, bool, error)
	set(key, value []byte) error
	del(key []byte) error
	// scan calls fn for every key with the given prefix, in key order, until
	// fn returns false or an error. The key and value slices are only valid
	// for the duration of the call.
	scan(prefix []byte, fn func(key, value []byte) (bool, error)) error
}

// dbKV reads and writes the database directly (autocommit, synced).
type dbKV struct{ db *pebble.DB }

func (d dbKV) get(key []byte) ([]byte, bool, error) {
	return pebbleGet(d.db.Get(key))
}

func (d dbKV) set(key, value []byte) error { return d.db.Set(key, value, pebble.Sync) }
func (d dbKV) del(key []byte) error        { return d.db.Delete(key, pebble.Sync) }

func (d dbKV) scan(prefix []byte, fn func(key, value []byte) (bool, error)) error {
	it, err := d.db.NewIter(prefixBounds(prefix))
	if err != nil {
		return err
	}
	return scanIter(it, fn)
}

// batchKV reads and writes an indexed batch: a transaction's working set.
type batchKV struct{ b *pebble.Batch }

func (b batchKV) get(key []byte) ([]byte, bool, error) {
	return pebbleGet(b.b.Get(key))
}

func (b batchKV) set(key, value []byte) error { return b.b.Set(key, value, nil) }
func (b batchKV) del(key []byte) error        { return b.b.Delete(key, nil) }

func (b batchKV) scan(prefix []byte, fn func(key, value []byte) (bool, error)) error {
	it, err := b.b.NewIter(prefixBounds(prefix))
	if err != nil {
		return err
	}
	return scanIter(it, fn)
}

// pebbleGet adapts Pebble's (value, closer, err) triple: the value is copied
// out before the closer releases it, and ErrNotFound becomes (nil, false).
func pebbleGet(value []byte, closer io.Closer, err error) ([]byte, bool, error) {
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := make([]byte, len(value))
	copy(out, value)
	_ = closer.Close()
	return out, true, nil
}

func scanIter(it *pebble.Iterator, fn func(key, value []byte) (bool, error)) error {
	defer it.Close()
	for ok := it.First(); ok; ok = it.Next() {
		more, err := fn(it.Key(), it.Value())
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return it.Error()
}

// prefixBounds returns the iterator bounds covering every key that starts
// with prefix: [prefix, prefix+1) in byte order.
func prefixBounds(prefix []byte) *pebble.IterOptions {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xff {
			upper[i]++
			upper = upper[:i+1]
			return &pebble.IterOptions{LowerBound: prefix, UpperBound: upper}
		}
	}
	return &pebble.IterOptions{LowerBound: prefix}
}
