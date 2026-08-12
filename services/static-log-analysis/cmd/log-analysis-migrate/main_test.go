package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
)

func TestRunRequiresDatabaseDSN(t *testing.T) {
	var called bool
	var stderr bytes.Buffer

	exit := run(context.Background(), nil, func(string) string { return "" }, &stderr,
		func(context.Context, string) error {
			called = true
			return nil
		})

	if exit != exitConfiguration {
		t.Fatalf("exit=%d, want %d", exit, exitConfiguration)
	}
	if called {
		t.Fatal("migration ran without a database DSN")
	}
	if got := stderr.String(); !strings.Contains(got, databaseDSNEnvVar) {
		t.Fatalf("stderr=%q, want the required environment variable name", got)
	}
}

func TestRunAppliesMigrationsWithoutPrintingDSN(t *testing.T) {
	const dsn = "postgresql://root:hunter2@cockroachdb:26257/logs?sslmode=disable"
	var received string
	var stderr bytes.Buffer

	exit := run(context.Background(), nil, func(name string) string {
		if name == databaseDSNEnvVar {
			return dsn
		}
		return ""
	}, &stderr, func(_ context.Context, got string) error {
		received = got
		return nil
	})

	if exit != exitSuccess {
		t.Fatalf("exit=%d, stderr=%q", exit, stderr.String())
	}
	if received != dsn {
		t.Fatalf("migrator received %q, want configured DSN", received)
	}
	if strings.Contains(stderr.String(), "hunter2") {
		t.Fatalf("stderr leaked database credentials: %q", stderr.String())
	}
}

func TestRunReportsCategoricalMigrationFailure(t *testing.T) {
	const dsn = "postgresql://root:hunter2@cockroachdb:26257/logs?sslmode=disable"
	var stderr bytes.Buffer

	exit := run(context.Background(), nil, func(string) string { return dsn }, &stderr,
		func(context.Context, string) error { return errors.New("driver echoed hunter2") })

	if exit != exitFailure {
		t.Fatalf("exit=%d, want %d", exit, exitFailure)
	}
	if got := stderr.String(); !strings.Contains(got, "migration failed") || strings.Contains(got, "hunter2") {
		t.Fatalf("stderr=%q, want a categorical error without credentials", got)
	}
}

func TestRunRejectsArgumentsBeforeOpeningDatabase(t *testing.T) {
	var called bool
	var stderr bytes.Buffer

	exit := run(context.Background(), []string{"unexpected"}, func(string) string { return "configured" }, &stderr,
		func(context.Context, string) error {
			called = true
			return nil
		})

	if exit != exitConfiguration || called {
		t.Fatalf("exit=%d called=%v, want configuration failure before migration", exit, called)
	}
}

func TestMigrationPoolDisablesDDLAutocommit(t *testing.T) {
	config, err := migrationPoolConfig("postgresql://root@cockroachdb:26257/logs?sslmode=disable&autocommit_before_ddl=true")
	if err != nil {
		t.Fatal(err)
	}
	if got := config.ConnConfig.RuntimeParams["autocommit_before_ddl"]; got != "false" {
		t.Fatalf("autocommit_before_ddl=%q, want false for transactional migrations", got)
	}
}

func TestMigrationPoolConfigurationErrorDoesNotLeakTheDSN(t *testing.T) {
	const secret = "hunter2"
	_, err := migrationPoolConfig("postgresql://root:" + secret + "@%gh&%ij")
	if !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("error=%v, want unavailable", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error leaked database credentials: %v", err)
	}
}
