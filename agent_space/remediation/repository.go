// Package remediation turns a verdict into candidate fixes: find the repository
// behind a failing service, run a coding harness against a throwaway checkout,
// record what came back. Nothing is ever cloned onto this host.
package remediation

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// like investigations, this is long-lived configuration and is deliberately
// absent from scripts/seed-cluster.sql, which drops what it recreates
const repositoryTable = "service_repositories"

// nothing maps this service to source code
var ErrNoRepository = errors.New("remediation: no repository is mapped to this service")

// where a service's code lives and what "verified" means for it; the commands
// are per-service because a polyglot repo has no single build or test
type Repository struct {
	ServiceID     string `json:"service_id"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"default_branch"`
	// Subdirectory narrows the harness to one service in a monorepo, e.g.
	// "src/checkout". Empty means the whole repository.
	Subdirectory string `json:"subdirectory,omitempty"`

	// the container the candidate is built and tested in
	RuntimeImage string `json:"runtime_image"`
	// SetupCommand restores dependencies, e.g. "npm ci". May be empty.
	SetupCommand string `json:"setup_command,omitempty"`
	// the weakest useful signal: it compiles or parses
	BuildCommand string `json:"build_command,omitempty"`
	// the strong signal; empty means this service has no tests,
	// which is a fact to report rather than a failure to hide.
	TestCommand string `json:"test_command,omitempty"`
}

// whether a passing candidate can honestly be called tested
func (r Repository) HasTests() bool { return strings.TrimSpace(r.TestCommand) != "" }

// the directory the harness and the commands run in, inside the container
func (r Repository) WorkingDir() string {
	const root = "/workspace"
	if sub := strings.Trim(r.Subdirectory, "/"); sub != "" {
		return root + "/" + sub
	}
	return root
}

// the URL to clone inside the sandbox. A token is embedded only when one is
// supplied, so a public repository is cloned with no credential at all.
func (r Repository) CloneURL(token string) string {
	if token == "" {
		return fmt.Sprintf("https://github.com/%s/%s.git", r.Owner, r.Repo)
	}
	// x-access-token is the documented user for a GitHub token over HTTPS
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		url.QueryEscape(token), r.Owner, r.Repo)
}

// Redacted renders the clone URL safely for logs.
func (r Repository) Redacted() string {
	return fmt.Sprintf("github.com/%s/%s", r.Owner, r.Repo)
}

// reads the service-to-repository mapping
type Repositories struct {
	Pool *pgxpool.Pool
}

// create the table if it is missing, the way the journal does
func NewRepositories(ctx context.Context, pool *pgxpool.Pool) (*Repositories, error) {
	if pool == nil {
		return nil, errors.New("remediation: repositories need a database pool")
	}

	const ddl = `
		CREATE TABLE IF NOT EXISTS ` + repositoryTable + ` (
			service_id     STRING PRIMARY KEY,
			owner          STRING NOT NULL,
			repo           STRING NOT NULL,
			default_branch STRING NOT NULL DEFAULT 'main',
			subdirectory   STRING NOT NULL DEFAULT '',
			runtime_image  STRING NOT NULL,
			setup_command  STRING NOT NULL DEFAULT '',
			build_command  STRING NOT NULL DEFAULT '',
			test_command   STRING NOT NULL DEFAULT '',
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		)`

	if _, err := pool.Exec(ctx, ddl); err != nil {
		return nil, fmt.Errorf("remediation: create %s: %w", repositoryTable, err)
	}
	return &Repositories{Pool: pool}, nil
}

// the repository mapped to a service
func (r *Repositories) Get(ctx context.Context, serviceID string) (Repository, error) {
	const q = `
		SELECT service_id, owner, repo, default_branch, subdirectory,
		       runtime_image, setup_command, build_command, test_command
		FROM ` + repositoryTable + `
		WHERE service_id = $1`

	var repo Repository
	err := r.Pool.QueryRow(ctx, q, serviceID).Scan(
		&repo.ServiceID, &repo.Owner, &repo.Repo, &repo.DefaultBranch,
		&repo.Subdirectory, &repo.RuntimeImage, &repo.SetupCommand,
		&repo.BuildCommand, &repo.TestCommand,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, fmt.Errorf("%w: %s", ErrNoRepository, serviceID)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("remediation: read repository: %w", err)
	}
	return repo, nil
}

// every mapping, for the UI's repo picker
func (r *Repositories) List(ctx context.Context) ([]Repository, error) {
	const q = `
		SELECT service_id, owner, repo, default_branch, subdirectory,
		       runtime_image, setup_command, build_command, test_command
		FROM ` + repositoryTable + `
		ORDER BY service_id`

	rows, err := r.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("remediation: list repositories: %w", err)
	}
	defer rows.Close()

	var out []Repository
	for rows.Next() {
		var repo Repository
		if err := rows.Scan(
			&repo.ServiceID, &repo.Owner, &repo.Repo, &repo.DefaultBranch,
			&repo.Subdirectory, &repo.RuntimeImage, &repo.SetupCommand,
			&repo.BuildCommand, &repo.TestCommand,
		); err != nil {
			return nil, fmt.Errorf("remediation: scan repository: %w", err)
		}
		out = append(out, repo)
	}
	return out, rows.Err()
}

// writes a mapping, so the demo can be configured from a script
func (r *Repositories) Upsert(ctx context.Context, repo Repository) error {
	const q = `
		UPSERT INTO ` + repositoryTable + ` (
			service_id, owner, repo, default_branch, subdirectory,
			runtime_image, setup_command, build_command, test_command, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`

	_, err := r.Pool.Exec(ctx, q,
		repo.ServiceID, repo.Owner, repo.Repo, repo.DefaultBranch, repo.Subdirectory,
		repo.RuntimeImage, repo.SetupCommand, repo.BuildCommand, repo.TestCommand,
	)
	if err != nil {
		return fmt.Errorf("remediation: save repository: %w", err)
	}
	return nil
}
