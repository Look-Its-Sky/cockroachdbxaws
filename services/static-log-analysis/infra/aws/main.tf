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
data "aws_ssm_parameter" "al2023_ami" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
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
      for source in values(var.cloudwatch_sources) :
      source.log_group_arn == "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${source.log_group_name}"
    ]) && length(distinct([for source in values(var.cloudwatch_sources) : source.log_group_arn])) == length(var.cloudwatch_sources)
    error_message = "CloudWatch sources must be unique canonical log-group ARNs in this AWS account and region."
  }
}

locals {
  tags = merge(var.tags, {
    Component = "static-log-analysis"
    Region    = var.region
  })
  database_parameter_arn = "arn:${data.aws_partition.current.partition}:ssm:${var.region}:${data.aws_caller_identity.current.account_id}:parameter${var.database_dsn_parameter_name}"
  cloudwatch_groups = join(",", [
    for key in sort(keys(var.cloudwatch_sources)) :
    "${var.cloudwatch_sources[key].log_group_name}=${var.cloudwatch_sources[key].service}=${var.cloudwatch_sources[key].environment}"
  ])
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
    resources = [for source in values(var.cloudwatch_sources) : source.log_group_arn]
  }

  statement {
    sid       = "PublishCommittedAssignments"
    effect    = "Allow"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.assignments.arn, aws_sqs_queue.dead_letter.arn]
  }

  statement {
    sid       = "ReadCockroachConnection"
    effect    = "Allow"
    actions   = ["ssm:GetParameter"]
    resources = [local.database_parameter_arn]
  }

  dynamic "statement" {
    for_each = var.database_kms_key_arn == "" ? [] : [var.database_kms_key_arn]
    content {
      sid       = "DecryptCockroachConnection"
      effect    = "Allow"
      actions   = ["kms:Decrypt"]
      resources = [statement.value]
    }
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

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.tags
}

resource "aws_instance" "service" {
  ami                         = data.aws_ssm_parameter.al2023_ami.value
  instance_type               = var.instance_type
  subnet_id                   = sort(data.aws_subnets.default.ids)[0]
  associate_public_ip_address = true
  iam_instance_profile        = aws_iam_instance_profile.service.name
  vpc_security_group_ids      = [aws_security_group.service.id]

  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    compose_base64          = filebase64("${path.module}/../../deploy/aws/compose.yaml")
    region                  = var.region
    tenant_id               = var.tenant_id
    classification          = var.classification
    source_account          = data.aws_caller_identity.current.account_id
    credential_identity     = aws_iam_role.service.arn
    cloudwatch_groups       = local.cloudwatch_groups
    queue_url               = aws_sqs_queue.assignments.url
    dead_letter_queue_url   = aws_sqs_queue.dead_letter.url
    database_parameter_name = var.database_dsn_parameter_name
    repository_url          = var.repository_url
    repository_ref          = var.repository_ref
    journal_max_bytes       = format("%.0f", var.journal_max_bytes)
    journal_min_free_bytes  = format("%.0f", var.journal_min_free_bytes)
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
