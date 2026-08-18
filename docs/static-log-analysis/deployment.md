# Deployment

There is one supported AWS deployment for this hackathon. Terraform creates an
Amazon Linux 2023 analysis host running Docker Compose, regional SQS queues, and
optionally a separate OpenTelemetry Demo host. The analysis host pulls logs from
CloudWatch, processes them against its local durable journal, stores incidents
in managed CockroachDB, and publishes agent assignments to SQS.

```text
CloudWatch Logs -> EC2: cloudwatch role -> CockroachDB
                                      \-> transactional outbox
CockroachDB -> EC2: outbox role -> SQS -> agent orchestrator
```

The instance accepts no inbound network traffic. AWS credentials come from its
IAM instance profile, and administration uses Systems Manager Session Manager.
Its encrypted gp3 root disk holds the Docker journal and checkpoint volumes.
The analysis service itself is deliberately single-instance; one process owns
its non-shared journal. The optional demo gets a separate instance because it
is public, disposable, and more memory-intensive than the analysis service.

Local `compose.yaml` remains the development and test environment only. It is
not a second production deployment method.

## Prerequisites

- An AWS account with a default VPC and at least one default public subnet.
- AWS CLI authentication with permission to create EC2, IAM, ECR, SQS,
  security group, Elastic IP, and Systems Manager resources, and to push images
  to the three Terraform-owned ECR repositories.
- Terraform 1.5 or newer.
- Docker Buildx on the operator workstation or CI runner. Production images are
  never built on the stateful analysis host.
- A managed CockroachDB cluster in the same physical region, with a database
  named `static_log_analysis` and a TLS connection string. The database may
  have no regional metadata or exactly one CockroachDB region; databases with
  multiple or secondary regions are rejected.
- Existing CloudWatch log groups in that region and account.
- The exact repository commit to deploy must be committed, pushed, and checked
  out with no tracked or untracked changes.

The instance uses a stable Elastic IP for outbound connections. Its security
group has no inbound rules; the address exists so CockroachDB Cloud can
authorize one stable `/32` source.

## 1. Authenticate to AWS

Use an AWS CLI session that is already authenticated:

```bash
export AWS_REGION=<region-containing-your-cockroachdb-cluster>
aws sts get-caller-identity
```

Do not create long-lived access keys for the EC2 service. Its instance profile
provides temporary credentials for CloudWatch, SQS, and Systems Manager.

## 2. Configure the deployment

From the service directory:

```bash
cd services/static-log-analysis/infra/aws
cp terraform.tfvars.example terraform.tfvars
```

Edit only `terraform.tfvars`. Set:

- the AWS region and tenant identity;
- the exact Amazon Linux 2023 x86_64 AMI already approved for that region;
- every CloudWatch log group and its trusted service/environment identity;
- the account number inside each exact log-group ARN.

The module rejects cross-account, cross-region, duplicate, wildcard, or
ambiguous log-group declarations.
The AMI is deliberately pinned. Changing `machine_image_id` replaces both EC2
hosts and can delete their local journal, checkpoints, and dashboard-auth
database, so an application release must leave it unchanged. Treat an AMI
upgrade as a separate host-migration change with backups and explicit review.
The generated IAM policy grants `logs:FilterLogEvents` to each exact configured
log group plus its required `:*` stream suffix; it does not grant account-wide
CloudWatch Logs access.

## 3. Create AWS resources

```bash
terraform init
terraform fmt -check
terraform validate
deployment_workspace="account-$(aws sts get-caller-identity --query Account --output text)"
terraform workspace select "$deployment_workspace"
terraform workspace show
terraform plan -out=static-log-analysis.tfplan
terraform apply static-log-analysis.tfplan
```

Use one account-specific workspace and verify it before every plan or apply.
Never continue from an empty `default` workspace when this account already has
a deployment: Terraform would treat the existing resources as unmanaged and
propose duplicates. If the account workspace does not exist, stop and look for
the prior state before creating it. Only a confirmed first deployment may run
`terraform workspace new "$deployment_workspace"`.

This creates the outbound-only analysis instance, its stable Elastic IP and
least-privilege IAM profile, the assignment and dead-letter queues, three
private ECR repositories with immutable tags, and—when `deploy_demo=true`—the
separate demo instance and payment log group. Bootstrap installs Docker Compose,
the service unit, and root-only setup and image-cutover helpers. It does not
clone source or compile an application on the host.

## 4. Authorize CockroachDB and install the database secret

Get the stable outbound IP:

```bash
terraform output -raw instance_public_ip
```

Authorize that address as a `/32` in CockroachDB Cloud. In the cluster's
**Connect** dialog, copy the general connection string. Then connect through
Session Manager:

```bash
$(terraform output -raw start_session_command)
```

Inside the session:

```bash
sudo set-static-log-analysis-database-dsn
sudo systemctl status static-log-analysis --no-pager
sudo docker compose \
  --project-name static-log-analysis-aws \
  --env-file /etc/static-log-analysis/secrets.env \
  --file /opt/static-log-analysis/compose.yaml \
  ps
```

The helper prompts without echoing the provider-issued DSN. The DSN must be a
`postgresql://` URL using `sslmode=verify-full`. CockroachDB Cloud's certificate
chain is verified using the system CA bundle already installed in the service
image, so the helper replaces any operator-computer certificate path with
`sslrootcert=system`.

Create the dedicated database first if it does not exist:

```sql
CREATE DATABASE static_log_analysis;
```

CockroachDB Cloud may automatically assign the cluster's one region as the
database primary region. That is supported. Startup verifies that it is the
only database region and that zone-survival placement is in use; it refuses a
database that could place service data across regions.

The DSN format is:

```text
postgresql://USERNAME:URL_ENCODED_PASSWORD@HOST:26257/static_log_analysis?sslmode=verify-full&sslrootcert=system&autocommit_before_ddl=false
```

The DSN is stored only in `/etc/static-log-analysis/secrets.env`, owned by
`root:root` with mode `0600` on the encrypted EC2 root disk. It never enters
Terraform state, `.tfvars`, EC2 user data, Git, or shell history. Running the
helper again rotates the connection and restarts an already-deployed release.
On a new host it records the secret and waits for the first digest-pinned image
release.

## 5. Publish and deploy an immutable application release

Run the full release gates before publishing. From the Terraform directory on
the operator workstation, collect the repository outputs without copying them
into source files:

```bash
release_sha=$(git -C ../../../.. rev-parse HEAD)
repositories=$(terraform output -json release_image_repositories)
analysis_repository=$(printf '%s' "$repositories" | jq -r .analysis)
dashboard_repository=$(printf '%s' "$repositories" | jq -r .dashboard)
dashboard_admin_repository=$(printf '%s' "$repositories" | jq -r '."dashboard-admin"')

../../scripts/publish-aws-images.sh \
  "$AWS_REGION" \
  "$analysis_repository" \
  "$dashboard_repository" \
  "$dashboard_admin_repository" \
  "$release_sha"
```

The publisher refuses a dirty checkout or a SHA other than `HEAD`. It builds
all three Linux/amd64 artifacts off-host, pushes the same immutable commit tag
to each repository, resolves the registry digests, and prints the three digest
references needed for cutover. Its checksum-pinned Buildx plugin and ECR login
live in a disposable Docker configuration that is removed on exit. Record the
values in the private release runbook; do not add an account-specific
repository URL to this public repo.

For an existing instance created before digest-based releases, apply Terraform
first and refresh the in-place bootstrap through Session Manager. Retrieve the
new EC2 user data with IMDSv2 and run it once; this preserves the encrypted disk,
Docker volumes, database secret, dashboard secret, and release history:

```bash
token=$(curl -fsS -X PUT \
  -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' \
  http://169.254.169.254/latest/api/token)
curl -fsS -H "X-aws-ec2-metadata-token: $token" \
  http://169.254.169.254/latest/user-data > /tmp/static-log-analysis-bootstrap
sudo bash /tmp/static-log-analysis-bootstrap
rm -f /tmp/static-log-analysis-bootstrap
```

Then deploy the publisher's exact values inside the Session Manager shell:

```bash
sudo deploy-static-log-analysis-images \
  '<analysis-repository>@sha256:<digest>' \
  '<dashboard-repository>@sha256:<digest>' \
  '<dashboard-admin-repository>@sha256:<digest>' \
  '<40-character-release-sha>'
```

The helper rejects a digest from any repository other than the three created by
this Terraform state. Before altering the running release it logs in with the
instance profile, pulls and inspects all images, and runs both migrations. It
then restarts the Compose project and allows 120 seconds for each required
service to become healthy. On failure it restores the prior root-only
`release.env`, restarts the prior digests, and exits unsuccessfully. The first
digest-based release has no digest-based predecessor; if it fails, the helper
stops the candidate and reports that manual recovery is required.

Routine systemd starts never authenticate to ECR, fetch source, build an image,
or change the release selection.

## 6. Configure the optional authenticated dashboard

Set these Terraform values and apply the reviewed plan:

```hcl
dashboard_enabled      = true
dashboard_hostname     = "logs.example.com"
dashboard_ingress_cidr = "0.0.0.0/0"
```

Create an `A` record for `dashboard_hostname` pointing to
`instance_public_ip`. After DNS resolves, use Session Manager and run:

```bash
sudo set-static-log-analysis-dashboard-auth
sudo create-static-log-analysis-dashboard-admin
```

The first command generates a high-entropy Better Auth secret on the host,
stores it in `/etc/static-log-analysis/dashboard.env` with mode `0600`, applies
the SQLite authentication schema from the already-pulled admin image, and
starts the dashboard and Caddy. The second command securely prompts for the first
administrator's email and password; the password is sent over the container's
standard input and is not placed in a command argument or environment file.

Public account creation is disabled. Run the administrator command again to
add another approved operator. Dashboard identities and sessions live only in
the local `dashboard-auth` SQLite volume; CockroachDB remains the independent
datastore for all analysis state.

Open the value printed by `terraform output -raw dashboard_url`. The internet
can reach the sign-in screen, but a missing, banned, or non-admin account is
denied. See [dashboard.md](dashboard.md) for the exact safe-data contract.

## 7. Verify the complete path

Inside the Session Manager shell:

```bash
curl -fsS http://127.0.0.1:9464/readyz
curl -fsS http://127.0.0.1:9465/readyz
curl -fsS http://127.0.0.1:9464/metrics | grep static_log_analysis

sudo docker compose \
  --project-name static-log-analysis-aws \
  --env-file /etc/static-log-analysis/secrets.env \
  --file /opt/static-log-analysis/compose.yaml \
  logs --tail=100 cloudwatch outbox
```

After the configured rules elect an investigation, inspect queue depth without
receiving or hiding a message:

```bash
aws sqs get-queue-attributes \
  --region "$AWS_REGION" \
  --queue-url "$(terraform output -raw assignment_queue_url)" \
  --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible
```

The agent orchestrator remains a separate SQS consumer. Until one is running,
assignments remain in the queue.

## Updating and removing it

For an application refresh, publish the next clean commit and call
`deploy-static-log-analysis-images` with its three resolved digests. Do not edit
`release.env` directly. When the bootstrap template itself changes, apply the
Terraform plan and deliberately refresh the current user data through Session
Manager; this preserves the local journal, dashboard auth database,
checkpoints, installed CockroachDB connection, and prior digest selection.
Replace the instance only for a deliberate state reset.

To remove the AWS resources:

```bash
terraform destroy
```

Destroying the EC2 instance deletes its root disk and therefore its journal,
checkpoints, and root-only `secrets.env`. Data already stored in managed
CockroachDB is not deleted by this module.
Non-empty ECR repositories are not force-deleted; removing published release
artifacts requires a separate, explicit registry cleanup operation.
