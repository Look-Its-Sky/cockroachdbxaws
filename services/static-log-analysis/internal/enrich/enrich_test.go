package enrich_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/enrich"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

// The load-bearing property of this package is one sentence from
// architecture.md: "Detection and persistence never wait for external
// metadata." Every test here is about some way that could stop being true.

func paymentKey() enrich.Key {
	return enrich.Key{Region: "us-east-1", Service: "paymentservice", Environment: "production"}
}

// stubProvider answers however a test says, and records what it was asked.
type stubProvider struct {
	mu       sync.Mutex
	answer   enrich.Deployment
	err      error
	calls    int
	release  chan struct{}
	answered chan struct{}
}

func newStub(answer enrich.Deployment) *stubProvider {
	return &stubProvider{answer: answer, answered: make(chan struct{}, 64)}
}

func (s *stubProvider) Deployment(ctx context.Context, _ enrich.Key) (enrich.Deployment, error) {
	s.mu.Lock()
	release, answer, err := s.release, s.answer, s.err
	s.calls++
	s.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return enrich.Deployment{}, ctx.Err()
		}
	}
	select {
	case s.answered <- struct{}{}:
	default:
	}
	return answer, err
}

func (s *stubProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubProvider) setAnswer(answer enrich.Deployment, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer, s.err = answer, err
}

func newCache(t *testing.T, provider enrich.Provider, adjust ...func(*enrich.Config)) *enrich.Cache {
	t.Helper()
	config := enrich.Config{Provider: provider, Clock: fakeclock.NewAtOrigin()}
	for _, apply := range adjust {
		apply(&config)
	}
	cache, err := enrich.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return cache
}

// awaitAnswer waits for the background resolution to finish, so the assertion
// that follows is about the cache's state rather than about a race.
func awaitAnswer(t *testing.T, provider *stubProvider) {
	t.Helper()
	select {
	case <-provider.answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the provider was never asked")
	}
}

// TestAMissIsPendingImmediatelyAndNeverBlocks is the whole point. A Lookup that
// waited for a provider would put an external dependency inside the ingestion
// path, where operations.md requires the service to keep running through
// exactly that dependency being down.
func TestAMissIsPendingImmediatelyAndNeverBlocks(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1", Version: "1.0.0"})
	provider.release = make(chan struct{}) // never released: the provider hangs
	// A short budget keeps Close from waiting out the default ten seconds for a
	// call that will never answer. What is under test is Lookup, not the budget.
	cache := newCache(t, provider, func(c *enrich.Config) { c.Budget = 100 * time.Millisecond })

	done := make(chan model.DeploymentIdentity, 1)
	go func() { done <- cache.Lookup(paymentKey()) }()
	select {
	case got := <-done:
		if got.Status != model.EnrichmentPending || got.ID != model.UnknownDeployment {
			t.Fatalf("a miss answered %+v, want pending and unknown-deployment", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lookup blocked on a provider; detection must never wait for external metadata")
	}
}

func TestACachedAnswerIsUsedImmediately(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1", Version: "1.0.0"})
	cache := newCache(t, provider)

	// The first lookup misses and schedules the fetch.
	if got := cache.Lookup(paymentKey()); got.Status != model.EnrichmentPending {
		t.Fatalf("first lookup answered %+v", got)
	}
	awaitAnswer(t, provider)

	got := cache.Lookup(paymentKey())
	if got.Status != model.EnrichmentAvailable || got.ID != "deploy-1" || got.Version != "1.0.0" {
		t.Fatalf("a cached answer was not used: %+v", got)
	}
}

// TestConcurrentMissesAskTheProviderOnce pins that a burst of records for one
// unknown service does not become a burst of provider calls. A metadata
// provider is the dependency most likely to fall over under exactly that load.
func TestConcurrentMissesAskTheProviderOnce(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1"})
	provider.release = make(chan struct{})
	cache := newCache(t, provider, func(c *enrich.Config) { c.Budget = 5 * time.Second })

	var running sync.WaitGroup
	for i := 0; i < 50; i++ {
		running.Add(1)
		go func() {
			defer running.Done()
			cache.Lookup(paymentKey())
		}()
	}
	running.Wait()
	close(provider.release)
	awaitAnswer(t, provider)

	if got := provider.callCount(); got != 1 {
		t.Fatalf("fifty concurrent misses made %d provider calls; in-flight work must be shared", got)
	}
}

// TestAnUnavailableProviderIsRepresentedExplicitlyAndRetried pins
// architecture.md's "unavailable providers are represented explicitly with
// reason and retryability". Reporting a failure as a permanent absence would
// group every record under unknown-deployment forever.
func TestAnUnavailableProviderIsRepresentedExplicitlyAndRetried(t *testing.T) {
	provider := newStub(enrich.Deployment{})
	provider.setAnswer(enrich.Deployment{}, errors.New("metadata service is down"))
	clock := fakeclock.NewAtOrigin()
	cache := newCache(t, provider, func(c *enrich.Config) {
		c.Clock = clock
		c.RetryAfter = time.Minute
	})

	cache.Lookup(paymentKey())
	awaitAnswer(t, provider)

	// A retryable failure is pending, not "not available": a later attempt can
	// still succeed, and pending is what says so.
	if got := cache.Lookup(paymentKey()); got.Status != model.EnrichmentPending {
		t.Fatalf("a retryable provider failure answered %+v, want pending", got)
	}
	if counters := cache.Counters(); counters.ProviderFailures == 0 {
		t.Fatal("a provider failure was not counted")
	}

	// Before the retry window passes, the provider is left alone.
	calls := provider.callCount()
	cache.Lookup(paymentKey())
	if provider.callCount() != calls {
		t.Fatal("a failing provider was retried immediately; a widening pause is what keeps one outage from becoming two")
	}

	// After it passes, the answer is tried again and succeeds.
	provider.setAnswer(enrich.Deployment{ID: "deploy-2", Version: "2.0.0"}, nil)
	clock.Advance(time.Minute + time.Second)
	cache.Lookup(paymentKey())
	awaitAnswer(t, provider)
	if got := cache.Lookup(paymentKey()); got.Status != model.EnrichmentAvailable || got.ID != "deploy-2" {
		t.Fatalf("a recovered provider was not used: %+v", got)
	}
}

// TestAProviderThatSaysThereIsNoDeploymentIsNotAFailure keeps a valid negative
// answer distinct from an outage, the same distinction the source adapter draws
// between an empty result and an unavailable API.
func TestAProviderThatSaysThereIsNoDeploymentIsNotAFailure(t *testing.T) {
	provider := newStub(enrich.Deployment{})
	provider.setAnswer(enrich.Deployment{}, enrich.ErrNoDeployment)
	cache := newCache(t, provider)

	cache.Lookup(paymentKey())
	awaitAnswer(t, provider)

	got := cache.Lookup(paymentKey())
	if got.Status != model.EnrichmentNotAvailable {
		t.Fatalf("a definite 'no deployment' answered %+v, want not_available", got)
	}
	if counters := cache.Counters(); counters.ProviderFailures != 0 {
		t.Fatal("a valid negative answer was counted as a provider failure")
	}
}

// TestAProviderIsBoundedByItsBudget pins the ten-second budget architecture.md
// gives normal-priority enrichment. Without it one hung provider call would hold
// a slot forever and the cache would stop refreshing that key at all.
func TestAProviderIsBoundedByItsBudget(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1"})
	provider.release = make(chan struct{})
	cache := newCache(t, provider, func(c *enrich.Config) { c.Budget = 50 * time.Millisecond })
	t.Cleanup(func() { close(provider.release) })

	cache.Lookup(paymentKey())
	deadline := time.Now().Add(10 * time.Second)
	for cache.Counters().Timeouts == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a hung provider call was never cut off by its budget")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAStaleEntryIsRefreshedAndTheOldAnswerIsUsedMeanwhile pins that a
// deployment change is picked up without ever making a record wait for it.
func TestAStaleEntryIsRefreshedAndTheOldAnswerIsUsedMeanwhile(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1", Version: "1.0.0"})
	clock := fakeclock.NewAtOrigin()
	cache := newCache(t, provider, func(c *enrich.Config) {
		c.Clock = clock
		c.TTL = 5 * time.Minute
	})

	cache.Lookup(paymentKey())
	awaitAnswer(t, provider)
	if got := cache.Lookup(paymentKey()); got.ID != "deploy-1" {
		t.Fatalf("first answer=%+v", got)
	}

	// A new release happens and the entry goes stale.
	provider.setAnswer(enrich.Deployment{ID: "deploy-2", Version: "2.0.0"}, nil)
	clock.Advance(6 * time.Minute)

	// The stale answer is still served immediately rather than becoming pending:
	// a known-slightly-old deployment groups better than no deployment at all.
	got := cache.Lookup(paymentKey())
	if got.Status != model.EnrichmentAvailable || got.ID != "deploy-1" {
		t.Fatalf("a stale entry answered %+v; it must still be used while it refreshes", got)
	}
	awaitAnswer(t, provider)
	if got := cache.Lookup(paymentKey()); got.ID != "deploy-2" || got.Version != "2.0.0" {
		t.Fatalf("the refreshed answer was not adopted: %+v", got)
	}
}

// TestTheCacheIsBoundedSoOneNoisyRegionCannotGrowItForever pins that an
// unbounded key space cannot become an unbounded map. Service and instance
// identities are attacker-influenced in the sense that a misbehaving deployment
// can invent them.
func TestTheCacheIsBoundedSoOneNoisyRegionCannotGrowItForever(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1"})
	cache := newCache(t, provider, func(c *enrich.Config) { c.MaxEntries = 32 })

	for i := 0; i < 500; i++ {
		key := paymentKey()
		key.InstanceID = string(rune('a'+i%26)) + string(rune('a'+i/26))
		cache.Lookup(key)
	}
	if got := cache.Len(); got > 32 {
		t.Fatalf("the cache holds %d entries, over its bound of 32", got)
	}
}

func TestNewRefusesACacheThatCouldNotWork(t *testing.T) {
	if _, err := enrich.New(enrich.Config{Clock: fakeclock.NewAtOrigin()}); !errors.Is(err, enrich.ErrInvalidConfig) {
		t.Fatal("a cache with no provider was built")
	}
	if _, err := enrich.New(enrich.Config{Provider: newStub(enrich.Deployment{})}); !errors.Is(err, enrich.ErrInvalidConfig) {
		t.Fatal("a cache with no clock was built")
	}
}

// TestLookupIsSafeUnderConcurrentUse is the race-detector's test. Ingestion is
// concurrent by construction, so every Lookup happens on some handler goroutine.
func TestLookupIsSafeUnderConcurrentUse(t *testing.T) {
	provider := newStub(enrich.Deployment{ID: "deploy-1"})
	cache := newCache(t, provider)
	var running sync.WaitGroup
	var seen atomic.Int64
	for i := 0; i < 64; i++ {
		running.Add(1)
		go func(i int) {
			defer running.Done()
			key := paymentKey()
			key.InstanceID = string(rune('a' + i%8))
			for j := 0; j < 50; j++ {
				if cache.Lookup(key).Status == model.EnrichmentAvailable {
					seen.Add(1)
				}
			}
		}(i)
	}
	running.Wait()
}
