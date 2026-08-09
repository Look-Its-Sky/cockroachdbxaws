# Regional AWS infrastructure

This Terraform module creates only the AWS resources owned by Static Log
Analysis:

- an encrypted SQS Standard agent-assignment queue;
- an encrypted dead-letter queue and restricted redrive relationship;
- a least-privilege outbox IAM role; and
- an optional least-privilege CloudWatch Logs reader role restricted to exact
  same-account regional log-group ARNs; and
- optional EKS Pod Identity associations for the outbox and separate agent
  orchestrator service accounts.

It does not create an EKS cluster, VPC, EBS storage class, CockroachDB cluster,
or Kubernetes secrets. Those remain owned by the deployment environment and the
official CockroachDB provider.

The AWS provider region is checked against `var.region`. A mismatch fails the
plan rather than creating a cross-region path accidentally.

## Usage

Use this directory as a module from the environment's existing Terraform root:

```hcl
module "static_log_analysis" {
  source = "../../services/static-log-analysis/infra/aws"

  region                = "us-east-1"
  eks_cluster_name      = "application-production"
  kubernetes_namespace  = "static-log-analysis"
  outbox_service_account = "static-log-analysis-outbox"

  cloudwatch_sources = {
    api = {
      log_group_arn  = "arn:aws:logs:us-east-1:111122223333:log-group:/aws/ecs/application/api"
      log_group_name = "/aws/ecs/application/api"
      service        = "api"
      environment    = "production"
    }
  }

  tags = {
    Environment = "production"
    Project     = "application"
  }
}
```

Configure the root AWS provider in the same region and use the environment's
normal remote backend and locking. Review a saved plan before applying:

```bash
terraform init
terraform fmt -check
terraform validate
terraform plan -out=static-log-analysis.tfplan
terraform apply static-log-analysis.tfplan
```

Transfer these non-secret outputs to the Helm values:

```bash
terraform output -raw assignment_queue_url
terraform output -raw dead_letter_queue_url
terraform output -raw outbox_role_arn
terraform output -raw cloudwatch_role_arn
terraform output -raw cloudwatch_log_groups
```

When `eks_cluster_name` is set, the module associates the outbox IAM role with
the chart's `static-log-analysis-outbox` service account. Do not add static AWS
access keys to the Helm release.

When `cloudwatch_sources` is non-empty, the module creates and associates a
second role whose only data-plane action is `logs:FilterLogEvents` on those
ARNs. `FilterLogEvents` is a log-group-level action, so these are canonical log
group ARNs without the stream-level `:*` suffix. A plan fails if an ARN belongs
to another account or region, does not match its declared log-group name, or is duplicated. Cross-account polling is
not implicit; it requires a separately reviewed assume-role design.
