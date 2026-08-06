// Package crdbtest runs tests against a real CockroachDB.
//
// Serialization conflicts, unique constraint races, retry callbacks, and
// contention on a hot incident row are behaviours of the database, not of the
// code around it. A fake would agree with whatever the code expects, so these
// tests use a real single-node CockroachDB in a container.
//
// One container is shared by every test in a process, because starting it is
// the expensive part. Each test gets its own database inside that container, so
// tests remain independent and can run in parallel.
package crdbtest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/cockroachdb"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

const (
	// imageEnv overrides the pinned image, for trying a new CockroachDB version
	// before pinning it.
	imageEnv = "CRDB_TEST_IMAGE"
	// requireEnv makes an unavailable container a failure rather than a skip.
	// Continuous integration sets it, so a broken container cannot quietly turn
	// the storage gate into no gate at all.
	requireEnv = "REQUIRE_DOCKER"
	// disableEnv skips these tests without attempting to start anything.
	disableEnv = "CRDB_TEST"

	// defaultImage is pinned by digest, not only by tag. A tag such as
	// latest-v25.3 moves between patch releases, so integration behaviour and
	// CI results could change with no commit in this repository. The digest
	// makes a database upgrade a reviewed change; the tag is retained for
	// readability.
	defaultImage = "cockroachdb/cockroach:v25.3.7@sha256:" +
		"2804a08ced78596780b6acde2ef203421ed5b79e371779491af49e4c9eb6aa4e"
)

// shared is the process-wide container, started at most once.
var shared struct {
	once      sync.Once
	container *cockroachdb.CockroachDBContainer
	dsn       string
	err       error
}

// databaseCounter names each test's database uniquely within a process.
var databaseCounter atomic.Uint64

// Pool returns a connection pool to a database created for this test alone.
//
// The database is dropped and the pool closed when the test ends. When Docker
// is unavailable the test is skipped, unless REQUIRE_DOCKER=1 is set, in which
// case it fails.
func Pool(t tb.TB, opts ...Option) *pgxpool.Pool {
	t.Helper()

	settings := &settings{}
	for _, opt := range opts {
		opt(settings)
	}

	ctx := context.Background()
	adminDSN, err := sharedDSN(ctx)
	if err != nil {
		unavailable(t, err)
		return nil
	}

	name := databaseName(t)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("crdbtest: connecting to the shared cluster: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoteIdentifier(name)); err != nil {
		t.Fatalf("crdbtest: creating database %s: %v", name, err)
	}

	pool, err := pgxpool.New(ctx, replaceDatabase(adminDSN, name))
	if err != nil {
		t.Fatalf("crdbtest: connecting to database %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx := context.Background()
		cleanup, err := pgxpool.New(dropCtx, adminDSN)
		if err != nil {
			t.Logf("crdbtest: reconnecting to drop %s: %v", name, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" CASCADE"); err != nil {
			t.Logf("crdbtest: dropping %s: %v", name, err)
		}
	})

	for i, statement := range settings.statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("crdbtest: applying setup statement %d to %s: %v\n%s", i, name, err, statement)
		}
	}
	return pool
}

// Option configures a test database.
type Option func(*settings)

type settings struct {
	statements []string
}

// WithStatements runs SQL against the new database before the test starts, in
// order. It is how a test declares the tables it needs before migrations exist.
func WithStatements(statements ...string) Option {
	return func(s *settings) { s.statements = append(s.statements, statements...) }
}

// Available reports whether a container could be started, without failing or
// skipping. It exists for tests about this harness itself.
func Available(ctx context.Context) error {
	_, err := sharedDSN(ctx)
	return err
}

// sharedDSN starts the container on first use and returns its connection
// string. Later callers reuse it, including its failure.
func sharedDSN(ctx context.Context) (string, error) {
	if strings.EqualFold(os.Getenv(disableEnv), "off") {
		return "", fmt.Errorf("%s=off", disableEnv)
	}

	shared.once.Do(func() {
		image := os.Getenv(imageEnv)
		if image == "" {
			image = defaultImage
		}
		container, err := cockroachdb.Run(ctx, image, cockroachdb.WithInsecure())
		if err != nil {
			shared.err = fmt.Errorf("starting %s: %w", image, err)
			return
		}
		// ConnectionString returns a database/sql registration name rather than
		// a DSN, so the DSN is built from the connection config instead.
		config, err := container.ConnectionConfig(ctx)
		if err != nil {
			shared.err = fmt.Errorf("reading the connection config: %w", err)
			return
		}
		shared.container = container
		shared.dsn = dsnFor(config)
		// The container is left running for the rest of the process and reaped
		// by testcontainers when the process exits. Terminating it after each
		// test would cost more than every test that uses it.
	})
	return shared.dsn, shared.err
}

// Terminate stops the shared container. A TestMain may call it; otherwise the
// container is reaped when the test process exits.
func Terminate(ctx context.Context) error {
	if shared.container == nil {
		return nil
	}
	return testcontainers.TerminateContainer(shared.container)
}

// unavailable decides between skipping and failing when no container can be
// started.
func unavailable(t tb.TB, err error) {
	t.Helper()
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf("crdbtest: CockroachDB is required because %s=1, but it is unavailable: %v", requireEnv, err)
		return
	}
	t.Skipf("crdbtest: skipping, CockroachDB is unavailable: %v.\n"+
		"Start Docker to run this test, or set %s=1 to make its absence a failure.", err, requireEnv)
}

// databaseName returns a database name unique to this test within the process.
func databaseName(t tb.TB) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, t.Name())

	// CockroachDB identifiers are bounded, and test names nest arbitrarily deep.
	const maxNamePart = 40
	if len(sanitized) > maxNamePart {
		sanitized = sanitized[:maxNamePart]
	}
	return fmt.Sprintf("t_%s_%d", sanitized, databaseCounter.Add(1))
}

// dsnFor renders a connection config as a URL. Tests connect with insecure
// mode, which is appropriate for a throwaway single-node container and never
// for a deployed cluster.
func dsnFor(config *pgx.ConnConfig) string {
	dsn := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))),
		Path:     "/" + config.Database,
		RawQuery: "sslmode=disable",
	}
	if config.Password != "" {
		dsn.User = url.UserPassword(config.User, config.Password)
	} else {
		dsn.User = url.User(config.User)
	}
	return dsn.String()
}

// replaceDatabase points a connection string at another database on the same
// cluster.
func replaceDatabase(dsn, database string) string {
	base, query, hasQuery := strings.Cut(dsn, "?")
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return dsn
	}
	rewritten := base[:slash+1] + database
	if hasQuery {
		return rewritten + "?" + query
	}
	return rewritten
}

// quoteIdentifier renders a SQL identifier safely. Names come from test names,
// which are not attacker controlled, but an unquoted identifier would still
// break on a name that collides with a keyword.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
