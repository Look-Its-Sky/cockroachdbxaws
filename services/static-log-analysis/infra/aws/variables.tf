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

variable "machine_image_id" {
  description = "Region-specific Amazon Linux 2023 x86_64 AMI pinned for both EC2 hosts. Change only as a deliberate host replacement."
  type        = string

  validation {
    condition     = can(regex("^ami-[0-9a-f]{17}$", var.machine_image_id))
    error_message = "machine_image_id must be a modern 17-hex-character EC2 AMI ID."
  }
}

variable "deploy_demo" {
  description = "Create a separate public OpenTelemetry Demo host and a watched CloudWatch payment-service log group."
  type        = bool
  default     = false
}

variable "dashboard_enabled" {
  description = "Expose the authenticated operations dashboard through HTTPS on the analysis host."
  type        = bool
  default     = false
}

variable "dashboard_hostname" {
  description = "Public DNS hostname whose A record points to instance_public_ip. Required when dashboard_enabled is true."
  type        = string
  default     = ""

  validation {
    condition = !var.dashboard_enabled || can(regex(
      "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$",
      var.dashboard_hostname
    ))
    error_message = "dashboard_hostname must be a valid DNS hostname when the dashboard is enabled."
  }
}

variable "dashboard_ingress_cidr" {
  description = "CIDR allowed to reach the authenticated dashboard on ports 80 and 443. Use 0.0.0.0/0 for public internet access."
  type        = string
  default     = "0.0.0.0/0"

  validation {
    condition     = can(cidrhost(var.dashboard_ingress_cidr, 0))
    error_message = "dashboard_ingress_cidr must be a valid CIDR."
  }
}

variable "demo_instance_type" {
  description = "EC2 size for the optional OpenTelemetry Demo host."
  type        = string
  default     = "t3.large"
}

variable "demo_ingress_cidr" {
  description = "Single operator CIDR allowed to reach the optional demo frontend on port 8080."
  type        = string
  default     = "127.0.0.1/32"

  validation {
    condition = (can(cidrhost(var.demo_ingress_cidr, 0)) &&
      var.demo_ingress_cidr != "0.0.0.0/0" &&
    var.demo_ingress_cidr != "::/0")
    error_message = "demo_ingress_cidr must be a valid CIDR and cannot allow the entire internet."
  }
}

variable "demo_repository_ref" {
  description = "Immutable OpenTelemetry Demo commit deployed to the optional demo host."
  type        = string
  default     = "2e72d8bcdf754603e956406808630bc9663c992c"

  validation {
    condition     = can(regex("^[0-9a-f]{40}$", var.demo_repository_ref))
    error_message = "demo_repository_ref must be a full lowercase Git commit SHA."
  }
}

variable "demo_version" {
  description = "Immutable OpenTelemetry Demo container release used instead of mutable latest tags."
  type        = string
  default     = "3.0.0"

  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+$", var.demo_version))
    error_message = "demo_version must be a numeric MAJOR.MINOR.PATCH release."
  }
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

variable "cloudwatch_sources" {
  description = "Exact same-account regional CloudWatch log groups and deterministic identity assigned to each."
  type = map(object({
    log_group_arn  = string
    log_group_name = string
    service        = string
    environment    = string
  }))

  validation {
    condition = alltrue([
      for source in values(var.cloudwatch_sources) :
      source.log_group_arn != "" && can(regex("^[.\\-_/#A-Za-z0-9]+$", source.log_group_name)) &&
      can(regex("^[A-Za-z0-9._-]+$", source.service)) &&
      can(regex("^[A-Za-z0-9._-]+$", source.environment)) &&
      !strcontains(source.log_group_name, "=") && !strcontains(source.log_group_name, ",") &&
      !strcontains(source.service, "=") && !strcontains(source.service, ",") &&
      !strcontains(source.environment, "=") && !strcontains(source.environment, ",")
    ])
    error_message = "CloudWatch source fields cannot contain '=', or ','."
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
