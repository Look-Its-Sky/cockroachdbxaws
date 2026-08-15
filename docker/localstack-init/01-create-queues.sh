#!/bin/bash
#
# Creates the assignment queue the worker polls, plus a dead-letter queue for
# messages that keep failing. LocalStack runs everything in ready.d once the
# services are up.
#
# The visibility timeout is 120s to match WORKER_VISIBILITY_TIMEOUT: the worker
# renews the lease while a run is in flight, but the initial value still has to
# be longer than the gap between receiving a message and the first heartbeat.
#
# maxReceiveCount is 5, the same number WORKER_MAX_ATTEMPTS gives up at. The
# worker normally gets there first and records a failure the API can report;
# the redrive policy is the backstop for a worker that dies mid-message.
set -euo pipefail

QUEUE=static-log-analysis
DLQ="${QUEUE}-dlq"

awslocal sqs create-queue --queue-name "$DLQ" >/dev/null

DLQ_ARN="$(awslocal sqs get-queue-attributes \
  --queue-url "http://localhost:4566/000000000000/${DLQ}" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' --output text)"

awslocal sqs create-queue --queue-name "$QUEUE" --attributes "$(cat <<JSON
{
  "VisibilityTimeout": "120",
  "MessageRetentionPeriod": "1209600",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${DLQ_ARN}\",\"maxReceiveCount\":\"5\"}"
}
JSON
)" >/dev/null

echo "localstack-init: created ${QUEUE} (dead-lettering to ${DLQ} after 5 deliveries)"
