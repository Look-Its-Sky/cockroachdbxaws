package tb_test

import (
	"os"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

// A real *testing.T must satisfy the interface, or harness helpers could not be
// called from an ordinary test.
var _ tb.TB = (*testing.T)(nil)

func TestExpectFatalReportsAFatalFailure(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	fataled := recorder.ExpectFatal(func() { recorder.Fatalf("rejected because %s", "the fixture is missing") })

	if !fataled {
		t.Fatal("want the fatal reported")
	}
	if got := recorder.Fatals(); len(got) != 1 || !strings.Contains(got[0], "the fixture is missing") {
		t.Fatalf("want the formatted message recorded, got %v", got)
	}
}

func TestExpectFatalReportsWhenNothingFailed(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	// Every "this helper must reject bad input" test depends on this returning
	// false. If it returned true regardless, all of them would pass vacuously.
	fataled := recorder.ExpectFatal(func() {})

	if fataled {
		t.Fatal("want no failure reported for a function that succeeded")
	}
}

func TestFatalfStopsTheCallingHelper(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	reached := false

	recorder.ExpectFatal(func() {
		recorder.Fatalf("stop here")
		reached = true
	})

	// The real Fatalf ends the test goroutine. A recorder that merely noted the
	// failure would let a helper continue with the bad state it just rejected.
	if reached {
		t.Fatal("want the helper stopped at Fatalf, it continued")
	}
}

func TestExpectFatalLetsAGenuinePanicEscape(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("want a genuine panic to escape, it was swallowed")
		}
		if message, ok := recovered.(string); !ok || message != "nil map write" {
			t.Fatalf("want the original panic value, got %v", recovered)
		}
	}()

	// A bug in a helper must not be reported as the expected rejection.
	recorder.ExpectFatal(func() { panic("nil map write") })
}

func TestSkipfIsRecordedSeparatelyFromFailure(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	stopped := recorder.ExpectFatal(func() { recorder.Skipf("no docker") })

	if !stopped {
		t.Fatal("want the helper stopped at Skipf")
	}
	if len(recorder.Skips()) != 1 {
		t.Fatalf("want the skip recorded, got %v", recorder.Skips())
	}
	// A skip is not a failure, and a test that distinguishes them relies on
	// that.
	if recorder.Failed() {
		t.Error("want a skip not to count as a failure")
	}
}

func TestErrorfIsRecordedWithoutStopping(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	recorder.Errorf("first %d", 1)
	recorder.Errorf("second %d", 2)

	if got := recorder.Errors(); len(got) != 2 {
		t.Fatalf("want both errors recorded, got %v", got)
	}
	if !recorder.Failed() {
		t.Error("want Errorf to count as a failure")
	}
}

func TestFailureTextCombinesEveryReason(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	recorder.Errorf("an error")
	recorder.ExpectFatal(func() { recorder.Fatalf("a fatal") })

	text := recorder.FailureText()

	for _, want := range []string{"an error", "a fatal"} {
		if !strings.Contains(text, want) {
			t.Errorf("want %q in the failure text, got:\n%s", want, text)
		}
	}
}

func TestCleanupsRunInReverseOrder(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	var order []string
	recorder.Cleanup(func() { order = append(order, "first") })
	recorder.Cleanup(func() { order = append(order, "second") })

	recorder.RunCleanups()

	// Reverse order is what testing does, and what lets a store close before
	// the directory holding it is removed.
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("want cleanups in reverse order, got %v", order)
	}
}

func TestCleanupsRunOnlyOnce(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	calls := 0
	recorder.Cleanup(func() { calls++ })

	recorder.RunCleanups()
	recorder.RunCleanups()

	if calls != 1 {
		t.Fatalf("want the cleanup run once, got %d calls", calls)
	}
}

func TestTempDirIsRealAndRemovedByCleanup(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	dir := recorder.TempDir()

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("want a real directory at %s, got %v", dir, err)
	}

	recorder.RunCleanups()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("want %s removed, got %v", dir, err)
	}
}

func TestRecordedMessagesAreCopies(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	recorder.Errorf("original")

	recorder.Errors()[0] = "rewritten"

	if got := recorder.Errors()[0]; got != "original" {
		t.Fatalf("want the recorded message unchanged, got %q", got)
	}
}
