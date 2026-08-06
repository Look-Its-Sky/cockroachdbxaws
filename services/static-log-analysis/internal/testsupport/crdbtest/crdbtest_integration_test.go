//go:build integration

// These tests need Docker. They run in the storage and concurrency gate:
//
//	REQUIRE_DOCKER=1 go test -race -tags=integration ./...
package crdbtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/crdbtest"
)

func TestPoolReturnsAUsableDatabase(t *testing.T) {
	pool := crdbtest.Pool(t)

	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if one != 1 {
		t.Fatalf("want 1, got %d", one)
	}
}

func TestSetupStatementsAreApplied(t *testing.T) {
	pool := crdbtest.Pool(t, crdbtest.WithStatements(
		`CREATE TABLE records_seen (record_id STRING PRIMARY KEY, region STRING NOT NULL)`,
		`INSERT INTO records_seen VALUES ('a', 'us-east-1')`,
	))

	var region string
	err := pool.QueryRow(context.Background(), `SELECT region FROM records_seen WHERE record_id = 'a'`).Scan(&region)
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if region != "us-east-1" {
		t.Fatalf("want us-east-1, got %s", region)
	}
}

func TestEachTestGetsItsOwnDatabase(t *testing.T) {
	first := crdbtest.Pool(t, crdbtest.WithStatements(
		`CREATE TABLE incidents (incident_id STRING PRIMARY KEY)`,
		`INSERT INTO incidents VALUES ('from-first')`,
	))
	second := crdbtest.Pool(t, crdbtest.WithStatements(
		`CREATE TABLE incidents (incident_id STRING PRIMARY KEY)`,
	))

	var count int
	if err := second.QueryRow(context.Background(), `SELECT count(*) FROM incidents`).Scan(&count); err != nil {
		t.Fatalf("querying the second database: %v", err)
	}
	// Isolation is what lets storage tests run in parallel without one test's
	// rows becoming another's fixtures.
	if count != 0 {
		t.Fatalf("want the second database empty, got %d rows", count)
	}

	if err := first.QueryRow(context.Background(), `SELECT count(*) FROM incidents`).Scan(&count); err != nil {
		t.Fatalf("querying the first database: %v", err)
	}
	if count != 1 {
		t.Fatalf("want the first database to keep its row, got %d", count)
	}
}

func TestAUniqueConstraintIsEnforcedByTheRealDatabase(t *testing.T) {
	pool := crdbtest.Pool(t, crdbtest.WithStatements(
		`CREATE TABLE records_seen (record_id STRING PRIMARY KEY)`,
	))
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO records_seen VALUES ('r1')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO records_seen VALUES ('r1')`)

	// The effectively-once gate is a unique constraint. A harness that could
	// not observe the violation could not test the gate.
	if err == nil {
		t.Fatal("want a duplicate key rejected, got success")
	}
}

func TestTheDatabaseIsDroppedWhenTheTestEnds(t *testing.T) {
	var name string
	t.Run("inner", func(t *testing.T) {
		pool := crdbtest.Pool(t)
		if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
			t.Fatalf("reading the database name: %v", err)
		}
	})

	// Leaving databases behind would make a long run slower and eventually
	// exhaust the container.
	admin := crdbtest.Pool(t)
	var count int
	err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM [SHOW DATABASES] WHERE database_name = $1`, name).Scan(&count)
	if err != nil {
		t.Fatalf("listing databases: %v", err)
	}
	if count != 0 {
		t.Fatalf("want %s dropped when its test ended, it is still present", name)
	}
}

func TestSerializationConflictsAreObservable(t *testing.T) {
	pool := crdbtest.Pool(t, crdbtest.WithStatements(
		`CREATE TABLE counters (id STRING PRIMARY KEY, total INT NOT NULL)`,
		`INSERT INTO counters VALUES ('incident-1', 0)`,
	))
	ctx := context.Background()

	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the first transaction: %v", err)
	}
	defer rollback(t, first)
	second, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the second transaction: %v", err)
	}
	defer rollback(t, second)

	var total int
	if err := first.QueryRow(ctx, `SELECT total FROM counters WHERE id = 'incident-1'`).Scan(&total); err != nil {
		t.Fatalf("reading in the first transaction: %v", err)
	}
	if _, err := second.Exec(ctx, `UPDATE counters SET total = total + 1 WHERE id = 'incident-1'`); err != nil {
		t.Fatalf("writing in the second transaction: %v", err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("committing the second transaction: %v", err)
	}

	_, writeErr := first.Exec(ctx, `UPDATE counters SET total = $1 WHERE id = 'incident-1'`, total+1)
	commitErr := first.Commit(ctx)

	// Retry behaviour is the reason incident transactions must be idempotent.
	// The specific SQLSTATE matters: accepting any error here would let a
	// closed connection, a timeout, or a mistyped statement stand in for a
	// conflict, and the retry paths would be untested while looking covered.
	//
	// CockroachDB may raise the retry error either on the conflicting statement
	// or at commit, and both are correct, so the test accepts either and
	// reports which one happened.
	switch {
	case isSerializationFailure(writeErr):
		t.Logf("retry error raised on the conflicting statement: %v", writeErr)
	case isSerializationFailure(commitErr):
		t.Logf("retry error raised at commit: %v", commitErr)
	default:
		t.Fatalf("want a serialization failure (SQLSTATE %s) from overlapping transactions, "+
			"got write=%v commit=%v", serializationFailureCode, writeErr, commitErr)
	}
}

// serializationFailureCode is the SQL standard SQLSTATE for a transaction that
// must be retried.
const serializationFailureCode = "40001"

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == serializationFailureCode
}

func rollback(t *testing.T, tx pgx.Tx) {
	t.Helper()
	err := tx.Rollback(context.Background())
	if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Logf("rolling back: %v", err)
	}
}
