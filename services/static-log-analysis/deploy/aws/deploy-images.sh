#!/bin/bash
set -euo pipefail

if [ "$EUID" -ne 0 ]; then
  echo "run this command with sudo" >&2
  exit 1
fi
if [ "$#" -ne 4 ]; then
  echo "usage: deploy-static-log-analysis-images ANALYSIS_IMAGE DASHBOARD_IMAGE DASHBOARD_ADMIN_IMAGE RELEASE_SHA" >&2
  exit 1
fi

analysis_image=$1
dashboard_image=$2
dashboard_admin_image=$3
release_sha=$4
environment_file=/etc/static-log-analysis.env
release_file=/etc/static-log-analysis/release.env
release_state_dir=/var/lib/static-log-analysis/releases
previous_release_file="$release_state_dir/previous.env"
candidate_release_file=$(mktemp /etc/static-log-analysis/release.env.XXXXXX)
trap 'rm -f "$candidate_release_file"' EXIT

set -a
. "$environment_file"
set +a

validate_image() {
  local label=$1 image=$2 repository=$3
  if [ "${image%@sha256:*}" != "$repository" ] || ! [[ "$image" =~ @sha256:[0-9a-f]{64}$ ]]; then
    echo "$label must use the configured repository and an immutable sha256 digest" >&2
    exit 1
  fi
}

validate_image analysis "$analysis_image" "$SLA_ANALYSIS_IMAGE_REPOSITORY"
validate_image dashboard "$dashboard_image" "$SLA_DASHBOARD_IMAGE_REPOSITORY"
validate_image dashboard-admin "$dashboard_admin_image" "$SLA_DASHBOARD_ADMIN_IMAGE_REPOSITORY"
if ! [[ "$release_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "RELEASE_SHA must be a full lowercase Git commit SHA" >&2
  exit 1
fi

for required_file in /etc/static-log-analysis/secrets.env /opt/static-log-analysis/compose.yaml; do
  if [ ! -f "$required_file" ] || [ -L "$required_file" ]; then
    echo "$required_file must be an installed regular file" >&2
    exit 1
  fi
done
if [ "$(stat -c '%u:%g:%a' /etc/static-log-analysis/secrets.env)" != "0:0:600" ]; then
  echo "/etc/static-log-analysis/secrets.env must be owned by root:root with mode 0600" >&2
  exit 1
fi

registry=${SLA_ANALYSIS_IMAGE_REPOSITORY%%/*}
aws ecr get-login-password --region "$SLA_REGION" |
  docker login --username AWS --password-stdin "$registry" >/dev/null
docker pull "$analysis_image"
docker pull "$dashboard_image"
docker pull "$dashboard_admin_image"
docker image inspect "$analysis_image" "$dashboard_image" "$dashboard_admin_image" >/dev/null

install -d -o root -g root -m 0700 "$release_state_dir"
if [ -f "$release_file" ]; then
  install -o root -g root -m 0600 "$release_file" "$previous_release_file"
else
  rm -f "$previous_release_file"
fi
chmod 0600 "$candidate_release_file"
printf "SLA_ANALYSIS_IMAGE='%s'\n" "$analysis_image" >"$candidate_release_file"
printf "SLA_DASHBOARD_IMAGE='%s'\n" "$dashboard_image" >>"$candidate_release_file"
printf "SLA_DASHBOARD_ADMIN_IMAGE='%s'\n" "$dashboard_admin_image" >>"$candidate_release_file"
printf "SLA_RELEASE_SHA='%s'\n" "$release_sha" >>"$candidate_release_file"
install -o root -g root -m 0600 "$candidate_release_file" "$release_file"

load_release() {
  set -a
  . "$release_file"
  set +a
}

compose() {
  docker compose \
    --project-name static-log-analysis-aws \
    --env-file /etc/static-log-analysis/secrets.env \
    --file /opt/static-log-analysis/compose.yaml "$@"
}

wait_for_service_health() {
  local service=$1 health_attempts=24 attempt container_id health
  for ((attempt = 1; attempt <= health_attempts; attempt++)); do
    container_id=$(compose ps --quiet "$service" 2>/dev/null || true)
    if [ -n "$container_id" ]; then
      health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)
      if [ "$health" = "healthy" ] || [ "$health" = "running" ]; then
        return 0
      fi
    fi
    sleep 5
  done
  echo "$service did not become healthy within 120 seconds" >&2
  return 1
}

wait_for_release_health() {
  wait_for_service_health cloudwatch && wait_for_service_health outbox || return 1
  if [ "$SLA_DASHBOARD_ENABLED" = "true" ] && [ -f /etc/static-log-analysis/dashboard.env ]; then
    wait_for_service_health dashboard || return 1
  fi
}

restore_previous_release() {
  if [ ! -f "$previous_release_file" ]; then
    echo "no prior digest-pinned release exists; leaving the failed candidate stopped" >&2
    systemctl stop static-log-analysis.service || true
    return 1
  fi
  install -o root -g root -m 0600 "$previous_release_file" "$release_file"
  load_release
  systemctl restart static-log-analysis.service
  wait_for_release_health
}

load_release
if ! compose run --rm migrate; then
  echo "analysis migration failed; restoring the previous release selection" >&2
  restore_previous_release || true
  exit 1
fi
if [ "$SLA_DASHBOARD_ENABLED" = "true" ] && [ -f /etc/static-log-analysis/dashboard.env ]; then
  if ! docker compose \
    --project-name static-log-analysis-aws \
    --env-file /etc/static-log-analysis/secrets.env \
    --env-file /etc/static-log-analysis/dashboard.env \
    --file /opt/static-log-analysis/compose.yaml \
    --profile dashboard run --rm dashboard-auth-migrate; then
    echo "dashboard migration failed; restoring the previous release selection" >&2
    restore_previous_release || true
    exit 1
  fi
fi

if ! systemctl restart static-log-analysis.service || ! wait_for_release_health; then
  echo "candidate release failed its bounded health gate; rolling back" >&2
  restore_previous_release || true
  exit 1
fi

echo "digest-pinned release is healthy"
