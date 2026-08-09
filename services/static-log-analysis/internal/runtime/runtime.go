package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwcheckpoint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwsink"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/enrich"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/normalize"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/otlpreceiver"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox/sqsaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// ErrRoleNotImplemented means a role exists in the contract but has no
// behaviour yet. It is returned instead of starting, because a role that runs
// and does nothing reports healthy while its work silently accumulates.
var ErrRoleNotImplemented = errors.New("runtime: role not implemented")

// startupFault marks a startup failure an operator must change something to
// fix, so the whole class reaches the caller as ErrInvalidConfig and leaves the
// command with one sentinel to map to its configuration exit status rather than
// a list that a new failure mode is silently missing from.
//
// The distinction is not cosmetic. runtime.md: configuration failures exit 2 and
// runtime failures exit 1, "so a supervisor does not restart-loop a process
// whose flags can never work". A misdeployed replica reported as a runtime
// failure restart-loops instead of reporting the fault an operator has to see.
func startupFault(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidConfig}, args...)...)
}

// Journal is the durable boundary a server owns. It is the coordinator's
// journal interface plus the close the server is responsible for.
type Journal interface {
	pipeline.Journal
	// Stats is the durable state an operator and a readiness check both need.
	// It is on this interface rather than the coordinator's because the
	// coordinator has no reason to ask; the replica that owns the volume does.
	Stats() (journal.Stats, error)
	Close() error
}

// Store is the CockroachDB boundary the coordinator persists through.
type Store = pipeline.Store

// OutboxStore is the CockroachDB boundary the publisher drains.
type OutboxStore = outbox.Store

// Transport is the queue boundary the publisher publishes through.
type Transport = outbox.Transport

// Source is the pull-based source boundary a source replica drives.
// *cloudwatch.Adapter satisfies it.
type Source interface {
	Poll(context.Context) (cloudwatch.PollResult, error)
}

// Deps are the process-wide dependencies. The Open* fields are fields rather
// than fixed calls so startup ordering, cadence, and shutdown ordering can be
// exercised without a database, a queue, or a slow disk; production leaves them
// nil and gets the real Pebble journal, the real pgx pool, and the real queue
// client. The storage and transport contracts themselves are never covered
// through these seams.
type Deps struct {
	Clock         clock.Clock
	IDs           ids.Source
	Logger        *slog.Logger
	OpenJournal   func(Config, *redact.Policy) (Journal, error)
	OpenStore     func(context.Context, Config) (Store, func(), error)
	OpenOutbox    func(context.Context, Config) (OutboxStore, func(), error)
	OpenTransport func(context.Context, Config) (Transport, func(), error)
	// OpenSource builds the source adapter and whatever durable state it owns.
	// It takes the coordinator rather than a built sink so a test can drive the
	// real adapter into the real coordinator without a network.
	OpenSource func(context.Context, Config, *redact.Policy, Deps, *pipeline.Service) (Source, func(), error)
}

func (d Deps) resolved() Deps {
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	if d.IDs == nil {
		d.IDs = ids.System()
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if d.OpenJournal == nil {
		clk := d.Clock
		d.OpenJournal = func(config Config, policy *redact.Policy) (Journal, error) {
			return OpenJournal(config, policy, clk)
		}
	}
	if d.OpenStore == nil {
		d.OpenStore = openStore
	}
	if d.OpenOutbox == nil {
		d.OpenOutbox = openOutboxStore
	}
	if d.OpenTransport == nil {
		d.OpenTransport = openTransport
	}
	if d.OpenSource == nil {
		d.OpenSource = openSource
	}
	return d
}

// Server is one running replica. It owns exactly the components its role owns:
// a journal volume, optionally a receiver, optionally a process worker,
// optionally a publisher and its queue client.
type Server struct {
	config         Config
	logger         *slog.Logger
	journal        Journal
	receiver       *otlpreceiver.Receiver
	worker         *worker
	closeDB        func()
	closeTransport func()
	closeSource    func()
	enrichment     *enrich.Cache
	admin          *adminServer
	service        *pipeline.Service
	clock          clock.Clock
	draining       atomic.Bool
	// failed carries a fault a running component discovered that no retry can
	// clear, so the replica stops instead of looking healthy.
	failed chan error

	shutdownOnce sync.Once
	shutdownErr  error
}

// Start validates everything, opens every boundary, and begins serving. It
// returns only after the listeners are bound, so a caller that gets a Server
// back has a replica that is actually reachable.
func Start(ctx context.Context, config Config, deps Deps) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	deps = deps.resolved()
	if config.Role.Publishes() {
		return startPublisher(ctx, config, deps)
	}
	policy, err := config.Policy()
	if err != nil {
		return nil, err
	}
	server := &Server{config: config, logger: deps.Logger, clock: deps.Clock}

	server.journal, err = deps.OpenJournal(config, policy)
	if err != nil {
		// A boundary mismatch means this replica was pointed at another
		// replica's volume, or its scope flags were typed wrong. No retry fixes
		// that, so it must reach the caller as a configuration fault rather
		// than as a failure a supervisor should restart-loop. Any other open
		// failure is genuinely transient (disk, IO) and stays as it is.
		if errors.Is(err, journal.ErrBoundaryMismatch) {
			return nil, startupFault("journal at %s: %v", config.Journal.Dir, err)
		}
		return nil, fmt.Errorf("runtime: open journal at %s: %w", config.Journal.Dir, err)
	}
	store, closeDB, err := server.openStore(ctx, config, deps)
	if err != nil {
		_ = server.journal.Close()
		return nil, err
	}
	server.closeDB = closeDB

	server.enrichment, err = openEnrichment(config, deps)
	if err != nil {
		server.closeBoundaries()
		return nil, err
	}
	// The protection classifier is compiled with the rules and runs before
	// shedding without mutating rule state. There is no rule-reload surface yet,
	// so it compiles from the empty set: every record is then uncertain unless
	// it is an explicitly unprotected DEBUG or INFO from an eligible source,
	// which is exactly the initial policy operations.md describes.
	classifier, err := admission.Compile(nil)
	if err != nil {
		server.closeBoundaries()
		return nil, startupFault("protection classifier: %v", err)
	}
	if config.Capacity.NearCapacityPercent == 0 {
		// Shedding is opt-in. Without a threshold the coordinator is given no
		// classifier at all, so nothing can be shed by accident.
		classifier = nil
	}
	service, err := pipeline.New(pipeline.Config{
		Clock: deps.Clock, IDs: deps.IDs, Journal: server.journal, Store: store, Policy: policy,
		Scope: config.Scope, Classification: config.Classification, WorkerOwner: config.WorkerOwner,
		Limits: config.Limits, Enrichment: server.resolver(),
		CapacityPolicy: config.CapacityPolicy(), Classifier: classifier,
	})
	if err != nil {
		server.closeBoundaries()
		// pipeline.New cross-validates this configuration against the journal
		// directory's manifest and the store's boundary, both of which are
		// immutable. Saying so here is the difference between an operator
		// fixing a flag and an operator watching every record look invalid.
		return nil, startupFault("coordinator refused this configuration; region %q, tenant %q, classification %q, and redaction policy %q must agree with the journal manifest at %s and the store boundary: %v",
			config.Scope.Region, config.Scope.TenantID, config.Classification, policy.Version(), config.Journal.Dir, err)
	}
	if err := server.startReceiver(config, policy, deps, service); err != nil {
		server.closeBoundaries()
		return nil, err
	}
	if config.Role.Processes() {
		server.worker = newWorker(service, config.Worker, deps.Clock, deps.Logger)
		server.worker.start()
	}
	if config.Role.Polls() {
		source, closeSource, err := deps.OpenSource(ctx, config, policy, deps, service)
		if err != nil {
			server.closeBoundaries()
			return nil, err
		}
		server.closeSource = closeSource
		server.worker = newPollWorker(source, config.Source.Cadence, deps.Clock, deps.Logger)
		server.worker.start()
	}
	server.service = service
	if err := server.startAdmin(config); err != nil {
		server.closeBoundaries()
		return nil, err
	}
	deps.Logger.Info("static-log-analysis started", slog.String("config", config.Describe()),
		slog.String("otlp_grpc_addr", server.GRPCAddr()), slog.String("otlp_http_addr", server.HTTPAddr()))
	return server, nil
}

// startPublisher builds the outbox replica. It opens no journal and no
// listener: runtime.md's role table gives this role a database credential and
// nothing else, and every message it publishes was made durable by the process
// role before this replica ever saw it.
func startPublisher(ctx context.Context, config Config, deps Deps) (*Server, error) {
	server := &Server{config: config, logger: deps.Logger, failed: make(chan error, 1)}
	store, closeDB, err := deps.OpenOutbox(ctx, config)
	if err != nil {
		return nil, err
	}
	server.closeDB = closeDB
	transport, closeTransport, err := deps.OpenTransport(ctx, config)
	if err != nil {
		server.closeBoundaries()
		return nil, err
	}
	server.closeTransport = closeTransport

	publisher, err := outbox.New(outbox.Config{
		Scope: config.Scope, Owner: config.WorkerOwner, Store: store, Transport: transport,
		Clock: deps.Clock, IDs: deps.IDs, Logger: deps.Logger,
		Batch: config.Outbox.Batch, ClaimTTL: config.Outbox.ClaimTTL, PublishTimeout: config.Outbox.PublishTimeout,
		BackoffMin: config.Outbox.RetryMin, BackoffMax: config.Outbox.RetryMax, MaxAttempts: config.Outbox.MaxAttempts,
	})
	if err != nil {
		server.closeBoundaries()
		// The publisher cross-validates its region against the transport's, so
		// this is where a replica pointed at another region's queue is caught.
		return nil, startupFault("outbox publisher refused this configuration: %v", err)
	}
	server.worker = newPublishWorker(&faultingPublisher{publisher: publisher, failed: server.failed},
		config.Outbox.Cadence, deps.Clock, deps.Logger)
	server.worker.start()
	// An outbox replica runs no coordinator, so its metrics are the publisher's
	// and its readiness is simply that it started.
	if err := server.startAdmin(config); err != nil {
		server.closeBoundaries()
		return nil, err
	}
	deps.Logger.Info("static-log-analysis started", slog.String("config", config.Describe()))
	return server, nil
}

// faultingPublisher reports a deployment fault once, so a replica whose queue it
// may not write to stops rather than backing off forever behind a healthy-looking
// process. Every other failure stays inside the worker's backoff, because every
// other failure is one a retry can clear.
type faultingPublisher struct {
	publisher *outbox.Publisher
	failed    chan error
	once      sync.Once
}

func (p *faultingPublisher) PublishCycle(ctx context.Context) (outbox.Result, error) {
	result, err := p.publisher.PublishCycle(ctx)
	if errors.Is(err, outbox.ErrDeploymentFault) {
		p.once.Do(func() {
			// Reported as a configuration fault: no supervisor restart makes a
			// deleted queue exist or a revoked credential valid, and runtime.md
			// reserves the configuration exit status for exactly that.
			select {
			case p.failed <- startupFault("outbox publisher cannot use its queue: %v", err):
			default:
			}
		})
	}
	return result, err
}

func (s *Server) openStore(ctx context.Context, config Config, deps Deps) (Store, func(), error) {
	if !config.Role.Processes() {
		// An ingest replica holds no database credential. security.md requires
		// least privilege per role, and operations.md requires ingestion to keep
		// running through a database outage, which a replica that had to reach
		// CockroachDB to start could not do. The coordinator still requires a
		// Store, so it is given one that refuses rather than one that pretends.
		policy, err := config.Policy()
		if err != nil {
			return nil, nil, err
		}
		return noDatabaseStore{boundary: persistence.Boundary{Scope: config.Scope,
			Classification: config.Classification, PolicyVersion: policy.Version()}}, func() {}, nil
	}
	return deps.OpenStore(ctx, config)
}

func (s *Server) startReceiver(config Config, policy *redact.Policy, deps Deps, service *pipeline.Service) error {
	if !config.Role.Ingests() {
		return nil
	}
	ingress := config.Ingress
	ingress.Policy, ingress.Clock, ingress.Logger, ingress.Limits = policy, deps.Clock, deps.Logger, config.Limits
	receiver, err := otlpreceiver.New(ingress, service)
	if err != nil {
		return startupFault("build OTLP receiver: %v", err)
	}
	if err := receiver.Start(); err != nil {
		// Unreadable TLS material and an address that cannot be bound are both
		// faults an operator must fix; neither becomes true by being retried.
		return startupFault("start OTLP receiver: %v", err)
	}
	s.receiver = receiver
	return nil
}

func (s *Server) GRPCAddr() string {
	if s.receiver == nil {
		return ""
	}
	return s.receiver.GRPCAddr()
}

func (s *Server) HTTPAddr() string {
	if s.receiver == nil {
		return ""
	}
	return s.receiver.HTTPAddr()
}

// Failed reports a component that stopped serving on its own. A replica whose
// receiver died must exit rather than hold its journal volume while accepting
// nothing.
func (s *Server) Failed() <-chan error {
	if s.failed != nil {
		return s.failed
	}
	if s.receiver == nil {
		return nil
	}
	return s.receiver.Failed()
}

// Shutdown performs the ordering operations.md requires, in this order and for
// these reasons:
//
//  1. Stop accepting new exports and drain the admitted ones. Every admitted
//     handler is inside Ingest, so draining is draining through journal commit.
//  2. Stop claiming new work and let the cycle in flight finish or be cut off.
//  3. Release the database connections that cycle was using.
//  4. Close the journal.
//
// The journal is last because everything above it can still need it. A forced
// stop at any step stays safe: an acknowledged record was synchronized before
// its response was written, and an abandoned cohort's claims lapse.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		var errs []error
		if s.receiver != nil {
			if err := s.receiver.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("runtime: drain receiver: %w", err))
			}
		}
		if s.worker != nil {
			if err := s.worker.stopAndWait(ctx); err != nil {
				errs = append(errs, fmt.Errorf("runtime: stop %s worker: %w", s.worker.name, err))
			}
		}
		// The queue client is released after the cycle that was using it, and
		// before the database, so a publisher can still record what it just
		// delivered.
		if s.admin != nil {
			// Stopped first: readiness has nothing useful to say once the
			// boundaries below start closing.
			s.admin.Shutdown(ctx)
		}
		if s.enrichment != nil {
			// Closed before the journal, because a resolution still in flight
			// is only ever consulted by ingestion, which has already stopped.
			s.enrichment.Close()
		}
		if s.closeSource != nil {
			// The checkpoint volume is closed after the poller has stopped, so a
			// cycle in flight can still commit the checkpoint for what it just
			// delivered.
			s.closeSource()
		}
		if s.closeTransport != nil {
			s.closeTransport()
		}
		// pgxpool.Close waits for every acquired connection to be returned, so
		// it has no bound of its own: during the database outage this service is
		// required to survive, a cycle can be parked on a query that never
		// answers. Closing it inline would hold the whole shutdown there and,
		// because the journal is closed after it, would leave Pebble open for
		// the supervisor to SIGKILL. Whatever the pool is still holding is
		// released when the process exits, which is the next thing to happen.
		if s.closeDB != nil {
			if err := withinDeadline(ctx, s.closeDB); err != nil {
				errs = append(errs, fmt.Errorf("runtime: close database pool: %w", err))
			}
		}
		if s.journal != nil {
			if err := s.journal.Close(); err != nil {
				errs = append(errs, fmt.Errorf("runtime: close journal: %w", err))
			}
		}
		s.shutdownErr = errors.Join(errs...)
		s.logger.Info("static-log-analysis stopped")
	})
	return s.shutdownErr
}

// withinDeadline runs a close that has no bound of its own and gives up waiting
// for it when the drain budget is spent. The work is abandoned, not cancelled:
// there is no way to interrupt it, and the point is that the steps after it
// still get to run.
func withinDeadline(ctx context.Context, release func()) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		release()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("did not finish within the shutdown budget: %w", ctx.Err())
	}
}

// closeBoundaries unwinds a partially built server. It is not Shutdown: nothing
// is serving yet, so there is nothing to drain.
// startAdmin binds the health, readiness, and metrics listener.
func (s *Server) startAdmin(config Config) error {
	if strings.TrimSpace(config.AdminListen) == "" {
		return nil
	}
	admin, err := newAdminServer(config.AdminListen, s.health, s.metrics)
	if err != nil {
		// A port that cannot be bound is an operator fault, the same as an
		// ingress port that cannot be bound.
		return startupFault("%v", err)
	}
	s.admin = admin
	return nil
}

// AdminAddr is the bound admin address, empty when no admin listener runs.
func (s *Server) AdminAddr() string {
	if s == nil {
		return ""
	}
	return s.admin.Addr()
}

// health answers readiness. A replica is ready when the boundaries its role
// owns are usable; it is deliberately not tied to whether a dependency it is
// designed to survive without is reachable.
func (s *Server) health() Health {
	if s == nil {
		return Health{Reason: "not started"}
	}
	if s.draining.Load() {
		// A draining replica must stop receiving before it stops existing, and
		// readiness is how a balancer is told.
		return Health{Reason: "draining"}
	}
	if s.journal != nil {
		// The journal is the one boundary whose failure a replica cannot work
		// around: an ingest replica with an unusable journal would acknowledge
		// nothing, and a process replica would claim nothing.
		if _, err := s.journal.Stats(); err != nil {
			return Health{Reason: "journal unusable"}
		}
	}
	return Health{Ready: true}
}

// metrics is the categorical, unlabelled counter set operations.md requires.
func (s *Server) metrics() []metric {
	if s == nil {
		return nil
	}
	var exported []metric
	if s.service != nil {
		counters := s.service.Counters()
		exported = append(exported,
			metric{"static_log_analysis_recovery_skipped_total", "Retained records the rule engine could not observe during startup replay.", counters.RecoverySkipped},
			metric{"static_log_analysis_scope_rejections_total", "Envelopes addressed to another region.", counters.ScopeRejections},
			metric{"static_log_analysis_journal_rejections_total", "Batches the journal permanently refused.", counters.JournalRejections},
			metric{"static_log_analysis_quarantined_total", "Records given a terminal categorical outcome.", counters.Quarantined},
			metric{"static_log_analysis_shed_total", "Optional records deliberately dropped near capacity.", counters.Shed},
			metric{"static_log_analysis_backpressured_total", "Batches refused because mandatory data could not be persisted.", counters.Backpressured},
		)
	}
	if s.journal != nil {
		if stats, err := s.journal.Stats(); err == nil {
			exported = append(exported,
				metric{"static_log_analysis_journal_bytes", "Bytes accounted to the journal.", stats.AccountedBytes},
				metric{"static_log_analysis_journal_pending_records", "Records acknowledged and not yet claimed.", stats.Pending},
				metric{"static_log_analysis_journal_claimed_records", "Records claimed and not yet committed.", stats.Claimed},
				metric{"static_log_analysis_journal_committed_records", "Records committed to CockroachDB.", stats.Committed},
				metric{"static_log_analysis_journal_quarantined_records", "Records whose payload was destroyed by a terminal outcome.", stats.Quarantined},
			)
		}
	}
	if s.enrichment != nil {
		counters := s.enrichment.Counters()
		exported = append(exported,
			metric{"static_log_analysis_enrichment_resolved_total", "Deployment identities resolved.", counters.Resolved},
			metric{"static_log_analysis_enrichment_no_deployment_total", "Definite negative answers from a metadata provider.", counters.NoDeployment},
			metric{"static_log_analysis_enrichment_provider_failures_total", "Metadata provider failures.", counters.ProviderFailures},
			metric{"static_log_analysis_enrichment_timeouts_total", "Metadata lookups cut off by their budget.", counters.Timeouts},
		)
	}
	return exported
}

// resolver is the deployment resolver the coordinator consumes, or nil when no
// deployments were declared.
//
// A typed nil would not do: pipeline stores it in an interface, and a non-nil
// interface holding a nil pointer would be consulted rather than skipped.
func (s *Server) resolver() normalize.DeploymentResolver {
	if s.enrichment == nil {
		return nil
	}
	return s.enrichment
}

// openEnrichment builds the deployment cache, or returns nil when nothing was
// declared. Enrichment is optional: a deployment with no metadata to declare
// must still be able to run, and its records group under unknown-deployment.
func openEnrichment(config Config, deps Deps) (*enrich.Cache, error) {
	if len(config.Enrichment.Deployments) == 0 {
		return nil, nil
	}
	provider, err := enrich.NewStatic(config.Enrichment.Deployments)
	if err != nil {
		return nil, startupFault("enrichment provider: %v", err)
	}
	cache, err := enrich.New(enrich.Config{
		Provider: provider, Clock: deps.Clock, Budget: config.Enrichment.Budget,
		TTL: config.Enrichment.TTL, RetryAfter: config.Enrichment.RetryAfter,
		MaxEntries: config.Enrichment.MaxEntries,
	})
	if err != nil {
		return nil, startupFault("enrichment cache: %v", err)
	}
	return cache, nil
}

// EnrichmentCounters reports the deployment cache's categorical outcomes.
//
// operations.md requires enrichment availability as a metric, and these are it:
// a rising ProviderFailures with a flat Resolved is a metadata provider an
// operator has to look at, while records keep being ingested regardless.
//
// A replica with no declared deployments reports zeros, because it has nothing
// to report rather than a failure to report it.
func (s *Server) EnrichmentCounters() enrich.Counters {
	if s == nil || s.enrichment == nil {
		return enrich.Counters{}
	}
	return s.enrichment.Counters()
}

// JournalStats reports this replica's durable journal state. It is exported
// because it is the one number an operator and a readiness check both need:
// pending work that has been acknowledged and not yet committed is exactly what
// a replica must not be shut down or rebuilt while holding.
//
// A replica whose role owns no journal reports zero and no error, because it
// has nothing to report rather than a failure to report it.
func (s *Server) JournalStats() (journal.Stats, error) {
	if s == nil || s.journal == nil {
		return journal.Stats{}, nil
	}
	return s.journal.Stats()
}

func (s *Server) closeBoundaries() {
	if s.admin != nil {
		ctx, cancel := context.WithTimeout(context.Background(), adminReadTime)
		defer cancel()
		s.admin.Shutdown(ctx)
	}
	if s.enrichment != nil {
		s.enrichment.Close()
	}
	if s.closeSource != nil {
		s.closeSource()
	}
	if s.closeTransport != nil {
		s.closeTransport()
	}
	if s.closeDB != nil {
		s.closeDB()
	}
	if s.journal != nil {
		_ = s.journal.Close()
	}
}

// Run starts the replica and blocks until ctx is done or a component fails,
// then drains within the configured shutdown budget. The shutdown context is
// detached from ctx: a cancelled ctx is the reason to shut down, so reusing it
// would cancel the drain it is supposed to start.
func Run(ctx context.Context, config Config, deps Deps) error {
	server, err := Start(ctx, config, deps)
	if err != nil {
		return err
	}
	var componentErr error
	select {
	case <-ctx.Done():
	case componentErr = <-server.Failed():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.ShutdownTimeout)
	defer cancel()
	return errors.Join(componentErr, server.Shutdown(shutdownCtx))
}

// OpenJournal opens this replica's Pebble journal. It is exported so a caller
// can wrap the real journal rather than substitute a fake one for it.
func OpenJournal(config Config, policy *redact.Policy, clk clock.Clock) (Journal, error) {
	opened, err := journal.Open(journal.Config{
		Dir: config.Journal.Dir, Owner: config.WorkerOwner, TenantID: config.Scope.TenantID,
		Region: config.Scope.Region, Classification: config.Classification, Clock: clk, Validator: policy,
		MaxBytes: config.Journal.MaxBytes, MinFreeBytes: config.Journal.MinFreeBytes, FreeSpace: freeSpace,
	})
	if err != nil {
		return nil, err
	}
	// The capacity policy is only consulted on a write, where an unmeasurable
	// filesystem is indistinguishable from a full one and every record is
	// refused as ErrCapacity. Left to that, a replica on a platform with no
	// free-space implementation would bind its listeners, log that it started,
	// and then refuse every export for the rest of its life. Startup is where a
	// misdeployed replica is caught, so the probe happens once, here, after
	// Open has created the directory it measures.
	if _, err := freeSpace(config.Journal.Dir); err != nil {
		_ = opened.Close()
		return nil, startupFault("journal free space is not measurable at %s, so the capacity policy could never be applied: %v",
			config.Journal.Dir, err)
	}
	return opened, nil
}

func openStore(ctx context.Context, config Config) (Store, func(), error) {
	return openPersistence(ctx, config)
}

func openOutboxStore(ctx context.Context, config Config) (OutboxStore, func(), error) {
	return openPersistence(ctx, config)
}

func openPersistence(ctx context.Context, config Config) (*persistence.Store, func(), error) {
	// pgxpool.New does not connect. Startup therefore does not depend on
	// CockroachDB being reachable, which is what lets a replica restart in the
	// middle of the 30-minute outage operations.md budgets for.
	pool, err := pgxpool.New(ctx, config.DatabaseDSN)
	if err != nil {
		// The driver's error echoes the connection string, which carries a
		// password.
		return nil, nil, fmt.Errorf("%w: %s could not be used to build a connection pool", ErrInvalidConfig, DatabaseDSNEnvVar)
	}
	policy, err := config.Policy()
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	store, err := persistence.New(pool, persistence.Config{Validator: policy, Scope: config.Scope,
		Classification: config.Classification, Topology: persistence.TopologySingleRegion})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("runtime: build CockroachDB store: %w", err)
	}
	return store, pool.Close, nil
}

// openSource builds the CloudWatch Logs adapter, its checkpoint volume, and the
// sink that delivers into the coordinator.
//
// The checkpoint volume is separate from the journal on purpose. The two have
// different lifetimes: a journal is drained and may be rebuilt, and a checkpoint
// rebuilt alongside it would reread the whole lookback window.
//
// The envelope's allowed sets are derived from the configured groups rather than
// configured beside them, so an allow-set can never be narrower than the groups
// it has to admit.
func openSource(ctx context.Context, config Config, policy *redact.Policy, deps Deps, service *pipeline.Service) (Source, func(), error) {
	db, err := pebble.Open(config.Source.CheckpointDir, &pebble.Options{})
	if err != nil {
		// A checkpoint volume that will not open is transient in the way a disk
		// is transient, so it is not a configuration fault.
		return nil, nil, fmt.Errorf("runtime: open checkpoint volume at %s: %w", config.Source.CheckpointDir, err)
	}
	closeDB := func() { _ = db.Close() }
	checkpoints, err := cwcheckpoint.New(db)
	if err != nil {
		closeDB()
		return nil, nil, startupFault("checkpoint store: %v", err)
	}
	options := []cwaws.Option{}
	if config.Source.EndpointURL != "" {
		options = append(options, cwaws.WithBaseEndpoint(config.Source.EndpointURL))
	}
	api, err := cwaws.New(ctx, config.Scope.Region, options...)
	if err != nil {
		closeDB()
		return nil, nil, startupFault("build CloudWatch Logs client: %v", err)
	}
	sink, err := cwsink.New(service)
	if err != nil {
		closeDB()
		return nil, nil, startupFault("build ingestion sink: %v", err)
	}
	groups := make([]cloudwatch.GroupConfig, 0, len(config.Source.Groups))
	for _, group := range config.Source.Groups {
		groups = append(groups, cloudwatch.GroupConfig{
			LogGroup: group.LogGroup, Service: group.Service, Environment: group.Environment,
		})
	}
	adapter, err := cloudwatch.New(cloudwatch.Config{
		Clock: deps.Clock, IDs: deps.IDs, Policy: policy, API: api, Checkpoints: checkpoints, Sink: sink,
		Account: config.Source.Account, Region: config.Scope.Region,
		SourceInstance: config.Source.SourceInstance, CredentialIdentity: config.Source.CredentialIdentity,
		AllowedServices: config.SourceAllowedServices(), AllowedEnvironments: config.SourceAllowedEnvironments(),
		Classification: config.Classification, Groups: groups,
		Lookback: config.Source.Lookback, MaxLookback: config.Source.MaxLookback,
		MaxWindow: config.Source.MaxWindow, PageLimit: config.Source.PageLimit, MaxPages: config.Source.MaxPages,
	})
	if err != nil {
		closeDB()
		// Every one of the adapter's construction failures is something an
		// operator typed: an account, a region, a group identity outside its own
		// allow-set. None becomes true by being retried.
		return nil, nil, startupFault("CloudWatch adapter refused this configuration: %v", err)
	}
	return adapter, closeDB, nil
}

// openTransport builds the Amazon SQS Standard client ADR 0005 names.
//
// The queue URLs and the endpoint override have already been validated against
// this replica's region by validateOutbox, so a client built here cannot be
// pointed at another region's queue. The adapter cross-checks the resolved AWS
// configuration as well, because a profile or environment variable can name a
// region the flags did not.
//
// There is nothing to close: the SQS client holds no long-lived connection of
// its own beyond the shared HTTP transport the process tears down when it
// exits.
func openTransport(ctx context.Context, config Config) (Transport, func(), error) {
	transport, err := sqsaws.New(ctx, sqsaws.Config{
		Region:             config.Scope.Region,
		QueueURL:           config.Outbox.QueueURL,
		DeadLetterQueueURL: config.Outbox.DeadLetterQueueURL,
		BaseEndpoint:       config.Outbox.EndpointURL,
	})
	if err != nil {
		// A queue client that cannot be built is a deployment fault: no retry of
		// this process turns an unresolvable region or an absent credential into
		// a working one, so it carries the sentinel the runtime maps to its
		// configuration exit status rather than restart-looping.
		return nil, nil, startupFault("build SQS transport: %v", err)
	}
	return transport, func() {}, nil
}

// noDatabaseStore is the Store an ingest-only replica is given. Its boundary
// agrees with the configuration by construction, which is why the ingest role's
// startup check rests on the journal manifest alone. ProcessBatch refuses so a
// misrouted process cycle can never look like it succeeded.
type noDatabaseStore struct{ boundary persistence.Boundary }

func (s noDatabaseStore) Boundary() persistence.Boundary { return s.boundary }

func (s noDatabaseStore) ProcessBatch(context.Context, []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	return nil, fmt.Errorf("%w: this replica runs the %s role and holds no database credential", ErrRoleNotImplemented, RoleIngest)
}

// ErrCannotDrain means this replica cannot empty its own journal.
var ErrCannotDrain = errors.New("runtime: replica cannot drain its own journal")

// Drain empties this replica's journal so its volume can be detached.
//
// architecture.md: "Scale-down drains a replica before its volume is detached or
// deleted." That is a different thing from shutdown. Shutdown drains the
// handlers in flight and then closes; drain drives the journal to empty, because
// a journal with pending records is acknowledged work that only this replica's
// volume holds.
//
// The order is: stop taking new work, then process what is already there. Doing
// it the other way round is a race the replica can never win, because ingestion
// would keep adding to what draining is trying to empty.
func (s *Server) Drain(ctx context.Context) error {
	if s == nil {
		return ErrCannotDrain
	}
	if !s.config.Role.Processes() {
		// An ingest-only or source-only replica holds a database credential for
		// nothing and runs no process worker, so nothing in it can turn its
		// journal into committed rows. Saying so is better than looping until
		// the deadline and reporting a timeout that suggests slowness.
		return fmt.Errorf("%w: role %q runs no process worker, so its journal can only be drained by a replica that does",
			ErrCannotDrain, s.config.Role)
	}
	s.draining.Store(true)

	// Stop accepting. Readiness already reports not-ready, so a balancer has had
	// the chance to stop sending before the listeners actually close.
	if s.receiver != nil {
		_ = s.receiver.Shutdown(ctx)
	}
	if s.worker != nil && s.config.Role.Polls() {
		_ = s.worker.stopAndWait(ctx)
	}

	for {
		stats, err := s.journal.Stats()
		if err != nil {
			return fmt.Errorf("runtime: drain: %w", err)
		}
		if stats.Pending == 0 && stats.Claimed == 0 {
			s.logger.Info("replica drained", slog.Uint64("committed", stats.Committed))
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("runtime: drain did not finish: %d pending and %d claimed remain: %w",
				stats.Pending, stats.Claimed, ctx.Err())
		}
		if _, err := s.service.Process(ctx, persistence.MaxProcessBatch); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("runtime: drain did not finish: %w", ctx.Err())
			}
			// A failing dependency during a drain is the ordinary case: the
			// operator is scaling down, and CockroachDB may be busy. Waiting is
			// what the drain budget is for.
			if err := s.clock.Sleep(ctx, s.config.Worker.BackoffMin); err != nil {
				return fmt.Errorf("runtime: drain did not finish: %w", err)
			}
		}
	}
}

// Draining reports whether a drain has been started. A draining replica is not
// ready, so nothing new is routed to it.
func (s *Server) Draining() bool {
	return s != nil && s.draining.Load()
}
