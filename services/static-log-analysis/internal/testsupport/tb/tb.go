// Package tb defines the subset of *testing.T that harness code depends on,
// together with a recording implementation.
//
// The standard library's testing.TB cannot be implemented outside package
// testing, so harness helpers that must fail a test take this narrower
// interface instead. That also makes the harness testable: a helper whose job
// is to fail on a missing fixture or an invalid record can be proven to fail,
// rather than being trusted to.
package tb

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// TB is satisfied by *testing.T and *testing.B.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Skipf(format string, args ...any)
	Cleanup(func())
	TempDir() string
	Name() string
	Failed() bool
}

// fatalSentinel is panicked by Recorder.Fatalf so that, like the real Fatalf,
// the calling helper stops immediately instead of continuing with bad state.
type fatalSentinel struct{ message string }

// Recorder implements TB by recording what a helper did instead of failing a
// real test.
type Recorder struct {
	mu       sync.Mutex
	name     string
	errors   []string
	fatals   []string
	logs     []string
	skips    []string
	cleanups []func()
	tempDirs []string
}

// NewRecorder returns a Recorder named for the helper under test.
func NewRecorder(name string) *Recorder { return &Recorder{name: name} }

func (r *Recorder) Helper() {}

func (r *Recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// Fatalf records the failure and panics with a sentinel. Callers observe it
// through ExpectFatal, which recovers only this sentinel and lets any genuine
// panic escape.
func (r *Recorder) Fatalf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.fatals = append(r.fatals, message)
	r.mu.Unlock()
	panic(fatalSentinel{message: message})
}

func (r *Recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *Recorder) Skipf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.skips = append(r.skips, message)
	r.mu.Unlock()
	panic(fatalSentinel{message: "skipped: " + message})
}

func (r *Recorder) Cleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}

// TempDir returns a real temporary directory, removed by RunCleanups.
func (r *Recorder) TempDir() string {
	dir, err := os.MkdirTemp("", "recorder-")
	if err != nil {
		panic(fmt.Sprintf("tb.Recorder: creating temp dir: %v", err))
	}
	r.mu.Lock()
	r.tempDirs = append(r.tempDirs, dir)
	r.mu.Unlock()
	return dir
}

func (r *Recorder) Name() string { return r.name }

func (r *Recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errors) > 0 || len(r.fatals) > 0
}

// Errors returns the recorded Errorf messages.
func (r *Recorder) Errors() []string { return r.snapshot(&r.errors) }

// Fatals returns the recorded Fatalf messages.
func (r *Recorder) Fatals() []string { return r.snapshot(&r.fatals) }

// Logs returns the recorded Logf messages.
func (r *Recorder) Logs() []string { return r.snapshot(&r.logs) }

// Skips returns the recorded Skipf messages.
func (r *Recorder) Skips() []string { return r.snapshot(&r.skips) }

func (r *Recorder) snapshot(field *[]string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), *field...)
}

// RunCleanups runs registered cleanups in reverse order and removes any
// temporary directories, mirroring what testing does at the end of a test.
func (r *Recorder) RunCleanups() {
	r.mu.Lock()
	cleanups := r.cleanups
	r.cleanups = nil
	dirs := r.tempDirs
	r.tempDirs = nil
	r.mu.Unlock()

	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
}

// ExpectFatal runs fn and reports whether it ended in Fatalf or Skipf on this
// Recorder. A panic from anything else propagates, so a genuine bug in the
// helper is never mistaken for an expected failure.
func (r *Recorder) ExpectFatal(fn func()) (fataled bool) {
	defer func() {
		switch recovered := recover().(type) {
		case nil:
			fataled = false
		case fatalSentinel:
			fataled = true
		default:
			panic(recovered)
		}
	}()
	fn()
	return false
}

// FailureText joins every recorded error, fatal, and skip message so a test can
// assert on the reason a helper rejected its input.
func (r *Recorder) FailureText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parts []string
	parts = append(parts, r.errors...)
	parts = append(parts, r.fatals...)
	parts = append(parts, r.skips...)
	return strings.Join(parts, "\n")
}
