#!/bin/sh
set -eu

service_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
terraform_image='hashicorp/terraform:1.11.3@sha256:c2c17884347f9b5f3d71067a3ef1fb736f748979f89b35a2e4b5225735e7fe01'

STATIC_LOG_ANALYSIS_DATABASE_DSN='postgresql://validated-at-runtime.invalid/defaultdb?sslmode=verify-full' \
SLA_REGION='us-east-1' \
SLA_TENANT_ID='deployment-test' \
SLA_SOURCE_ACCOUNT='111122223333' \
SLA_SOURCE_INSTANCE='deployment-test' \
SLA_CREDENTIAL_IDENTITY='arn:aws:iam::111122223333:role/static-log-analysis-service' \
SLA_CLOUDWATCH_LOG_GROUPS='/aws/ecs/example=example=production' \
SLA_OUTBOX_QUEUE_URL='https://sqs.us-east-1.amazonaws.com/111122223333/static-log-analysis' \
SLA_OUTBOX_DEAD_LETTER_QUEUE_URL='https://sqs.us-east-1.amazonaws.com/111122223333/static-log-analysis-dlq' \
  docker compose --file "$service_root/deploy/aws/compose.yaml" config --quiet --no-path-resolution

docker run --rm \
  --entrypoint /bin/sh \
  -e TF_DATA_DIR=/tmp/terraform-data \
  -v "$service_root:/work:ro" \
  -w /work/infra/aws \
  "$terraform_image" \
  -ec 'terraform fmt -check && terraform init -backend=false -input=false -lockfile=readonly >/dev/null && terraform validate'
