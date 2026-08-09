#!/usr/bin/env bash
#
# Measure whether the configured model actually emits well-formed tool calls.
# A thin wrapper over cmd/toolcheck, so it can be run from anywhere rather than
# only from the Go module root.
#
#   ./scripts/toolcheck.sh                        # 20 iterations against OPENROUTER_MODEL
#   ./scripts/toolcheck.sh -n 40 -v               # more samples, print every iteration
#   ./scripts/toolcheck.sh -model qwen/qwen3-coder-next
#   ./scripts/toolcheck.sh -threshold 0.9         # stricter gate
#
# Every flag is passed through untouched — see ./scripts/toolcheck.sh -h.
#
# This spends tokens: -n iterations of a real tool-calling request against
# whatever OPENROUTER_MODEL points at. It exits non-zero below -threshold
# (default 0.8), so it can gate CI.
#
# The whole agent rests on the model emitting well-formed tool calls, and
# OpenRouter advertising "tools" in supported_parameters is a claim about the
# API surface rather than about behaviour. Run this before trusting a new model.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"

cd "$MODULE_DIR"

# `go run` builds to a cache directory, so flag's default usage line names a
# path under ~/.cache/go-build rather than anything the reader can type. Print
# this file's own header first, then let cmd/toolcheck list the flags.
case "${1:-}" in
  -h|--help|-help)
    sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    echo "Flags:"
    go run ./cmd/toolcheck -h 2>&1 | tail -n +2
    exit 0
    ;;
esac

exec go run ./cmd/toolcheck "$@"
