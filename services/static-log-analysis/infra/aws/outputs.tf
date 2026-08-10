output "instance_id" {
  description = "Use this ID with AWS Systems Manager Session Manager."
  value       = aws_instance.service.id
}

output "instance_public_ip" {
  description = "Stable outbound address to authorize in CockroachDB Cloud. The security group permits no inbound traffic."
  value       = aws_eip.service.public_ip
}

output "assignment_queue_url" {
  value = aws_sqs_queue.assignments.url
}

output "dead_letter_queue_url" {
  value = aws_sqs_queue.dead_letter.url
}

output "service_role_arn" {
  value = aws_iam_role.service.arn
}

output "start_session_command" {
  value = "aws ssm start-session --region ${var.region} --target ${aws_instance.service.id}"
}

output "cloudwatch_log_groups" {
  value = local.cloudwatch_groups
}
