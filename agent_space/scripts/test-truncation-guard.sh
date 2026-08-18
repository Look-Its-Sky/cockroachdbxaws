#!/usr/bin/env bash
#
# Extensive battery for the truncation guard (checkWholeFiles in
# remediation/patch.go, called from Propose and Repair).
#
#   ./scripts/test-truncation-guard.sh          # everything, 15s of fuzzing
#   ./scripts/test-truncation-guard.sh -q       # quiet: no per-attack output
#   ./scripts/test-truncation-guard.sh -F 60    # fuzz for 60 seconds
#   ./scripts/test-truncation-guard.sh -F 0     # skip the fuzzer
#   ./scripts/test-truncation-guard.sh -r       # also under the race detector
#
# Needs no credentials, no network, and no container runtime: the model is
# scripted and the apply stage runs against a temp dir. The red-team table
# asserts both directions — that an attack is refused for the right reason,
# and that an accepted file is written back through the real apply script,
# byte for byte.
#
# Stages:
#   1. gofmt, go build, go vet over the module
#   2. the red-team table: named attacks through the real Propose path,
#      each one expected to land or to be refused for a specific reason
#   3. the pipeline proof: a truncated proposal fails every candidate and
#      spends zero candidate containers
#   4. the repair-round checks: a repair is compared against the files the
#      model left last round
#   5. the fuzzer: property invariants on checkWholeFiles
#   6. the whole remediation package, so the battery never hides a regression
#
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"

QUIET=0
RACE=0
FUZZTIME=15

usage() { sed -n '2,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while getopts ':qrF:h' opt; do
  case "$opt" in
    q) QUIET=1 ;;
    r) RACE=1 ;;
    F) FUZZTIME="$OPTARG" ;;
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

# like stage, but the output is the point: the table of attacks is shown
stage_show() {
  local name="$1"; shift
  printf '  %-28s' "$name"
  local out
  if out="$("$@" 2>&1)"; then
    printf '%s\n' "$(green ✓)"
    [ -n "$out" ] && printf '%s\n' "$out" | sed 's/^/      /'
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

TEST_FLAGS=()
[ "$RACE" = "1" ] && TEST_FLAGS+=(-race)
[ "$QUIET" = "0" ] && TEST_FLAGS+=(-v)

echo "Module: $MODULE_DIR"
echo "Go:     $(go version | awk '{print $3, $4}')"
[ "$RACE" = "1" ] && echo "Race:   on"
[ "$FUZZTIME" != "0" ] && echo "Fuzz:   ${FUZZTIME}s"
echo

echo "Static checks"
stage "gofmt" check_fmt
stage "go build ./..." go build ./...
stage "go vet ./..." go vet ./...

echo
echo "The guard"
stage_show "red team (34 attacks)" go test ./remediation/ "${TEST_FLAGS[@]}" -run 'TestGuardRedTeam'
stage "spends no container" go test ./remediation/ "${TEST_FLAGS[@]}" -run 'TestTruncatedProposalSpendsNoContainer'
stage "repair round" go test ./remediation/ "${TEST_FLAGS[@]}" -run 'TestRepairRejectsATruncatedRewrite|TestRepairAcceptsACompleteRewrite'

echo
echo "Fuzzer"
if [ "$FUZZTIME" = "0" ]; then
  printf '  %s\n' "$(dim "skipped — pass -F <seconds> to run it")"
else
  stage "FuzzCheckWholeFiles ${FUZZTIME}s" \
    go test ./remediation/ -run '^$' -fuzz 'FuzzCheckWholeFiles' -fuzztime "${FUZZTIME}s"
fi

echo
echo "No regressions"
stage "go test ./remediation/" go test ./remediation/ "${TEST_FLAGS[@]}"

echo
echo "───────────────────────────────────────────────"
if [ "$FAIL" -eq 0 ]; then
  printf '%s  %s stage(s) passed\n' "$(green 'ALL GOOD')" "$PASS"
  exit 0
fi
printf '%s  %s passed, %s failed: %s\n' "$(red FAILED)" "$PASS" "$FAIL" "${FAILED_STAGES[*]}"
exit 1
