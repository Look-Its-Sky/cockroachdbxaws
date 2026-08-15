#!/usr/bin/env bash
#
# Reset the vector store. A thin wrapper over cmd/nuke, so it can be run from
# anywhere rather than only from the Go module root.
#
#   ./scripts/nuke.sh                   # empty the tables, keep the schema
#   ./scripts/nuke.sh -mode=drop        # remove them entirely
#   ./scripts/nuke.sh -mode=drop -yes   # unattended
#   ./scripts/nuke.sh -database-url '...'   # target something other than .env
#
# Every flag is passed through untouched — see ./scripts/nuke.sh -h for the
# full list. The confirmation prompt lives in cmd/nuke and still applies: this
# wrapper deliberately adds no -yes of its own, because the destructive
# decision should stay with the person typing the command.
#
# drop is required after changing the embedding width, since the vector column
# is sized at CREATE TABLE. The tables come back on the next server start.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"

cd "$MODULE_DIR"

# `go run` builds to a cache directory, so flag's default usage line names a
# path under ~/.cache/go-build rather than anything the reader can type. Print
# this file's own header first, then let cmd/nuke list the flags.
case "${1:-}" in
  -h|--help|-help)
    sed -n '2,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    echo "Flags:"
    go run ./cmd/nuke -h 2>&1 | tail -n +2
    exit 0
    ;;
esac

exec go run ./cmd/nuke "$@"
