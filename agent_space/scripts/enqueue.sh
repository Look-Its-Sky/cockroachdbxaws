#!/usr/bin/env bash
#
# Send an agent.assignment.v1 message to the queue the worker polls, in the
# exact shape the static-log-analysis producer sends.
#
#   ./scripts/enqueue.sh                      # the seeded checkout incident
#   ./scripts/enqueue.sh -p                   # the seeded payment incident
#   ./scripts/enqueue.sh -i <incident_id>     # a different incident
#   ./scripts/enqueue.sh -v 2                 # a different context_version
#   ./scripts/enqueue.sh -s cart -S critical  # different service / severity
#
# It prints the investigation_id, which is what GET /agent/{id} takes:
#
#   curl -s "$API_URL/agent/$(./scripts/enqueue.sh -q)" -H "X-Agent-Token: $API_TOKEN" | jq
#
# The default is the checkout incident, because checkout is the only service in
# the mapped repository with a test suite: it is the one where a proposed fix
# can be verified rather than merely compiled.
#
# Both defaults are ids scripts/seed-cluster.sql writes an incident_context row
# for, so an unmodified run resolves end to end. Any other id will sit on the
# queue being redelivered until a context row exists, which is the intended
# behaviour, not a bug — the producer commits context and the assignment
# independently.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(dirname "$SCRIPT_DIR")"

# .env is the same file the server reads, so the queue cannot drift between them
if [ -f "$MODULE_DIR/../.env" ]; then
  # shellcheck disable=SC1091
  set -a; . "$MODULE_DIR/../.env"; set +a
fi

QUEUE_URL="${SQS_QUEUE_URL:-http://localhost:4566/000000000000/static-log-analysis}"
ENDPOINT="${AWS_ENDPOINT_URL:-http://localhost:4566}"
REGION="${AWS_REGION:-us-east-1}"

# Incident ids as written by scripts/seed-cluster.sql.
CHECKOUT_INCIDENT="3f8a1c94d2b7e6053a4c81fd9e27b6cae05d413f8a92c7be14d0f6a35c8e29b7"
PAYMENT_INCIDENT="6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed"

INCIDENT_ID="$CHECKOUT_INCIDENT"
SERVICE="checkout"
ENVIRONMENT="production"
SEVERITY="error"
CONTEXT_VERSION=1
QUIET=0

usage() { sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while getopts ':i:s:e:S:v:pqh' opt; do
  case "$opt" in
    i) INCIDENT_ID="$OPTARG" ;;
    s) SERVICE="$OPTARG" ;;
    e) ENVIRONMENT="$OPTARG" ;;
    S) SEVERITY="$OPTARG" ;;
    v) CONTEXT_VERSION="$OPTARG" ;;
    p) INCIDENT_ID="$PAYMENT_INCIDENT"; SERVICE="payment" ;;
    q) QUIET=1 ;;                      # print only the investigation id
    h) usage ;;
    *) echo "unknown option: -$OPTARG" >&2; exit 2 ;;
  esac
done

command -v aws >/dev/null || { echo "enqueue.sh needs the aws CLI" >&2; exit 1; }

# A fresh investigation per send, so repeated runs are not deduplicated. The
# producer uses UUIDv7; ordering does not matter here, only uniqueness.
INVESTIGATION_ID="$(uuidgen | tr 'A-Z' 'a-z')"
MESSAGE_ID="$(uuidgen | tr 'A-Z' 'a-z')"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%S.%6NZ)"

BODY=$(cat <<JSON
{"schema_version":"1.0","message_id":"${MESSAGE_ID}","message_type":"agent.assignment.v1","created_at":"${CREATED_AT}","region":"${REGION}","tenant_id":"local","classification":"SENSITIVE","producer":"static-log-analysis","correlation_id":"${INVESTIGATION_ID}","incident_id":"${INCIDENT_ID}","incident_generation":3387192194469240033,"investigation_id":"${INVESTIGATION_ID}","service_id":"${SERVICE}","environment":"${ENVIRONMENT}","severity":"${SEVERITY}","context_version":${CONTEXT_VERSION}}
JSON
)

ATTRS=$(cat <<JSON
{
  "region":            {"StringValue":"${REGION}","DataType":"String"},
  "message_id":        {"StringValue":"${MESSAGE_ID}","DataType":"String"},
  "deduplication_key": {"StringValue":"assignment:${INVESTIGATION_ID}","DataType":"String"},
  "message_type":      {"StringValue":"agent.assignment.v1","DataType":"String"}
}
JSON
)

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}" \
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}" \
aws --endpoint-url "$ENDPOINT" --region "$REGION" sqs send-message \
  --queue-url "$QUEUE_URL" \
  --message-body "$BODY" \
  --message-attributes "$ATTRS" \
  >/dev/null

if [ "$QUIET" = "1" ]; then
  echo "$INVESTIGATION_ID"
else
  echo "queued   ${QUEUE_URL}"
  echo "incident ${INCIDENT_ID}"
  echo "poll     GET /agent/${INVESTIGATION_ID}"
fi
