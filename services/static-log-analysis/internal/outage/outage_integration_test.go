//go:build integration

package outage_test

import (
	"context"
	"errors"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/crdbtest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/netgate"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

const (
	tenantID       = "tenant-a"
	classification = "SENSITIVE"
	recordCount    = 7
)

// TestIngestionSurvivesACockroachDBOutageAndEveryRecordContributesOnce is
// testing.md's required resilience scenarios 1 through 3, and the evidence for
// the SLO "zero acknowledged mandatory-record loss during a 30-minute
// CockroachDB outage when the persistent volume remains healthy and sized
// capacity is not exceeded".
//
// Two things about it are deliberate.
//
// The outage is a real severed network rather than a store that returns errors
// on demand. A double would prove only that the coordinator handles the errors
// the double chose to return, and the failures that matter here — a pool whose
// connections die mid-query, a cycle that has to be retried whole — are ones a
// double cannot produce.
//
// The literal thirty minutes is not reproduced. What thirty minutes buys is
// that the journal must hold every acknowledged record and that recovery must
// be recovery rather than a fresh start. Both are asserted directly, and
// wall-clock duration adds nothing a CI job can afford to wait for.
func TestIngestionSurvivesACockroachDBOutageAndEveryRecordContributesOnce(t *testing.T) {
	ctx := context.Background()
	direct := crdbtest.Pool(t)
	if err := persistence.ApplyMigrations(ctx, direct, persistence.TopologySingleRegion); err != nil {
		t.Fatal(err)
	}

	// The service reaches CockroachDB only through the gate, so closing it is
	// an outage from the service's point of view and a no-op from the data's.
	gate, err := netgate.New(upstreamOf(t, direct))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Close)

	pool, err := pgxpool.New(ctx, throughGate(t, direct, gate.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	scope := persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}
	policy := redact.MinimalPolicy()
	store, err := persistence.New(pool, persistence.Config{Validator: policy, Scope: scope,
		Classification: classification, Topology: persistence.TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	clk := fakeclock.NewAtOrigin()
	j, err := journal.Open(journal.Config{Dir: filepath.Join(t.TempDir(), "journal"), Owner: "outage",
		TenantID: tenantID, Region: otlpgen.DefaultRegion, Classification: classification, Clock: clk,
		Validator: policy, MaxBytes: 64 << 20, MinFreeBytes: 1,
		FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	service, err := pipeline.New(pipeline.Config{Clock: clk, IDs: testids.New(testids.WithClock(clk)),
		Journal: j, Store: store, Policy: policy, Scope: scope, Classification: classification})
	if err != nil {
		t.Fatal(err)
	}

	envelope := model.TrustedEnvelope{SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a",
		Region: otlpgen.DefaultRegion, AllowedEnvironments: []string{otlpgen.DefaultEnvironment},
		AllowedServices: []string{otlpgen.DefaultService}, SourceInstance: "collector-a",
		CredentialIdentity: "workload-a"}
	producer := otlpgen.New(otlpgen.WithClock(clk),
		otlpgen.WithIDs(testids.New(testids.WithClock(clk), testids.WithSeed(7))))

	ingest := func(index int) {
		t.Helper()
		record := producer.PaymentError(otlpgen.WithBody("payment declined"),
			otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(index)*time.Second).UnixNano())))
		payload, err := otlpgen.Encode(producer.Request(record))
		if err != nil {
			t.Fatal(err)
		}
		envelope.ReceivedAt = clk.Now()
		result, err := service.Ingest(ctx, envelope, payload, admission.EncodingIdentity)
		if err != nil || !result.Acknowledged {
			t.Fatalf("ingest %d: result=%+v err=%v", index, result, err)
		}
	}

	// Two records before the outage, so recovery has to be recovery.
	ingest(0)
	ingest(1)

	gate.Block()

	// Scenario 2: ingestion remains successful while the database is
	// unreachable. Every one of these is acknowledged, which means every one is
	// already durable.
	for i := 2; i < recordCount; i++ {
		ingest(i)
	}

	// Move past the rule window so there is finalized work to persist. A cycle
	// with nothing to persist succeeds without ever reaching the database, so
	// asserting failure before this point would assert nothing at all.
	clk.Advance(2*time.Minute + 5*time.Second)

	if _, err := service.Process(ctx, 100); err == nil {
		t.Fatal("processing succeeded while CockroachDB was unreachable, with finalized records to persist")
	}
	stats, err := j.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Committed != 0 {
		t.Fatalf("journal committed %d records during an outage", stats.Committed)
	}
	if stats.Pending+stats.Claimed != recordCount {
		t.Fatalf("journal holds %d pending and %d claimed of %d acknowledged records; acknowledged work was lost during the outage",
			stats.Pending, stats.Claimed, recordCount)
	}

	// Scenario 3: restore, and verify every record contributes exactly once.
	gate.Unblock()
	// Far enough past every record's event time that the last one's horizon has
	// finalized too. A record still inside its window is held, not lost, but a
	// drained journal is the clearer end state to assert.
	clk.Advance(10 * time.Minute)
	if err := processUntilDrained(ctx, service); err != nil {
		t.Fatal(err)
	}

	stats, err = j.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 || stats.Claimed != 0 {
		t.Fatalf("journal still holds unprocessed work after recovery: %+v", stats)
	}
	if stats.Committed != recordCount {
		t.Fatalf("journal committed %d of %d acknowledged records; acknowledged work was lost", stats.Committed, recordCount)
	}

	// Exactly once. A record counted twice by a retried cycle would show up
	// here as an extra occurrence, and a lost one as a missing occurrence.
	var occurrences int
	if err := direct.QueryRow(ctx, `SELECT count(*) FROM occurrences`).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if occurrences != recordCount {
		t.Fatalf("%d occurrences for %d acknowledged records; each must contribute exactly once", occurrences, recordCount)
	}
	var investigations int
	if err := direct.QueryRow(ctx, `SELECT count(*) FROM investigations`).Scan(&investigations); err != nil {
		t.Fatal(err)
	}
	if investigations != 1 {
		t.Fatalf("%d investigations; an outage must not elect more than one", investigations)
	}
}

// upstreamOf is the host:port the harness pool connects to directly.
func upstreamOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	config := pool.Config().ConnConfig
	return net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))
}

// throughGate rewrites the harness DSN to point at the gate instead of at the
// database, keeping the same database name.
func throughGate(t *testing.T, pool *pgxpool.Pool, addr string) string {
	t.Helper()
	config := pool.Config().ConnConfig
	return "postgres://" + config.User + "@" + addr + "/" + config.Database + "?sslmode=disable"
}

// processUntilDrained runs cycles until nothing is left to claim, so the
// assertion is about the end state rather than about how many cycles recovery
// happened to need.
func processUntilDrained(ctx context.Context, service *pipeline.Service) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		result, err := service.Process(ctx, 100)
		if err != nil {
			if time.Now().After(deadline) {
				return err
			}
			// The pool is re-establishing connections through the reopened gate.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if result.Claimed == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("processing never drained after the outage")
		}
	}
}
