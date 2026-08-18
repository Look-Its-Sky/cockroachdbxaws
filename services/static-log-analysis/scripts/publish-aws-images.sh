#!/bin/bash
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: publish-aws-images.sh REGION ANALYSIS_REPOSITORY DASHBOARD_REPOSITORY DASHBOARD_ADMIN_REPOSITORY COMMIT_SHA" >&2
  exit 1
fi

region=$1
analysis_repository=$2
dashboard_repository=$3
dashboard_admin_repository=$4
commit_sha=$5
service_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
repository_root=$(CDPATH= cd -- "$service_root/../.." && pwd)

if ! [[ "$commit_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "COMMIT_SHA must be a full lowercase Git commit SHA" >&2
  exit 1
fi
if [ "$(git -C "$repository_root" rev-parse HEAD)" != "$commit_sha" ]; then
  echo "COMMIT_SHA must equal the checked-out commit" >&2
  exit 1
fi
if [ -n "$(git -C "$repository_root" status --porcelain)" ]; then
  echo "tracked files must be clean before publishing release images" >&2
  exit 1
fi

registry=${analysis_repository%%/*}
for repository in "$analysis_repository" "$dashboard_repository" "$dashboard_admin_repository"; do
  if [ "${repository%%/*}" != "$registry" ]; then
    echo "all repositories must share one registry" >&2
    exit 1
  fi
done

aws ecr get-login-password --region "$region" |
  docker login --username AWS --password-stdin "$registry" >/dev/null

docker buildx build \
  --platform linux/amd64 \
  --provenance=true \
  --sbom=true \
  --tag "$analysis_repository:$commit_sha" \
  --push "$service_root"
docker buildx build \
  --platform linux/amd64 \
  --provenance=true \
  --sbom=true \
  --target runtime \
  --tag "$dashboard_repository:$commit_sha" \
  --push "$repository_root/services/dashboard"
docker buildx build \
  --platform linux/amd64 \
  --provenance=true \
  --sbom=true \
  --target admin \
  --tag "$dashboard_admin_repository:$commit_sha" \
  --push "$repository_root/services/dashboard"

resolve_digest() {
  local repository_url=$1 repository_name digest
  repository_name=${repository_url#*/}
  digest=$(aws ecr describe-images \
    --region "$region" \
    --repository-name "$repository_name" \
    --image-ids "imageTag=$commit_sha" \
    --query 'imageDetails[0].imageDigest' \
    --output text)
  if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "registry did not return an immutable digest for $repository_name" >&2
    exit 1
  fi
  printf '%s@%s\n' "$repository_url" "$digest"
}

echo "analysis_image=$(resolve_digest "$analysis_repository")"
echo "dashboard_image=$(resolve_digest "$dashboard_repository")"
echo "dashboard_admin_image=$(resolve_digest "$dashboard_admin_repository")"
echo "release_sha=$commit_sha"
