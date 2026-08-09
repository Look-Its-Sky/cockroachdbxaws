data "aws_region" "current" {}
data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

check "regional_boundary" {
  assert {
    condition     = data.aws_region.current.region == var.region
    error_message = "The AWS provider region must equal var.region; cross-region queues are forbidden."
  }
}

check "cloudwatch_source_boundaries" {
  assert {
    condition = alltrue([
      for source in values(var.cloudwatch_sources) :
      source.log_group_arn == "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${source.log_group_name}"
    ]) && length(distinct([for source in values(var.cloudwatch_sources) : source.log_group_arn])) == length(var.cloudwatch_sources)
    error_message = "CloudWatch sources must be unique canonical log-group ARNs in the configured region and AWS account; stream wildcards are not accepted."
  }
}

locals {
  tags = merge(var.tags, {
    Component = "static-log-analysis"
    Region    = var.region
  })
}

resource "aws_sqs_queue" "dead_letter" {
  name                      = "${var.name}-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.tags, { QueueRole = "dead-letter" })
}

resource "aws_sqs_queue" "assignments" {
  name                       = var.name
  message_retention_seconds  = 1209600
  receive_wait_time_seconds  = 20
  visibility_timeout_seconds = 900
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead_letter.arn
    maxReceiveCount     = 5
  })
  tags = merge(local.tags, { QueueRole = "agent-assignments" })
}

resource "aws_sqs_queue_redrive_allow_policy" "dead_letter" {
  queue_url = aws_sqs_queue.dead_letter.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.assignments.arn]
  })
}

data "aws_iam_policy_document" "pod_identity_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "outbox" {
  name               = "${var.name}-outbox"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_trust.json
  tags               = local.tags
}

data "aws_iam_policy_document" "outbox" {
  statement {
    sid       = "PublishCommittedAssignments"
    effect    = "Allow"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.assignments.arn, aws_sqs_queue.dead_letter.arn]
  }
}

resource "aws_iam_role_policy" "outbox" {
  name   = "publish-assignments"
  role   = aws_iam_role.outbox.id
  policy = data.aws_iam_policy_document.outbox.json
}

resource "aws_eks_pod_identity_association" "outbox" {
  count           = var.eks_cluster_name == "" ? 0 : 1
  cluster_name    = var.eks_cluster_name
  namespace       = var.kubernetes_namespace
  service_account = var.outbox_service_account
  role_arn        = aws_iam_role.outbox.arn
}

resource "aws_iam_role" "cloudwatch" {
  count              = length(var.cloudwatch_sources) == 0 ? 0 : 1
  name               = "${var.name}-cloudwatch"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_trust.json
  tags               = local.tags
}

data "aws_iam_policy_document" "cloudwatch" {
  count = length(var.cloudwatch_sources) == 0 ? 0 : 1

  statement {
    sid       = "ReadConfiguredLogGroups"
    effect    = "Allow"
    actions   = ["logs:FilterLogEvents"]
    resources = [for source in values(var.cloudwatch_sources) : source.log_group_arn]
  }
}

resource "aws_iam_role_policy" "cloudwatch" {
  count  = length(var.cloudwatch_sources) == 0 ? 0 : 1
  name   = "read-configured-log-groups"
  role   = aws_iam_role.cloudwatch[0].id
  policy = data.aws_iam_policy_document.cloudwatch[0].json
}

resource "aws_eks_pod_identity_association" "cloudwatch" {
  count           = var.eks_cluster_name != "" && length(var.cloudwatch_sources) > 0 ? 1 : 0
  cluster_name    = var.eks_cluster_name
  namespace       = var.kubernetes_namespace
  service_account = var.cloudwatch_service_account
  role_arn        = aws_iam_role.cloudwatch[0].arn
}

resource "aws_iam_role" "agent" {
  count              = var.agent_service_account == "" ? 0 : 1
  name               = "${var.name}-agent-consumer"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_trust.json
  tags               = local.tags
}

data "aws_iam_policy_document" "agent" {
  statement {
    sid    = "ConsumeAgentAssignments"
    effect = "Allow"
    actions = [
      "sqs:ChangeMessageVisibility",
      "sqs:DeleteMessage",
      "sqs:GetQueueAttributes",
      "sqs:ReceiveMessage"
    ]
    resources = [aws_sqs_queue.assignments.arn]
  }
}

resource "aws_iam_role_policy" "agent" {
  count  = var.agent_service_account == "" ? 0 : 1
  name   = "consume-assignments"
  role   = aws_iam_role.agent[0].id
  policy = data.aws_iam_policy_document.agent.json
}

resource "aws_eks_pod_identity_association" "agent" {
  count           = var.eks_cluster_name != "" && var.agent_service_account != "" ? 1 : 0
  cluster_name    = var.eks_cluster_name
  namespace       = var.kubernetes_namespace
  service_account = var.agent_service_account
  role_arn        = aws_iam_role.agent[0].arn
}
