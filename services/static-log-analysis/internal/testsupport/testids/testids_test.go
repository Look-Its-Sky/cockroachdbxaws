package testids_test

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

func TestIdentifiersAreValidUUIDv7(t *testing.T) {
	source := testids.New()
	for i := 0; i < 100; i++ {
		id := mustNew(t, source)
		// Anything the harness hands to production code has to survive the same
		// validation production input does, or harness fixtures would pass
		// where real input fails.
		if err := ids.Validate(id); err != nil {
			t.Fatalf("identifier %d (%s) is not a canonical UUIDv7: %v", i, id, err)
		}
	}
}

func TestIdentifiersSortByCreationOrder(t *testing.T) {
	source := testids.New()
	issued := make([]string, 200)
	for i := range issued {
		issued[i] = mustNew(t, source)
	}

	sorted := append([]string(nil), issued...)
	sort.Strings(sorted)

	for i := range issued {
		if issued[i] != sorted[i] {
			t.Fatalf("identifier %d is out of order: issued %s, sorted %s", i, issued[i], sorted[i])
		}
	}
}

func TestTwoSourcesWithTheSameSeedProduceTheSameSequence(t *testing.T) {
	first := testids.New(testids.WithSeed(7))
	second := testids.New(testids.WithSeed(7))

	for i := 0; i < 50; i++ {
		a, b := mustNew(t, first), mustNew(t, second)
		if a != b {
			t.Fatalf("identifier %d differs between identical sources: %s and %s", i, a, b)
		}
	}
}

func TestDifferentSeedsProduceDifferentIdentifiers(t *testing.T) {
	first := testids.New(testids.WithSeed(1))
	second := testids.New(testids.WithSeed(2))

	// Two sources in one scenario must be distinguishable, otherwise a test
	// cannot tell which component produced an identifier.
	for i := 0; i < 50; i++ {
		if a, b := mustNew(t, first), mustNew(t, second); a == b {
			t.Fatalf("identifier %d is identical across seeds: %s", i, a)
		}
	}
}

func TestIdentifiersAreUnique(t *testing.T) {
	source := testids.New()
	seen := map[string]int{}
	for i := 0; i < 10_000; i++ {
		id := mustNew(t, source)
		if previous, ok := seen[id]; ok {
			t.Fatalf("identifier %s issued at %d and again at %d", id, previous, i)
		}
		seen[id] = i
	}
}

func TestAClockDrivenSourceFollowsTheScenarioTimeline(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	source := testids.New(testids.WithClock(c))

	before := mustNew(t, source)
	c.Advance(time.Hour)
	after := mustNew(t, source)

	if before >= after {
		t.Fatalf("want identifiers to follow the clock, got %s then %s", before, after)
	}
	// The timestamp field is the leading 48 bits, so an hour of scenario time
	// has to be visible in the identifier itself.
	if timestampOf(t, before) == timestampOf(t, after) {
		t.Fatalf("want the timestamp field to advance with the clock, both were %d", timestampOf(t, before))
	}
}

func TestOrderSurvivesMoreIdentifiersThanOneMillisecondHolds(t *testing.T) {
	// A frozen clock plus a 12-bit within-millisecond counter means the counter
	// overflows after 4096 identifiers. Ordering must survive that.
	c := fakeclock.NewAtOrigin()
	source := testids.New(testids.WithClock(c))

	const count = 5000
	issued := make([]string, count)
	seen := map[string]bool{}
	for i := range issued {
		issued[i] = mustNew(t, source)
		if seen[issued[i]] {
			t.Fatalf("identifier %d is a duplicate: %s", i, issued[i])
		}
		seen[issued[i]] = true
		if i > 0 && issued[i] <= issued[i-1] {
			t.Fatalf("identifier %d is not greater than its predecessor: %s then %s",
				i, issued[i-1], issued[i])
		}
	}
}

func TestOrderSurvivesAClockThatDoesNotMove(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	source := testids.New(testids.WithClock(c))
	first := mustNew(t, source)
	second := mustNew(t, source)

	if first >= second {
		t.Fatalf("want a strictly increasing sequence on a frozen clock, got %s then %s", first, second)
	}
}

func TestConcurrentCallersGetUniqueIdentifiers(t *testing.T) {
	source := testids.New()
	const goroutines, each = 8, 500

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				id, err := source.New()
				if err != nil {
					t.Errorf("New failed: %v", err)
					return
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate identifier under concurrency: %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != goroutines*each {
		t.Fatalf("want %d distinct identifiers, got %d", goroutines*each, len(seen))
	}
}

func TestIssuedCountsIdentifiers(t *testing.T) {
	source := testids.New()
	for i := 0; i < 5; i++ {
		mustNew(t, source)
	}
	if got := source.Issued(); got != 5 {
		t.Fatalf("want 5 issued, got %d", got)
	}
}

func mustNew(t *testing.T, source ids.Source) string {
	t.Helper()
	id, err := source.New()
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return id
}

// timestampOf returns the 48-bit millisecond field of a UUIDv7.
func timestampOf(t *testing.T, id string) uint64 {
	t.Helper()
	var millis uint64
	digits := 0
	for _, r := range id {
		if r == '-' {
			continue
		}
		if digits == 12 {
			break
		}
		millis = millis<<4 | uint64(hexValue(t, r))
		digits++
	}
	return millis
}

func hexValue(t *testing.T, r rune) int {
	t.Helper()
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	default:
		t.Fatalf("unexpected character %q in identifier", r)
		return 0
	}
}
