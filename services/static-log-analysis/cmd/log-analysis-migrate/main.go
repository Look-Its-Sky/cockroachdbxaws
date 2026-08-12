// Command log-analysis-migrate applies the embedded, reviewed CockroachDB
// migrations. It is a one-shot deployment utility, not a long-running service
// role; the same container image carries it for use by an init job.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	serviceruntime "github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/runtime"
)

const (
	databaseDSNEnvVar = serviceruntime.DatabaseDSNEnvVar
	exitSuccess       = 0
	exitFailure       = 1
	exitConfiguration = 2
)

type migrateFunc func(context.Context, string) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stderr, apply))
}

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, migrate migrateFunc) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: log-analysis-migrate")
		return exitConfiguration
	}
	dsn := ""
	if getenv != nil {
		dsn = getenv(databaseDSNEnvVar)
	}
	if dsn == "" {
		fmt.Fprintf(stderr, "%s must be set\n", databaseDSNEnvVar)
		return exitConfiguration
	}
	if migrate == nil {
		fmt.Fprintln(stderr, "migration failed")
		return exitFailure
	}
	if err := migrate(ctx, dsn); err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			fmt.Fprintln(stderr, "migration cancelled")
		case errors.Is(err, persistence.ErrIncompatibleSchema):
			fmt.Fprintln(stderr, "migration failed: incompatible schema")
		case errors.Is(err, persistence.ErrUnavailable):
			fmt.Fprintln(stderr, "migration failed: database unavailable")
		default:
			// Dependency errors can echo their connection strings. The command's
			// output is intentionally categorical so credentials never reach logs.
			fmt.Fprintln(stderr, "migration failed")
		}
		return exitFailure
	}
	fmt.Fprintln(stderr, "migrations applied")
	return exitSuccess
}

func apply(ctx context.Context, dsn string) error {
	config, err := migrationPoolConfig(dsn)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return persistence.ErrUnavailable
	}
	defer pool.Close()
	return persistence.ApplyMigrations(ctx, pool, persistence.TopologySingleRegion)
}

func migrationPoolConfig(dsn string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx can include the connection string in a parse error. Preserve only
		// the retry category at this boundary.
		return nil, persistence.ErrUnavailable
	}
	// CockroachDB v25.2 changed this session default to true. Migrations rely
	// on atomic DDL and must override both the server default and any value in
	// an operator-supplied connection string.
	config.ConnConfig.RuntimeParams["autocommit_before_ddl"] = "false"
	return config, nil
}
