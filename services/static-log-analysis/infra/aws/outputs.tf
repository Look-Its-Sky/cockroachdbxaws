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

output "dashboard_url" {
  description = "Authenticated operations dashboard URL, after DNS and local authentication are configured."
  value       = var.dashboard_enabled ? "https://${var.dashboard_hostname}" : null
}

output "demo_instance_id" {
  description = "Optional OpenTelemetry Demo instance ID."
  value       = try(aws_instance.demo[0].id, null)
}

output "demo_public_ip" {
  description = "Stable public address of the optional OpenTelemetry Demo host."
  value       = try(aws_eip.demo[0].public_ip, null)
}

output "demo_url" {
  description = "Browser URL for the optional OpenTelemetry Demo frontend."
  value       = try("http://${aws_eip.demo[0].public_ip}:8080", null)
}

output "demo_payment_log_group" {
  description = "CloudWatch log group watched as service=payment, environment=demo."
  value       = var.deploy_demo ? local.demo_log_group_name : null
}

output "demo_start_session_command" {
  description = "Session Manager command for the optional OpenTelemetry Demo host."
  value       = try("aws ssm start-session --region ${var.region} --target ${aws_instance.demo[0].id}", null)
}
