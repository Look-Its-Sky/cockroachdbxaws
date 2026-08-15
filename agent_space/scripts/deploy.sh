#!/usr/bin/env bash
#
# Build the image, push it to ECR, and create or update the App Runner service.
#
#   AWS_REGION=us-east-1 ./scripts/deploy.sh
#
# Requires: docker, aws CLI, and an IAM principal with
#   - ECR push rights            (AmazonEC2ContainerRegistryPowerUser)
#   - App Runner rights          (AWSAppRunnerFullAccess)
#   - an App Runner ECR access role (see APPRUNNER_ACCESS_ROLE_ARN below)
#
# Runtime configuration is read from the repository-root .env. That file holds
# live secrets, so it is never baked into the image — the values are passed to
# App Runner as runtime environment variables.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(dirname "$APP_DIR")"

AWS_REGION="${AWS_REGION:-us-east-1}"
SERVICE_NAME="${SERVICE_NAME:-agent-space}"
ECR_REPO="${ECR_REPO:-agent-space}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"

die() { echo "deploy: $*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker is not installed"
command -v aws >/dev/null 2>&1 || die "the aws CLI is not installed"
[ -f "$ENV_FILE" ] || die "no .env at $ENV_FILE"

ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)" \
  || die "cannot resolve the AWS account; check your credentials"
REGISTRY="$ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com"
IMAGE="$REGISTRY/$ECR_REPO:$IMAGE_TAG"

echo "account:  $ACCOUNT_ID"
echo "region:   $AWS_REGION"
echo "image:    $IMAGE"
echo

echo "==> Ensuring the ECR repository exists"
aws ecr describe-repositories --repository-names "$ECR_REPO" --region "$AWS_REGION" >/dev/null 2>&1 \
  || aws ecr create-repository --repository-name "$ECR_REPO" --region "$AWS_REGION" >/dev/null

echo "==> Logging in to ECR"
aws ecr get-login-password --region "$AWS_REGION" \
  | docker login --username AWS --password-stdin "$REGISTRY"

echo "==> Building (linux/amd64 — App Runner does not run arm64 images)"
docker build --platform linux/amd64 -t "$IMAGE" "$APP_DIR"

echo "==> Pushing"
docker push "$IMAGE"

# Turn .env into the JSON map App Runner wants, skipping comments and blanks.
echo "==> Collecting runtime configuration from $ENV_FILE"
RUNTIME_ENV="$(ENV_FILE="$ENV_FILE" python3 - <<'PY'
import json, os

wanted = {
    "DATABASE_URL",
    "OPENROUTER_API_KEY",
    "OPENROUTER_MODEL",
    "OPENROUTER_EMBEDDING_MODEL",
    "OPENROUTER_EMBEDDING_DIMENSIONS",
    "COCKROACH_API_KEY",
    "COCKROACH_CLUSTER_ID",
    "COCKROACH_MCP_URL",
}

env = {}
with open(os.environ["ENV_FILE"]) as f:
    for line in f:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key, value = key.strip(), value.strip().strip('"').strip("'")
        if key in wanted and value:
            env[key] = value

missing = {"DATABASE_URL", "OPENROUTER_API_KEY"} - env.keys()
if missing:
    raise SystemExit(f"deploy: .env is missing {', '.join(sorted(missing))}")
if "COCKROACH_API_KEY" not in env:
    print("deploy: warning — COCKROACH_API_KEY unset, /agent and /tools will 503", flush=True)

print(json.dumps(env))
PY
)"

SERVICE_ARN="$(aws apprunner list-services --region "$AWS_REGION" \
  --query "ServiceSummaryList[?ServiceName=='$SERVICE_NAME'].ServiceArn | [0]" \
  --output text 2>/dev/null || echo "None")"

# App Runner needs a role it can assume to pull from a private ECR repository.
ACCESS_ROLE_ARN="${APPRUNNER_ACCESS_ROLE_ARN:-arn:aws:iam::$ACCOUNT_ID:role/service-role/AppRunnerECRAccessRole}"

SOURCE_CONFIG="$(python3 - "$IMAGE" "$ACCESS_ROLE_ARN" "$RUNTIME_ENV" <<'PY'
import json, sys

image, role, runtime_env = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])
print(json.dumps({
    "ImageRepository": {
        "ImageIdentifier": image,
        "ImageRepositoryType": "ECR",
        "ImageConfiguration": {
            "Port": "8080",
            "RuntimeEnvironmentVariables": runtime_env,
        },
    },
    "AutoDeploymentsEnabled": False,
    "AuthenticationConfiguration": {"AccessRoleArn": role},
}))
PY
)"

if [ "$SERVICE_ARN" = "None" ] || [ -z "$SERVICE_ARN" ]; then
  echo "==> Creating App Runner service $SERVICE_NAME"
  SERVICE_ARN="$(aws apprunner create-service \
    --service-name "$SERVICE_NAME" \
    --region "$AWS_REGION" \
    --source-configuration "$SOURCE_CONFIG" \
    --health-check-configuration 'Protocol=HTTP,Path=/ping,Interval=10,Timeout=5,HealthyThreshold=1,UnhealthyThreshold=5' \
    --instance-configuration 'Cpu=1024,Memory=2048' \
    --query 'Service.ServiceArn' --output text)"
else
  echo "==> Updating App Runner service $SERVICE_NAME"
  aws apprunner update-service \
    --service-arn "$SERVICE_ARN" \
    --region "$AWS_REGION" \
    --source-configuration "$SOURCE_CONFIG" >/dev/null
  aws apprunner start-deployment --service-arn "$SERVICE_ARN" --region "$AWS_REGION" >/dev/null
fi

echo "==> Waiting for the service to become RUNNING"
for _ in $(seq 1 60); do
  STATUS="$(aws apprunner describe-service --service-arn "$SERVICE_ARN" --region "$AWS_REGION" \
    --query 'Service.Status' --output text)"
  echo "    status: $STATUS"
  case "$STATUS" in
    RUNNING) break ;;
    CREATE_FAILED|DELETE_FAILED) die "service entered $STATUS; check the App Runner console logs" ;;
  esac
  sleep 15
done

URL="https://$(aws apprunner describe-service --service-arn "$SERVICE_ARN" --region "$AWS_REGION" \
  --query 'Service.ServiceUrl' --output text)"

echo
echo "Deployed: $URL"
echo
echo "Verify:"
echo "  curl $URL/ping"
echo "  curl $URL/tools"
echo "  API_URL=$URL $SCRIPT_DIR/demo.sh"
