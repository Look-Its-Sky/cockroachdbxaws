#!/bin/sh
set -eu

service_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
helm_image='alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895'
terraform_image='hashicorp/terraform:1.11.3@sha256:c2c17884347f9b5f3d71067a3ef1fb736f748979f89b35a2e4b5225735e7fe01'
rendered=$(mktemp)
trap 'rm -f "$rendered"' EXIT

docker run --rm \
  -v "$service_root:/work:ro" \
  -w /work \
  "$helm_image" \
  lint chart/static-log-analysis \
  -f chart/static-log-analysis/values-production.example.yaml

docker run --rm \
  -v "$service_root:/work:ro" \
  -w /work \
  "$helm_image" \
  template static-log-analysis chart/static-log-analysis \
  -f chart/static-log-analysis/values-production.example.yaml >"$rendered"

if grep -Eq -- '-journal-(max|min-free)-bytes=[0-9.]+[eE][+-]' "$rendered"; then
  echo "Helm rendered a journal byte count in scientific notation; Go's uint flag parser will reject it" >&2
  exit 1
fi

docker run --rm \
  --entrypoint /bin/sh \
  -e TF_DATA_DIR=/tmp/terraform-data \
  -v "$service_root/infra/aws:/work:ro" \
  -w /work \
  "$terraform_image" \
  -ec 'terraform fmt -check && terraform init -backend=false -input=false -lockfile=readonly >/dev/null && terraform validate'
