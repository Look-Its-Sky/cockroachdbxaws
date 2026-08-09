variable "name" {
  description = "Name prefix for queues and IAM resources."
  type        = string
  default     = "static-log-analysis"
}

variable "region" {
  description = "Hard regional boundary. The configured AWS provider must use this region."
  type        = string

  validation {
    condition     = can(regex("^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$", var.region))
    error_message = "region must be an AWS region identifier."
  }
}

variable "eks_cluster_name" {
  description = "EKS cluster for Pod Identity associations; leave empty to create IAM resources without associations."
  type        = string
  default     = ""
}

variable "kubernetes_namespace" {
  description = "Namespace containing the Helm release."
  type        = string
  default     = "static-log-analysis"
}

variable "outbox_service_account" {
  description = "Service account used by the chart's outbox Deployment."
  type        = string
  default     = "static-log-analysis-outbox"
}

variable "agent_service_account" {
  description = "Service account used by the separate agent orchestrator."
  type        = string
  default     = ""
}

variable "cloudwatch_service_account" {
  description = "Service account used by the chart's combined CloudWatch polling and processing StatefulSet."
  type        = string
  default     = "static-log-analysis-cloudwatch"
}

variable "cloudwatch_sources" {
  description = "Exact same-account regional CloudWatch log groups and the deterministic identity assigned to each."
  type = map(object({
    log_group_arn  = string
    log_group_name = string
    service        = string
    environment    = string
  }))
  default = {}

  validation {
    condition = alltrue([
      for source in values(var.cloudwatch_sources) :
      source.log_group_arn != "" && can(regex("^[.\\-_/#A-Za-z0-9]+$", source.log_group_name)) &&
      source.service != "" && source.environment != "" &&
      !strcontains(source.log_group_name, "=") && !strcontains(source.log_group_name, ",") &&
      !strcontains(source.service, "=") && !strcontains(source.service, ",") &&
      !strcontains(source.environment, "=") && !strcontains(source.environment, ",")
    ])
    error_message = "Every CloudWatch source needs exact non-empty fields; names and identities cannot contain '=' or ','."
  }
}

variable "tags" {
  description = "Tags applied to AWS resources."
  type        = map(string)
  default     = {}
}
