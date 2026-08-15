#!/usr/bin/env bash
#
# Apply scripts/seed-cluster.sql to the cluster in DATABASE_URL. A thin wrapper
# over cmd/seed, so it can be run from anywhere rather than only from the Go
# module root.
#
#   ./scripts/seed.sh                    # with a confirmation prompt
#   ./scripts/seed.sh -yes               # unattended
#   ./scripts/seed.sh -file=other.sql    # a different script
#   ./scripts/seed.sh -database-url '...'
#
# This replaces the `docker exec … cockroach sql < seed-cluster.sql` route,
# which needs a container runtime and a local cluster — neither of which exists
# when the target is CockroachDB Cloud.
#
# It DROPs and recreates the tables the script names. Run it before recording
# anything, and against whatever cluster the demo points at: on an unseeded
# cluster the agent invents table names, every tool call fails, and it answers
# from the vector store alone while looking grounded.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"

cd "$MODULE_DIR"

case "${1:-}" in
  -h|--help|-help)
    sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    echo "Flags:"
    go run ./cmd/seed -h 2>&1 | tail -n +2
    exit 0
    ;;
esac

exec go run ./cmd/seed "$@"
