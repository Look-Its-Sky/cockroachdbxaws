# Deployment

There is one supported AWS deployment for this hackathon: one Amazon Linux 2023
EC2 instance running Docker Compose. It pulls logs from CloudWatch, processes
them against its local durable journal, stores incidents in managed CockroachDB,
and publishes agent assignments to Amazon SQS.

```text
CloudWatch Logs -> EC2: cloudwatch role -> CockroachDB
                                      \-> transactional outbox
CockroachDB -> EC2: outbox role -> SQS -> agent orchestrator
```

The instance accepts no inbound network traffic. AWS credentials come from its
IAM instance profile, and administration uses Systems Manager Session Manager.
Its encrypted gp3 root disk holds the Docker journal and checkpoint volumes.
This is deliberately a single-instance deployment; it is easy to demonstrate
and preserves the rule that one process owns each non-shared journal.

Local `compose.yaml` remains the development and test environment only. It is
not a second production deployment method.

## Prerequisites

- An AWS account with a default VPC and at least one default public subnet.
- AWS CLI authentication with permission to create EC2, IAM, SQS, security
  group, Elastic IP, and Systems Manager resources.
- Terraform 1.5 or newer.
- A managed CockroachDB cluster in the same region, with a database named
  `static_log_analysis` and a TLS connection string.
- Existing CloudWatch log groups in that region and account.
- The repository commit or branch to deploy must be pushed to GitHub.

The instance uses a stable Elastic IP for outbound connections. Its security
group has no inbound rules; the address exists so CockroachDB Cloud can
authorize one stable `/32` source.

## 1. Authenticate to AWS

Use your organization's normal identity flow. For AWS IAM Identity Center:

```bash
aws configure sso --profile static-log-analysis
aws sso login --profile static-log-analysis
export AWS_PROFILE=static-log-analysis
export AWS_REGION=us-east-1
aws sts get-caller-identity
```

Do not create long-lived access keys for the EC2 service. Its instance profile
provides temporary credentials for CloudWatch, SQS, and Systems Manager.

## 2. Store the CockroachDB connection string

Store the provider-issued TLS DSN in SSM Parameter Store before applying
Terraform. The value never belongs in a Terraform variable, `.tfvars`, user
data, or Git.

```bash
read -rsp 'CockroachDB TLS DSN: ' CRDB_DSN
echo
aws ssm put-parameter \
  --region "$AWS_REGION" \
  --name /static-log-analysis/database-dsn \
  --type SecureString \
  --value "$CRDB_DSN" \
  --overwrite
unset CRDB_DSN
```

The DSN must use TLS. The local `sslmode=disable` connection string is never
valid for this deployment.

## 3. Configure the deployment

From the service directory:

```bash
cd services/static-log-analysis/infra/aws
cp terraform.tfvars.example terraform.tfvars
```

Edit only `terraform.tfvars`. Set:

- the AWS region and tenant identity;
- a pushed branch, tag, or commit in `repository_ref`;
- every CloudWatch log group and its trusted service/environment identity;
- the account number inside each exact log-group ARN.

The module rejects cross-account, cross-region, duplicate, wildcard, or
ambiguous log-group declarations.

## 4. Create AWS resources

```bash
terraform init
terraform fmt -check
terraform validate
terraform plan -out=static-log-analysis.tfplan
terraform apply static-log-analysis.tfplan
```

This creates exactly one EC2 instance, an outbound-only security group, a
stable Elastic IP, its least-privilege IAM instance profile, the assignment
queue, and the dead-letter queue. EC2 user data installs Docker, checks out the
declared repository revision, builds the image, retrieves the DSN directly from
SSM, runs migrations, and starts the `cloudwatch` and `outbox` containers.

## 5. Authorize CockroachDB and start the service

Get the stable outbound IP:

```bash
terraform output -raw instance_public_ip
```

Authorize that address as a `/32` in CockroachDB Cloud. The first bootstrap may
fail until this authorization exists. Connect through Session Manager and
restart it after the allowlist is updated:

```bash
$(terraform output -raw start_session_command)
```

Inside the session:

```bash
sudo systemctl restart static-log-analysis
sudo systemctl status static-log-analysis --no-pager
sudo docker compose \
  --project-name static-log-analysis-aws \
  --file /opt/static-log-analysis/compose.yaml \
  ps
```

## 6. Verify the complete path

Inside the Session Manager shell:

```bash
curl -fsS http://127.0.0.1:9464/readyz
curl -fsS http://127.0.0.1:9465/readyz
curl -fsS http://127.0.0.1:9464/metrics | grep static_log_analysis

sudo docker compose \
  --project-name static-log-analysis-aws \
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
safe `SLA_REPOSITORY_REF` value in `/etc/static-log-analysis.env`, and restart
`static-log-analysis`. The startup script fetches and builds that revision while
preserving the existing Docker volumes.

Changing `repository_ref` in Terraform replaces the EC2 instance because user
data is an immutable bootstrap input. That also deletes the instance's journal
and checkpoints, so use Terraform replacement only for a deliberate reset.

To remove the AWS resources:

```bash
terraform destroy
```

Destroying the EC2 instance deletes its root disk and therefore its journal and
checkpoints. CockroachDB data and the separately created SSM SecureString are
not deleted by this module.
