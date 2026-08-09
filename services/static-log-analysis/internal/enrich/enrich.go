// Package enrich resolves the deployment identity a record belongs to without
// ever putting an external dependency inside the ingestion path.
//
// architecture.md states the rule this package exists to keep: "Detection and
// persistence never wait for external metadata. Cached enrichment is used
// immediately." Deployment identity is not decoration — a deployment change
// creates a linked incident generation, so it is part of grouping — which makes
// the temptation to wait for it real. Lookup therefore never blocks: it answers
// from the cache and schedules the work, and a record whose deployment is not
// yet known groups under unknown-deployment until it is.
//
// The cost of that choice is bounded and known: records that arrive before the
// first answer group under unknown-deployment permanently, because an identity
// is assigned once and adopted thereafter. That is the correct trade. The
// alternative is an ingestion path that stops when a metadata service does.
package enrich

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

var (
	ErrInvalidConfig = errors.New("enrich: invalid configuration")
	// ErrNoDeployment is a provider's definite answer that this service has no
	// deployment identity. It is not a failure: telling "there is none" apart
	// from "I could not tell you" is the same distinction the source adapter
	// draws between an empty result and an unavailable API, and conflating them
	// would either retry forever or give up permanently.
	ErrNoDeployment = errors.New("enrich: no deployment identity for this service")
)

// Key identifies what is being enriched. It is the authenticated service
// identity plus the region, never anything a payload claimed for itself.
type Key struct {
	Region      string
	Service     string
	Namespace   string
	InstanceID  string
	Environment string
}

// Deployment is what a provider answers.
type Deployment struct {
	ID      string
	Version string
}

// Provider resolves deployment identity from whatever system of record a
// deployment has. It is the port; implementations live outside this package.
//
// A provider MUST return ErrNoDeployment for a definite negative and any other
// error for "I could not tell you". The two are recovered differently and only
// the provider knows which it means.
type Provider interface {
	Deployment(ctx context.Context, key Key) (Deployment, error)
}

// Counters are cumulative, categorical, process-lifetime counts. They carry no
// labels: operations.md forbids unbounded label cardinality, and a service
// instance identity is unbounded.
type Counters struct {
	Hits             uint64
	Misses           uint64
	Resolved         uint64
	NoDeployment     uint64
	ProviderFailures uint64
	Timeouts         uint64
	Evicted          uint64
}

type counters struct {
	hits, misses, resolved, noDeployment, providerFailures, timeouts, evicted atomic.Uint64
}

// Defaults for a cache nobody has tuned.
const (
	// DefaultBudget is architecture.md's initial enrichment budget for
	// normal-priority work. It bounds one provider call, not the wait a record
	// experiences, which is always zero.
	DefaultBudget = 10 * time.Second
	// DefaultTTL is how long an answer is fresh. A stale answer keeps being
	// served while it refreshes: a slightly old deployment groups better than
	// no deployment at all.
	DefaultTTL = 5 * time.Minute
	// DefaultRetryAfter is the pause before a failed resolution is attempted
	// again, so one metadata outage does not become a second one.
	DefaultRetryAfter = 30 * time.Second
	// DefaultMaxEntries bounds the cache. Service and instance identities are
	// unbounded in the sense that a misbehaving deployment can invent them, so
	// an unbounded map here would be an unbounded map an operator did not
	// choose.
	DefaultMaxEntries = 8192
	// DefaultConcurrency bounds simultaneous provider calls, so a burst of
	// unknown keys cannot become a burst of requests at the dependency most
	// likely to fall over under one.
	DefaultConcurrency = 4
)

type Config struct {
	Provider Provider
	Clock    clock.Clock
	// Budget bounds one provider call.
	Budget time.Duration
	// TTL is how long a resolved answer stays fresh.
	TTL time.Duration
	// RetryAfter is the pause after a failed resolution.
	RetryAfter time.Duration
	// MaxEntries bounds the cache.
	MaxEntries int
	// Concurrency bounds simultaneous provider calls.
	Concurrency int
}

// entry is one key's state. Only the mutex in Cache guards it.
type entry struct {
	deployment Deployment
	status     model.EnrichmentStatus
	// freshUntil is when a resolved answer becomes stale. It keeps being served
	// after that, and a refresh is scheduled.
	freshUntil time.Time
	// retryAfter is when a failed resolution may be attempted again.
	retryAfter time.Time
	inFlight   bool
	// touched orders eviction. It is the last time this entry was looked up.
	touched time.Time
}

// Cache is a non-blocking deployment resolver.
type Cache struct {
	config Config

	mu      sync.Mutex
	entries map[Key]*entry

	slots    chan struct{}
	counters counters

	closeOnce sync.Once
	closed    chan struct{}
	resolving sync.WaitGroup
}

func New(config Config) (*Cache, error) {
	if config.Provider == nil {
		return nil, fmt.Errorf("%w: a cache needs a provider to resolve through", ErrInvalidConfig)
	}
	if config.Clock == nil {
		return nil, fmt.Errorf("%w: a cache needs a clock; nothing here reads wall-clock time directly", ErrInvalidConfig)
	}
	if config.Budget <= 0 {
		config.Budget = DefaultBudget
	}
	if config.TTL <= 0 {
		config.TTL = DefaultTTL
	}
	if config.RetryAfter <= 0 {
		config.RetryAfter = DefaultRetryAfter
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = DefaultMaxEntries
	}
	if config.Concurrency <= 0 {
		config.Concurrency = DefaultConcurrency
	}
	return &Cache{
		config: config, entries: make(map[Key]*entry),
		slots: make(chan struct{}, config.Concurrency), closed: make(chan struct{}),
	}, nil
}

// Close stops accepting new resolutions and waits for the ones in flight, so a
// replica does not exit with provider calls still running.
func (c *Cache) Close() {
	c.closeOnce.Do(func() { close(c.closed) })
	c.resolving.Wait()
}

// Lookup answers what is known now and schedules whatever is not.
//
// It never blocks on the provider. That is the whole contract: every caller is
// on an ingestion path, and a wait here would be a wait there.
func (c *Cache) Lookup(key Key) model.DeploymentIdentity {
	if c == nil {
		return pending()
	}
	now := c.config.Clock.Now()

	c.mu.Lock()
	found, ok := c.entries[key]
	if !ok {
		found = &entry{status: model.EnrichmentPending}
		c.insertLocked(key, found, now)
	}
	found.touched = now
	status, deployment := found.status, found.deployment
	schedule := c.shouldResolveLocked(found, now)
	if schedule {
		found.inFlight = true
	}
	c.mu.Unlock()

	if ok && status != model.EnrichmentPending {
		c.counters.hits.Add(1)
	} else {
		c.counters.misses.Add(1)
	}
	if schedule {
		c.startResolve(key)
	}

	switch status {
	case model.EnrichmentAvailable:
		return model.DeploymentIdentity{ID: deployment.ID, Version: deployment.Version, Status: model.EnrichmentAvailable}
	case model.EnrichmentNotAvailable:
		// A definite negative. The record still needs a grouping key, and
		// unknown-deployment is the one data-model.md names for it.
		return model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentNotAvailable}
	default:
		return pending()
	}
}

func pending() model.DeploymentIdentity {
	return model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentPending}
}

// shouldResolveLocked decides whether this lookup starts a provider call.
func (c *Cache) shouldResolveLocked(found *entry, now time.Time) bool {
	if found.inFlight {
		// Another lookup is already asking. Sharing it is what keeps a burst of
		// records for one unknown service from becoming a burst of requests.
		return false
	}
	switch found.status {
	case model.EnrichmentAvailable:
		return !now.Before(found.freshUntil)
	case model.EnrichmentNotAvailable:
		// A definite negative is refreshed on the same schedule as a positive:
		// a service that has no deployment today may have one tomorrow.
		return !now.Before(found.freshUntil)
	default:
		return !now.Before(found.retryAfter)
	}
}

// insertLocked adds an entry, evicting the least recently looked-up one when
// the cache is full.
func (c *Cache) insertLocked(key Key, e *entry, now time.Time) {
	if len(c.entries) >= c.config.MaxEntries {
		// Prefer an entry with no call in flight: evicting one of those wastes
		// nothing. But the bound is hard, so if every entry is in flight the
		// oldest is evicted anyway. A flood of unknown keys is exactly when the
		// bound matters, and it is also exactly when everything is in flight —
		// sparing them would make the limit no limit at all. An answer that
		// lands on an evicted key is dropped by finish.
		if candidate, ok := c.oldestLocked(true); ok {
			delete(c.entries, candidate)
			c.counters.evicted.Add(1)
		} else if candidate, ok := c.oldestLocked(false); ok {
			delete(c.entries, candidate)
			c.counters.evicted.Add(1)
		}
	}
	c.entries[key] = e
}

// oldestLocked returns the least recently looked-up key, optionally restricted
// to entries with no provider call in flight.
func (c *Cache) oldestLocked(idleOnly bool) (Key, bool) {
	var oldestKey Key
	var oldest time.Time
	found := false
	for candidate, existing := range c.entries {
		if idleOnly && existing.inFlight {
			continue
		}
		if !found || existing.touched.Before(oldest) {
			oldestKey, oldest, found = candidate, existing.touched, true
		}
	}
	return oldestKey, found
}

// startResolve runs one provider call in the background.
func (c *Cache) startResolve(key Key) {
	select {
	case <-c.closed:
		c.finish(key, Deployment{}, errClosed)
		return
	default:
	}
	c.resolving.Add(1)
	go func() {
		defer c.resolving.Done()
		// The concurrency bound is taken here rather than before the goroutine
		// starts, so a caller on an ingestion path never waits for a slot.
		select {
		case c.slots <- struct{}{}:
		case <-c.closed:
			c.finish(key, Deployment{}, errClosed)
			return
		}
		defer func() { <-c.slots }()

		ctx, cancel := context.WithTimeout(context.Background(), c.config.Budget)
		defer cancel()
		deployment, err := c.config.Provider.Deployment(ctx, key)
		if err != nil && ctx.Err() != nil {
			c.counters.timeouts.Add(1)
		}
		c.finish(key, deployment, err)
	}()
}

var errClosed = errors.New("enrich: cache is closing")

// finish records one resolution's outcome.
func (c *Cache) finish(key Key, deployment Deployment, err error) {
	now := c.config.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	found, ok := c.entries[key]
	if !ok {
		// Evicted while the call was in flight. Nothing to record.
		return
	}
	found.inFlight = false
	switch {
	case err == nil && deployment.ID != "":
		found.deployment = deployment
		found.status = model.EnrichmentAvailable
		found.freshUntil = now.Add(c.config.TTL)
		found.retryAfter = time.Time{}
		c.counters.resolved.Add(1)
	case errors.Is(err, ErrNoDeployment), err == nil:
		// A definite negative, including a provider that answered successfully
		// with nothing. It is not a failure and must not be retried as one.
		found.deployment = Deployment{}
		found.status = model.EnrichmentNotAvailable
		found.freshUntil = now.Add(c.config.TTL)
		found.retryAfter = time.Time{}
		c.counters.noDeployment.Add(1)
	default:
		// Retryable. The status stays pending, which is what says a later
		// attempt can still succeed, and the pause is what stops one outage
		// becoming two.
		found.retryAfter = now.Add(c.config.RetryAfter)
		if !errors.Is(err, errClosed) {
			c.counters.providerFailures.Add(1)
		}
	}
}

// Counters returns a snapshot.
func (c *Cache) Counters() Counters {
	if c == nil {
		return Counters{}
	}
	return Counters{
		Hits: c.counters.hits.Load(), Misses: c.counters.misses.Load(),
		Resolved: c.counters.resolved.Load(), NoDeployment: c.counters.noDeployment.Load(),
		ProviderFailures: c.counters.providerFailures.Load(), Timeouts: c.counters.timeouts.Load(),
		Evicted: c.counters.evicted.Load(),
	}
}

// Len is the number of entries held. It exists so a bound can be asserted.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Deployment resolves the deployment identity for one record's service.
//
// It is the shape internal/normalize consumes, so that package needs no
// dependency on this one: normalization is domain logic and must not import a
// cache with background goroutines in it.
func (c *Cache) Deployment(service model.ServiceIdentity, region string) model.DeploymentIdentity {
	if c == nil {
		return pending()
	}
	if service.Name == "" {
		// A record with no service identity has nothing to enrich against.
		// data-model.md admits such records by design; they simply group under
		// unknown-deployment, and asking a provider about an empty name would
		// be a request nobody can answer.
		return model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentNotApplicable}
	}
	return c.Lookup(Key{
		Region: region, Service: service.Name, Namespace: service.Namespace,
		InstanceID: service.InstanceID, Environment: service.Environment,
	})
}
