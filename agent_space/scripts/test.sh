#!/usr/bin/env bash
#
# Run everything that can be checked without a deploy: formatting, build, vet,
# and the Go test suite. This is the one to run before committing.
#
#   ./scripts/test.sh          # fmt, build, vet, unit tests
#   ./scripts/test.sh -r       # also under the race detector
#   ./scripts/test.sh -c       # also report coverage per package
#   ./scripts/test.sh -a       # also run the live API smoke test (see below)
#   ./scripts/test.sh -r -c -a # everything
#
# The unit tests need no credentials and no network: the MCP client and the
# agent loop run against an in-process MCP server, and the agent tests drive
# the loop with a scripted model.
#
# -a additionally runs test_api.sh against a running server. That is left
# opt-in rather than automatic because it WRITES one row into whatever database
# the API is pointed at, which may be a live cluster.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"
API_URL="${API_URL:-http://localhost:8080}"

RACE=0
COVER=0
WITH_API=0

usage() { sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while getopts ':rcah' opt; do
  case "$opt" in
    r) RACE=1 ;;
    c) COVER=1 ;;
    a) WITH_API=1 ;;
    h) usage ;;
    *) echo "unknown option: -$OPTARG" >&2; exit 2 ;;
  esac
done

cd "$MODULE_DIR"

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
dim()   { printf '\033[2m%s\033[0m' "$1"; }

PASS=0
FAIL=0
FAILED_STAGES=()

# stage <name> <command...> — runs a stage, keeps going when it fails so one
# broken thing does not hide the rest.
stage() {
  local name="$1"; shift
  printf '  %-28s' "$name"
  local out
  if out="$("$@" 2>&1)"; then
    printf '%s\n' "$(green ✓)"
    PASS=$((PASS + 1))
  else
    printf '%s\n' "$(red ✗)"
    [ -n "$out" ] && printf '%s\n' "$out" | sed 's/^/      /'
    FAIL=$((FAIL + 1))
    FAILED_STAGES+=("$name")
  fi
}

# gofmt -l prints the files it would change and exits 0 either way, so the
# emptiness of its output is the actual result.
check_fmt() {
  local unformatted
  unformatted="$(gofmt -l . 2>/dev/null)"
  [ -z "$unformatted" ] && return 0
  echo "these files are not gofmt-clean:"
  echo "$unformatted"
  echo "fix with: gofmt -w ."
  return 1
}

echo "Module: $MODULE_DIR"
echo "Go:     $(go version | awk '{print $3, $4}')"
echo

echo "Static checks"
stage "gofmt" check_fmt
stage "go build ./..." go build ./...
stage "go vet ./..." go vet ./...

echo
echo "Unit tests"
TEST_FLAGS=()
[ "$RACE" = "1" ] && TEST_FLAGS+=(-race)
[ "$COVER" = "1" ] && TEST_FLAGS+=(-cover)
stage "go test ./...${TEST_FLAGS[*]:+ ${TEST_FLAGS[*]}}" go test "${TEST_FLAGS[@]}" ./...

# Show the per-package detail too: a bare ✓ hides which packages have no tests
# at all, which is worth seeing.
go test "${TEST_FLAGS[@]}" ./... 2>&1 | sed 's/^/      /'

# ── live API ───────────────────────────────────────────────────────────────

echo
echo "Live API"
API_UP=0
curl -sS -m 5 -o /dev/null "$API_URL/ping" 2>/dev/null && API_UP=1

if [ "$WITH_API" = "1" ]; then
  if [ "$API_UP" = "1" ]; then
    echo
    "$SCRIPT_DIR/test_api.sh" || { FAIL=$((FAIL + 1)); FAILED_STAGES+=("test_api.sh"); }
  else
    printf '  %-28s%s\n' "test_api.sh" "$(red ✗)"
    printf '      %s\n' "no server on $API_URL — start it with: cd agent_space && go run ."
    FAIL=$((FAIL + 1))
    FAILED_STAGES+=("test_api.sh")
  fi
elif [ "$API_UP" = "1" ]; then
  printf '  %s\n' "$(dim "skipped — a server is up on $API_URL; pass -a to include the smoke test (writes one row)")"
else
  printf '  %s\n' "$(dim "skipped — no server on $API_URL, and -a was not passed")"
fi

# ── summary ────────────────────────────────────────────────────────────────

echo
echo "───────────────────────────────────────────────"
if [ "$FAIL" -eq 0 ]; then
  printf '%s  %s stage(s) passed\n' "$(green 'ALL GOOD')" "$PASS"
  exit 0
fi
printf '%s  %s passed, %s failed: %s\n' "$(red FAILED)" "$PASS" "$FAIL" "${FAILED_STAGES[*]}"
exit 1
