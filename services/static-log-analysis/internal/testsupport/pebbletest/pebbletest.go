// Package pebbletest opens real Pebble databases for tests.
//
// The journal's guarantees are durability guarantees, and a mock cannot have
// them: whether an acknowledged record survives a crash is a property of
// Pebble, its write-ahead log, and the filesystem underneath. So the harness
// gives tests a real database, either on disk under the test's temporary
// directory or on a filesystem that can be made to lose exactly the writes a
// real crash would lose.
package pebbletest

import (
	"path/filepath"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

// Store is an open Pebble database together with the directory it lives in.
type Store struct {
	db  *pebble.DB
	dir string
	// fs is the crashable in-memory filesystem when the store supports crash
	// simulation, and nil when the store is on disk.
	fs      *vfs.MemFS
	options *pebble.Options
	closed  bool
}

// Option adjusts the Pebble options a store is opened with.
type Option func(*pebble.Options)

// WithOptions exposes the raw options, for tests about format versions,
// comparers, or size limits.
func WithOptions(configure func(*pebble.Options)) Option { return configure }

// New opens a database on disk under the test's temporary directory, and closes
// it when the test ends.
//
// Use this for tests about content: keys, batches, indexes, accounting, and
// compaction. Use NewCrashable for tests about what survives a crash.
func New(t tb.TB, opts ...Option) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "journal")
	store := &Store{dir: dir, options: buildOptions(vfs.Default, opts)}
	store.open(t)
	t.Cleanup(func() { store.Close(t) })
	return store
}

// NewCrashable opens a database on a filesystem that records which writes were
// synced, so that CrashAndReopen can discard exactly the writes a power loss
// would have discarded.
func NewCrashable(t tb.TB, opts ...Option) *Store {
	t.Helper()
	filesystem := vfs.NewCrashableMem()
	store := &Store{dir: "/journal", fs: filesystem, options: buildOptions(filesystem, opts)}
	store.open(t)
	t.Cleanup(func() { store.Close(t) })
	return store
}

func buildOptions(filesystem vfs.FS, opts []Option) *pebble.Options {
	options := &pebble.Options{
		FS: filesystem,
		// Pebble's own logging is noise in a passing test and is unreadable
		// after the test that produced it has finished.
		Logger: discardLogger{},
	}
	for _, opt := range opts {
		opt(options)
	}
	return options
}

func (s *Store) open(t tb.TB) {
	t.Helper()
	db, err := pebble.Open(s.dir, s.options)
	if err != nil {
		t.Fatalf("pebbletest: opening %s: %v", s.dir, err)
	}
	s.db = db
	s.closed = false
}

// DB returns the open database.
func (s *Store) DB() *pebble.DB { return s.db }

// Dir returns the directory the database lives in.
func (s *Store) Dir() string { return s.dir }

// Options returns the options the database was opened with, so a reopen in a
// test uses the same configuration.
func (s *Store) Options() *pebble.Options { return s.options }

// Close closes the database. It is safe to call more than once, because tests
// that close explicitly still have the cleanup registered.
func (s *Store) Close(t tb.TB) {
	t.Helper()
	if s.closed {
		return
	}
	s.closed = true
	if err := s.db.Close(); err != nil {
		t.Fatalf("pebbletest: closing %s: %v", s.dir, err)
	}
}

// Reopen closes the database cleanly and opens it again, which is what a
// planned restart does. Everything written before the close survives, synced or
// not, because a clean close flushes.
func (s *Store) Reopen(t tb.TB) {
	t.Helper()
	s.Close(t)
	s.open(t)
}

// CrashAndReopen simulates losing power: the database is reopened on a copy of
// the filesystem that contains only what was synced.
//
// A record written with pebble.Sync survives. A record written with
// pebble.NoSync may not, which is exactly why only a synced commit permits an
// OTLP acknowledgement.
func (s *Store) CrashAndReopen(t tb.TB) {
	t.Helper()
	if s.fs == nil {
		t.Fatalf("pebbletest: this store is on disk; use NewCrashable for crash simulation")
	}

	// The clone is taken before the old database is closed, because closing
	// flushes and syncs, which is the opposite of a crash.
	crashed := s.fs.CrashClone(vfs.CrashCloneCfg{})
	if err := s.db.Close(); err != nil {
		t.Logf("pebbletest: closing the abandoned database: %v", err)
	}
	s.closed = true

	s.fs = crashed
	s.options = cloneOptions(s.options, crashed)
	s.open(t)
}

func cloneOptions(options *pebble.Options, filesystem vfs.FS) *pebble.Options {
	cloned := *options
	cloned.FS = filesystem
	return &cloned
}

// Set writes a key with the given durability and fails the test if it cannot.
func (s *Store) Set(t tb.TB, key, value string, options *pebble.WriteOptions) {
	t.Helper()
	if err := s.db.Set([]byte(key), []byte(value), options); err != nil {
		t.Fatalf("pebbletest: setting %s: %v", key, err)
	}
}

// Get returns the value stored under key, and whether it was present.
func (s *Store) Get(t tb.TB, key string) (string, bool) {
	t.Helper()
	value, closer, err := s.db.Get([]byte(key))
	if err == pebble.ErrNotFound {
		return "", false
	}
	if err != nil {
		t.Fatalf("pebbletest: getting %s: %v", key, err)
	}
	defer func() {
		if err := closer.Close(); err != nil {
			t.Fatalf("pebbletest: releasing %s: %v", key, err)
		}
	}()
	return string(value), true
}

// Keys returns every key in the database in order, for assertions about what a
// batch or a compaction left behind.
func (s *Store) Keys(t tb.TB) []string {
	t.Helper()
	iterator, err := s.db.NewIter(nil)
	if err != nil {
		t.Fatalf("pebbletest: creating iterator: %v", err)
	}
	defer func() {
		if err := iterator.Close(); err != nil {
			t.Fatalf("pebbletest: closing iterator: %v", err)
		}
	}()

	var keys []string
	for iterator.First(); iterator.Valid(); iterator.Next() {
		keys = append(keys, string(iterator.Key()))
	}
	if err := iterator.Error(); err != nil {
		t.Fatalf("pebbletest: iterating: %v", err)
	}
	return keys
}

// discardLogger silences Pebble's informational output.
type discardLogger struct{}

func (discardLogger) Infof(string, ...any)  {}
func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Fatalf(string, ...any) {}
