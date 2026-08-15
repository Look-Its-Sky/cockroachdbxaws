#!/usr/bin/env bash
#
# Find out whether GITHUB_TOKEN can actually open a pull request, before an
# engineer discovers it cannot by clicking the button during a demo.
#
#   ./scripts/tokencheck.sh                       # checks the checkout mapping's repo
#   ./scripts/tokencheck.sh -r owner/repo         # check a different repository
#   ./scripts/tokencheck.sh -t github_pat_xxx     # test a token WITHOUT editing .env
#
# Exits non-zero if the token cannot write.
#
# -t is there because the usual failure is having edited or created the wrong
# token: prove a candidate works first, then paste it into .env.
#
# This exists because a fine-grained token that is missing a permission says
# only "Resource not accessible by personal access token", and the repository's
# own `permissions` block reports push: true regardless — that is the *account's*
# access, not the token's grant. The two look identical until you write.
#
# The write probe creates a dangling blob: a loose object with nothing
# referencing it, which GitHub garbage-collects. It changes no branch, no file
# and no history, and it is the exact first call remediation.Publisher makes.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"

REPO="Look-Its-Sky/opentelemetry-demo-auto-sre-test"
OVERRIDE_TOKEN=""

usage() { sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while getopts ':r:t:h' opt; do
  case "$opt" in
    r) REPO="$OPTARG" ;;
    t) OVERRIDE_TOKEN="$OPTARG" ;;
    h) usage ;;
    *) echo "unknown option: -$OPTARG" >&2; exit 2 ;;
  esac
done

# the token lives in the repo-root .env, the same one the server reads
for candidate in "$MODULE_DIR/.env" "$MODULE_DIR/../.env"; do
  if [[ -f "$candidate" ]]; then
    set -a; . "$candidate"; set +a
    break
  fi
done

# -t wins over .env, so a candidate token can be proven before it is pasted in
if [[ -n "$OVERRIDE_TOKEN" ]]; then
  GITHUB_TOKEN="$OVERRIDE_TOKEN"
  echo "using the token passed with -t, not the one in .env"
fi

green() { printf '\033[32m%s\033[0m\n' "$1"; }
red()   { printf '\033[31m%s\033[0m\n' "$1"; }

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  red "GITHUB_TOKEN is not set. Fixes will be proposed and verified, but picking one cannot open a draft."
  exit 1
fi

API="https://api.github.com"
AUTH=(-H "Authorization: Bearer ${GITHUB_TOKEN}")

echo "repository: $REPO"

# ── who the token is, and when it lapses ─────────────────────────────────────
LOGIN="$(curl -sS "${AUTH[@]}" "$API/user" | jq -r '.login // "?"')"
EXPIRY="$(curl -sS -I "${AUTH[@]}" "$API/user" \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="github-authentication-token-expiration"{print $2}')"

echo "identity:   $LOGIN"
echo "expires:    ${EXPIRY:-never (classic token)}"

# ── the write probe ──────────────────────────────────────────────────────────
RESPONSE="$(curl -sS -i -X POST "${AUTH[@]}" \
  "$API/repos/$REPO/git/blobs" \
  -d '{"content":"tokencheck probe","encoding":"utf-8"}')"

STATUS="$(printf '%s' "$RESPONSE" | awk 'NR==1{print $2}')"
NEEDED="$(printf '%s' "$RESPONSE" | tr -d '\r' \
  | awk -F': ' 'tolower($1)=="x-accepted-github-permissions"{print $2}')"

echo

case "$STATUS" in
  201)
    green "can write: a draft pull request will open."
    echo "  (the probe blob is unreferenced and will be garbage-collected)"
    exit 0
    ;;
  403)
    red "cannot write: the token is valid but read-only."
    echo "  GitHub says it needs: ${NEEDED:-contents=write}"
    echo
    echo "  Fix at https://github.com/settings/personal-access-tokens"
    echo "    Repository permissions -> Contents:      Read and write"
    echo "    Repository permissions -> Pull requests: Read and write"
    echo
    echo "  Everything else still works: fixes are proposed, verified and ranked."
    echo "  Only POST /solutions/{id}/pr fails."
    exit 1
    ;;
  404)
    red "cannot see $REPO."
    echo "  Either it does not exist, or it is not in this token's selected repositories."
    echo "  Check both at https://github.com/settings/personal-access-tokens"
    exit 1
    ;;
  *)
    red "unexpected response: HTTP ${STATUS:-none}"
    printf '%s\n' "$RESPONSE" | tail -n 5
    exit 1
    ;;
esac
