#!/usr/bin/env bash
#
# Smoke test for every route the service exposes. Fast, and it exits non-zero
# when something is broken, so it can gate a deploy.
#
#   ./scripts/test_api.sh
#   API_URL=https://xxxx.us-east-1.awsapprunner.com ./scripts/test_api.sh
#   API_TOKEN=... ./scripts/test_api.sh
#
# This is not the demo — see demo.sh for the narrated version, and bench.sh for
# latency. This only answers "is anything obviously broken".
#
# WRITES: /store inserts one row into whatever database the API is pointed at.
# The row is tagged SMOKETEST so it is easy to find and remove:
#   go run ./cmd/nuke -mode=truncate

set -uo pipefail

API_URL="${API_URL:-http://localhost:8080}"

# The protected routes need the shared secret when the server sets API_TOKEN.
AUTH=()
[ -n "${API_TOKEN:-}" ] && AUTH=(-H "X-Agent-Token: $API_TOKEN")

PASS=0
FAIL=0
SKIP=0

BODY="$(mktemp)"
trap 'rm -f "$BODY"' EXIT

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
dim()   { printf '\033[2m%s\033[0m' "$1"; }

# request <method> <path> [json-body] -> http code, response written to $BODY
request() {
  local method="$1" path="$2" body="${3:-}" code
  if [ -n "$body" ]; then
    code="$(curl -sS -m 900 -o "$BODY" -w '%{http_code}' -X "$method" "$API_URL$path" \
      -H 'Content-Type: application/json' "${AUTH[@]}" -d "$body" 2>/dev/null)"
  else
    code="$(curl -sS -m 60 -o "$BODY" -w '%{http_code}' -X "$method" "$API_URL$path" \
      "${AUTH[@]}" 2>/dev/null)"
  fi
  # curl writes 000 itself when it cannot connect, so a `|| echo 000` fallback
  # appends a second one and reports "HTTP 000000" — which is the first thing
  # you see when the API is down, and reads like a bug in the test. This covers
  # only curl failing to run at all, which would otherwise report an empty code.
  echo "${code:-000}"
}

# report <name> <ok> [detail]
report() {
  local name="$1" ok="$2" detail="${3:-}"
  if [ "$ok" = "1" ]; then
    printf '  %s %s\n' "$(green ✓)" "$name"
    PASS=$((PASS + 1))
  else
    printf '  %s %s\n' "$(red ✗)" "$name"
    [ -n "$detail" ] && printf '      %s\n' "$(dim "$detail")"
    FAIL=$((FAIL + 1))
  fi
}

skip() {
  printf '  %s %s\n' "$(dim —)" "$1"
  [ -n "${2:-}" ] && printf '      %s\n' "$(dim "$2")"
  SKIP=$((SKIP + 1))
}

# field <jq-path> reads a value out of the last response. jq is not installed
# everywhere, so python3 covers the same expressions used below — printing the
# raw body instead would bury the API's own error message in JSON escaping.
field() {
  if command -v jq >/dev/null 2>&1; then
    jq -r "$1 // empty" "$BODY" 2>/dev/null
    return
  fi
  FIELD_EXPR="$1" python3 - "$BODY" <<'PY' 2>/dev/null
import json, os, re, sys

expr = os.environ["FIELD_EXPR"]
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception:
    sys.exit(0)

def emit(v):
    if v is None:
        return
    print(v if isinstance(v, str) else json.dumps(v) if isinstance(v, (dict, list)) else v)

# .field / .a.b
if re.fullmatch(r"\.[\w.]+", expr):
    cur = doc
    for part in expr.strip(".").split("."):
        cur = cur.get(part) if isinstance(cur, dict) else None
    emit(cur)
# '.list | length'
elif m := re.fullmatch(r"\.(\w+) \| length", expr):
    v = doc.get(m.group(1))
    if v is not None:
        print(len(v))
# '.list[0]'
elif m := re.fullmatch(r"\.(\w+)\[(\d+)\]", expr):
    v = doc.get(m.group(1)) or []
    i = int(m.group(2))
    if isinstance(v, list) and len(v) > i:
        emit(v[i])
# '[.trace[] | select(.failed)] | length'
elif expr == "[.trace[] | select(.failed)] | length":
    print(sum(1 for s in (doc.get("trace") or []) if s.get("failed")))
PY
}

# err summarises a failed response: the API's own message beats a status code.
err() {
  local code="$1" msg
  msg="$(field .error)"
  [ -n "$msg" ] && echo "HTTP $code: $msg" || echo "HTTP $code: $(head -c 160 "$BODY")"
}

echo "Target: $API_URL"
[ ${#AUTH[@]} -gt 0 ] && echo "Auth:   X-Agent-Token set" || echo "Auth:   none (API_TOKEN unset)"
echo

# ── liveness ───────────────────────────────────────────────────────────────

echo "Liveness"
code="$(request GET /ping)"
if [ "$code" != "200" ]; then
  report "GET /ping" 0 "$(err "$code")"
  echo
  echo "The API is not answering. Start it with: cd agent_space && go run ." >&2
  exit 1
fi
report "GET /ping" 1

# ── MCP ────────────────────────────────────────────────────────────────────

echo
echo "CockroachDB MCP"
code="$(request GET /tools)"
MCP_UP=0
if [ "$code" = "200" ]; then
  MCP_UP=1
  report "GET /tools  ($(field '.tools | length') tools from $(field .endpoint))" 1
elif [ "$code" = "503" ]; then
  skip "GET /tools" "MCP is not configured: $(field .error)"
else
  report "GET /tools" 0 "$(err "$code")"
fi

# ── vector store ───────────────────────────────────────────────────────────

echo
echo "Vector index"
MARKER="SMOKETEST-$(date +%s)"
code="$(request POST /store "{\"text\":\"$MARKER INC-901: checkout returned 500s on every write within 6 minutes of deploying commit f0e9d88, which added a NOT NULL column without a backfill. Resolution: ROLLBACK.\"}")"
STORED=0
if [ "$code" = "200" ]; then
  STORED=1
  report "POST /store" 1
else
  detail="$(err "$code")"
  case "$detail" in
    *dimensions*) detail="$detail
      The vector column was created at a different width than the embedder now
      produces. Recreate it: go run ./cmd/nuke -mode=drop, then restart." ;;
  esac
  report "POST /store" 0 "$detail"
fi

code="$(request POST /retrieve '{"query":"constraint added without a backfill broke writes","limit":2}')"
if [ "$code" = "200" ] && [ -n "$(field '.results[0]')" ]; then
  report "POST /retrieve  ($(field '.results | length') hit(s))" 1
elif [ "$code" = "200" ]; then
  report "POST /retrieve" 0 "200 but no results — the index is empty. Did /store fail?"
else
  report "POST /retrieve" 0 "$(err "$code")"
fi

# ── generation ─────────────────────────────────────────────────────────────

echo
echo "Model"
code="$(request POST /ask '{"question":"A deploy just broke writes with a NOT NULL constraint. Rollback or hotfix?","limit":4}')"
if [ "$code" = "200" ] && [ -n "$(field .answer)" ]; then
  report "POST /ask  ($(field '.answer | length') chars, $(field .sources) sources)" 1
else
  report "POST /ask" 0 "$(err "$code")"
fi

# ── the agent loop ─────────────────────────────────────────────────────────

echo
echo "Agent"
if [ "$MCP_UP" != "1" ]; then
  skip "POST /agent" "needs MCP; would return 503"
else
  # Ask about a service seed-cluster.sql actually creates. Naming one it does
  # not — this asked about "orders" — sends the agent hunting for tables that
  # were never there, and the run reports four rejected calls on the way to a
  # correct answer. That is the agent recovering exactly as designed, but it
  # reads as breakage in a smoke test.
  code="$(request POST /agent '{"question":"The checkout service started returning 500s and latency spiked this morning. Which commit caused it, and should we roll back or hotfix?","limit":4}')"
  if [ "$code" = "200" ] && [ -n "$(field .answer)" ]; then
    report "POST /agent  ($(field .iterations) iterations, $(field '.trace | length') tool calls, $(field '[.trace[] | select(.failed)] | length') failed)" 1
  else
    report "POST /agent" 0 "$(err "$code")"
  fi
fi

# ── auth ───────────────────────────────────────────────────────────────────

echo
echo "Authentication"
if [ ${#AUTH[@]} -eq 0 ]; then
  skip "protected routes reject an unauthenticated call" \
       "API_TOKEN is unset, so every route is open. Never deploy this way."
else
  code="$(curl -sS -m 60 -o "$BODY" -w '%{http_code}' -X POST "$API_URL/store" \
    -H 'Content-Type: application/json' -d '{"text":"unauthenticated"}' 2>/dev/null)"
  code="${code:-000}"
  if [ "$code" = "401" ]; then
    report "POST /store without a token → 401" 1
  else
    report "POST /store without a token → 401" 0 "got HTTP $code; the route is not protected"
  fi
fi

# ── summary ────────────────────────────────────────────────────────────────

echo
echo "───────────────────────────────────────────────"
printf '%s passed, %s failed, %s skipped\n' "$PASS" "$FAIL" "$SKIP"
if [ "$STORED" = "1" ]; then
  echo
  echo "$(dim "Wrote one row tagged $MARKER. Clear it with: go run ./cmd/nuke -mode=truncate")"
fi

[ "$FAIL" -eq 0 ] || exit 1
