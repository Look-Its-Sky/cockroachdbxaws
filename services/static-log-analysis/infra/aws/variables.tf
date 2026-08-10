variable "name" {
  description = "Name prefix for the EC2 host, queues, and IAM resources."
  type        = string
  default     = "static-log-analysis"

  validation {
    condition     = can(regex("^[a-zA-Z0-9-]{3,40}$", var.name))
    error_message = "name must contain only letters, digits, and hyphens."
  }
}

variable "region" {
  description = "Hard regional boundary. The AWS provider and every source must use this region."
  type        = string

  validation {
    condition     = can(regex("^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$", var.region))
    error_message = "region must be an AWS region identifier."
  }
}

variable "tenant_id" {
  description = "Single tenant identity stored with every incident."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._-]+$", var.tenant_id))
    error_message = "tenant_id must contain only letters, digits, dots, underscores, and hyphens."
  }
}

variable "classification" {
  description = "Highest data classification accepted by this deployment."
  type        = string
  default     = "SENSITIVE"

  validation {
    condition     = contains(["PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED"], var.classification)
    error_message = "classification must be PUBLIC, INTERNAL, SENSITIVE, or RESTRICTED."
  }
}

variable "instance_type" {
  description = "EC2 size for the hackathon deployment."
  type        = string
  default     = "t3.small"
}

variable "root_volume_gib" {
  description = "Encrypted gp3 root disk containing Docker volumes, the journal, and checkpoints."
  type        = number
  default     = 40

  validation {
    condition     = var.root_volume_gib >= 20
    error_message = "root_volume_gib must be at least 20 GiB."
  }
}

variable "repository_url" {
  description = "Git repository the instance builds the service image from."
  type        = string
  default     = "https://github.com/Look-Its-Sky/cockroachdbxaws.git"

  validation {
    condition     = can(regex("^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\.git$", var.repository_url))
    error_message = "repository_url must be an HTTPS GitHub clone URL ending in .git."
  }
}

variable "repository_ref" {
  description = "Branch, tag, or commit fetched and built by the instance. Pin a commit for a repeatable demo."
  type        = string
  default     = "Static-Log-Analysis"

  validation {
    condition     = can(regex("^[A-Za-z0-9._/-]+$", var.repository_ref))
    error_message = "repository_ref contains unsupported characters."
  }
}

variable "database_dsn_parameter_name" {
  description = "Existing SSM SecureString parameter containing the managed CockroachDB TLS DSN. Its value is never placed in Terraform state or EC2 user data."
  type        = string
  default     = "/static-log-analysis/database-dsn"

  validation {
    condition     = can(regex("^/[A-Za-z0-9_.\\-/]+$", var.database_dsn_parameter_name))
    error_message = "database_dsn_parameter_name must be an absolute SSM parameter name."
  }
}

variable "database_kms_key_arn" {
  description = "Optional customer-managed KMS key ARN protecting the database DSN parameter. Leave empty for the AWS-managed SSM key."
  type        = string
  default     = ""
}

variable "cloudwatch_sources" {
  description = "Exact same-account regional CloudWatch log groups and deterministic identity assigned to each."
  type = map(object({
    log_group_arn  = string
    log_group_name = string
    service        = string
    environment    = string
  }))

  validation {
    condition = length(var.cloudwatch_sources) > 0 && alltrue([
      for source in values(var.cloudwatch_sources) :
      source.log_group_arn != "" && can(regex("^[.\\-_/#A-Za-z0-9]+$", source.log_group_name)) &&
      can(regex("^[A-Za-z0-9._-]+$", source.service)) &&
      can(regex("^[A-Za-z0-9._-]+$", source.environment)) &&
      !strcontains(source.log_group_name, "=") && !strcontains(source.log_group_name, ",") &&
      !strcontains(source.service, "=") && !strcontains(source.service, ",") &&
      !strcontains(source.environment, "=") && !strcontains(source.environment, ",")
    ])
    error_message = "At least one exact CloudWatch source is required; fields cannot contain '=' or ','."
  }
}

variable "journal_max_bytes" {
  description = "Maximum bytes accounted to the durable journal."
  type        = number
  default     = 16106127360
}

variable "journal_min_free_bytes" {
  description = "Disk headroom below which the journal refuses to grow."
  type        = number
  default     = 2147483648
}

variable "tags" {
  description = "Tags applied to AWS resources."
  type        = map(string)
  default     = {}
}
