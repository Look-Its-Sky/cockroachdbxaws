data "aws_region" "current" {}
data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_vpc" "default" {
  default = true
}
data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
}
data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-kernel-6.1-x86_64"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }

  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

check "regional_boundary" {
  assert {
    condition     = data.aws_region.current.region == var.region
    error_message = "The AWS provider region must equal var.region; cross-region data movement is forbidden."
  }
}

check "cloudwatch_source_boundaries" {
  assert {
    condition = alltrue([
      for source in values(local.cloudwatch_sources) :
      source.log_group_arn == "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${source.log_group_name}"
    ]) && length(distinct([for source in values(local.cloudwatch_sources) : source.log_group_arn])) == length(local.cloudwatch_sources)
    error_message = "CloudWatch sources must be unique canonical log-group ARNs in this AWS account and region."
  }
}

check "at_least_one_cloudwatch_source" {
  assert {
    condition     = length(local.cloudwatch_sources) > 0
    error_message = "Configure at least one existing CloudWatch source or set deploy_demo=true."
  }
}

locals {
  tags = merge(var.tags, {
    Component = "static-log-analysis"
    Region    = var.region
  })
  demo_log_group_name = "/static-log-analysis/demo/payment"
  demo_log_group_arn  = "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${local.demo_log_group_name}"
  demo_cloudwatch_sources = var.deploy_demo ? {
    demo_payment = {
      log_group_arn  = local.demo_log_group_arn
      log_group_name = local.demo_log_group_name
      service        = "payment"
      environment    = "demo"
    }
  } : {}
  cloudwatch_sources = merge(var.cloudwatch_sources, local.demo_cloudwatch_sources)
  cloudwatch_groups = join(",", [
    for key in sort(keys(local.cloudwatch_sources)) :
    "${local.cloudwatch_sources[key].log_group_name}=${local.cloudwatch_sources[key].service}=${local.cloudwatch_sources[key].environment}"
  ])
  dashboard_ingress_ports = var.dashboard_enabled ? toset([80, 443]) : toset([])
}

resource "aws_cloudwatch_log_group" "demo_payment" {
  count             = var.deploy_demo ? 1 : 0
  name              = local.demo_log_group_name
  retention_in_days = 7
  tags              = merge(local.tags, { Service = "payment", Environment = "demo" })
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

data "aws_iam_policy_document" "service_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "service" {
  name               = "${var.name}-service"
  assume_role_policy = data.aws_iam_policy_document.service_trust.json
  tags               = local.tags
}

resource "aws_iam_instance_profile" "service" {
  name = "${var.name}-service"
  role = aws_iam_role.service.name
}

resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.service.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "service" {
  statement {
    sid       = "ReadConfiguredLogGroups"
    effect    = "Allow"
    actions   = ["logs:FilterLogEvents"]
    resources = [for source in values(local.cloudwatch_sources) : "${source.log_group_arn}:*"]
  }

  statement {
    sid       = "PublishCommittedAssignments"
    effect    = "Allow"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.assignments.arn, aws_sqs_queue.dead_letter.arn]
  }

  statement {
    sid       = "InspectAssignmentQueueDepth"
    effect    = "Allow"
    actions   = ["sqs:GetQueueAttributes"]
    resources = [aws_sqs_queue.assignments.arn, aws_sqs_queue.dead_letter.arn]
  }
}

resource "aws_iam_role_policy" "service" {
  name   = "run-static-log-analysis"
  role   = aws_iam_role.service.id
  policy = data.aws_iam_policy_document.service.json
}

resource "aws_security_group" "service" {
  name_prefix = "${var.name}-"
  description = "No inbound access; administration uses SSM Session Manager"
  vpc_id      = data.aws_vpc.default.id

  dynamic "ingress" {
    for_each = local.dashboard_ingress_ports
    content {
      description = "Authenticated dashboard HTTPS and certificate redirect"
      from_port   = ingress.value
      to_port     = ingress.value
      protocol    = "tcp"
      cidr_blocks = [var.dashboard_ingress_cidr]
    }
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.tags
}

resource "aws_instance" "service" {
  ami                         = data.aws_ami.al2023.id
  instance_type               = var.instance_type
  subnet_id                   = sort(data.aws_subnets.default.ids)[0]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.service.name
  vpc_security_group_ids      = [aws_security_group.service.id]

  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    compose_base64         = filebase64("${path.module}/../../deploy/aws/compose.yaml")
    caddy_base64           = filebase64("${path.module}/../../deploy/aws/Caddyfile")
    region                 = var.region
    tenant_id              = var.tenant_id
    classification         = var.classification
    source_account         = data.aws_caller_identity.current.account_id
    credential_identity    = aws_iam_role.service.arn
    cloudwatch_groups      = local.cloudwatch_groups
    queue_url              = aws_sqs_queue.assignments.url
    dead_letter_queue_url  = aws_sqs_queue.dead_letter.url
    repository_url         = var.repository_url
    repository_ref         = var.repository_ref
    journal_max_bytes      = format("%.0f", var.journal_max_bytes)
    journal_min_free_bytes = format("%.0f", var.journal_min_free_bytes)
    dashboard_enabled      = var.dashboard_enabled
    dashboard_hostname     = var.dashboard_hostname
  })
  user_data_replace_on_change = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  root_block_device {
    encrypted             = true
    volume_type           = "gp3"
    volume_size           = var.root_volume_gib
    delete_on_termination = true
  }

  tags = merge(local.tags, { Name = var.name })

  depends_on = [aws_iam_role_policy.service, aws_iam_role_policy_attachment.ssm_core]
}

resource "aws_eip" "service" {
  domain = "vpc"
  tags   = merge(local.tags, { Name = var.name })
}

resource "aws_eip_association" "service" {
  instance_id   = aws_instance.service.id
  allocation_id = aws_eip.service.id
}

resource "aws_iam_role" "demo" {
  count              = var.deploy_demo ? 1 : 0
  name               = "${var.name}-demo"
  assume_role_policy = data.aws_iam_policy_document.service_trust.json
  tags               = merge(local.tags, { DeploymentRole = "demo" })
}

resource "aws_iam_instance_profile" "demo" {
  count = var.deploy_demo ? 1 : 0
  name  = "${var.name}-demo"
  role  = aws_iam_role.demo[0].name
}

resource "aws_iam_role_policy_attachment" "demo_ssm_core" {
  count      = var.deploy_demo ? 1 : 0
  role       = aws_iam_role.demo[0].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "demo" {
  count = var.deploy_demo ? 1 : 0

  statement {
    sid       = "WritePaymentContainerLogs"
    effect    = "Allow"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${local.demo_log_group_arn}:log-stream:*"]
  }
}

resource "aws_iam_role_policy" "demo" {
  count  = var.deploy_demo ? 1 : 0
  name   = "write-static-analysis-demo-logs"
  role   = aws_iam_role.demo[0].id
  policy = data.aws_iam_policy_document.demo[0].json
}

resource "aws_security_group" "demo" {
  count       = var.deploy_demo ? 1 : 0
  name_prefix = "${var.name}-demo-"
  description = "Operator-only access to the OpenTelemetry Demo frontend"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    description = "OpenTelemetry Demo frontend"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [var.demo_ingress_cidr]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.tags, { DeploymentRole = "demo" })
}

resource "aws_instance" "demo" {
  count                       = var.deploy_demo ? 1 : 0
  ami                         = data.aws_ami.al2023.id
  instance_type               = var.demo_instance_type
  subnet_id                   = sort(data.aws_subnets.default.ids)[0]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.demo[0].name
  vpc_security_group_ids      = [aws_security_group.demo[0].id]

  user_data = templatefile("${path.module}/demo-user-data.sh.tftpl", {
    region         = var.region
    log_group_name = local.demo_log_group_name
    repository_ref = var.demo_repository_ref
    demo_version   = var.demo_version
  })
  user_data_replace_on_change = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  root_block_device {
    encrypted             = true
    volume_type           = "gp3"
    volume_size           = 40
    delete_on_termination = true
  }

  tags = merge(local.tags, { Name = "${var.name}-demo", DeploymentRole = "demo" })

  depends_on = [
    aws_cloudwatch_log_group.demo_payment,
    aws_iam_role_policy.demo,
    aws_iam_role_policy_attachment.demo_ssm_core,
  ]
}

resource "aws_eip" "demo" {
  count  = var.deploy_demo ? 1 : 0
  domain = "vpc"
  tags   = merge(local.tags, { Name = "${var.name}-demo", DeploymentRole = "demo" })
}

resource "aws_eip_association" "demo" {
  count         = var.deploy_demo ? 1 : 0
  instance_id   = aws_instance.demo[0].id
  allocation_id = aws_eip.demo[0].id
}
