package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/runtime"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/queuetest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// idleStore stands in for CockroachDB in tests about startup, cadence, and
// shutdown ordering. Every storage contract it would be wrong to fake is
// covered against a real database in the integration tests.
type idleStore struct{ boundary persistence.Boundary }

func (s idleStore) Boundary() persistence.Boundary { return s.boundary }

func (s idleStore) ProcessBatch(context.Context, []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	return nil, persistence.ErrUnavailable
}

// blockingJournal holds every append until a test releases it, which puts an
// admitted request deterministically in flight across a shutdown.
type blockingJournal struct {
	runtime.Journal
	admitted chan struct{}
	release  chan struct{}
}

func (j *blockingJournal) AppendBatch(batchID string, admissions []journal.Admission) error {
	j.admitted <- struct{}{}
	<-j.release
	return j.Journal.AppendBatch(batchID, admissions)
}

// foreignManifestJournal reports a manifest from another region, which is what
// a replica pointed at the wrong persistent volume would find.
type foreignManifestJournal struct{ runtime.Journal }

func (j *foreignManifestJournal) Manifest() journal.Manifest {
	manifest := j.Journal.Manifest()
	manifest.Region = "eu-central-1"
	return manifest
}

type countingJournal struct {
	runtime.Journal
	claims atomic.Int64
}

func (j *countingJournal) Claim(limit int, owner string) ([]journal.ClaimedRecord, error) {
	j.claims.Add(1)
	return j.Journal.Claim(limit, owner)
}

type countingSource struct{ polls atomic.Int64 }

func (s *countingSource) Poll(context.Context) (cloudwatch.PollResult, error) {
	s.polls.Add(1)
	return cloudwatch.PollResult{}, nil
}

func serverArgs(t *testing.T, role string, overrides ...string) []string {
	t.Helper()
	args := []string{role,
		"-region=" + otlpgen.DefaultRegion, "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + filepath.Join(t.TempDir(), "journal"), "-journal-max-bytes=67108864",
		"-otlp-grpc-listen=127.0.0.1:0", "-otlp-http-listen=127.0.0.1:0",
		"-admin-listen=127.0.0.1:0",
		"-otlp-trust-source=static_local", "-otlp-source-account=aws-account-a",
		"-otlp-source-instance=collector-a", "-otlp-credential-identity=workload-a",
		"-otlp-allowed-environments=" + otlpgen.DefaultEnvironment, "-otlp-allowed-services=" + otlpgen.DefaultService,
		"-process-interval=10ms", "-process-idle-interval=10ms", "-process-backoff-min=10ms", "-process-backoff-max=20ms",
	}
	return append(args, overrides...)
}

func testDeps(t *testing.T) runtime.Deps {
	t.Helper()
	return runtime.Deps{
		Clock: clock.System(), IDs: testids.New(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenStore: func(_ context.Context, config runtime.Config) (runtime.Store, func(), error) {
			policy, err := config.Policy()
			if err != nil {
				return nil, nil, err
			}
			return idleStore{boundary: persistence.Boundary{Scope: config.Scope,
				Classification: config.Classification, PolicyVersion: policy.Version()}}, func() {}, nil
		},
	}
}

// outboxArgs is a complete publisher configuration. It names a local endpoint
// because a queue URL that is not on AWS must be declared as one.
func outboxArgs(t *testing.T) []string {
	t.Helper()
	return []string{"outbox",
		"-region=" + otlpgen.DefaultRegion, "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-outbox-endpoint-url=http://127.0.0.1:4566",
		"-outbox-queue-url=http://127.0.0.1:4566/000000000000/agent-assignments",
		"-outbox-dead-letter-queue-url=http://127.0.0.1:4566/000000000000/agent-assignments-dlq",
		"-outbox-interval=10ms", "-outbox-idle-interval=10ms",
		"-outbox-backoff-min=10ms", "-outbox-backoff-max=20ms",
		"-admin-listen=127.0.0.1:0",
	}
}

func outboxDeps(t *testing.T, store runtime.OutboxStore, transport runtime.Transport) runtime.Deps {
	t.Helper()
	deps := testDeps(t)
	deps.OpenOutbox = func(context.Context, runtime.Config) (runtime.OutboxStore, func(), error) {
		return store, func() {}, nil
	}
	deps.OpenTransport = func(context.Context, runtime.Config) (runtime.Transport, func(), error) {
		return transport, func() {}, nil
	}
	return deps
}

// stubOutboxStore hands out a fixed set of claims once and records what the
// publisher did with them. The durable outbox contract - claim ordering,
// fencing, expiry, the content digest - belongs to CockroachDB and is covered
// against a real one; what this pins is that the replica actually drives a
// publisher and records its results.
type stubOutboxStore struct {
	mu        sync.Mutex
	claimable []persistence.OutboxClaim
	marks     []string
	retries   []string
}

func (s *stubOutboxStore) ClaimOutbox(_ context.Context, scope persistence.Scope, owner string, tokens []string, now time.Time, ttl time.Duration) ([]persistence.OutboxClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size := min(len(tokens), len(s.claimable))
	claims := make([]persistence.OutboxClaim, 0, size)
	for i := 0; i < size; i++ {
		claim := s.claimable[i]
		claim.Scope, claim.Owner, claim.Token, claim.ExpiresAt = scope, owner, tokens[i], now.Add(ttl)
		claims = append(claims, claim)
	}
	s.claimable = s.claimable[size:]
	return claims, nil
}

func (s *stubOutboxStore) MarkOutboxPublished(_ context.Context, claim persistence.OutboxClaim, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, claim.Message.MessageID)
	return nil
}

func (s *stubOutboxStore) RetryOutbox(_ context.Context, claim persistence.OutboxClaim, _ persistence.OutboxFailure, _, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries = append(s.retries, claim.Message.MessageID)
	return nil
}

func (s *stubOutboxStore) markedPublished() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.marks...)
}

// outboxClaim builds the claim CockroachDB would hand a publisher for one
// committed investigation.
func outboxClaim(t *testing.T, source *testids.Source) persistence.OutboxClaim {
	t.Helper()
	messageID, investigationID := mustTestID(t, source), mustTestID(t, source)
	body, err := agent.EncodeAssignment(agent.Assignment{
		SchemaVersion: agent.AssignmentSchemaVersion, MessageID: messageID, MessageType: agent.AssignmentMessageType,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond), Region: otlpgen.DefaultRegion, TenantID: "tenant-a",
		Classification: "SENSITIVE", Producer: agent.AssignmentProducer, CorrelationID: investigationID,
		IncidentID: strings.Repeat("a", 64), IncidentGeneration: 1, InvestigationID: investigationID,
		ServiceID: otlpgen.DefaultService, Environment: otlpgen.DefaultEnvironment, Severity: "error", ContextVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return persistence.OutboxClaim{
		Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: "tenant-a"},
		Message: queue.Message{MessageID: messageID, DeduplicationKey: "assignment:" + investigationID,
			Type: agent.AssignmentMessageType, Body: body, Attributes: map[string]string{"region": otlpgen.DefaultRegion}},
		Attempt: 1,
	}
}

func mustTestID(t *testing.T, source *testids.Source) string {
	t.Helper()
	id, err := source.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newFakeTransport(region string) *queuetest.Transport { return queuetest.New(region) }

func stopServer(t *testing.T, server *runtime.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustParse(t *testing.T, args []string) runtime.Config {
	t.Helper()
	config, err := runtime.Parse(args, func(string) string { return validDSN }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func cloudWatchServerArgs(t *testing.T) []string {
	t.Helper()
	return []string{"cloudwatch",
		"-region=" + otlpgen.DefaultRegion, "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + filepath.Join(t.TempDir(), "journal"), "-journal-max-bytes=67108864",
		"-cloudwatch-account=000000000000",
		"-cloudwatch-log-groups=/aws/ecs/payments=" + otlpgen.DefaultService + "=" + otlpgen.DefaultEnvironment,
		"-cloudwatch-source-instance=cw-adapter-a", "-cloudwatch-credential-identity=workload-a",
		"-cloudwatch-checkpoint-dir=" + filepath.Join(t.TempDir(), "checkpoints"),
		"-cloudwatch-interval=10ms", "-cloudwatch-idle-interval=10ms",
		"-cloudwatch-backoff-min=10ms", "-cloudwatch-backoff-max=20ms",
		"-process-interval=10ms", "-process-idle-interval=10ms",
		"-process-backoff-min=10ms", "-process-backoff-max=20ms",
		"-admin-listen=127.0.0.1:0",
	}
}

func TestCloudWatchRoleRunsPollingAndProcessingAgainstTheSameJournal(t *testing.T) {
	deps := testDeps(t)
	var ownedJournal *countingJournal
	deps.OpenJournal = func(config runtime.Config, policy *redact.Policy) (runtime.Journal, error) {
		real, err := runtime.OpenJournal(config, policy, deps.Clock)
		if err != nil {
			return nil, err
		}
		ownedJournal = &countingJournal{Journal: real}
		return ownedJournal, nil
	}
	source := &countingSource{}
	deps.OpenSource = func(context.Context, runtime.Config, *redact.Policy, runtime.Deps, *pipeline.Service) (runtime.Source, func(), error) {
		return source, func() {}, nil
	}

	server, err := runtime.Start(context.Background(), mustParse(t, cloudWatchServerArgs(t)), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer stopServer(t, server)

	if server.GRPCAddr() != "" || server.HTTPAddr() != "" {
		t.Fatalf("cloudwatch role opened OTLP listeners %q and %q", server.GRPCAddr(), server.HTTPAddr())
	}
	waitFor(t, "the CloudWatch poller to run", func() bool { return source.polls.Load() > 0 })
	waitFor(t, "the processor to claim from the same journal", func() bool { return ownedJournal.claims.Load() > 0 })
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Drain(drainCtx); err != nil {
		t.Fatalf("drain combined CloudWatch role: %v", err)
	}
	pollsAfterDrain := source.polls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := source.polls.Load(); got != pollsAfterDrain {
		t.Fatalf("CloudWatch poller continued after drain: polls %d -> %d", pollsAfterDrain, got)
	}
}

// This replaces TestOutboxRoleRefusesToStartRatherThanPublishNothing, which
// pinned the deliberate refusal that stood in for a publisher. The refusal was
// correct while nothing published; now that the publisher exists, the same
// requirement is that the role publishes what the process role committed. A
// publisher that started and published nothing would still look healthy while
// assignments piled up, so the assertion below is that messages actually leave.
func TestOutboxRoleStartsAPublisherThatDrainsCommittedAssignments(t *testing.T) {
	source := testids.New()
	transport := newFakeTransport(otlpgen.DefaultRegion)
	store := &stubOutboxStore{claimable: []persistence.OutboxClaim{outboxClaim(t, source)}}
	deps := outboxDeps(t, store, transport)
	server, err := runtime.Start(context.Background(), mustParse(t, outboxArgs(t)), deps)
	if err != nil {
		t.Fatalf("the outbox role did not start: %v", err)
	}
	defer stopServer(t, server)

	// runtime.md's role table: this role opens no listener.
	if server.GRPCAddr() != "" || server.HTTPAddr() != "" {
		t.Fatalf("the outbox role opened %q and %q", server.GRPCAddr(), server.HTTPAddr())
	}
	waitFor(t, "the committed assignment to be published", func() bool { return len(transport.Delivered()) == 1 })
	if marks := store.markedPublished(); len(marks) != 1 {
		t.Fatalf("%d published messages were recorded", len(marks))
	}
}

func TestAListenerStartupFailureDoesNotStartABackgroundWorker(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	source := testids.New()
	transport := newFakeTransport(otlpgen.DefaultRegion)
	store := &stubOutboxStore{claimable: []persistence.OutboxClaim{outboxClaim(t, source)}}
	args := append(outboxArgs(t), "-admin-listen="+listener.Addr().String())
	if server, err := runtime.Start(context.Background(), mustParse(t, args), outboxDeps(t, store, transport)); err == nil {
		_ = server.Shutdown(context.Background())
		t.Fatal("startup succeeded while its admin address was occupied")
	}
	time.Sleep(50 * time.Millisecond)
	if delivered := transport.Delivered(); len(delivered) != 0 {
		t.Fatalf("a worker ran after startup failed and delivered %d messages", len(delivered))
	}
}

// A publisher whose queue it may not write to must not sit in a backoff loop
// looking healthy. No supervisor restart makes a deleted queue exist, so the
// replica stops and reports the configuration exit status.
func TestAPublisherThatCannotUseItsQueueStopsWithAConfigurationFault(t *testing.T) {
	source := testids.New()
	transport := newFakeTransport(otlpgen.DefaultRegion)
	transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, "AccessDenied")
	}
	store := &stubOutboxStore{claimable: []persistence.OutboxClaim{outboxClaim(t, source)}}
	done := make(chan error, 1)
	go func() {
		done <- runtime.Run(context.Background(), mustParse(t, outboxArgs(t)), outboxDeps(t, store, transport))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, runtime.ErrInvalidConfig) {
			t.Fatalf("a revoked queue credential exited with %v, which a supervisor would restart-loop", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a publisher that may not write to its queue kept running")
	}
	if len(transport.DeadLetters()) != 0 {
		t.Fatal("a deployment fault moved committed assignments to the dead-letter queue")
	}
}

// operations.md: shutdown stops claiming new work and finishes the transaction
// in flight. For the publisher that means a message already in the queue is
// recorded before the process leaves, so it is not delivered a second time by
// the next replica for no reason.
func TestOutboxShutdownFinishesTheDeliveryInFlightBeforeReleasingItsBoundaries(t *testing.T) {
	source := testids.New()
	transport := newFakeTransport(otlpgen.DefaultRegion)
	inFlight, release := make(chan struct{}, 1), make(chan struct{})
	transport.BeforeSend = func(queue.Message) {
		select {
		case inFlight <- struct{}{}:
			<-release
		default:
		}
	}
	store := &stubOutboxStore{claimable: []persistence.OutboxClaim{outboxClaim(t, source)}}
	server, err := runtime.Start(context.Background(), mustParse(t, outboxArgs(t)), outboxDeps(t, store, transport))
	if err != nil {
		t.Fatal(err)
	}
	<-inFlight

	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		shutdown <- server.Shutdown(ctx)
	}()
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown abandoned a delivery in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-shutdown; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if delivered := transport.Delivered(); len(delivered) != 1 {
		t.Fatalf("%d deliveries survived the shutdown, want exactly the one in flight", len(delivered))
	}
	if marks := store.markedPublished(); len(marks) != 1 {
		t.Fatalf("the delivery in flight was not recorded before shutdown finished: %+v", marks)
	}
}

// The journal manifest is immutable and is the only evidence a replica has that
// it is on its own volume. A contradiction is a deployment fault, and startup is
// the only place it can be reported as one.
func TestStartRefusesAJournalWhoseManifestContradictsTheConfiguredScope(t *testing.T) {
	deps := testDeps(t)
	deps.OpenJournal = func(config runtime.Config, policy *redact.Policy) (runtime.Journal, error) {
		real, err := runtime.OpenJournal(config, policy, deps.Clock)
		if err != nil {
			return nil, err
		}
		return &foreignManifestJournal{Journal: real}, nil
	}
	server, err := runtime.Start(context.Background(), mustParse(t, serverArgs(t, "all")), deps)
	if server != nil {
		t.Fatal("a replica started on a journal belonging to another region")
	}
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("the startup error does not point an operator at the journal: %v", err)
	}
}

// A directory already carrying another region's manifest is the same fault seen
// from the journal's own side.
func TestStartRefusesAJournalDirectoryOwnedByAnotherRegion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	// Only the region differs from what this replica is configured for.
	foreign, err := journal.Open(journal.Config{Dir: dir, Owner: pipeline.DefaultWorkerOwner, TenantID: "tenant-a",
		Region: "eu-central-1", Classification: "SENSITIVE", Clock: clock.System(), Validator: redact.MinimalPolicy(),
		MaxBytes: 1 << 26, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	config := mustParse(t, serverArgs(t, "all", "-journal-dir="+dir))
	if _, err := runtime.Start(context.Background(), config, testDeps(t)); err == nil {
		t.Fatal("a replica opened another region's journal directory")
	}
}

// operations.md: on graceful shutdown, stop accepting new requests, drain
// admitted handlers through journal commit, and close the journal cleanly.
func TestGracefulShutdownDrainsAdmittedExportsThroughJournalCommit(t *testing.T) {
	const inFlight = 3
	blocking := &blockingJournal{admitted: make(chan struct{}, inFlight), release: make(chan struct{})}
	deps := testDeps(t)
	deps.OpenJournal = func(config runtime.Config, policy *redact.Policy) (runtime.Journal, error) {
		real, err := runtime.OpenJournal(config, policy, deps.Clock)
		if err != nil {
			return nil, err
		}
		blocking.Journal = real
		return blocking, nil
	}
	dir := filepath.Join(t.TempDir(), "journal")
	config := mustParse(t, serverArgs(t, "all", "-journal-dir="+dir))
	server, err := runtime.Start(context.Background(), config, deps)
	if err != nil {
		t.Fatal(err)
	}

	producer := otlpgen.New(otlpgen.WithIDs(testids.New()))
	results := make(chan error, inFlight)
	var acknowledged sync.WaitGroup
	for i := 0; i < inFlight; i++ {
		record := producer.PaymentError(otlpgen.WithBody(fmt.Sprintf("card declined %d", i)))
		payload, encodeErr := otlpgen.Encode(producer.Request(record))
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		acknowledged.Add(1)
		go func(index int, payload []byte) {
			defer acknowledged.Done()
			if index%2 == 0 {
				results <- exportGRPC(t, server.GRPCAddr(), payload)
				return
			}
			results <- exportHTTP(t, server.HTTPAddr(), payload)
		}(i, payload)
	}
	// Every request is now admitted and inside the journal append.
	for i := 0; i < inFlight; i++ {
		<-blocking.admitted
	}

	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		shutdown <- server.Shutdown(ctx)
	}()
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown abandoned %d admitted requests: %v", inFlight, err)
	case <-time.After(100 * time.Millisecond):
	}

	close(blocking.release)
	if err := <-shutdown; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	acknowledged.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err != nil {
			t.Fatalf("an admitted request failed across shutdown: %v", err)
		}
		successes++
	}
	if successes != inFlight {
		t.Fatalf("%d of %d admitted requests were acknowledged", successes, inFlight)
	}

	// The journal was closed cleanly, so it reopens, and every acknowledged
	// record is still there.
	reopened, err := journal.Open(journal.Config{Dir: dir, Owner: pipeline.DefaultWorkerOwner, TenantID: "tenant-a",
		Region: otlpgen.DefaultRegion, Classification: "SENSITIVE", Clock: clock.System(), Validator: redact.MinimalPolicy(),
		MaxBytes: 1 << 26, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatalf("the journal did not close cleanly: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	stats, err := reopened.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if total := stats.Pending + stats.Claimed + stats.Committed; total != uint64(successes) {
		t.Fatalf("%d acknowledged records, %d durable: %+v", successes, total, stats)
	}
}

func TestShutdownStopsAcceptingNewExports(t *testing.T) {
	config := mustParse(t, serverArgs(t, "all"))
	server, err := runtime.Start(context.Background(), config, testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	grpcAddr, httpAddr := server.GRPCAddr(), server.HTTPAddr()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	producer := otlpgen.New(otlpgen.WithIDs(testids.New()))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError(otlpgen.WithBody("card declined"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := exportGRPC(t, grpcAddr, payload); err == nil {
		t.Fatal("a stopped receiver acknowledged a gRPC export")
	}
	if err := exportHTTP(t, httpAddr, payload); err == nil {
		t.Fatal("a stopped receiver acknowledged an HTTP export")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	server, err := runtime.Start(context.Background(), mustParse(t, serverArgs(t, "all")), testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

func TestRunStopsWhenItsContextIsCancelled(t *testing.T) {
	config := mustParse(t, serverArgs(t, "all"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx, config, testDeps(t)) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v on an orderly stop", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run never returned after its context was cancelled")
	}
}

// The process role owns a journal volume but never listens, and the ingest role
// listens but holds no database credential.
func TestRolesOpenOnlyWhatTheyOwn(t *testing.T) {
	process, err := runtime.Start(context.Background(), mustParse(t, serverArgs(t, "process")), testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	if process.GRPCAddr() != "" || process.HTTPAddr() != "" {
		t.Fatalf("the process role opened %q and %q", process.GRPCAddr(), process.HTTPAddr())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := process.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	ingestConfig, err := runtime.Parse(serverArgs(t, "ingest"), func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("ingest requires a database credential it must not hold: %v", err)
	}
	// The opener must be a double that reports being called, not nil. A nil
	// opener is replaced by resolved() with the real one, and pgxpool.New does
	// not connect, so nil-ing it would let this test pass even if the ingest
	// role opened a store - which is the exact thing the test is named for.
	deps := testDeps(t)
	var storeOpened atomic.Bool
	deps.OpenStore = func(context.Context, runtime.Config) (runtime.Store, func(), error) {
		storeOpened.Store(true)
		return nil, nil, errors.New("the ingest role must not open a store")
	}
	ingest, err := runtime.Start(context.Background(), ingestConfig, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ingest.Shutdown(ctx) }()
	if storeOpened.Load() {
		t.Fatal("the ingest role opened a database store; it holds no database credential and must not try")
	}
	if ingest.GRPCAddr() == "" || ingest.HTTPAddr() == "" {
		t.Fatal("the ingest role opened no listener")
	}
}

func exportGRPC(t *testing.T, addr string, payload []byte) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	request := &collectorlogs.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := collectorlogs.NewLogsServiceClient(conn).Export(ctx, request); err != nil {
		return fmt.Errorf("gRPC export: %s: %w", status.Code(err), err)
	}
	return nil
}

func exportHTTP(t *testing.T, addr string, payload []byte) error {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/logs", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP export: status %d", response.StatusCode)
	}
	return nil
}

// unhealthyJournal is a real journal that refuses to append. A failure to
// commit must never be answered with success, and must leave nothing behind.
type unhealthyJournal struct{ runtime.Journal }

func (j *unhealthyJournal) AppendBatch(string, []journal.Admission) error {
	return journal.ErrNotHealthy
}

func startServer(t *testing.T, dir string, deps runtime.Deps, overrides ...string) *runtime.Server {
	t.Helper()
	args := serverArgs(t, "ingest", append([]string{"-journal-dir=" + dir}, overrides...)...)
	config, err := runtime.Parse(args, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtime.Start(context.Background(), config, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return server
}

func verifyJournal(t *testing.T, dir string) *journal.Journal {
	t.Helper()
	opened, err := journal.Open(journal.Config{Dir: dir, Owner: pipeline.DefaultWorkerOwner, TenantID: "tenant-a",
		Region: otlpgen.DefaultRegion, Classification: "SENSITIVE", Clock: clock.System(), Validator: redact.MinimalPolicy(),
		MaxBytes: 1 << 26, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

// operations.md: if mandatory data cannot be persisted, ingestion returns a
// retryable failure. It never acknowledges a silently dropped record.
func TestAJournalFailureIsRetryableAndAcknowledgesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	deps := testDeps(t)
	deps.OpenStore = nil
	deps.OpenJournal = func(config runtime.Config, policy *redact.Policy) (runtime.Journal, error) {
		real, err := runtime.OpenJournal(config, policy, clock.System())
		if err != nil {
			return nil, err
		}
		return &unhealthyJournal{Journal: real}, nil
	}
	server := startServer(t, dir, deps)
	producer := otlpgen.New(otlpgen.WithIDs(testids.New()))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError(otlpgen.WithBody("card declined"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := exportGRPC(t, server.GRPCAddr(), payload); err == nil || !strings.Contains(err.Error(), "Unavailable") {
		t.Fatalf("a journal failure answered gRPC %v", err)
	}
	if err := exportHTTP(t, server.HTTPAddr(), payload); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a journal failure answered HTTP %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := verifyJournal(t, dir).Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending+stats.Claimed+stats.Committed != 0 {
		t.Fatalf("an unacknowledged batch left records behind: %+v", stats)
	}
}

// A body that is not an OTLP request cannot become one on retry.
func TestAMalformedExportIsRejectedPermanently(t *testing.T) {
	deps := testDeps(t)
	deps.OpenStore = nil
	server := startServer(t, filepath.Join(t.TempDir(), "journal"), deps)
	if err := exportHTTP(t, server.HTTPAddr(), []byte{0xff, 0xff, 0xff, 0xff}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a malformed body answered HTTP %v", err)
	}
}

// security.md: a service or region claim conflicting with the trusted envelope
// is rejected and audited, never silently accepted or rewritten. The valid
// sibling in the same batch is still acknowledged, so the Collector drops
// exactly the bad record instead of replaying the batch forever.
func TestAForeignServiceClaimIsRejectedWhileItsValidSiblingIsAcknowledged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	deps := testDeps(t)
	deps.OpenStore = nil
	server := startServer(t, dir, deps)

	honest := otlpgen.New(otlpgen.WithIDs(testids.New()))
	forged := otlpgen.New(otlpgen.WithIDs(testids.New(testids.WithSeed(7))),
		otlpgen.WithService("attacker-service"), otlpgen.WithRegion("eu-central-1"))
	request := honest.Request(honest.PaymentError(otlpgen.WithBody("card declined")))
	request.ResourceLogs = append(request.ResourceLogs,
		forged.Request(forged.PaymentError(otlpgen.WithBody("card declined"))).ResourceLogs...)
	conn, err := grpc.NewClient(server.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	response, err := collectorlogs.NewLogsServiceClient(conn).Export(ctx, request)
	if err != nil {
		t.Fatalf("a batch with one forged claim failed entirely: %v", err)
	}
	if got := response.GetPartialSuccess().GetRejectedLogRecords(); got != 1 {
		t.Fatalf("rejected_log_records=%d, want exactly the forged record", got)
	}
	if message := response.GetPartialSuccess().GetErrorMessage(); !strings.Contains(message, "claim_not_permitted") {
		t.Fatalf("the rejection was not attributed to the trusted envelope: %q", message)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	stats, err := verifyJournal(t, dir).Stats()
	if err != nil {
		t.Fatal(err)
	}
	if total := stats.Pending + stats.Claimed + stats.Committed; total != 1 {
		t.Fatalf("%d records became durable, want only the honest one: %+v", total, stats)
	}
}

// closeRecordingJournal reports whether the server actually closed the journal.
// Pebble releasing its lock and flushing is the durable half of shutdown, so a
// shutdown that never reaches it is not a shutdown.
type closeRecordingJournal struct {
	runtime.Journal
	closed *atomic.Bool
}

func (j closeRecordingJournal) Close() error {
	j.closed.Store(true)
	return j.Journal.Close()
}

// TestShutdownStaysBoundedEvenIfTheDatabasePoolWillNotClose pins that a hung
// database connection cannot hold the process open or, worse, keep the journal
// from being closed.
//
// pgxpool.Close waits for every acquired connection to be returned. During a
// CockroachDB outage - the exact case operations.md requires this service to
// survive - a cycle can be parked on a query that never answers. Closing the
// pool inline therefore has no bound at all, and because the journal is closed
// after it, the replica would be SIGKILLed by its supervisor with Pebble still
// open rather than shut down cleanly.
func TestShutdownStaysBoundedEvenIfTheDatabasePoolWillNotClose(t *testing.T) {
	deps := testDeps(t)
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	inner := deps.OpenStore
	deps.OpenStore = func(ctx context.Context, config runtime.Config) (runtime.Store, func(), error) {
		store, _, err := inner(ctx, config)
		if err != nil {
			return nil, nil, err
		}
		return store, func() { <-wedged }, nil
	}
	var closed atomic.Bool
	deps.OpenJournal = func(config runtime.Config, policy *redact.Policy) (runtime.Journal, error) {
		opened, err := runtime.OpenJournal(config, policy, clock.System())
		if err != nil {
			return nil, err
		}
		return closeRecordingJournal{Journal: opened, closed: &closed}, nil
	}

	server, err := runtime.Start(context.Background(), mustParse(t, serverArgs(t, "process")), deps)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(ctx) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("shutdown never returned; closing the pool inline has no bound, so a hung query stops the process from stopping")
	}
	if !closed.Load() {
		t.Fatal("shutdown returned without closing the journal; the durable half of a drain was skipped behind a blocked pool close")
	}
}

// TestDeclaredDeploymentsReachDurableRecordsThroughAStartedReplica is the
// end-to-end half of acceptance item 5: enrichment is not a library sitting
// beside the service, it is wired into the ingestion path a real replica runs.
func TestDeclaredDeploymentsReachDurableRecordsThroughAStartedReplica(t *testing.T) {
	args := serverArgs(t, "all",
		"-enrichment-deployments="+otlpgen.DefaultService+"="+otlpgen.DefaultEnvironment+"=deploy-7=7.0.0")
	server, err := runtime.Start(context.Background(), mustParse(t, args), testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = server.Shutdown(ctx) })

	producer := otlpgen.New()
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError()))
	if err != nil {
		t.Fatal(err)
	}

	// The first export misses the cache and schedules the lookup; enrichment
	// never blocks, so that record groups under unknown-deployment. Later
	// exports get the resolved identity. Both are correct, and which one a
	// record gets is exactly the trade architecture.md describes.
	//
	// The assertion is on the counters rather than on a record, because that is
	// what a started replica exposes; that the resolved identity reaches the
	// record itself is pinned in internal/pipeline against a real journal.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := exportGRPC(t, server.GRPCAddr(), payload); err != nil {
			t.Fatal(err)
		}
		if counters := server.EnrichmentCounters(); counters.Resolved > 0 {
			if counters.ProviderFailures != 0 {
				t.Fatalf("a declared deployment reported %d provider failures", counters.ProviderFailures)
			}
			// The all role's worker claims records as it goes, so a record may
			// already have moved past pending. Any of the three states means it
			// was durably written.
			stats, err := server.JournalStats()
			if err != nil || stats.Pending+stats.Claimed+stats.Committed == 0 {
				t.Fatalf("nothing was journaled: stats=%+v err=%v", stats, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a declared deployment was never resolved: %+v", server.EnrichmentCounters())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestARoleThatNormalizesNothingRefusesDeploymentDeclarations keeps the same
// least-surprise rule the database credential and the source configuration
// follow: a replica must not hold configuration it cannot act on.
func TestARoleThatNormalizesNothingRefusesDeploymentDeclarations(t *testing.T) {
	args := append(outboxArgs(t), "-enrichment-deployments=paymentservice=production=deploy-7=7.0.0")
	if _, err := runtime.Parse(args, func(string) string { return validDSN }, io.Discard); !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("an outbox replica accepted deployment declarations it never uses: %v", err)
	}
}

// TestTheAdminSurfaceAnswersHealthReadinessAndMetrics pins the operational
// surface operations.md requires. It is on its own listener because whoever may
// export logs must not thereby be able to read this replica's state.
func TestTheAdminSurfaceAnswersHealthReadinessAndMetrics(t *testing.T) {
	args := serverArgs(t, "all", "-admin-listen=127.0.0.1:0")
	server, err := runtime.Start(context.Background(), mustParse(t, args), testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = server.Shutdown(ctx) })

	admin := server.AdminAddr()
	if admin == "" {
		t.Fatal("no admin listener was bound")
	}
	if admin == server.HTTPAddr() || admin == server.GRPCAddr() {
		t.Fatal("the admin surface shares a listener with ingress")
	}

	for path, want := range map[string]int{
		runtime.HealthPath:  http.StatusOK,
		runtime.ReadyPath:   http.StatusOK,
		runtime.MetricsPath: http.StatusOK,
	} {
		status, body := adminGet(t, admin, path)
		if status != want {
			t.Fatalf("%s answered %d, want %d", path, status, want)
		}
		if path == runtime.MetricsPath {
			// The counters must actually be exported, and must carry no labels:
			// every interesting label in this service is unbounded.
			for _, required := range []string{
				"static_log_analysis_journal_pending_records",
				"static_log_analysis_shed_total",
				"static_log_analysis_backpressured_total",
				"static_log_analysis_derived_identity_total",
				"static_log_analysis_rejected_invalid_missing_timestamps_total",
				"static_log_analysis_rejected_invalid_structural_total",
			} {
				if !strings.Contains(body, required) {
					t.Fatalf("metrics do not export %s:\n%s", required, body)
				}
			}
			if strings.Contains(body, "{") {
				t.Fatalf("a metric carries labels, which operations.md forbids:\n%s", body)
			}
		}
	}
}

// TestAReplicaWithNoAdminListenerStillRuns keeps the surface optional, so a
// deployment that exposes nothing is a deployment that still ingests.
func TestAReplicaWithNoAdminListenerStillRuns(t *testing.T) {
	args := serverArgs(t, "ingest", "-admin-listen=")
	config, err := runtime.Parse(args, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t)
	deps.OpenStore = nil
	server, err := runtime.Start(context.Background(), config, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = server.Shutdown(ctx) })
	if server.AdminAddr() != "" {
		t.Fatal("an admin listener was bound after being disabled")
	}
}

func adminGet(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	response, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// committingStore accepts every record, so a drain can actually finish. What a
// real CockroachDB does with these inputs is covered against a real one in
// internal/persistence; this exists so the drain loop has somewhere to drain to.
type committingStore struct{ boundary persistence.Boundary }

func (s committingStore) Boundary() persistence.Boundary { return s.boundary }

func (s committingStore) ProcessBatch(_ context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	results := make([]persistence.ProcessResult, 0, len(inputs))
	for _, input := range inputs {
		results = append(results, persistence.ProcessResult{RecordID: input.Record.RecordID})
	}
	return results, nil
}

// TestDrainingEmptiesTheJournalAndReportsNotReadyMeanwhile is scenario 11's
// drain half: a replica must be able to be emptied before its volume is
// detached, and must stop being routed to while that happens.
//
// The clock is a fake one because a drain cannot finish faster than the records
// in the journal can finalize: a record inside its allowed-lateness window is
// held, not stuck. Advancing past that window is the difference between testing
// the drain and testing the rule engine's patience.
func TestDrainingEmptiesTheJournalAndReportsNotReadyMeanwhile(t *testing.T) {
	clk := fakeclock.NewAtOrigin()
	deps := testDeps(t)
	deps.Clock = clk
	deps.OpenStore = func(_ context.Context, config runtime.Config) (runtime.Store, func(), error) {
		policy, err := config.Policy()
		if err != nil {
			return nil, nil, err
		}
		return committingStore{boundary: persistence.Boundary{Scope: config.Scope,
			Classification: config.Classification, PolicyVersion: policy.Version()}}, func() {}, nil
	}
	args := serverArgs(t, "all", "-admin-listen=127.0.0.1:0")
	server, err := runtime.Start(context.Background(), mustParse(t, args), deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = server.Shutdown(ctx) })

	producer := otlpgen.New(otlpgen.WithClock(clk))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError()))
	if err != nil {
		t.Fatal(err)
	}
	if err := exportGRPC(t, server.GRPCAddr(), payload); err != nil {
		t.Fatal(err)
	}

	if status, _ := adminGet(t, server.AdminAddr(), runtime.ReadyPath); status != http.StatusOK {
		t.Fatalf("a healthy replica reported %d before draining", status)
	}

	// Past every record's allowed-lateness window, so the journal can finalize.
	clk.Advance(10 * time.Minute)

	if err := server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !server.Draining() {
		t.Fatal("Draining() disagrees with Drain()")
	}

	// The volume is now safe to detach: nothing acknowledged is left unhandled.
	stats, err := server.JournalStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 || stats.Claimed != 0 {
		t.Fatalf("drain returned with work still in the journal: %+v", stats)
	}

	// And a balancer has been told to stop sending.
	if status, body := adminGet(t, server.AdminAddr(), runtime.ReadyPath); status != http.StatusServiceUnavailable {
		t.Fatalf("a drained replica reported %d (%q), want 503", status, body)
	}
}

// TestAReplicaThatCannotProcessRefusesToPretendItDrained pins the honest
// failure. An ingest-only replica runs no process worker, so nothing in it can
// turn its journal into committed rows; looping until the deadline and
// reporting a timeout would suggest slowness rather than impossibility.
func TestAReplicaThatCannotProcessRefusesToPretendItDrained(t *testing.T) {
	config, err := runtime.Parse(serverArgs(t, "ingest"), func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t)
	deps.OpenStore = nil
	server, err := runtime.Start(context.Background(), config, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = server.Shutdown(ctx) })

	if err := server.Drain(ctx); !errors.Is(err, runtime.ErrCannotDrain) {
		t.Fatalf("an ingest-only replica answered %v, want ErrCannotDrain", err)
	}
}
