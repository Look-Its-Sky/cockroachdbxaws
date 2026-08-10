# AWS deployment module

This module implements the repository's one supported hackathon deployment:
one Amazon Linux 2023 EC2 instance running the production Docker Compose file.
It also creates regional SQS queues, a no-ingress security group, a stable
Elastic IP, and the instance's least-privilege IAM role.

Use the [single deployment guide](../../../docs/static-log-analysis/deployment.md)
for the complete sequence. Do not put the CockroachDB DSN in Terraform; the
instance retrieves it at runtime from the SSM SecureString named by
`database_dsn_parameter_name`.

Important outputs:

- `instance_public_ip`: authorize this `/32` in CockroachDB Cloud.
- `start_session_command`: private administrative access through SSM.
- `assignment_queue_url`: queue consumed by the separate agent orchestrator.
- `dead_letter_queue_url`: terminal assignment failures.

The module assumes the account still has a default VPC with a public subnet.
That tradeoff is intentional for the hackathon package; it avoids owning a VPC,
NAT gateway, load balancer, container registry, or orchestration cluster.
