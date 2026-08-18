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
docker_config=$(mktemp -d)
trap 'rm -rf -- "$docker_config"' EXIT
export DOCKER_CONFIG=$docker_config
install -d -m 0700 "$DOCKER_CONFIG/cli-plugins"
curl -fsSL \
  https://github.com/docker/buildx/releases/download/v0.34.1/buildx-v0.34.1.linux-amd64 \
  -o "$DOCKER_CONFIG/cli-plugins/docker-buildx"
echo "f1332ddb9010bd0b72628266c3a906d9a6979848033df4c8d9bd2cd113bae12b  $DOCKER_CONFIG/cli-plugins/docker-buildx" |
  sha256sum --check >/dev/null
chmod 0700 "$DOCKER_CONFIG/cli-plugins/docker-buildx"
docker buildx version >/dev/null

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

published_digest() {
  local repository_url=$1 repository_name
  repository_name=${repository_url#*/}
  aws ecr describe-images \
    --region "$region" \
    --repository-name "$repository_name" \
    --image-ids "imageTag=$commit_sha" \
    --query 'imageDetails[0].imageDigest' \
    --output text 2>/dev/null || true
}

publish_image() {
  local repository_url=$1 context=$2 target=$3 existing_digest
  existing_digest=$(published_digest "$repository_url")
  if [[ "$existing_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "reusing the existing immutable commit artifact in ${repository_url#*/}" >&2
    return
  fi

  local -a build_args=(
    --platform linux/amd64
    --provenance=true
    --sbom=true
    --tag "$repository_url:$commit_sha"
    --push
  )
  if [ -n "$target" ]; then
    build_args+=(--target "$target")
  fi
  docker buildx build "${build_args[@]}" "$context"
}

publish_image "$analysis_repository" "$service_root" ""
publish_image "$dashboard_repository" "$repository_root/services/dashboard" runtime
publish_image "$dashboard_admin_repository" "$repository_root/services/dashboard" admin

resolve_digest() {
  local repository_url=$1 repository_name digest
  repository_name=${repository_url#*/}
  digest=$(published_digest "$repository_url")
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
