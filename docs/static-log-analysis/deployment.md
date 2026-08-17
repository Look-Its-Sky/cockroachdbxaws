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
- AWS CLI authentication with permission to create EC2, IAM, SQS, security
  group, Elastic IP, and Systems Manager resources.
- Terraform 1.5 or newer.
- A managed CockroachDB cluster in the same physical region, with a database
  named `static_log_analysis` and a TLS connection string. The database may
  have no regional metadata or exactly one CockroachDB region; databases with
  multiple or secondary regions are rejected.
- Existing CloudWatch log groups in that region and account.
- The repository commit or branch to deploy must be pushed to GitHub.

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
- a pushed branch, tag, or commit in `repository_ref`;
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
least-privilege IAM profile, the assignment and dead-letter queues, and—when
`deploy_demo=true`—the separate demo instance and payment log group. Bootstrap
installs Docker, checks out the declared repository revision, and installs the
service unit and database setup helper. It does not start the analysis
containers until the database connection is installed.

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
helper again rotates the connection and restarts the already-deployed service.

Routine systemd starts never fetch source or rebuild an image. This keeps a
restart from silently changing the deployed release. To deploy the configured
`repository_ref` explicitly after it has been pushed, run:

```bash
sudo deploy-static-log-analysis
```

That helper fetches the configured Git revision, builds it successfully, and
only then restarts the service on the new local image.

## 5. Configure the optional authenticated dashboard

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
stores it in `/etc/static-log-analysis/dashboard.env` with mode `0600`, builds
the optional dashboard containers, applies the SQLite authentication schema,
and starts Caddy. The second command securely prompts for the first
administrator's email and password; the password is sent over the container's
standard input and is not placed in a command argument or environment file.

Public account creation is disabled. Run the administrator command again to
add another approved operator. Dashboard identities and sessions live only in
the local `dashboard-auth` SQLite volume; CockroachDB remains the independent
datastore for all analysis state.

Open the value printed by `terraform output -raw dashboard_url`. The internet
can reach the sign-in screen, but a missing, banned, or non-admin account is
denied. See [dashboard.md](dashboard.md) for the exact safe-data contract.

## 6. Verify the complete path

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

For a hackathon code refresh, connect through Session Manager, replace the
safe `SLA_REPOSITORY_REF` value in `/etc/static-log-analysis.env`, and run
`sudo deploy-static-log-analysis`. The deployment helper fetches and builds that
revision while preserving the existing Docker volumes.

Changing `repository_ref` updates the desired bootstrap configuration without
replacing the analysis EC2 instance. Run `sudo deploy-static-log-analysis` to
fetch and deploy the new ref. When the bootstrap template itself changes, an
operator must deliberately refresh it through Session Manager; this preserves
the local journal, dashboard auth database, checkpoints, and installed
CockroachDB connection. Replace the instance only for a deliberate reset.

To remove the AWS resources:

```bash
terraform destroy
```

Destroying the EC2 instance deletes its root disk and therefore its journal,
checkpoints, and root-only `secrets.env`. Data already stored in managed
CockroachDB is not deleted by this module.
