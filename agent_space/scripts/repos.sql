-- Maps each failing service to the code behind it, and to what "verified"
-- means for that service. Apply with:
--
--   go run ./cmd/seed -file scripts/repos.sql
--
-- Safe to re-run: every statement is an UPSERT and nothing is dropped. This
-- table is created at boot by the server, not here, because it is long-lived
-- configuration — seed-cluster.sql drops what it recreates and must never take
-- this with it.
--
-- The verification commands differ per service because they have to. In the
-- OpenTelemetry demo, checkout is Go with a test suite; payment is Node with no
-- tests whatsoever. A candidate for payment can be shown to install and parse,
-- and that is all — the pipeline reports which of build and test actually ran,
-- so "it compiles" is never presented as "it passes".

UPSERT INTO service_repositories
    (service_id, owner, repo, default_branch, subdirectory, runtime_image,
     setup_command, build_command, test_command, updated_at)
VALUES
    -- Go, and the only service here with a real test (money/money_test.go).
    -- Prefer this one for a demo: hermetic, fast, and the strong signal exists.
    ('checkout', 'Look-Its-Sky', 'opentelemetry-demo-auto-sre-test', 'main',
     'src/checkout', 'golang:1.24',
     '', 'go build ./...', 'go test ./...', now()),

    -- Node, and there is nothing to test: package.json declares only `start`.
    -- The empty test command is the honest answer, and HasTests() reports it.
    ('payment', 'Look-Its-Sky', 'opentelemetry-demo-auto-sre-test', 'main',
     'src/payment', 'node:22-alpine',
     'npm ci --omit=dev', 'node --check index.js && node --check charge.js', '', now()),

    -- .NET, included so the picker has a third entry; unverified beyond build.
    ('cart', 'Look-Its-Sky', 'opentelemetry-demo-auto-sre-test', 'main',
     'src/cart', 'mcr.microsoft.com/dotnet/sdk:9.0',
     '', 'dotnet build', '', now());
