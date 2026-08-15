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
    --
    -- golang:1.25, not 1.24: src/checkout/go.mod requires go >= 1.25.0, and the
    -- official Go images pin GOTOOLCHAIN=local, so an older image cannot fetch
    -- a newer toolchain to compensate. On 1.24 every candidate failed to build
    -- with "go.mod requires go >= 1.25.0", found by cmd/sandboxcheck.
    ('checkout', 'Look-Its-Sky', 'opentelemetry-demo-auto-sre-test', 'main',
     'src/checkout', 'golang:1.25',
     '', 'go build ./...', 'go test ./...', now()),

    -- Node, and there is nothing to test: package.json declares only `start`.
    -- The empty test command is the honest answer, and HasTests() reports it.
    --
    -- node:22, not node:22-alpine: the sandbox clones the repository inside the
    -- container, and alpine ships no git. On alpine the clone produced nothing
    -- but "git: not found", found by cmd/sandboxcheck.
    ('payment', 'Look-Its-Sky', 'opentelemetry-demo-auto-sre-test', 'main',
     'src/payment', 'node:22',
     'npm ci --omit=dev', 'node --check index.js && node --check charge.js', '', now()),

    -- .NET, included so the picker has a third entry; unverified beyond build.
    --
    -- sdk:10.0, not sdk:9.0: src/cart targets net10.0, and a 9.0 SDK refuses it
    -- with NETSDK1045 rather than falling back. Same class of mistake as
    -- checkout on golang:1.24 — the image has to be new enough for the service.
    ('cart', 'Look-Its-Sky', 'opentelemetry-demo-auto-sre-test', 'main',
     'src/cart', 'mcr.microsoft.com/dotnet/sdk:10.0',
     '', 'dotnet build', '', now());
