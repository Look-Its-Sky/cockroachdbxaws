output "assignment_queue_url" {
  description = "Set as outbox.queueURL in Helm values."
  value       = aws_sqs_queue.assignments.url
}

output "assignment_queue_arn" {
  value = aws_sqs_queue.assignments.arn
}

output "dead_letter_queue_url" {
  description = "Set as outbox.deadLetterQueueURL in Helm values."
  value       = aws_sqs_queue.dead_letter.url
}

output "dead_letter_queue_arn" {
  value = aws_sqs_queue.dead_letter.arn
}

output "outbox_role_arn" {
  value = aws_iam_role.outbox.arn
}

output "agent_role_arn" {
  value = try(aws_iam_role.agent[0].arn, null)
}

output "cloudwatch_role_arn" {
  description = "Set as cloudwatch.credentialIdentity in Helm values."
  value       = try(aws_iam_role.cloudwatch[0].arn, null)
}

output "cloudwatch_log_groups" {
  description = "Deterministic role flag value represented by cloudwatch.sources in Helm."
  value = join(",", [
    for key in sort(keys(var.cloudwatch_sources)) :
    "${var.cloudwatch_sources[key].log_group_name}=${var.cloudwatch_sources[key].service}=${var.cloudwatch_sources[key].environment}"
  ])
}
