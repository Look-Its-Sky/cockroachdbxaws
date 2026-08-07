#!/usr/bin/env bash
#
# End-to-end demo of the SRE agent, paced for a short screen recording.
#
#   ./scripts/demo.sh
#   API_URL=https://xxxx.us-east-1.awsapprunner.com ./scripts/demo.sh
#
# Set PACE=0 to remove the pauses when running non-interactively.

set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
PACE="${PACE:-2}"

pause() { [ "$PACE" != "0" ] && sleep "$PACE" || true; }

heading() {
  echo
  echo "═══════════════════════════════════════════════════════════════"
  echo "  $1"
  echo "═══════════════════════════════════════════════════════════════"
}

# jq narrows the output to what matters on screen. Without it the whole payload
# is printed, indented — readable, just noisier.
HAVE_JQ=0
command -v jq >/dev/null 2>&1 && HAVE_JQ=1

# pretty <jq-filter> [--raw]
pretty() {
  local filter="$1" raw="${2:-}"
  if [ "$HAVE_JQ" = "1" ]; then
    if [ "$raw" = "--raw" ]; then jq -r "$filter"; else jq "$filter"; fi
  else
    python3 -m json.tool 2>/dev/null || cat
  fi
}

post() {
  curl -sS -X POST "$API_URL$1" -H 'Content-Type: application/json' -d "$2"
}

# mcp_ready reports whether the MCP handshake succeeded, so the demo can say
# plainly that it is running degraded instead of printing a bare error.
mcp_ready() {
  curl -sS -o /dev/null -w '%{http_code}' "$API_URL/tools" | grep -q '^200$'
}

heading "0. Service is up"
curl -sS "$API_URL/ping" | pretty .
if ! mcp_ready; then
  echo
  echo "  ⚠  The MCP handshake did not succeed, so steps 3 and 4 will report 503."
  echo "     Set COCKROACH_API_KEY (and optionally COCKROACH_CLUSTER_ID) and restart."
fi
pause

heading "1. CockroachDB tool #1 — seeding the incident store (vector index)"
echo "Each incident is embedded and written to a pgvector-compatible index in CockroachDB."

incidents=(
  "INC-412 (2026-03-14): Checkout service returned 500s within 8 minutes of deploying commit a3f9c21, which added a NOT NULL constraint to orders.promo_code without a backfill. Existing rows violated it. Resolution: ROLLBACK. A correct backfill would have taken 4 hours and the payment path was fully down."
  "INC-388 (2026-02-02): Search latency p99 rose from 120ms to 3.4s after commit b1d0e44 changed the ORDER BY on the listings query, dropping index usage. Resolution: HOTFIX. Adding the covering index took 40 minutes and the service was degraded, not down."
  "INC-455 (2026-05-21): Inventory sync began double-counting after commit c7e1a09 removed the idempotency key from the ingest handler. Resolution: ROLLBACK. Reconstructing the key required a schema change and data already written was wrong."
  "INC-470 (2026-06-30): Login page blank for 12% of users after commit d4b8f31 shipped a null-unsafe read of user.preferences. Resolution: HOTFIX. A one-line guard shipped in 25 minutes and the fallback path kept most users signed in."
)

for incident in "${incidents[@]}"; do
  printf '  → %.72s...\n' "$incident"
  # Let a JSON encoder do the escaping rather than hand-rolling it in sed.
  body=$(INCIDENT="$incident" python3 -c \
    'import json, os; print(json.dumps({"text": os.environ["INCIDENT"]}))')
  post /store "$body" >/dev/null
done
echo "  seeded ${#incidents[@]} incidents"
pause

heading "2. Semantic recall — the index finds the relevant precedent"
post /retrieve '{"query": "constraint added without a backfill broke writes", "limit": 1}' | pretty .
pause

heading "3. CockroachDB tool #2 — the Cloud Managed MCP Server"
echo "These tools were discovered over MCP at boot from https://cockroachlabs.cloud/mcp:"
curl -sS "$API_URL/tools" | pretty '{endpoint, cluster, tools: [.tools[].name]}'
pause

heading "4. The decision — recall, inspect the live cluster, then commit"
echo 'POST /agent  {"question": "..."}'
echo
RESULT="$(mktemp)"
trap 'rm -f "$RESULT"' EXIT

post /agent '{
  "question": "Deploy went out 20 minutes ago. The orders service is returning 500s on every write and the payment path is down. The deploy contained commit e5c2b17, which added a NOT NULL constraint to orders.discount_code. What do we do?",
  "limit": 4
}' > "$RESULT"

echo "── Decision ────────────────────────────────────────────────────"
pretty '.answer' --raw < "$RESULT"
echo
echo "── Evidence ────────────────────────────────────────────────────"
pretty '{sources, iterations, truncated, tools_called: [.trace[] | {tool, arguments, duration_ms}]}' < "$RESULT"
echo
echo "Every cluster query the agent made is in the trace — the decision is auditable,"
echo "not a black box."
