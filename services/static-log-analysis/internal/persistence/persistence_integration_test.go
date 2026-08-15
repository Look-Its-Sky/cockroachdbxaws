//go:build integration

package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/fingerprint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/crdbtest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/cockroachdb"
)

func TestMigrationsApplyReapplyAndRefuseUnknownFutureSchema(t *testing.T) {
	pool := crdbtest.Pool(t)
	ctx := context.Background()
	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); err != nil {
		t.Fatalf("idempotent reapply: %v", err)
	}
	for _, table := range []string{"records_seen", "incident_families", "incident_generations", "occurrences", "evidence", "investigations", "investigation_claims", "investigation_progress", "investigation_contexts", "reports", "outbox_messages", "audit_events"} {
		var found bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)`, table).Scan(&found); err != nil || !found {
			t.Fatalf("migration table %s: found=%v err=%v", table, found, err)
		}
	}
	for _, constraint := range []string{"occurrences_generation_fk", "evidence_generation_fk"} {
		var updateRule string
		if err := pool.QueryRow(ctx, `SELECT update_rule FROM information_schema.referential_constraints
			WHERE constraint_schema='public' AND constraint_name=$1`, constraint).Scan(&updateRule); err != nil || updateRule != "NO ACTION" {
			t.Fatalf("generation FK %s must prevent mutation of immutable identity: rule=%q err=%v", constraint, updateRule, err)
		}
	}
	var checksum []byte
	if err := pool.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=1`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum=b'tampered' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("want checksum mismatch refused, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=1`, checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES (99,'future',b'future')`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("want future schema refused, got %v", err)
	}
}

func TestMigrationsRefuseLedgerAndLiveSchemaDrift(t *testing.T) {
	tests := []struct {
		name  string
		drift string
	}{
		{name: "ledger name", drift: `UPDATE schema_migrations SET name='renamed.sql' WHERE version=1`},
		{name: "ledger extra zero", drift: `INSERT INTO schema_migrations(version,name,checksum) VALUES (0,'invalid.sql',b'invalid')`},
		{name: "ledger negative", drift: `INSERT INTO schema_migrations(version,name,checksum) VALUES (-1,'invalid.sql',b'invalid')`},
		{name: "required column", drift: `ALTER TABLE records_seen DROP COLUMN safe_outcome`},
		{name: "required constraint", drift: `ALTER TABLE evidence DROP CONSTRAINT evidence_expiry`},
		{name: "required index", drift: `DROP INDEX records_seen_by_scope`},
		{name: "same-name wrong constraint", drift: `ALTER TABLE evidence DROP CONSTRAINT evidence_expiry; ALTER TABLE evidence ADD CONSTRAINT evidence_expiry CHECK (expires_at >= created_at)`},
		{name: "same-name wrong foreign key", drift: `ALTER TABLE investigation_claims DROP CONSTRAINT investigation_claims_investigation_fk; ALTER TABLE investigation_claims ADD CONSTRAINT investigation_claims_investigation_fk FOREIGN KEY (region,tenant_id,investigation_id) REFERENCES investigations (region,tenant_id,investigation_id)`},
		{name: "same-name wrong index", drift: `DROP INDEX records_seen_by_scope; CREATE INDEX records_seen_by_scope ON records_seen (tenant_id,region,record_id)`},
		{name: "required default", drift: `ALTER TABLE outbox_messages ALTER COLUMN attempts DROP DEFAULT`},
		{name: "required digest column", drift: `ALTER TABLE outbox_messages DROP COLUMN content_digest`},
		{name: "required digest constraint", drift: `ALTER TABLE outbox_messages DROP CONSTRAINT outbox_messages_content_digest_length`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := crdbtest.Pool(t)
			ctx := context.Background()
			if err := ApplyMigrations(ctx, pool, TopologySingleRegion); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, test.drift); err != nil {
				t.Fatalf("inject drift: %v", err)
			}
			if err := ApplyMigrations(ctx, pool, TopologySingleRegion); !errors.Is(err, ErrIncompatibleSchema) {
				t.Fatalf("want incompatible schema for %s, got %v", test.name, err)
			}
		})
	}
}

func TestSingleRegionTopologyMustBeExplicitAndDatabaseHasNoMultiRegionConfiguration(t *testing.T) {
	pool := crdbtest.Pool(t)
	ctx := context.Background()
	if err := ApplyMigrations(ctx, pool, Topology("multi-region")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsupported topology accepted: %v", err)
	}
	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); err != nil {
		t.Fatal(err)
	}
	compatible, err := verifySingleRegionTopology(ctx, pool)
	if err != nil || !compatible {
		t.Fatalf("single-region harness rejected: compatible=%v err=%v", compatible, err)
	}
	if _, err := New(pool, Config{Validator: redact.MinimalPolicy(), Scope: Scope{Region: builders.DefaultRegion, TenantID: "tenant-a"}, Classification: "SENSITIVE"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("store without explicit topology accepted: %v", err)
	}
}

func TestMigrationAcceptsDatabaseAssignedExactlyOneCockroachRegion(t *testing.T) {
	ctx := context.Background()
	pool := oneRegionManagedPool(t)
	compatible, err := verifySingleRegionTopology(ctx, pool)
	if err != nil || !compatible {
		t.Fatalf("one-region managed database rejected: compatible=%v err=%v", compatible, err)
	}
	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); err != nil {
		t.Fatalf("migrate one-region managed database: %v", err)
	}
	var migrationTable bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_migrations')`).Scan(&migrationTable); err != nil {
		t.Fatal(err)
	}
	if !migrationTable {
		t.Fatal("migration did not create its ledger")
	}
}

func oneRegionManagedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := cockroachdb.Run(ctx, crdbtest.Image(),
		cockroachdb.WithInsecure(), testcontainers.WithCmdArgs("--locality=region=us-east-1"))
	if err != nil {
		if os.Getenv("REQUIRE_DOCKER") == "1" {
			t.Fatalf("start dedicated localized CockroachDB: %v", err)
		}
		t.Skipf("dedicated localized CockroachDB unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate dedicated CockroachDB: %v", err)
		}
	})
	config, err := container.ConnectionConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE multi_region_test PRIMARY REGION "us-east-1"`); err != nil {
		admin.Close(ctx)
		t.Fatalf("create real multi-region database metadata: %v", err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.Database = "multi_region_test"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrationTemporarilyUnlocksManagedTablesAndRestoresTheLock(t *testing.T) {
	pool := oneRegionManagedPool(t)
	ctx := context.Background()
	loaded, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
		version INT8 PRIMARY KEY,
		name STRING NOT NULL,
		checksum BYTES NOT NULL,
		catalog_checksum BYTES NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range splitMigration(loaded[0].sql) {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		loaded[0].version, loaded[0].name, loaded[0].checksum[:]); err != nil {
		t.Fatal(err)
	}
	for table := range requiredSchema {
		if _, err := pool.Exec(ctx, `ALTER TABLE `+fmt.Sprintf(`%q`, table)+` SET (schema_locked=true)`); err != nil {
			t.Fatalf("lock %s: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx, `SET CLUSTER SETTING sql.schema.auto_unlock.enabled = false`); err != nil {
		t.Fatal(err)
	}

	if err := ApplyMigrations(ctx, pool, TopologySingleRegion); err != nil {
		checksum, checksumErr := liveCatalogChecksum(ctx, pool)
		t.Fatalf("resume migration with a locked table: %v (catalog checksum=%x, checksum error=%v)", err, checksum, checksumErr)
	}
	var name, createStatement string
	if err := pool.QueryRow(ctx, `SHOW CREATE TABLE occurrences`).Scan(&name, &createStatement); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(createStatement, "schema_locked = true") {
		t.Fatalf("migration did not restore schema lock:\n%s", createStatement)
	}
	var secondVersion bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=2)`).Scan(&secondVersion); err != nil {
		t.Fatal(err)
	}
	if !secondVersion {
		t.Fatal("second migration was not recorded")
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE occurrences SET (schema_locked=false)`); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := restoreTableLocks(cancelled, pool, []string{"occurrences"}); err != nil {
		t.Fatalf("restore lock after migration cancellation: %v", err)
	}
	if err := pool.QueryRow(ctx, `SHOW CREATE TABLE occurrences`).Scan(&name, &createStatement); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(createStatement, "schema_locked = true") {
		t.Fatal("cancelled migration cleanup left the table unlocked")
	}
}

func TestConcurrentStableRecordIDContributesOnce(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	ids := testids.New()
	record := factory.Record(t)
	input := preparedInput(t, ids, record, false)

	const workers = 24
	results := make(chan ProcessResult, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := store.Process(context.Background(), input)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("process: %v", err)
	}
	created := 0
	for result := range results {
		if !result.Duplicate {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("want one contribution, got %d", created)
	}
	assertCount(t, pool, "records_seen", 1)
	assertCount(t, pool, "occurrences", 1)
	snapshot, err := store.Incident(context.Background(), input.Scope, input.IncidentID)
	if err != nil || snapshot.OccurrenceCount != 1 {
		t.Fatalf("aggregate: %v %+v", err, snapshot)
	}
}

func TestConcurrentFamilyGenerationAndInvestigationCreationHasOneCanonicalResult(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	const workers = 16
	inputs := make([]ProcessInput, workers)
	for i := range inputs {
		inputs[i] = preparedInput(t, idSource, factory.Record(t), true)
	}

	results := make(chan ProcessResult, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for _, input := range inputs {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := store.Process(context.Background(), input)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("process: %v", err)
	}
	families := map[string]bool{}
	investigations := map[string]bool{}
	created := 0
	for result := range results {
		families[result.IncidentID] = true
		investigations[result.InvestigationID] = true
		if result.InvestigationCreated {
			created++
		}
	}
	if len(families) != 1 || len(investigations) != 1 || created != 1 {
		t.Fatalf("want one canonical family/investigation, families=%v investigations=%v created=%d", families, investigations, created)
	}
	assertCount(t, pool, "incident_families", 1)
	assertCount(t, pool, "incident_generations", 1)
	assertCount(t, pool, "investigations", 1)
	assertCount(t, pool, "investigation_claims", 1)
	assertCount(t, pool, "investigation_contexts", 1)
	assertCount(t, pool, "outbox_messages", 1)
}

func TestConcurrentDistinctEpisodesReconstructGenerationNumbersIndependentOfLockWinner(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	first := preparedInput(t, idSource, factory.Record(t), false)
	initial, err := store.Process(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}

	inputs := make([]ProcessInput, 2)
	for i := range inputs {
		record := factory.Record(t,
			builders.WithObservedTime(first.Record.ObservedTime.Add(time.Duration(i+1)*time.Hour)),
			builders.WithEventTime(first.Record.EventTime.Add(time.Duration(i+1)*time.Hour)),
			builders.WithDeployment(fmt.Sprintf("deployment-%d", i+2), "test"),
		)
		inputs[i] = preparedInput(t, idSource, record, false)
		inputs[i].EpisodeStart = record.EventTime
		inputs[i].Generation = mustGeneration(t, inputs[i].DeploymentID, inputs[i].EpisodeStart)
		setDecision(t, &inputs[i])
	}
	var wait sync.WaitGroup
	results := make(chan ProcessResult, 2)
	for _, input := range inputs {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := store.Process(context.Background(), input)
			if err != nil {
				t.Errorf("process: %v", err)
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	completed := 0
	for result := range results {
		if result.IncidentID != initial.IncidentID {
			t.Fatalf("episode left canonical family: %+v", result)
		}
		completed++
	}
	if completed != 2 {
		t.Fatalf("want both concurrent episodes committed, got %d", completed)
	}
	rows, err := pool.Query(context.Background(), `SELECT generation,deployment_id FROM incident_generations
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 ORDER BY episode_start,deployment_id`,
		first.Scope.Region, first.Scope.TenantID, initial.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var generation int64
		var deployment string
		if err := rows.Scan(&generation, &deployment); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s", generation, deployment))
	}
	want := []string{
		fmt.Sprintf("%d:%s", mustGeneration(t, first.DeploymentID, first.EpisodeStart), first.DeploymentID),
		fmt.Sprintf("%d:deployment-2", mustGeneration(t, "deployment-2", first.EpisodeStart.Add(time.Hour))),
		fmt.Sprintf("%d:deployment-3", mustGeneration(t, "deployment-3", first.EpisodeStart.Add(2*time.Hour))),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want event-time canonical topology %v, got %v", want, got)
	}

	// A deliberately reverse arrival order proves the canonical numbering is a
	// projection of event time/deployment, rather than whichever transaction won.
	var reverseIncident string
	for _, offset := range []int{2, 0, 1} {
		deployment := first.DeploymentID
		if offset > 0 {
			deployment = fmt.Sprintf("deployment-%d", offset+1)
		}
		record := factory.Record(t,
			builders.WithService("permutation-service"),
			builders.WithObservedTime(first.Record.ObservedTime.Add(time.Duration(offset)*time.Hour)),
			builders.WithEventTime(first.Record.EventTime.Add(time.Duration(offset)*time.Hour)),
			builders.WithDeployment(deployment, "test"),
		)
		input := preparedInput(t, idSource, record, false)
		input.Evidence = &EvidenceInput{EvidenceID: mustID(t, idSource), Version: 1, Classification: "SENSITIVE", Provenance: "normalized_log", CreatedAt: input.ProcessedAt, ExpiresAt: input.ProcessedAt.Add(time.Hour)}
		result, err := store.Process(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		reverseIncident = result.IncidentID
	}
	rows, err = pool.Query(context.Background(), `SELECT generation,deployment_id FROM incident_generations
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 ORDER BY episode_start,deployment_id`,
		first.Scope.Region, first.Scope.TenantID, reverseIncident)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got = got[:0]
	for rows.Next() {
		var generation int64
		var deployment string
		if err := rows.Scan(&generation, &deployment); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s", generation, deployment))
	}
	want = []string{
		fmt.Sprintf("%d:%s", mustGeneration(t, first.DeploymentID, first.EpisodeStart), first.DeploymentID),
		fmt.Sprintf("%d:deployment-2", mustGeneration(t, "deployment-2", first.EpisodeStart.Add(time.Hour))),
		fmt.Sprintf("%d:deployment-3", mustGeneration(t, "deployment-3", first.EpisodeStart.Add(2*time.Hour))),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opposite arrival order changed episode-to-number mapping: want %v, got %v", want, got)
	}
	reverseSnapshot, err := store.Incident(context.Background(), first.Scope, reverseIncident)
	if err != nil {
		t.Fatal(err)
	}
	wantLatest := mustGeneration(t, "deployment-3", first.EpisodeStart.Add(2*time.Hour))
	if reverseSnapshot.LatestGeneration != wantLatest {
		t.Fatalf("late earlier arrival replaced chronologically latest generation: want %d got %+v", wantLatest, reverseSnapshot)
	}
	var brokenReferences int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM occurrences o JOIN incident_generations g
		 ON (o.region,o.tenant_id,o.incident_id,o.generation)=(g.region,g.tenant_id,g.incident_id,g.generation)
		 WHERE o.incident_id=$1) +
		(SELECT count(*) FROM evidence e JOIN incident_generations g
		 ON (e.region,e.tenant_id,e.incident_id,e.generation)=(g.region,g.tenant_id,g.incident_id,g.generation)
		 WHERE e.incident_id=$1)`, reverseIncident).Scan(&brokenReferences); err != nil || brokenReferences != 6 {
		t.Fatalf("renumbering did not preserve three occurrence and three evidence references: count=%d err=%v", brokenReferences, err)
	}
	var seenGenerations []int64
	seenRows, err := pool.Query(context.Background(), `SELECT r.generation FROM records_seen r JOIN occurrences o ON o.record_id=r.record_id
		WHERE r.incident_id=$1 ORDER BY o.event_time`, reverseIncident)
	if err != nil {
		t.Fatal(err)
	}
	defer seenRows.Close()
	for seenRows.Next() {
		var generation int64
		if err := seenRows.Scan(&generation); err != nil {
			t.Fatal(err)
		}
		seenGenerations = append(seenGenerations, generation)
	}
	wantGenerations := []int64{
		mustGeneration(t, first.DeploymentID, first.EpisodeStart),
		mustGeneration(t, "deployment-2", first.EpisodeStart.Add(time.Hour)),
		mustGeneration(t, "deployment-3", first.EpisodeStart.Add(2*time.Hour)),
	}
	if !reflect.DeepEqual(seenGenerations, wantGenerations) {
		t.Fatalf("records_seen did not retain immutable generation identities: want %v got %v", wantGenerations, seenGenerations)
	}
}

func TestDashboardOverviewIsBoundedToSafeOperationalMetadata(t *testing.T) {
	store, _ := integrationStore(t)
	input := preparedInput(t, testids.New(), builders.NewFactory().Record(t), true)
	if _, err := store.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	overview, err := store.Overview(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if overview.RecordsSeen != 1 || overview.IncidentFamilies != 1 || overview.Investigations["queued"] != 1 ||
		overview.Outbox["pending"] != 1 || len(overview.Recent) != 1 {
		t.Fatalf("unexpected overview: %+v", overview)
	}
	item := overview.Recent[0]
	if item.InvestigationID != input.Investigation.InvestigationID || item.Service != input.Record.Service.Name ||
		item.Environment != input.Record.Service.Environment || item.TriggerReason != "five_in_five" {
		t.Fatalf("unexpected recent investigation: %+v", item)
	}
}

func TestFinalizedM1DecisionKeepsOneGenerationAcrossArrivalPermutationAndReplay(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "T9_then_T10"
		if reverse {
			name = "T10_then_T9"
		}
		t.Run(name, func(t *testing.T) {
			store, pool := integrationStore(t)
			factory := builders.NewFactory()
			idSource := testids.New()
			origin := factory.Record(t).EventTime
			t9 := factory.Record(t, builders.WithObservedTime(origin.Add(9*time.Minute+builders.DefaultEventLag)), builders.WithEventTime(origin.Add(9*time.Minute)))
			t10 := factory.Record(t, builders.WithObservedTime(origin.Add(10*time.Minute+builders.DefaultEventLag)), builders.WithEventTime(origin.Add(10*time.Minute)))
			arrival := []model.NormalizedLog{t9, t10}
			if reverse {
				arrival = []model.NormalizedLog{t10, t9}
			}
			engine := incident.New()
			for _, record := range arrival {
				if _, err := engine.Observe(record); err != nil {
					t.Fatal(err)
				}
			}
			engine.Advance(t10.EventTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
			inputs := make([]ProcessInput, 0, len(arrival))
			for _, record := range arrival {
				decision, ok := engine.PersistenceDecision(record.RecordID)
				if !ok {
					t.Fatalf("missing finalized decision for %s", record.RecordID)
				}
				input := preparedInput(t, idSource, record, false)
				applyDecision(&input, decision)
				inputs = append(inputs, input)
				if _, err := store.Process(context.Background(), input); err != nil {
					t.Fatalf("persist finalized decision: %v", err)
				}
			}
			var generations, occurrences int
			var episodeStart time.Time
			if err := pool.QueryRow(context.Background(), `SELECT count(*),min(episode_start) FROM incident_generations`).Scan(&generations, &episodeStart); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM occurrences`).Scan(&occurrences); err != nil {
				t.Fatal(err)
			}
			if generations != 1 || occurrences != 2 || !episodeStart.Equal(t9.EventTime) {
				t.Fatalf("M1 one-generation decision split in storage: generations=%d occurrences=%d start=%s", generations, occurrences, episodeStart)
			}
			// A journal replay may redeliver the same committed semantic records;
			// the immutable decisions and global record gate keep it a no-op.
			for i := len(inputs) - 1; i >= 0; i-- {
				result, err := store.Process(context.Background(), inputs[i])
				if err != nil || !result.Duplicate {
					t.Fatalf("journal replay was not idempotent: result=%+v err=%v", result, err)
				}
			}
			assertCount(t, pool, "incident_generations", 1)
			assertCount(t, pool, "occurrences", 2)
		})
	}
}

func TestActualJournalClaimReopenAndRedeliveryIsOnePersistenceContribution(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "T9_then_T10"
		if reverse {
			name = "T10_then_T9"
		}
		t.Run(name, func(t *testing.T) {
			store, pool := integrationStore(t)
			journalClock := fakeclock.NewAtOrigin()
			factory := builders.NewFactory(builders.WithClock(journalClock))
			idSource := testids.New()
			origin := factory.Record(t).EventTime
			t9 := factory.Record(t, builders.WithObservedTime(origin.Add(9*time.Minute+builders.DefaultEventLag)), builders.WithEventTime(origin.Add(9*time.Minute)))
			t10 := factory.Record(t, builders.WithObservedTime(origin.Add(10*time.Minute+builders.DefaultEventLag)), builders.WithEventTime(origin.Add(10*time.Minute)))
			cfg := journal.Config{
				Dir: t.TempDir(), Owner: "replica-a", TenantID: "tenant-a", Region: builders.DefaultRegion,
				Classification: "SENSITIVE", Clock: journalClock, Validator: redact.MinimalPolicy(),
				MaxBytes: 64 << 20, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil },
			}
			j, err := journal.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range []model.NormalizedLog{t9, t10} {
				if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityNormal}}); err != nil {
					t.Fatalf("journal append: %v", err)
				}
			}
			firstClaims, err := j.Claim(2, "worker-a")
			if err != nil || len(firstClaims) != 2 {
				t.Fatalf("first journal claim: count=%d err=%v", len(firstClaims), err)
			}
			byID := make(map[string]journal.ClaimedRecord, 2)
			for _, claim := range firstClaims {
				byID[claim.Record.RecordID] = claim
			}
			arrival := []journal.ClaimedRecord{byID[t9.RecordID], byID[t10.RecordID]}
			if reverse {
				arrival[0], arrival[1] = arrival[1], arrival[0]
			}
			engine := incident.New()
			for _, claim := range arrival {
				if _, err := engine.Observe(claim.Record); err != nil {
					t.Fatalf("M1 observe claimed record: %v", err)
				}
			}
			engine.Advance(t10.EventTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
			type decisionValues struct {
				Version, IncidentID, RecordID, DeploymentID string
				EpisodeStart, FinalizedThrough              time.Time
				Generation                                  int64
				EvidenceOnly                                bool
			}
			decisionValue := func(value incident.PersistenceDecision) decisionValues {
				return decisionValues{Version: value.Version(), IncidentID: value.IncidentID(), RecordID: value.RecordID(),
					DeploymentID: value.DeploymentID(), EpisodeStart: value.EpisodeStart(), FinalizedThrough: value.FinalizedThrough(),
					Generation: value.GenerationKey(), EvidenceOnly: value.EvidenceOnly()}
			}
			originalDecisions := make(map[string]decisionValues, 2)
			firstReplays := make(map[string]journal.ReplayIdentity, 2)
			for _, claim := range arrival {
				decision, ok := engine.PersistenceDecision(claim.Record.RecordID)
				if !ok {
					t.Fatalf("M1 did not finalize journal claim %s", claim.Record.RecordID)
				}
				input := preparedInput(t, idSource, claim.Record, false)
				applyDecision(&input, decision)
				input.Replay = claim.Replay
				result, err := store.Process(context.Background(), input)
				if err != nil || result.Duplicate {
					t.Fatalf("first persistence contribution: result=%+v err=%v", result, err)
				}
				originalDecisions[claim.Record.RecordID] = decisionValue(decision)
				firstReplays[claim.Record.RecordID] = claim.Replay
			}
			engine = nil

			// Cockroach committed, but the journal claim was not acknowledged.
			// Reopening after claim expiry exercises Pebble's real recovery and
			// reclaim indexes rather than reconstructing a replay identity.
			journalClock.Advance(journal.DefaultClaimTTL + time.Nanosecond)
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			j, err = journal.Open(cfg)
			if err != nil {
				t.Fatalf("journal reopen: %v", err)
			}
			defer j.Close()
			redelivered, err := j.Claim(2, "worker-b")
			if err != nil || len(redelivered) != 2 {
				t.Fatalf("journal reclaim: count=%d err=%v", len(redelivered), err)
			}
			redeliveredByID := make(map[string]journal.ClaimedRecord, 2)
			for _, claim := range redelivered {
				redeliveredByID[claim.Record.RecordID] = claim
			}
			freshArrival := []journal.ClaimedRecord{redeliveredByID[t9.RecordID], redeliveredByID[t10.RecordID]}
			if reverse {
				freshArrival[0], freshArrival[1] = freshArrival[1], freshArrival[0]
			}
			freshEngine := incident.New()
			for _, claim := range freshArrival {
				if claim.Attempt != 2 {
					t.Fatalf("fresh M1 received non-reclaimed attempt: %+v", claim)
				}
				if _, err := freshEngine.Observe(claim.Record); err != nil {
					t.Fatalf("fresh M1 observe reclaimed record: %v", err)
				}
			}
			freshEngine.Advance(t10.EventTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
			commits := make([]journal.CommitClaim, 0, 2)
			for _, claim := range freshArrival {
				prior := firstReplays[claim.Record.RecordID]
				if claim.Attempt != 2 || claim.Replay.Digest() != prior.Digest() || claim.Replay.Version() != prior.Version() || claim.Replay.Priority() != prior.Priority() {
					t.Fatalf("journal changed sealed replay identity: first=%+v redelivery=%+v", prior, claim)
				}
				freshDecision, ok := freshEngine.PersistenceDecision(claim.Record.RecordID)
				if !ok {
					t.Fatalf("fresh M1 did not reconstruct decision for %s", claim.Record.RecordID)
				}
				if got, want := decisionValue(freshDecision), originalDecisions[claim.Record.RecordID]; !reflect.DeepEqual(got, want) {
					t.Fatalf("fresh M1 decision changed across restart: want=%+v got=%+v", want, got)
				}
				input := preparedInput(t, idSource, claim.Record, false)
				applyDecision(&input, freshDecision)
				input.Replay = claim.Replay
				result, err := store.Process(context.Background(), input)
				if err != nil || !result.Duplicate {
					t.Fatalf("actual journal redelivery contributed twice: result=%+v err=%v", result, err)
				}
				commits = append(commits, journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token})
			}
			if err := j.MarkCommittedBatch(commits); err != nil {
				t.Fatalf("journal commit after successful duplicate: %v", err)
			}
			assertCount(t, pool, "records_seen", 2)
			assertCount(t, pool, "occurrences", 2)
			assertCount(t, pool, "incident_generations", 1)
			var occurrenceCount int64
			if err := pool.QueryRow(context.Background(), `SELECT occurrence_count FROM incident_generations`).Scan(&occurrenceCount); err != nil || occurrenceCount != 2 {
				t.Fatalf("redelivery changed aggregate contribution: count=%d err=%v", occurrenceCount, err)
			}
		})
	}
}

func TestFinalizedM1DecisionPersistsExactReopenBoundaryAsNewGeneration(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	first := factory.Record(t)
	boundary := first.EventTime.Add(incident.DefaultReopenWindow)
	second := factory.Record(t, builders.WithObservedTime(boundary.Add(builders.DefaultEventLag)), builders.WithEventTime(boundary))
	engine := incident.New()
	for _, record := range []model.NormalizedLog{first, second} {
		if _, err := engine.Observe(record); err != nil {
			t.Fatal(err)
		}
	}
	engine.Advance(boundary.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	keys := map[int64]bool{}
	for _, record := range []model.NormalizedLog{first, second} {
		decision, ok := engine.PersistenceDecision(record.RecordID)
		if !ok {
			t.Fatalf("missing finalized decision for %s", record.RecordID)
		}
		input := preparedInput(t, idSource, record, false)
		applyDecision(&input, decision)
		if _, err := store.Process(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		keys[decision.GenerationKey()] = true
	}
	if len(keys) != 2 {
		t.Fatalf("exact reopen boundary collapsed M1 generations: %v", keys)
	}
	assertCount(t, pool, "incident_generations", 2)
}

func TestInvestigationPolicyCanCreateZeroOrOneActiveInvestigation(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), false)
	if _, err := store.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, "investigations", 0)
	assertCount(t, pool, "outbox_messages", 0)
}

func TestEvidenceOnlyOccurrenceIsRetainedWithoutLifecycleOrAgentMutation(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	base := preparedInput(t, idSource, factory.Record(t), false)
	baseResult, err := store.Process(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	type generationState struct {
		FirstOccurrence, LatestOccurrence, QuietAt, ReopenUntil time.Time
		Severity, DetectionStatus                               string
		OccurrenceCount, ContextVersion                         int64
	}
	readState := func() generationState {
		var state generationState
		if err := pool.QueryRow(context.Background(), `SELECT first_occurrence,latest_occurrence,quiet_at,reopen_until,
			severity,detection_status,occurrence_count,context_version FROM incident_generations
			WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4`, base.Scope.Region,
			base.Scope.TenantID, baseResult.IncidentID, baseResult.Generation).Scan(&state.FirstOccurrence,
			&state.LatestOccurrence, &state.QuietAt, &state.ReopenUntil, &state.Severity,
			&state.DetectionStatus, &state.OccurrenceCount, &state.ContextVersion); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := readState()
	record := factory.Record(t, builders.WithEventTime(base.Record.EventTime.Add(-time.Hour)), builders.SeverityFatal())
	input := preparedInput(t, idSource, record, true)
	engine := incident.New()
	if _, err := engine.Observe(base.Record); err != nil {
		t.Fatal(err)
	}
	engine.Advance(base.Record.EventTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	if _, err := engine.Observe(record); err != nil {
		t.Fatal(err)
	}
	decision, ok := engine.PersistenceDecision(record.RecordID)
	if !ok || !decision.EvidenceOnly() {
		t.Fatalf("M1 did not produce attached evidence-only decision: %+v %v", decision, ok)
	}
	applyDecision(&input, decision)
	// Evidence-only rows change no aggregate, so a stale lifecycle proposal
	// carried alongside them must be ignored rather than applied.
	input.DetectionStatus = "ended"
	input.ContextVersion = 99
	input.Evidence = &EvidenceInput{EvidenceID: mustID(t, idSource), Version: 1, Classification: "SENSITIVE", Provenance: "normalized_log", CreatedAt: input.ProcessedAt, ExpiresAt: input.ProcessedAt.Add(30 * 24 * time.Hour)}
	if !store.validProcessInput(input) {
		t.Fatalf("evidence-only fixture rejected before SQL: decision=%+v input=%+v", decision, input)
	}
	result, err := store.Process(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.InvestigationCreated || result.InvestigationID != "" {
		t.Fatalf("evidence-only launched or altered agent execution: %+v", result)
	}
	after := readState()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("evidence-only mutated finalized generation:\nbefore=%+v\nafter=%+v", before, after)
	}
	assertCount(t, pool, "incident_families", 1)
	assertCount(t, pool, "incident_generations", 1)
	assertCount(t, pool, "occurrences", 2)
	assertCount(t, pool, "evidence", 1)
	assertCount(t, pool, "investigations", 0)
	assertCount(t, pool, "outbox_messages", 0)
}

func TestEvidenceOnlyWithoutExistingFamilyOrGenerationFailsClosed(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), false)
	anchor := factory.Record(t, builders.WithObservedTime(input.Record.ObservedTime.Add(time.Hour)),
		builders.WithEventTime(input.Record.EventTime.Add(time.Hour)))
	engine := incident.New()
	if _, err := engine.Observe(anchor); err != nil {
		t.Fatal(err)
	}
	engine.Advance(anchor.EventTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	if _, err := engine.Observe(input.Record); err != nil {
		t.Fatal(err)
	}
	decision, ok := engine.PersistenceDecision(input.Record.RecordID)
	if !ok || !decision.EvidenceOnly() {
		t.Fatalf("M1 did not produce unattached-store evidence decision: %+v %v", decision, ok)
	}
	applyDecision(&input, decision)
	input.Evidence = &EvidenceInput{EvidenceID: mustID(t, idSource), Version: 1, Classification: "SENSITIVE", Provenance: "normalized_log", CreatedAt: input.ProcessedAt, ExpiresAt: input.ProcessedAt.Add(time.Hour)}
	if _, err := store.Process(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unattached evidence-only record did not fail closed: %v", err)
	}
	for _, table := range []string{"records_seen", "incident_families", "incident_generations", "occurrences", "evidence", "investigations", "outbox_messages"} {
		assertCount(t, pool, table, 0)
	}
}

func TestPersistenceVerifiesM1FingerprintIncidentIdentityAndSourceAccountGrouping(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	for _, mutate := range []func(*ProcessInput){
		func(input *ProcessInput) { input.Fingerprint = strings.Repeat("f", 64) },
		func(input *ProcessInput) { input.FingerprintVersion = "error:v999" },
		func(input *ProcessInput) { input.IncidentID = strings.Repeat("e", 64) },
	} {
		input := preparedInput(t, idSource, factory.Record(t), false)
		mutate(&input)
		if _, err := store.Process(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("forged M1 identity accepted: %v", err)
		}
	}
	assertCount(t, pool, "records_seen", 0)

	first := preparedInput(t, idSource, factory.Record(t), false)
	if _, err := store.Process(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	otherEnvelope := factory.Envelope(t)
	otherEnvelope.SourceAccount = "000000000002"
	secondRecord := factory.Record(t, builders.WithSourceEnvelope(otherEnvelope))
	second := preparedInput(t, idSource, secondRecord, false)
	if first.Fingerprint != second.Fingerprint || first.IncidentID == second.IncidentID {
		t.Fatalf("invalid source-account fixture: first=%+v second=%+v", first, second)
	}
	if _, err := store.Process(context.Background(), second); err != nil {
		t.Fatalf("second source account collapsed or failed: %v", err)
	}
	assertCount(t, pool, "incident_families", 2)
	var accounts int
	if err := pool.QueryRow(context.Background(), `SELECT count(DISTINCT source_account) FROM incident_families`).Scan(&accounts); err != nil || accounts != 2 {
		t.Fatalf("source account not retained in family identity: count=%d err=%v", accounts, err)
	}
}

func TestConcurrentDifferentSourceAccountsCreateDistinctFamilies(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	envelopes := []model.TrustedEnvelope{factory.Envelope(t), factory.Envelope(t)}
	envelopes[1].SourceAccount = "000000000002"
	inputs := make([]ProcessInput, 2)
	for i := range inputs {
		inputs[i] = preparedInput(t, idSource, factory.Record(t, builders.WithSourceEnvelope(envelopes[i])), false)
	}
	if inputs[0].Fingerprint != inputs[1].Fingerprint || inputs[0].IncidentID == inputs[1].IncidentID {
		t.Fatalf("invalid cross-account race fixture: first=%+v second=%+v", inputs[0], inputs[1])
	}
	start := make(chan struct{})
	errs := make(chan error, len(inputs))
	var wait sync.WaitGroup
	for _, input := range inputs {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.Process(context.Background(), input)
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent account process: %v", err)
		}
	}
	assertCount(t, pool, "incident_families", 2)
	var accounts int
	if err := pool.QueryRow(context.Background(), `SELECT count(DISTINCT source_account) FROM incident_families`).Scan(&accounts); err != nil || accounts != 2 {
		t.Fatalf("concurrent accounts collapsed: count=%d err=%v", accounts, err)
	}
}

func TestDuplicateRecordGateUsesM3SemanticReplayEquivalence(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), false)
	if _, err := store.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	retry := input
	retry.Record.BatchID = factory.Record(t).BatchID
	retry.Record.Source.ReceivedAt = retry.Record.Source.ReceivedAt.Add(time.Second)
	if result, err := store.Process(context.Background(), retry); err != nil || !result.Duplicate {
		t.Fatalf("transport-only replay was not idempotent: result=%+v err=%v", result, err)
	}
	changedBody := input
	changedBody.Record.Attributes["customer.segment"] = model.SafeString("materially-different")
	if _, err := store.Process(context.Background(), changedBody); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("same record_id with changed semantic content did not fail closed: %v", err)
	}
	changedSource := input
	changedSource.Record.Source.SourceType = model.SourceTypeCloudWatch
	changedSource.Record.RecordIDVersion = model.RecordIDVersionCloudWatchV1
	if changedSource.Record.Validate() != nil {
		t.Fatalf("changed-source fixture invalid: %v", changedSource.Record.Validate())
	}
	if _, err := store.Process(context.Background(), changedSource); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("same record_id with changed source/version did not fail closed: %v", err)
	}
	changedPriority := input
	setReplayIdentity(t, &changedPriority, journal.PriorityHigh, "SENSITIVE")
	if _, err := store.Process(context.Background(), changedPriority); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("same record_id with changed journal priority did not fail closed: %v", err)
	}
	otherClassificationStore, err := New(pool, Config{Validator: redact.MinimalPolicy(), Scope: input.Scope, Classification: "INTERNAL", Topology: TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	changedClassification := input
	setReplayIdentity(t, &changedClassification, journal.PriorityNormal, "INTERNAL")
	if _, err := otherClassificationStore.Process(context.Background(), changedClassification); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("same record_id with changed durable classification did not fail closed: %v", err)
	}
	var digest []byte
	if err := pool.QueryRow(context.Background(), `SELECT semantic_digest FROM records_seen WHERE record_id=$1`, input.Record.RecordID).Scan(&digest); err != nil || len(digest) != 32 {
		t.Fatalf("durable semantic digest missing: len=%d err=%v", len(digest), err)
	}
	assertCount(t, pool, "records_seen", 1)
	assertCount(t, pool, "occurrences", 1)
}

func TestIncidentEvidenceInvestigationAndOutboxAreAtomicInBothFailureDirections(t *testing.T) {
	for _, point := range []string{"before-outbox", "after-outbox"} {
		t.Run(point, func(t *testing.T) {
			store, pool := integrationStore(t)
			injected := errors.New("unsafe injected password=hunter2")
			if point == "before-outbox" {
				store.hooks.beforeOutbox = func(context.Context, pgx.Tx) error { return injected }
			} else {
				store.hooks.afterOutbox = func(context.Context, pgx.Tx) error { return injected }
			}
			factory := builders.NewFactory()
			idSource := testids.New()
			input := preparedInput(t, idSource, factory.Record(t), true)
			input.Evidence = &EvidenceInput{EvidenceID: mustID(t, idSource), Version: 1, Classification: "SENSITIVE", Provenance: "normalized_log", CreatedAt: input.ProcessedAt, ExpiresAt: input.ProcessedAt.Add(time.Hour)}
			_, err := store.Process(context.Background(), input)
			if !errors.Is(err, ErrUnavailable) || stringsContains(err.Error(), "hunter2") {
				t.Fatalf("want opaque transaction failure, got %v", err)
			}
			for _, table := range []string{"records_seen", "incident_families", "incident_generations", "occurrences", "evidence", "investigations", "investigation_contexts", "outbox_messages"} {
				assertCount(t, pool, table, 0)
			}
		})
	}
}

func TestOrdinaryStoresAreImmutablyBoundAndCannotCrossRegionOrTenantBoundaries(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), false)
	result, err := store.Process(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	otherTenant := Scope{Region: input.Scope.Region, TenantID: "tenant-b"}
	tenantStore, err := New(pool, Config{Validator: redact.MinimalPolicy(), Scope: otherTenant, Classification: "SENSITIVE", Topology: TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	tenantInput := preparedInput(t, idSource, factory.Record(t), false)
	tenantInput.Scope = otherTenant
	setReplayIdentity(t, &tenantInput, journal.PriorityNormal, "SENSITIVE")
	tenantResult, err := tenantStore.Process(context.Background(), tenantInput)
	if err != nil {
		t.Fatalf("seed tenant-b: %v", err)
	}
	tenantSecond := preparedInput(t, idSource, factory.Record(t), false)
	tenantSecond.Scope = otherTenant
	setReplayIdentity(t, &tenantSecond, journal.PriorityNormal, "SENSITIVE")
	if _, err := tenantStore.Process(context.Background(), tenantSecond); err != nil {
		t.Fatalf("seed second tenant-b occurrence: %v", err)
	}

	westEnvelope := factory.Envelope(t)
	westEnvelope.Region = "us-west-2"
	westFactory := builders.NewFactory(builders.WithEnvelope(westEnvelope))
	otherRegion := Scope{Region: westEnvelope.Region, TenantID: input.Scope.TenantID}
	regionStore, err := New(pool, Config{Validator: redact.MinimalPolicy(), Scope: otherRegion, Classification: "SENSITIVE", Topology: TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	regionInput := preparedInput(t, idSource, westFactory.Record(t, builders.WithRecordID(strings.Repeat("f", 64))), false)
	regionInput.Scope = otherRegion
	if !regionStore.validProcessInput(regionInput) {
		generation, generationErr := CanonicalGeneration(regionInput.DeploymentID, regionInput.EpisodeStart)
		t.Fatalf("west input fixture invalid: record=%v final_scan=%v scope=%+v store_scope=%+v generation=%d/%d generation_err=%v",
			regionInput.Record.Validate(), regionStore.validator.ValidateRecord(regionInput.Record), regionInput.Scope,
			regionStore.scope, regionInput.Generation, generation, generationErr)
	}
	regionResult, err := regionStore.Process(context.Background(), regionInput)
	if err != nil {
		t.Fatalf("seed west region: %v", err)
	}

	for _, foreign := range []struct {
		scope      Scope
		incidentID string
		recordID   string
	}{{otherTenant, tenantResult.IncidentID, tenantInput.Record.RecordID}, {otherRegion, regionResult.IncidentID, regionInput.Record.RecordID}} {
		if _, err := store.Incident(context.Background(), foreign.scope, foreign.incidentID); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("caller-selected cross-scope incident read was not rejected: %v", err)
		}
		if _, err := store.OccurrenceExists(context.Background(), foreign.scope, foreign.recordID); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("caller-selected cross-scope occurrence read was not rejected: %v", err)
		}
	}
	boundSnapshot, err := store.Incident(context.Background(), input.Scope, result.IncidentID)
	if err != nil || boundSnapshot.OccurrenceCount != 1 {
		t.Fatalf("tenant-b rows affected tenant-a view: %v %+v", err, boundSnapshot)
	}
	tenantSnapshot, err := tenantStore.Incident(context.Background(), otherTenant, tenantResult.IncidentID)
	if err != nil || tenantSnapshot.OccurrenceCount != 2 {
		t.Fatalf("tenant-b bound view: %v %+v", err, tenantSnapshot)
	}
	// A scope this Store does not serve is a configuration failure, not a
	// malformed record: a caller must never isolate it by destroying payloads.
	forgedScope := tenantInput
	if _, err := store.Process(context.Background(), forgedScope); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("ordinary store accepted another tenant's write: %v", err)
	}
	if _, err := store.Process(context.Background(), forgedScope); errors.Is(err, ErrInvalidInput) {
		t.Fatal("a cross-scope write was reported as record-local invalidity")
	}
	globalDuplicate := input
	globalDuplicate.Scope = otherTenant
	if _, err := tenantStore.Process(context.Background(), globalDuplicate); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("global duplicate from another tenant must fail closed, got %v", err)
	}
	assertCount(t, pool, "occurrences", 4)
}

func TestOccurrenceProjectionPreservesSafeDiagnosticContextAndRefusesForbiddenContent(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	record := factory.Record(t,
		builders.WithBody("payment failed safely"),
		builders.WithEventName("payment.declined"),
		builders.WithAttribute("payment.code", model.SafeString("card_declined")),
		builders.WithException("PaymentDeclined", "safe decline", model.StackFrame{Function: "charge", Module: "payment/handler", File: "handler.go", InApplication: true}),
		builders.WithDeployment("paymentservice-context", "2026.8.7"),
	)
	record.Redaction.RuleIDs = []string{"authorization", "payment-card"}
	record.Redaction.WithheldFields = []string{"attributes.3"}
	input := preparedInput(t, idSource, record, false)
	input.Evidence = &EvidenceInput{EvidenceID: mustID(t, idSource), Version: 1, Classification: "SENSITIVE", Provenance: "normalized_log", CreatedAt: input.ProcessedAt, ExpiresAt: input.ProcessedAt.Add(time.Hour)}
	if _, err := store.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Occurrence(context.Background(), input.Scope, record.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Projection.SchemaVersion != OccurrenceProjectionSchemaVersion ||
		!reflect.DeepEqual(stored.Projection.Exception, record.Exception) ||
		!reflect.DeepEqual(stored.Projection.Attributes, record.Attributes) ||
		!reflect.DeepEqual(stored.Projection.Redaction, record.Redaction) ||
		!reflect.DeepEqual(stored.Projection.Deployment, record.Deployment) ||
		stored.Projection.EventName != record.EventName {
		t.Fatalf("safe diagnostic projection lost context:\nwant=%+v\ngot=%+v", record, stored.Projection)
	}
	var occurrenceJSON, evidenceJSON []byte
	if err := pool.QueryRow(context.Background(), `SELECT safe_summary FROM occurrences WHERE record_id=$1`, record.RecordID).Scan(&occurrenceJSON); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT safe_payload FROM evidence WHERE evidence_id=$1`, input.Evidence.EvidenceID).Scan(&evidenceJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(occurrenceJSON, evidenceJSON) {
		t.Fatal("selected evidence did not retain the exact versioned occurrence projection")
	}

	unsafeRecord := factory.Record(t)
	secret := "password=hunter2"
	unsafeRecord.Exception = &model.NormalizedException{Type: "Unsafe", SafeMessage: secret}
	unsafe := preparedInput(t, idSource, unsafeRecord, false)
	_, err = store.Process(context.Background(), unsafe)
	if !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), secret) {
		t.Fatalf("forbidden projection was not rejected categorically: %v", err)
	}
	assertCount(t, pool, "occurrences", 1)
}

func TestCorruptOrOversizeOccurrenceContextAndOutboxPayloadsFailClosed(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), true)
	created, err := store.Process(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var projection []byte
	if err := pool.QueryRow(context.Background(), `SELECT safe_summary FROM occurrences WHERE record_id=$1`, input.Record.RecordID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(projection, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["severity_class"] = "invented"
	corrupt, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE occurrences SET safe_summary=$2 WHERE record_id=$1`, input.Record.RecordID, corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Occurrence(context.Background(), input.Scope, input.Record.RecordID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("structurally corrupt occurrence escaped: %v", err)
	}
	decoded["severity_class"] = string(input.Record.SeverityClass)
	decoded["redaction"].(map[string]any)["policy_version"] = "other-policy"
	wrongPolicy, _ := json.Marshal(decoded)
	if _, err := pool.Exec(context.Background(), `UPDATE occurrences SET safe_summary=$2 WHERE record_id=$1`, input.Record.RecordID, wrongPolicy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Occurrence(context.Background(), input.Scope, input.Record.RecordID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("occurrence policy mismatch escaped: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE occurrences SET safe_summary=jsonb_build_object('oversize',repeat('x',$2)) WHERE record_id=$1`, input.Record.RecordID, MaxProjectionBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Occurrence(context.Background(), input.Scope, input.Record.RecordID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversize occurrence escaped bounded read: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE investigation_contexts SET policy_version='other-policy'
		WHERE investigation_id=$1`, created.InvestigationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvestigationContext(context.Background(), input.Scope, created.InvestigationID, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("context policy mismatch escaped: %v", err)
	}
	oversize := InvestigationContext{Scope: input.Scope, InvestigationID: created.InvestigationID, Version: 2,
		Snapshot: model.SafeString(strings.Repeat("x", MaxContextBytes+1)), Classification: "SENSITIVE",
		PolicyVersion: redact.MinimalPolicy().Version(), CreatedAt: input.ProcessedAt.Add(time.Second)}
	if err := store.AppendInvestigationContext(context.Background(), oversize); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversize context accepted: %v", err)
	}
	unsafeOutbox := preparedInput(t, idSource, factory.Record(t), true)
	unsafeOutbox.Investigation.Outbox.Attributes["route"] = "password=hunter2"
	if _, err := store.Process(context.Background(), unsafeOutbox); !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("prohibited outbox routing was not rejected categorically: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE outbox_messages SET message_type='password=hunter2'`); err != nil {
		t.Fatal(err)
	}
	claims, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher", []string{mustID(t, idSource)}, input.ProcessedAt.Add(time.Second), time.Minute)
	if !errors.Is(err, ErrUnavailable) || len(claims) != 0 || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("corrupt outbox routing did not fail closed categorically: claims=%+v err=%v", claims, err)
	}
}

func TestClaimOutboxRejectsEveryCorruptAssignmentCatalogueFieldWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		set  string
		arg  func(*testing.T, *testids.Source) any
	}{
		{name: "message type", set: "message_type=$1", arg: func(*testing.T, *testids.Source) any { return "agent.other.v1" }},
		{name: "aggregate type", set: "aggregate_type=$1", arg: func(*testing.T, *testids.Source) any { return "incident" }},
		{name: "aggregate id", set: "aggregate_id=$1", arg: func(t *testing.T, ids *testids.Source) any { return mustID(t, ids) }},
		{name: "payload version", set: "payload_version=$1", arg: func(*testing.T, *testids.Source) any { return "2.0" }},
		{name: "deduplication key", set: "deduplication_key=$1", arg: func(t *testing.T, ids *testids.Source) any { return "assignment:" + mustID(t, ids) }},
		{name: "payload schema", set: "payload=$1", arg: func(*testing.T, *testids.Source) any { return []byte(`{"schema_version":"2.0"}`) }},
		{name: "content digest", set: "content_digest=$1", arg: func(*testing.T, *testids.Source) any { return bytes.Repeat([]byte{0x5a}, 32) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			factory := builders.NewFactory()
			idSource := testids.New()
			input := preparedInput(t, idSource, factory.Record(t), true)
			if _, err := store.Process(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), "UPDATE outbox_messages SET "+test.set, test.arg(t, idSource)); err != nil {
				t.Fatalf("corrupt fixture: %v", err)
			}
			claims, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher",
				[]string{mustID(t, idSource)}, input.ProcessedAt.Add(time.Second), time.Minute)
			if !errors.Is(err, ErrUnavailable) || len(claims) != 0 {
				t.Fatalf("corrupt row was claimable: claims=%+v err=%v", claims, err)
			}
			var state string
			var attempts int64
			var owner, token *string
			var expires *time.Time
			if err := pool.QueryRow(context.Background(), `SELECT state,attempts,claim_owner,claim_token,claim_expires_at FROM outbox_messages`).
				Scan(&state, &attempts, &owner, &token, &expires); err != nil {
				t.Fatal(err)
			}
			if state != "pending" || attempts != 0 || owner != nil || token != nil || expires != nil {
				t.Fatalf("failed claim mutated corrupt row: state=%s attempts=%d owner=%v token=%v expires=%v", state, attempts, owner, token, expires)
			}
		})
	}
}

func TestProcessCrossChecksEveryAssignmentPayloadFieldBeforeWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *testids.Source, *ProcessInput, agent.Assignment) []byte
	}{
		{name: "schema version", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.SchemaVersion = "1.7"
			return mustAssignment(t, value)
		}},
		{name: "message id", mutate: func(t *testing.T, ids *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.MessageID = mustID(t, ids)
			return mustAssignment(t, value)
		}},
		{name: "message type", mutate: func(_ *testing.T, _ *testids.Source, input *ProcessInput, _ agent.Assignment) []byte {
			return bytes.Replace(input.Investigation.Outbox.Body, []byte(agent.AssignmentMessageType), []byte("agent.other.v1"), 1)
		}},
		{name: "created at", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.CreatedAt = value.CreatedAt.Add(time.Second)
			return mustAssignment(t, value)
		}},
		{name: "region", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.Region = "us-west-2"
			return mustAssignment(t, value)
		}},
		{name: "tenant", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.TenantID = "tenant-b"
			return mustAssignment(t, value)
		}},
		{name: "classification", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.Classification = "INTERNAL"
			return mustAssignment(t, value)
		}},
		{name: "producer", mutate: func(_ *testing.T, _ *testids.Source, input *ProcessInput, _ agent.Assignment) []byte {
			return bytes.Replace(input.Investigation.Outbox.Body, []byte(agent.AssignmentProducer), []byte("other-producer"), 1)
		}},
		{name: "correlation id", mutate: func(t *testing.T, ids *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.CorrelationID = mustID(t, ids)
			return mustAssignment(t, value)
		}},
		{name: "incident id", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.IncidentID = strings.Repeat("b", 64)
			return mustAssignment(t, value)
		}},
		{name: "generation", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.IncidentGeneration++
			return mustAssignment(t, value)
		}},
		{name: "investigation id", mutate: func(t *testing.T, ids *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.InvestigationID = mustID(t, ids)
			return mustAssignment(t, value)
		}},
		{name: "service", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.ServiceID = "otherservice"
			return mustAssignment(t, value)
		}},
		{name: "environment", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.Environment = "staging"
			return mustAssignment(t, value)
		}},
		{name: "severity", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.Severity = "fatal"
			return mustAssignment(t, value)
		}},
		{name: "context version", mutate: func(t *testing.T, _ *testids.Source, _ *ProcessInput, value agent.Assignment) []byte {
			value.ContextVersion++
			return mustAssignment(t, value)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			idSource := testids.New()
			input := preparedInput(t, idSource, builders.NewFactory().Record(t), true)
			payload, err := agent.DecodeAssignment(input.Investigation.Outbox.Body)
			if err != nil {
				t.Fatal(err)
			}
			input.Investigation.Outbox.Body = test.mutate(t, idSource, &input, payload)
			if _, err := store.Process(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("mismatched assignment payload accepted: %v", err)
			}
			assertCount(t, pool, "records_seen", 0)
			assertCount(t, pool, "outbox_messages", 0)
		})
	}
}

func TestProcessRejectsNonCanonicalAssignmentRoutingAttributesBeforePersistence(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]string
	}{
		{name: "missing", attributes: map[string]string{}},
		{name: "region mismatch", attributes: map[string]string{"region": "us-west-2"}},
		{name: "extra", attributes: map[string]string{"region": builders.DefaultRegion, "queue": "priority"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			input := preparedInput(t, testids.New(), builders.NewFactory().Record(t), true)
			input.Investigation.Outbox.Attributes = test.attributes
			if _, err := store.Process(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("non-canonical assignment routing persisted: %v", err)
			}
			assertCount(t, pool, "records_seen", 0)
			assertCount(t, pool, "outbox_messages", 0)
		})
	}
}

func TestOutboxContentDigestRejectsSemanticallyValidStoredMutationBeforeClaim(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Store, *pgxpool.Pool, *testids.Source, *builders.Factory, ProcessInput, agent.Assignment)
	}{
		{name: "severity", mutate: func(t *testing.T, _ *Store, pool *pgxpool.Pool, _ *testids.Source, _ *builders.Factory, _ ProcessInput, payload agent.Assignment) {
			payload.Severity = "fatal"
			if _, err := pool.Exec(context.Background(), `UPDATE outbox_messages SET payload=$1`, mustAssignment(t, payload)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "another existing generation", mutate: func(t *testing.T, store *Store, pool *pgxpool.Pool, ids *testids.Source, factory *builders.Factory, initial ProcessInput, payload agent.Assignment) {
			record := factory.Record(t, builders.WithDeployment("paymentservice-digest-generation", "2026.8.8"),
				builders.WithEventTime(initial.Record.EventTime.Add(time.Hour)), builders.WithObservedTime(initial.Record.ObservedTime.Add(time.Hour)))
			other := preparedInput(t, ids, record, false)
			if other.IncidentID != initial.IncidentID || other.Generation == initial.Generation {
				t.Fatalf("invalid second-generation fixture: initial=%+v other=%+v", initial, other)
			}
			if _, err := store.Process(context.Background(), other); err != nil {
				t.Fatal(err)
			}
			payload.IncidentGeneration = other.Generation
			if _, err := pool.Exec(context.Background(), `UPDATE outbox_messages SET payload=$1`, mustAssignment(t, payload)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "another existing context", mutate: func(t *testing.T, store *Store, pool *pgxpool.Pool, _ *testids.Source, _ *builders.Factory, initial ProcessInput, payload agent.Assignment) {
			contextTwo := InvestigationContext{Scope: initial.Scope, InvestigationID: initial.Investigation.InvestigationID,
				Version: 2, Snapshot: model.SafeString("context-two"), Classification: "SENSITIVE",
				PolicyVersion: redact.MinimalPolicy().Version(), CreatedAt: initial.ProcessedAt.Add(time.Second)}
			if err := store.AppendInvestigationContext(context.Background(), contextTwo); err != nil {
				t.Fatal(err)
			}
			payload.ContextVersion = 2
			if _, err := pool.Exec(context.Background(), `UPDATE outbox_messages SET payload=$1`, mustAssignment(t, payload)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "safe routing attribute", mutate: func(t *testing.T, _ *Store, pool *pgxpool.Pool, _ *testids.Source, _ *builders.Factory, _ ProcessInput, _ agent.Assignment) {
			if _, err := pool.Exec(context.Background(), `UPDATE outbox_messages SET attributes='{"region":"us-west-2"}'::JSONB`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			idSource := testids.New()
			factory := builders.NewFactory()
			input := preparedInput(t, idSource, factory.Record(t), true)
			if _, err := store.Process(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			payload, err := agent.DecodeAssignment(input.Investigation.Outbox.Body)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, store, pool, idSource, factory, input, payload)
			claims, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher",
				[]string{mustID(t, idSource)}, input.ProcessedAt.Add(2*time.Hour), time.Minute)
			if !errors.Is(err, ErrUnavailable) || len(claims) != 0 {
				t.Fatalf("digest-mismatched row was claimable: claims=%+v err=%v", claims, err)
			}
			var state string
			var attempts int64
			var owner, token *string
			var expires *time.Time
			if err := pool.QueryRow(context.Background(), `SELECT state,attempts,claim_owner,claim_token,claim_expires_at FROM outbox_messages`).
				Scan(&state, &attempts, &owner, &token, &expires); err != nil {
				t.Fatal(err)
			}
			if state != "pending" || attempts != 0 || owner != nil || token != nil || expires != nil {
				t.Fatalf("digest failure mutated row: state=%s attempts=%d owner=%v token=%v expires=%v", state, attempts, owner, token, expires)
			}
		})
	}
}

func TestClaimOutboxRejectsSafeLookingAssignmentPayloadCorruptionWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *testids.Source, agent.Assignment, []byte) []byte
	}{
		{name: "schema version", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.SchemaVersion = "1.7"
			return mustAssignment(t, value)
		}},
		{name: "message id", mutate: func(t *testing.T, ids *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.MessageID = mustID(t, ids)
			return mustAssignment(t, value)
		}},
		{name: "message type", mutate: func(_ *testing.T, _ *testids.Source, _ agent.Assignment, body []byte) []byte {
			return bytes.Replace(body, []byte(agent.AssignmentMessageType), []byte("agent.other.v1"), 1)
		}},
		{name: "created at", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.CreatedAt = value.CreatedAt.Add(time.Second)
			return mustAssignment(t, value)
		}},
		{name: "region", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.Region = "us-west-2"
			return mustAssignment(t, value)
		}},
		{name: "tenant", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.TenantID = "tenant-b"
			return mustAssignment(t, value)
		}},
		{name: "classification", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.Classification = "INTERNAL"
			return mustAssignment(t, value)
		}},
		{name: "producer", mutate: func(_ *testing.T, _ *testids.Source, _ agent.Assignment, body []byte) []byte {
			return bytes.Replace(body, []byte(agent.AssignmentProducer), []byte("other-producer"), 1)
		}},
		{name: "correlation id", mutate: func(t *testing.T, ids *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.CorrelationID = mustID(t, ids)
			return mustAssignment(t, value)
		}},
		{name: "incident id", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.IncidentID = strings.Repeat("b", 64)
			return mustAssignment(t, value)
		}},
		{name: "generation", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.IncidentGeneration++
			return mustAssignment(t, value)
		}},
		{name: "investigation id", mutate: func(t *testing.T, ids *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.InvestigationID = mustID(t, ids)
			return mustAssignment(t, value)
		}},
		{name: "service", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.ServiceID = "otherservice"
			return mustAssignment(t, value)
		}},
		{name: "environment", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.Environment = "staging"
			return mustAssignment(t, value)
		}},
		{name: "context version", mutate: func(t *testing.T, _ *testids.Source, value agent.Assignment, _ []byte) []byte {
			value.ContextVersion++
			return mustAssignment(t, value)
		}},
		{name: "unknown field", mutate: func(_ *testing.T, _ *testids.Source, _ agent.Assignment, body []byte) []byte {
			return bytes.Replace(body, []byte("}"), []byte(`,"future":true}`), 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			idSource := testids.New()
			input := preparedInput(t, idSource, builders.NewFactory().Record(t), true)
			if _, err := store.Process(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			payload, err := agent.DecodeAssignment(input.Investigation.Outbox.Body)
			if err != nil {
				t.Fatal(err)
			}
			corrupt := test.mutate(t, idSource, payload, input.Investigation.Outbox.Body)
			if _, err := pool.Exec(context.Background(), `UPDATE outbox_messages SET payload=$1`, corrupt); err != nil {
				t.Fatal(err)
			}
			claims, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher",
				[]string{mustID(t, idSource)}, input.ProcessedAt.Add(time.Second), time.Minute)
			if !errors.Is(err, ErrUnavailable) || len(claims) != 0 {
				t.Fatalf("corrupt payload was claimable: claims=%+v err=%v", claims, err)
			}
			var state string
			var attempts int64
			var owner, token *string
			var expires *time.Time
			if err := pool.QueryRow(context.Background(), `SELECT state,attempts,claim_owner,claim_token,claim_expires_at FROM outbox_messages`).
				Scan(&state, &attempts, &owner, &token, &expires); err != nil {
				t.Fatal(err)
			}
			if state != "pending" || attempts != 0 || owner != nil || token != nil || expires != nil {
				t.Fatalf("failed payload claim mutated row: state=%s attempts=%d owner=%v token=%v expires=%v", state, attempts, owner, token, expires)
			}
		})
	}
}

func TestInvestigationLeaseExpiryAndFencingPreventExpiredOwnerOverwrite(t *testing.T) {
	store, _ := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), true)
	result, err := store.Process(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	now := input.ProcessedAt
	first, err := store.AcquireInvestigation(context.Background(), input.Scope, result.InvestigationID, "worker-a", mustID(t, idSource), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AcquireInvestigation(context.Background(), input.Scope, result.InvestigationID, "worker-b", mustID(t, idSource), now.Add(time.Minute), time.Minute)
	if err != nil || second.RenewalSequence <= first.RenewalSequence {
		t.Fatalf("exact-expiry successor: %v first=%+v second=%+v", err, first, second)
	}
	if err := store.CompleteInvestigation(context.Background(), first, now.Add(time.Minute+time.Second)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired predecessor completed successor's work: %v", err)
	}
	renewed, err := store.RenewInvestigation(context.Background(), second, now.Add(time.Minute+time.Second), time.Minute)
	if err != nil || renewed.RenewalSequence <= second.RenewalSequence {
		t.Fatalf("renew successor: %v %+v", err, renewed)
	}
	if err := store.CompleteInvestigation(context.Background(), renewed, now.Add(time.Minute+2*time.Second)); err != nil {
		t.Fatalf("complete successor: %v", err)
	}
}

func TestConcurrentInvestigationClaimersHaveExactlyOneOwnerAtUnownedAndExpiryBoundaries(t *testing.T) {
	store, _ := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), true)
	created, err := store.Process(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	now := input.ProcessedAt.Add(time.Second)
	race := func(at time.Time, owners ...string) []Lease {
		start := make(chan struct{})
		leases := make(chan Lease, len(owners))
		errs := make(chan error, len(owners))
		var wait sync.WaitGroup
		for _, owner := range owners {
			owner := owner
			token := mustID(t, idSource)
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				lease, err := store.AcquireInvestigation(context.Background(), input.Scope,
					created.InvestigationID, owner, token, at, time.Minute)
				if err != nil {
					errs <- err
					return
				}
				leases <- lease
			}()
		}
		close(start)
		wait.Wait()
		close(leases)
		close(errs)
		var winners []Lease
		for lease := range leases {
			winners = append(winners, lease)
		}
		stale := 0
		for err := range errs {
			if !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("unexpected lease race error: %v", err)
			}
			stale++
		}
		if len(winners) != 1 || stale != len(owners)-1 {
			t.Fatalf("want exactly one lease owner: winners=%+v stale=%d", winners, stale)
		}
		return winners
	}
	first := race(now, "worker-a", "worker-b")[0]
	second := race(first.ExpiresAt, "worker-c", "worker-d")[0]
	if second.Token == first.Token || second.RenewalSequence <= first.RenewalSequence {
		t.Fatalf("expiry successor did not advance fencing: first=%+v second=%+v", first, second)
	}
	if err := store.CompleteInvestigation(context.Background(), first, first.ExpiresAt.Add(time.Nanosecond)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired predecessor overwrote successor: %v", err)
	}
}

func TestOutboxClaimExpiryFencingAndRepublishReuseMessageIdentity(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), true)
	if _, err := store.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	now := input.ProcessedAt
	first, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher-a", []string{mustID(t, idSource)}, now, time.Minute)
	if err != nil || len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("first claim: %v %+v", err, first)
	}
	if err := store.RetryOutbox(context.Background(), first[0], OutboxFailureLostAck, now.Add(time.Second), now.Add(30*time.Second)); err != nil {
		t.Fatalf("safe retry transition: %v", err)
	}
	if err := store.MarkOutboxPublished(context.Background(), first[0], now.Add(2*time.Second)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released publisher retained fencing authority: %v", err)
	}
	second, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher-b", []string{mustID(t, idSource)}, now.Add(30*time.Second), time.Minute)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("pending retry claim: %v %+v", err, second)
	}
	if !reflect.DeepEqual(first[0].Message, second[0].Message) {
		t.Fatalf("republish changed message identity/payload:\nfirst=%+v\nsecond=%+v", first[0].Message, second[0].Message)
	}
	third, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher-c", []string{mustID(t, idSource)}, now.Add(90*time.Second), time.Minute)
	if err != nil || len(third) != 1 || third[0].Attempt != 3 {
		t.Fatalf("exact-expiry republish claim: %v %+v", err, third)
	}
	if !reflect.DeepEqual(second[0].Message, third[0].Message) {
		t.Fatalf("expiry republish changed identity/payload")
	}
	if err := store.MarkOutboxPublished(context.Background(), second[0], now.Add(91*time.Second)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired publisher overwrote successor: %v", err)
	}
	if err := store.MarkOutboxPublished(context.Background(), third[0], now.Add(91*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertCountWhere(t, pool, "outbox_messages", "state='published'", 1)
	claimed, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher-d", []string{mustID(t, idSource)}, now.Add(3*time.Minute), time.Minute)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("published row reclaimed: %v %+v", err, claimed)
	}
}

func TestConcurrentOutboxClaimersHaveDisjointOwnershipAndFenceExpiredPublisher(t *testing.T) {
	store, _ := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), true)
	if _, err := store.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	now := input.ProcessedAt.Add(time.Second)
	start := make(chan struct{})
	claims := make(chan []OutboxClaim, 2)
	var wait sync.WaitGroup
	for _, owner := range []string{"publisher-a", "publisher-b"} {
		owner := owner
		token := mustID(t, idSource)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claimed, err := store.ClaimOutbox(context.Background(), input.Scope, owner,
				[]string{token}, now, time.Minute)
			if err != nil {
				t.Errorf("claim race: %v", err)
				return
			}
			claims <- claimed
		}()
	}
	close(start)
	wait.Wait()
	close(claims)
	var winner OutboxClaim
	total := 0
	for claimed := range claims {
		total += len(claimed)
		if len(claimed) == 1 {
			winner = claimed[0]
		}
	}
	if total != 1 {
		t.Fatalf("outbox race produced %d owners", total)
	}
	successor, err := store.ClaimOutbox(context.Background(), input.Scope, "publisher-c",
		[]string{mustID(t, idSource)}, winner.ExpiresAt, time.Minute)
	if err != nil || len(successor) != 1 {
		t.Fatalf("exact-expiry reclaim: %+v err=%v", successor, err)
	}
	if successor[0].Message.MessageID != winner.Message.MessageID ||
		successor[0].Message.DeduplicationKey != winner.Message.DeduplicationKey ||
		!reflect.DeepEqual(successor[0].Message.Body, winner.Message.Body) {
		t.Fatalf("reclaim changed stable republish identity: before=%+v after=%+v", winner, successor[0])
	}
	if err := store.MarkOutboxPublished(context.Background(), winner, winner.ExpiresAt.Add(time.Nanosecond)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired publisher overwrote successor: %v", err)
	}
}

func TestContextVersionsAreImmutableAndActiveInvestigationGetsNewSnapshotWithoutNewOutbox(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	first := preparedInput(t, idSource, factory.Record(t), true)
	created, err := store.Process(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.InvestigationContext(context.Background(), first.Scope, created.InvestigationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if original.CreatedAt.Location() != time.UTC {
		t.Fatalf("database timestamp did not normalize to UTC: %v", original.CreatedAt.Location())
	}
	secondRecord := factory.Record(t,
		builders.WithObservedTime(first.Record.ObservedTime.Add(time.Minute)),
		builders.WithEventTime(first.Record.EventTime.Add(time.Minute)),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	)
	second := preparedInput(t, idSource, secondRecord, false)
	second.ContextVersion = 2
	second.ContextUpdate = &ContextInput{Version: 2, Snapshot: model.SafeMap(map[string]model.SafeValue{"generation": model.SafeInt(2)}), Classification: "SENSITIVE", CreatedAt: second.ProcessedAt}
	updated, err := store.Process(context.Background(), second)
	if err != nil || updated.InvestigationID != created.InvestigationID || updated.InvestigationCreated {
		t.Fatalf("context update created/replaced investigation: %v %+v", err, updated)
	}
	if _, err := store.InvestigationContext(context.Background(), first.Scope, created.InvestigationID, 2); err != nil {
		t.Fatalf("new immutable context missing: %v", err)
	}
	again, err := store.InvestigationContext(context.Background(), first.Scope, created.InvestigationID, 1)
	if err != nil || !reflect.DeepEqual(original.Snapshot, again.Snapshot) {
		t.Fatalf("version 1 changed: %v before=%+v after=%+v", err, original, again)
	}
	immutable := original
	immutable.Snapshot = model.SafeString("replacement")
	if err := store.AppendInvestigationContext(context.Background(), immutable); !errors.Is(err, ErrImmutable) {
		t.Fatalf("existing version overwritten: %v", err)
	}
	assertCount(t, pool, "investigations", 1)
	assertCount(t, pool, "outbox_messages", 1)
}

func TestInvestigationContextReplayConflictAndOlderVersionAreFenced(t *testing.T) {
	store, _ := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	input := preparedInput(t, idSource, factory.Record(t), true)
	created, err := store.Process(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	versionThree := InvestigationContext{Scope: input.Scope, InvestigationID: created.InvestigationID, Version: 3,
		Snapshot: model.SafeString("three"), Classification: "SENSITIVE", PolicyVersion: redact.MinimalPolicy().Version(), CreatedAt: input.ProcessedAt.Add(3 * time.Second)}
	if err := store.AppendInvestigationContext(context.Background(), versionThree); err != nil {
		t.Fatalf("append newer context: %v", err)
	}
	olderMissing := versionThree
	olderMissing.Version = 2
	olderMissing.Snapshot = model.SafeString("late-two")
	olderMissing.CreatedAt = input.ProcessedAt.Add(2 * time.Second)
	if err := store.AppendInvestigationContext(context.Background(), olderMissing); !errors.Is(err, ErrImmutable) {
		t.Fatalf("missing older context version appended after current: %v", err)
	}

	createdAt := input.ProcessedAt.Add(4 * time.Second)
	values := []InvestigationContext{
		{Scope: input.Scope, InvestigationID: created.InvestigationID, Version: 4, Snapshot: model.SafeString("winner-a"), Classification: "SENSITIVE", PolicyVersion: redact.MinimalPolicy().Version(), CreatedAt: createdAt},
		{Scope: input.Scope, InvestigationID: created.InvestigationID, Version: 4, Snapshot: model.SafeString("winner-b"), Classification: "SENSITIVE", PolicyVersion: redact.MinimalPolicy().Version(), CreatedAt: createdAt},
	}
	errs := make(chan error, len(values))
	var wait sync.WaitGroup
	for _, value := range values {
		value := value
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- store.AppendInvestigationContext(context.Background(), value)
		}()
	}
	wait.Wait()
	close(errs)
	succeeded, immutable := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrImmutable):
			immutable++
		default:
			t.Fatalf("unexpected concurrent context result: %v", err)
		}
	}
	if succeeded != 1 || immutable != 1 {
		t.Fatalf("want one immutable winner and one conflict, success=%d immutable=%d", succeeded, immutable)
	}
	winner, err := store.InvestigationContext(context.Background(), input.Scope, created.InvestigationID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInvestigationContext(context.Background(), winner); err != nil {
		t.Fatalf("byte-equivalent immutable replay was not idempotent: %v", err)
	}
	loser := values[0]
	if reflect.DeepEqual(loser.Snapshot, winner.Snapshot) {
		loser = values[1]
	}
	if err := store.AppendInvestigationContext(context.Background(), loser); !errors.Is(err, ErrImmutable) {
		t.Fatalf("conflicting same-version replay was not immutable: %v", err)
	}
}

func TestProcessContextReplayRequiresExactImmutableRowAndRollsBackConflicts(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	initial := preparedInput(t, idSource, factory.Record(t), true)
	created, err := store.Process(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.InvestigationContext(context.Background(), initial.Scope, created.InvestigationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	newContextInput := func(version int64, snapshot model.SafeValue, createdAt time.Time) ProcessInput {
		record := factory.Record(t)
		input := preparedInput(t, idSource, record, false)
		input.ContextVersion = version
		input.ContextUpdate = &ContextInput{Version: version, Snapshot: snapshot, Classification: "SENSITIVE", CreatedAt: createdAt}
		return input
	}

	exact := newContextInput(1, original.Snapshot, original.CreatedAt)
	if result, err := store.Process(context.Background(), exact); err != nil || result.InvestigationID != created.InvestigationID {
		t.Fatalf("exact Process replay failed: result=%+v err=%v", result, err)
	}
	assertCount(t, pool, "occurrences", 2)

	conflict := newContextInput(1, model.SafeString("conflict"), original.CreatedAt)
	if _, err := store.Process(context.Background(), conflict); !errors.Is(err, ErrImmutable) {
		t.Fatalf("same-version Process conflict accepted: %v", err)
	}
	assertCountWhere(t, pool, "records_seen", "record_id='"+conflict.Record.RecordID+"'", 0)
	assertCountWhere(t, pool, "occurrences", "record_id='"+conflict.Record.RecordID+"'", 0)

	versionThree := InvestigationContext{Scope: initial.Scope, InvestigationID: created.InvestigationID, Version: 3,
		Snapshot: model.SafeString("three"), Classification: "SENSITIVE", PolicyVersion: redact.MinimalPolicy().Version(), CreatedAt: initial.ProcessedAt.Add(3 * time.Second)}
	if err := store.AppendInvestigationContext(context.Background(), versionThree); err != nil {
		t.Fatal(err)
	}
	missing := newContextInput(2, model.SafeString("missing-two"), initial.ProcessedAt.Add(2*time.Second))
	if _, err := store.Process(context.Background(), missing); !errors.Is(err, ErrImmutable) {
		t.Fatalf("missing older Process context accepted: %v", err)
	}
	assertCountWhere(t, pool, "records_seen", "record_id='"+missing.Record.RecordID+"'", 0)

	createdAt := initial.ProcessedAt.Add(4 * time.Second)
	concurrent := []ProcessInput{
		newContextInput(4, model.SafeString("winner-a"), createdAt),
		newContextInput(4, model.SafeString("winner-b"), createdAt),
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, input := range concurrent {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.Process(context.Background(), input)
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	successes, immutable := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrImmutable):
			immutable++
		default:
			t.Fatalf("unexpected concurrent Process result: %v", err)
		}
	}
	if successes != 1 || immutable != 1 {
		t.Fatalf("want one Process context winner/conflict: successes=%d immutable=%d", successes, immutable)
	}
	assertCount(t, pool, "occurrences", 3)
}

func TestProcessContextUpdateRejectsEveryImmutableFieldConflictAndRollsBack(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*ContextInput)
		corruptStored bool
	}{
		{name: "snapshot", mutate: func(update *ContextInput) { update.Snapshot = model.SafeString("different") }},
		{name: "classification", mutate: func(update *ContextInput) { update.Classification = "INTERNAL" }},
		{name: "created at", mutate: func(update *ContextInput) { update.CreatedAt = update.CreatedAt.Add(time.Second) }},
		{name: "persisted policy version", corruptStored: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			factory := builders.NewFactory()
			idSource := testids.New()
			initial := preparedInput(t, idSource, factory.Record(t), true)
			created, err := store.Process(context.Background(), initial)
			if err != nil {
				t.Fatal(err)
			}
			original, err := store.InvestigationContext(context.Background(), initial.Scope, created.InvestigationID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if test.corruptStored {
				if _, err := pool.Exec(context.Background(), `UPDATE investigation_contexts SET policy_version='other-policy'
					WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 AND version=1`, initial.Scope.Region,
					initial.Scope.TenantID, created.InvestigationID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Incident(context.Background(), initial.Scope, initial.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			input := preparedInput(t, idSource, factory.Record(t), false)
			input.ContextVersion = 1
			input.ContextUpdate = &ContextInput{Version: 1, Snapshot: original.Snapshot,
				Classification: original.Classification, CreatedAt: original.CreatedAt}
			if test.mutate != nil {
				test.mutate(input.ContextUpdate)
			}
			if _, err := store.Process(context.Background(), input); !errors.Is(err, ErrImmutable) {
				t.Fatalf("immutable %s conflict accepted: %v", test.name, err)
			}
			assertCountWhere(t, pool, "records_seen", "record_id='"+input.Record.RecordID+"'", 0)
			assertCountWhere(t, pool, "occurrences", "record_id='"+input.Record.RecordID+"'", 0)
			after, err := store.Incident(context.Background(), initial.Scope, initial.IncidentID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("immutable conflict changed aggregate: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestExistingActiveInvestigationBranchExactReplayAndConcurrentConflict(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	initial := preparedInput(t, idSource, factory.Record(t), true)
	created, err := store.Process(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.InvestigationContext(context.Background(), initial.Scope, created.InvestigationID, 1)
	if err != nil {
		t.Fatal(err)
	}

	exact := preparedInput(t, idSource, factory.Record(t), true)
	exact.ProcessedAt = original.CreatedAt
	exact.Investigation.Context = original.Snapshot
	exact.Investigation.Classification = original.Classification
	setAssignmentBody(t, &exact)
	exactResult, err := store.Process(context.Background(), exact)
	if err != nil || exactResult.InvestigationID != created.InvestigationID || exactResult.InvestigationCreated {
		t.Fatalf("existing-active exact Investigation replay: result=%+v err=%v", exactResult, err)
	}
	assertCount(t, pool, "investigations", 1)
	assertCount(t, pool, "outbox_messages", 1)

	createdAt := initial.ProcessedAt.Add(time.Minute)
	proposals := make([]ProcessInput, 2)
	for i := range proposals {
		proposals[i] = preparedInput(t, idSource, factory.Record(t), true)
		proposals[i].ProcessedAt = createdAt
		proposals[i].ContextVersion = 2
		proposals[i].Investigation.ContextVersion = 2
		proposals[i].Investigation.Context = model.SafeString(fmt.Sprintf("proposal-%d", i))
		setAssignmentBody(t, &proposals[i])
	}
	type outcome struct {
		input  ProcessInput
		result ProcessResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, input := range proposals {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := store.Process(context.Background(), input)
			outcomes <- outcome{input: input, result: result, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	successes, immutable := 0, 0
	for outcome := range outcomes {
		switch {
		case outcome.err == nil:
			successes++
			if outcome.result.InvestigationID != created.InvestigationID || outcome.result.InvestigationCreated {
				t.Fatalf("winner replaced active investigation: %+v", outcome.result)
			}
		case errors.Is(outcome.err, ErrImmutable):
			immutable++
			assertCountWhere(t, pool, "records_seen", "record_id='"+outcome.input.Record.RecordID+"'", 0)
			assertCountWhere(t, pool, "occurrences", "record_id='"+outcome.input.Record.RecordID+"'", 0)
		default:
			t.Fatalf("unexpected existing-active proposal result: %v", outcome.err)
		}
	}
	if successes != 1 || immutable != 1 {
		t.Fatalf("want one active-context winner and one immutable loser: success=%d immutable=%d", successes, immutable)
	}
	snapshot, err := store.Incident(context.Background(), initial.Scope, initial.IncidentID)
	if err != nil || snapshot.OccurrenceCount != 3 || snapshot.ContextVersion != 2 {
		t.Fatalf("loser changed aggregate: snapshot=%+v err=%v", snapshot, err)
	}
	assertCount(t, pool, "investigations", 1)
	assertCount(t, pool, "outbox_messages", 1)
	assertCount(t, pool, "investigation_contexts", 2)
}

func TestGenerationHashCollisionIsTerminalAndRollsBackBeforeContribution(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	base := preparedInput(t, idSource, factory.Record(t), false)
	if _, err := store.Process(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	targetRecord := factory.Record(t, builders.WithDeployment("paymentservice-collision-target", "2026.8.7"),
		builders.WithEventTime(base.Record.EventTime.Add(time.Hour)), builders.WithObservedTime(base.Record.ObservedTime.Add(time.Hour)))
	target := preparedInput(t, idSource, targetRecord, false)
	if target.IncidentID != base.IncidentID || target.Generation == base.Generation {
		t.Fatalf("invalid collision fixture: base=%+v target=%+v", base, target)
	}
	seedEpisode := target.EpisodeStart.Add(-time.Second)
	if _, err := pool.Exec(context.Background(), `INSERT INTO incident_generations
		(region,tenant_id,incident_id,generation,deployment_id,episode_start,first_occurrence,latest_occurrence,
		 occurrence_count,severity,detection_status,quiet_at,reopen_until,rule_trigger,context_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,0,$8,'active',$9,$10,'ordinary_error_v1',0)`, target.Scope.Region,
		target.Scope.TenantID, target.IncidentID, target.Generation, "different-deployment", seedEpisode,
		target.Record.EventTime, target.Severity, target.QuietAt, target.ReopenUntil); err != nil {
		t.Fatalf("seed occupied generation key: %v", err)
	}
	var attempts atomic.Int32
	store.hooks.onAttempt = func(_ int, _ []ProcessInput) { attempts.Add(1) }
	for call := int32(1); call <= 2; call++ {
		if _, err := store.Process(context.Background(), target); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("collision call %d: want identity conflict, got %v", call, err)
		}
		if attempts.Load() != call {
			t.Fatalf("collision retried internally: calls=%d attempts=%d", call, attempts.Load())
		}
		assertCountWhere(t, pool, "records_seen", "record_id='"+target.Record.RecordID+"'", 0)
		assertCountWhere(t, pool, "occurrences", "record_id='"+target.Record.RecordID+"'", 0)
	}
	snapshot, err := store.Incident(context.Background(), base.Scope, base.IncidentID)
	if err != nil || snapshot.OccurrenceCount != 1 {
		t.Fatalf("collision changed aggregate: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRetryCallbackReusesPreparedValuesUnderRealSerializationFailure(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	initial := preparedInput(t, idSource, factory.Record(t), false)
	canonical, err := store.Process(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	next := preparedInput(t, idSource, factory.Record(t), false)
	want := next
	var attempts atomic.Int32
	store.hooks.onAttempt = func(_ int, observed []ProcessInput) {
		attempts.Add(1)
		if !reflect.DeepEqual(observed[0], want) {
			t.Errorf("retry callback values changed:\nwant=%+v\ngot=%+v", want, observed[0])
		}
	}
	var once sync.Once
	store.hooks.beforeFamilyLock = func(ctx context.Context, tx pgx.Tx, input ProcessInput) error {
		var severity string
		if err := tx.QueryRow(ctx, `SELECT severity FROM incident_families WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, input.Scope.Region, input.Scope.TenantID, canonical.IncidentID).Scan(&severity); err != nil {
			return err
		}
		var conflictErr error
		once.Do(func() {
			_, conflictErr = pool.Exec(ctx, `UPDATE incident_families SET severity='warn' WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, input.Scope.Region, input.Scope.TenantID, canonical.IncidentID)
		})
		return conflictErr
	}
	result, err := store.Process(context.Background(), next)
	if err != nil {
		t.Fatalf("retrying process: %v", err)
	}
	if attempts.Load() < 2 {
		t.Fatalf("want actual SQLSTATE 40001 callback retry, got %d attempt(s)", attempts.Load())
	}
	if result.IncidentID != canonical.IncidentID {
		t.Fatalf("retry changed canonical family: %+v", result)
	}
}

func TestBatchAndDatabaseConstraintsAreCorrectnessControls(t *testing.T) {
	store, pool := integrationStore(t)
	if _, err := store.ProcessBatch(context.Background(), make([]ProcessInput, MaxProcessBatch+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("over-limit process batch accepted: %v", err)
	}
	if _, err := store.ClaimOutbox(context.Background(), Scope{Region: builders.DefaultRegion, TenantID: "tenant-a"}, "worker", make([]string, MaxClaimBatch+1), time.Now().UTC(), time.Minute); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("over-limit claim batch accepted: %v", err)
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO records_seen(record_id,region,tenant_id,identity_version,source_type,first_processed_at,safe_outcome) VALUES ('not-a-record-id','r','t','v','otlp',now(),'processed')`)
	if err == nil {
		t.Fatal("database record identity constraint accepted malformed identity")
	}
}

func TestInvestigationForeignKeysCannotCrossIncidentFamilies(t *testing.T) {
	store, pool := integrationStore(t)
	factory := builders.NewFactory()
	idSource := testids.New()
	first := preparedInput(t, idSource, factory.Record(t), true)
	firstResult, err := store.Process(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord := factory.Record(t, builders.WithService("inventoryservice"))
	second := preparedInput(t, idSource, secondRecord, true)
	secondResult, err := store.Process(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE incident_families SET active_investigation_id=$4
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, first.Scope.Region, first.Scope.TenantID,
		firstResult.IncidentID, secondResult.InvestigationID); err == nil {
		t.Fatal("family accepted an active investigation owned by another incident")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM investigation_claims
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, first.Scope.Region, first.Scope.TenantID,
		firstResult.IncidentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO investigation_claims
		(region,tenant_id,incident_id,investigation_id) VALUES ($1,$2,$3,$4)`, first.Scope.Region,
		first.Scope.TenantID, firstResult.IncidentID, secondResult.InvestigationID); err == nil {
		t.Fatal("claim accepted a mismatched incident/investigation pair")
	}
}

func integrationStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := crdbtest.Pool(t)
	if err := ApplyMigrations(context.Background(), pool, TopologySingleRegion); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store, err := New(pool, Config{Validator: redact.MinimalPolicy(), Scope: Scope{Region: builders.DefaultRegion, TenantID: "tenant-a"}, Classification: "SENSITIVE", Topology: TopologySingleRegion})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store, pool
}

func preparedInput(t *testing.T, idSource *testids.Source, record model.NormalizedLog, investigate bool) ProcessInput {
	t.Helper()
	result, err := fingerprint.Error(record)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	input := ProcessInput{
		Scope:              Scope{Region: record.Region, TenantID: "tenant-a"},
		Record:             record,
		ProcessedAt:        record.ObservedTime,
		FingerprintVersion: result.Version,
		Fingerprint:        result.Digest,
		IncidentID:         mustIncidentID(t, record),
		Generation:         mustGeneration(t, record.Deployment.ID, record.EventTime),
		DeploymentID:       record.Deployment.ID,
		EpisodeStart:       record.EventTime,
		Severity:           string(record.SeverityClass),
		RuleTrigger:        "ordinary_error_v1",
	}
	// Detection status, quiet bounds, and lateness come from the decision.
	setDecision(t, &input)
	if investigate {
		input.ContextVersion = 1
		investigationID := mustID(t, idSource)
		messageID := mustID(t, idSource)
		input.Investigation = &InvestigationInput{
			InvestigationID: investigationID,
			TriggerReason:   "five_in_five",
			ContextVersion:  1,
			Context:         model.SafeMap(map[string]model.SafeValue{"generation": model.SafeInt(1)}),
			Classification:  "SENSITIVE",
			Outbox: queue.Message{
				MessageID:        messageID,
				DeduplicationKey: "assignment:" + investigationID,
				Type:             agent.AssignmentMessageType,
				Attributes:       map[string]string{"region": record.Region},
			},
		}
		setAssignmentBody(t, &input)
	}
	setReplayIdentity(t, &input, journal.PriorityNormal, "SENSITIVE")
	return input
}

func setAssignmentBody(t *testing.T, input *ProcessInput) {
	t.Helper()
	if input.Investigation == nil {
		t.Fatal("assignment body requires an investigation")
	}
	investigation := input.Investigation
	body, err := agent.EncodeAssignment(agent.Assignment{
		SchemaVersion: agent.AssignmentSchemaVersion, MessageID: investigation.Outbox.MessageID, MessageType: agent.AssignmentMessageType,
		CreatedAt: input.ProcessedAt, Region: input.Scope.Region, TenantID: input.Scope.TenantID,
		Classification: "SENSITIVE", Producer: agent.AssignmentProducer, CorrelationID: investigation.InvestigationID,
		IncidentID: input.IncidentID, IncidentGeneration: input.Generation, InvestigationID: investigation.InvestigationID,
		ServiceID: input.Record.Service.Name, Environment: input.Record.Service.Environment,
		Severity: input.Severity, ContextVersion: input.ContextVersion,
	})
	if err != nil {
		t.Fatalf("assignment payload: %v", err)
	}
	input.Investigation.Outbox.Body = body
}

func mustAssignment(t *testing.T, value agent.Assignment) []byte {
	t.Helper()
	encoded, err := agent.EncodeAssignment(value)
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	return encoded
}

func setReplayIdentity(t *testing.T, input *ProcessInput, priority journal.Priority, classification string) {
	t.Helper()
	replay, err := journal.NewReplayIdentity(input.Record, input.Scope.TenantID, classification, priority)
	if err != nil {
		t.Fatalf("M3 replay identity: %v", err)
	}
	input.Replay = replay
}

func setDecision(t *testing.T, input *ProcessInput) {
	t.Helper()
	engine := incident.New()
	if _, err := engine.Observe(input.Record); err != nil {
		t.Fatalf("M1 observe: %v", err)
	}
	engine.Advance(input.Record.EventTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	decision, ok := engine.PersistenceDecision(input.Record.RecordID)
	if !ok {
		t.Fatal("M1 did not finalize singleton persistence decision")
	}
	applyDecision(input, decision)
}

func applyDecision(input *ProcessInput, decision incident.PersistenceDecision) {
	input.Decision = decision
	input.IncidentID = decision.IncidentID()
	input.Generation = decision.GenerationKey()
	input.DeploymentID = decision.DeploymentID()
	input.EpisodeStart = decision.EpisodeStart()
	input.EvidenceOnly = decision.EvidenceOnly()
	// The generation lifecycle is M1's; a caller reconstruction of detection
	// status, quiet bounds, or lateness is rejected by the Store.
	input.DetectionStatus = string(decision.DetectionStatus())
	input.QuietAt = decision.QuietAt()
	input.ReopenUntil = decision.ReopenUntil()
	input.Late = decision.Late()
}

func mustID(t *testing.T, source *testids.Source) string {
	t.Helper()
	id, err := source.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustGeneration(t *testing.T, deploymentID string, episodeStart time.Time) int64 {
	t.Helper()
	generation, err := CanonicalGeneration(deploymentID, episodeStart)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func mustIncidentID(t *testing.T, record model.NormalizedLog) string {
	t.Helper()
	update, err := incident.New().Observe(record)
	if err != nil {
		t.Fatal(err)
	}
	return update.IncidentID
}

func assertCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	assertCountWhere(t, pool, table, "true", want)
}

func assertCountWhere(t *testing.T, pool *pgxpool.Pool, table, predicate string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+predicate).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s where %s: want %d rows, got %d", table, predicate, want, got)
	}
}

func stringsContains(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}
