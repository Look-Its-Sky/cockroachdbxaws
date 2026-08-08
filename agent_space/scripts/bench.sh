#!/usr/bin/env bash
#
# Latency benchmark for the SRE agent.
#
#   ./scripts/bench.sh                       # 3 runs each of /retrieve, /ask, /agent
#   ./scripts/bench.sh -n 10 -e retrieve     # 10 runs of one endpoint
#   ./scripts/bench.sh -l "glm-5.2 via OpenRouter" -o bench-openrouter.jsonl
#
# The point is the /agent breakdown. Its trace carries duration_ms per tool
# call, so wall time splits into MCP time and model time — which settles
# whether a slow run is the database or the LLM.
#
# Options:
#   -u URL     API base URL               (default $API_URL or localhost:8080)
#   -n N       measured runs per endpoint  (default 3)
#   -w N       warmup runs, not measured   (default 1)
#   -e LIST    comma-separated: ping,store,retrieve,ask,agent
#   -q TEXT    question for /ask and /agent
#   -l LABEL   tag for the run, e.g. the model name (default: timestamp)
#   -o FILE    write raw per-run JSONL here
#   -m SECS    per-request timeout         (default 900)
#   -s         seed two incident rows first, so /retrieve and /ask have grounding
#   -h         this help
#
# On timeouts: curl disconnecting cancels the request context server-side, and
# the handler correctly returns 500. A run that fails at exactly -m seconds is
# that, not a server bug. Keep -m generous on slow local models.

set -euo pipefail

# curl renders %{time_total} with the locale's decimal separator, and jq only
# parses a dot. Force the C locale so a comma locale cannot corrupt every number.
export LC_ALL=C

API_URL="${API_URL:-http://localhost:8080}"
RUNS=3
WARMUP=1
ENDPOINTS="retrieve,ask,agent"
QUESTION="Checkout p99 latency jumped to 8s right after commit a91f3c2 shipped. Recall similar past incidents and tell me: rollback or hotfix?"
LABEL=""
OUTFILE=""
TIMEOUT=900
SEED=0

# The protected routes need the shared secret when the server sets API_TOKEN.
AUTH=()
[ -n "${API_TOKEN:-}" ] && AUTH=(-H "X-Agent-Token: $API_TOKEN")

usage() { sed -n '2,28p' "$0" | sed 's/^#\{1,2\} \{0,1\}//'; exit 0; }

while getopts ":u:n:w:e:q:l:o:m:sh" opt; do
  case "$opt" in
    u) API_URL="$OPTARG" ;;
    n) RUNS="$OPTARG" ;;
    w) WARMUP="$OPTARG" ;;
    e) ENDPOINTS="$OPTARG" ;;
    q) QUESTION="$OPTARG" ;;
    l) LABEL="$OPTARG" ;;
    o) OUTFILE="$OPTARG" ;;
    m) TIMEOUT="$OPTARG" ;;
    s) SEED=1 ;;
    h) usage ;;
    \?) echo "unknown option -$OPTARG (try -h)" >&2; exit 2 ;;
    :)  echo "-$OPTARG needs a value" >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "bench.sh needs jq." >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
RESULTS="$WORK/results.jsonl"
: > "$RESULTS"

# ── helpers ────────────────────────────────────────────────────────────────

# payload <endpoint> prints the request body, or nothing for GET endpoints.
payload() {
  case "$1" in
    store)    jq -n --arg t "benchmark row $RANDOM, synthetic, ignore." '{text: $t}' ;;
    retrieve) jq -n '{query: "latency regression from a new blocking call", limit: 4}' ;;
    ask)      jq -n --arg q "$QUESTION" '{question: $q}' ;;
    agent)    jq -n --arg q "$QUESTION" '{question: $q}' ;;
    *)        printf '' ;;
  esac
}

# call <endpoint> <outfile> -> "<seconds> <http_code>"
call() {
  local ep="$1" out="$2" body
  body="$(payload "$ep")"

  if [ -z "$body" ]; then
    curl -sS -m "$TIMEOUT" -o "$out" -w '%{time_total} %{http_code}' "${AUTH[@]}" "$API_URL/$ep" || echo "0 000"
  else
    curl -sS -m "$TIMEOUT" -o "$out" -w '%{time_total} %{http_code}' "${AUTH[@]}" \
      -X POST "$API_URL/$ep" -H 'Content-Type: application/json' -d "$body" || echo "0 000"
  fi
}

# ── preflight ──────────────────────────────────────────────────────────────

echo "Target:    $API_URL"

if ! curl -sS -m 10 -o /dev/null "$API_URL/ping"; then
  echo "The API is not answering on $API_URL." >&2
  echo "  Start it with: cd agent_space && go run ." >&2
  exit 1
fi

# /tools is the cheapest proof that MCP came up. Benchmarking /agent without it
# measures the latency of a 503, which is fast and meaningless.
MCP_CODE="$(curl -sS -m 10 -o "$WORK/tools.json" -w '%{http_code}' "$API_URL/tools" || echo 000)"
if [ "$MCP_CODE" = "200" ]; then
  echo "MCP:       connected, $(jq '.tools | length' "$WORK/tools.json") tools"
else
  echo "MCP:       unavailable (HTTP $MCP_CODE)"
  if [[ ",$ENDPOINTS," == *,agent,* ]]; then
    echo "           /agent would return 503 instantly; dropping it." >&2
    ENDPOINTS="$(printf '%s' "$ENDPOINTS" | tr ',' '\n' | grep -vx agent | paste -sd, -)"
  fi
fi

if [ "$SEED" = "1" ]; then
  echo "Seeding:   two incident rows"
  while IFS= read -r text; do
    curl -sS -m 60 -o /dev/null -X POST "$API_URL/store" -H 'Content-Type: application/json' \
      "${AUTH[@]}" -d "$(jq -n --arg t "$text" '{text: $t}')"
  done <<'EOF'
INC-412: checkout latency spiked to 8s after commit a91f3c2 added a synchronous fraud-check call in the request path. Rolled back; the proper async fix could not land same-day.
INC-388: 500s on /cart after commit 77bd10e introduced a null map write. Hotfixed in 40 minutes with a one-line nil guard; no rollback needed.
EOF
fi

[ -n "$ENDPOINTS" ] || { echo "Nothing left to benchmark." >&2; exit 1; }
[ -n "$LABEL" ] || LABEL="$(date +%Y-%m-%dT%H:%M:%S)"

echo "Label:     $LABEL"
echo "Plan:      $RUNS runs (+$WARMUP warmup) of: $ENDPOINTS"

# ── measure ────────────────────────────────────────────────────────────────

IFS=',' read -r -a EPS <<< "$ENDPOINTS"

for ep in "${EPS[@]}"; do
  [ -n "$ep" ] || continue
  printf '\n/%s' "$ep"

  # A cold model pays for weight loading on its first call. Charging that to
  # run 1 would misreport the steady-state latency the demo actually sees.
  for _ in $(seq 1 "$WARMUP"); do
    printf ' .'
    call "$ep" "$WORK/warmup.json" >/dev/null
  done

  for i in $(seq 1 "$RUNS"); do
    out="$WORK/$ep-$i.json"
    read -r secs code <<< "$(call "$ep" "$out")"
    printf ' %s' "$code"

    # A non-JSON body (a proxy error page, a truncated response) must not abort
    # the benchmark — record the run and carry on.
    jq -e . "$out" >/dev/null 2>&1 || echo 'null' > "$out"

    jq -n --arg ep "$ep" --argjson run "$i" --argjson secs "$secs" \
          --arg code "$code" --slurpfile doc "$out" '
      ($doc[0] // null) as $d
      | {endpoint: $ep, run: $run, seconds: $secs, code: $code}
      + (if ($d | type) == "object" then
           ($d | {iterations, sources, truncated} | with_entries(select(.value != null)))
           + (if ($d.answer | type) == "string" then {answer_chars: ($d.answer | length)} else {} end)
           + (if ($d.error != null) then {error: ($d.error | tostring | .[0:200])} else {} end)
           + (if ($d.trace | type) == "array" then
                # duration_ms is per tool call; the remainder is the model plus
                # our own overhead, which is what the comparison is about.
                ([$d.trace[].duration_ms // 0] | add // 0) as $ms
                | {tool_calls:    ($d.trace | length),
                   tool_failures: ([$d.trace[] | select(.failed)] | length),
                   tool_seconds:  (($ms / 1000 * 1000 | round) / 1000),
                   model_seconds: ((($secs - $ms / 1000) * 1000 | round) / 1000)}
              else {} end)
         else {} end)' >> "$RESULTS"
  done
done

printf '\n\n'

# ── report ─────────────────────────────────────────────────────────────────

jq -rs --arg label "$LABEL" '
  def rpad($n): tostring | . as $s | $s + ((($n - ($s | length)) as $p
    | if $p > 0 then " " * $p else "" end));
  def lpad($n): tostring | . as $s | ((($n - ($s | length)) as $p
    | if $p > 0 then " " * $p else "" end)) + $s;
  # Two fixed decimals; jq would print 5.0 as "5" and misalign the columns.
  def secs: (. * 100 | round) as $c
    | (($c / 100 | floor | tostring) + "."
       + (($c % 100 | tostring) | if length == 1 then "0" + . else . end) + "s");
  def mean: if length == 0 then 0 else (add / length) end;
  # Two significant figures below 1%, so a genuinely tiny share reads as
  # "0.01%" rather than rounding to a flattering "0%".
  def pct($total): (. / $total * 100) as $p
    | if $p >= 1 then (($p * 10 | round) / 10 | tostring)
      elif $p > 0 then (($p * 100 | round) / 100 | tostring)
      else "0" end;
  def median: sort | length as $n
    | if $n == 0 then 0
      elif $n % 2 == 1 then .[($n - 1) / 2]
      else (.[$n / 2 - 1] + .[$n / 2]) / 2 end;

  . as $rows
  | (map(.endpoint) | unique) as $eps
  | "=" * 72,
    "  \($label)",
    "=" * 72,
    ("endpoint" | rpad(12)) + ("n" | lpad(3)) + "  " + ("min" | lpad(8))
      + ("median" | lpad(9)) + ("mean" | lpad(9)) + ("max" | lpad(9)) + "  ok",
    "-" * 72,

    ( $eps[] as $ep
      | ($rows | map(select(.endpoint == $ep))) as $rs
      | ($rs | map(select(.code == "200"))) as $ok
      | ($ok | map(.seconds)) as $ts
      | (if ($ts | length) == 0 then [0] else $ts end) as $t
      | ("/" + $ep | rpad(12)) + ($rs | length | lpad(3)) + "  "
        + ($t | min | secs | lpad(8)) + ($t | median | secs | lpad(9))
        + ($t | mean | secs | lpad(9)) + ($t | max | secs | lpad(9))
        + "  \($ok | length)/\($rs | length)",
        ( $rs[] | select(.code != "200")
          | "                 run \(.run): HTTP \(.code) \(.error // "")" )
    ),

    # The decomposition exists only for /agent, and it is the headline number.
    ( ($rows | map(select(.endpoint == "agent" and .code == "200" and .tool_seconds != null))) as $a
      | if ($a | length) == 0 then empty else
          ($a | map(.tool_seconds) | mean) as $tool
          | ($a | map(.model_seconds) | mean) as $model
          | ($tool + $model) as $total
          | "",
            "/agent breakdown (mean of \($a | length) runs)",
            "-" * 72,
            "  MCP tool calls   " + ($tool | secs | lpad(8))
              + ("  \($tool | pct($total))%" | lpad(8))
              + "   \((($a | map(.tool_calls) | mean * 10 | round) / 10)) calls, "
              + "\($a | map(.tool_failures) | add) failed",
            "  model + overhead " + ($model | secs | lpad(8))
              + ("  \($model | pct($total))%" | lpad(8))
              + "   \((($a | map(.iterations // 0) | mean * 10 | round) / 10)) iterations",
            (if $model > $tool * 10 then
               "",
               "  The database is not the bottleneck. To speed the demo up, change",
               "  the model or lower MaxIterations in agent/runner.go."
             else empty end)
        end )
' "$RESULTS"

if [ -n "$OUTFILE" ]; then
  cp "$RESULTS" "$OUTFILE"
  echo
  echo "Raw per-run records: $OUTFILE"
fi
