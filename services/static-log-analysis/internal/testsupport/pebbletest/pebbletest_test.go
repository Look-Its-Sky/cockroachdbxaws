package pebbletest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/pebbletest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

func TestAnOnDiskStoreLivesUnderTheTestDirectoryAndIsRemoved(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	store := pebbletest.New(recorder)
	dir := store.Dir()

	if _, err := os.Stat(filepath.Join(dir, "LOCK")); err != nil {
		t.Fatalf("want an initialized and locked database at %s: %v", dir, err)
	}

	recorder.RunCleanups()

	// A leaked directory per test would fill a developer's disk over a long
	// suite, and a leaked open database would keep its file lock.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("want %s removed when the test ended, got %v", dir, err)
	}
}

func TestReopenPreservesWrittenData(t *testing.T) {
	store := pebbletest.New(t)
	store.Set(t, "record/1", "admitted", pebble.Sync)

	store.Reopen(t)

	value, present := store.Get(t, "record/1")
	if !present || value != "admitted" {
		t.Fatalf("want the record to survive a planned restart, got %q present=%v", value, present)
	}
}

func TestASecondOpenOfTheSameDirectoryIsRefused(t *testing.T) {
	store := pebbletest.New(t)

	// One process owns one database on one volume. If a second open succeeded,
	// two replicas could share a journal and each acknowledge work the other
	// owns.
	second, err := pebble.Open(store.Dir(), store.Options())
	if err == nil {
		if closeErr := second.Close(); closeErr != nil {
			t.Errorf("closing the unexpected second database: %v", closeErr)
		}
		t.Fatal("want a second open of the same directory to fail, got success")
	}
}

func TestASyncedWriteSurvivesACrash(t *testing.T) {
	store := pebbletest.NewCrashable(t)
	store.Set(t, "record/1", "acknowledged", pebble.Sync)

	store.CrashAndReopen(t)

	value, present := store.Get(t, "record/1")
	if !present {
		t.Fatal("want a synced write to survive a crash, it was lost")
	}
	if value != "acknowledged" {
		t.Fatalf("want the stored value back, got %q", value)
	}
}

func TestAnUnsyncedWriteIsLostInACrash(t *testing.T) {
	store := pebbletest.NewCrashable(t)
	store.Set(t, "record/synced", "acknowledged", pebble.Sync)
	store.Set(t, "record/unsynced", "not acknowledged", pebble.NoSync)

	store.CrashAndReopen(t)

	// This is the property the acknowledgement rule rests on: without it, a
	// test could not tell a durable commit from an unsynced one, and the
	// harness would report the journal safe when it was not.
	if _, present := store.Get(t, "record/unsynced"); present {
		t.Fatal("want an unsynced write lost in a crash, it survived")
	}
	if _, present := store.Get(t, "record/synced"); !present {
		t.Fatal("want the synced write to survive alongside it")
	}
}

func TestASyncedBatchSurvivesACrashInFull(t *testing.T) {
	store := pebbletest.NewCrashable(t)

	batch := store.DB().NewBatch()
	for _, key := range []string{"batch/meta", "record/1", "record/2", "pending/1"} {
		if err := batch.Set([]byte(key), []byte("v"), nil); err != nil {
			t.Fatalf("staging %s: %v", key, err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		t.Fatalf("committing: %v", err)
	}

	store.CrashAndReopen(t)

	// A partial disk write must never expose a subset of a transport batch as
	// acknowledged, so either all members are present or none are.
	want := []string{"batch/meta", "pending/1", "record/1", "record/2"}
	if got := store.Keys(t); !equal(got, want) {
		t.Fatalf("want the whole batch %v, got %v", want, got)
	}
}

func TestKeysAreReturnedInOrder(t *testing.T) {
	store := pebbletest.New(t)
	for _, key := range []string{"record/3", "record/1", "batch/9", "record/2"} {
		store.Set(t, key, "v", pebble.Sync)
	}

	want := []string{"batch/9", "record/1", "record/2", "record/3"}
	if got := store.Keys(t); !equal(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestGetReportsAnAbsentKeyRatherThanFailing(t *testing.T) {
	store := pebbletest.New(t)

	value, present := store.Get(t, "record/absent")

	if present {
		t.Fatalf("want the key reported absent, got %q", value)
	}
}

func TestCrashSimulationIsRefusedOnAnOnDiskStore(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	defer recorder.RunCleanups()
	store := pebbletest.New(recorder)

	// Silently doing a clean reopen instead would make a crash test pass
	// without ever simulating a crash.
	fataled := recorder.ExpectFatal(func() { store.CrashAndReopen(recorder) })

	if !fataled {
		t.Fatal("want crash simulation refused on an on-disk store, got no failure")
	}
	if failure := recorder.FailureText(); !strings.Contains(failure, "NewCrashable") {
		t.Errorf("want the failure to name the alternative, got:\n%s", failure)
	}
}

func TestOptionsCanBeAdjusted(t *testing.T) {
	store := pebbletest.New(t, pebbletest.WithOptions(func(o *pebble.Options) {
		o.MemTableSize = 2 << 20
	}))

	if got := store.Options().MemTableSize; got != 2<<20 {
		t.Fatalf("want the configured memtable size, got %d", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
