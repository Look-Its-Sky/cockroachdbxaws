# AWS deployment module

This module implements the repository's one supported hackathon deployment:
one Amazon Linux 2023 EC2 instance running the production Docker Compose file.
It also creates regional SQS queues, a private-by-default security group with
optional dashboard HTTPS ingress, a stable Elastic IP, and the instance's
least-privilege IAM role.

Use the [single deployment guide](../../../docs/static-log-analysis/deployment.md)
for the complete sequence. Do not put the CockroachDB DSN in Terraform. After
the instance exists, the root-only setup helper replaces any laptop-specific
certificate path with `sslrootcert=system`, writes the DSN to
`/etc/static-log-analysis/secrets.env` on the encrypted instance disk, and
starts the service.

Systemd restarts only start the image already present on the host. They do not
fetch or rebuild source. After pushing the configured repository ref, deploy it
explicitly with `sudo deploy-static-log-analysis`; the helper builds first and
restarts the service only after a successful build.

Terraform does not replace the analysis instance when user data changes because
its encrypted disk contains the journal, checkpoints, dashboard auth database,
and locally installed CockroachDB connection. Apply bootstrap changes through
the documented Session Manager refresh. Instance replacement is a deliberate
state reset, not a routine upgrade.

Important outputs:

- `instance_public_ip`: authorize this `/32` in CockroachDB Cloud.
- `start_session_command`: private administrative access through SSM.
- `assignment_queue_url`: queue consumed by the separate agent orchestrator.
- `dead_letter_queue_url`: terminal assignment failures.
- `dashboard_url`: optional authenticated, public HTTPS operations overview.

The dashboard is disabled by default. Enable it with one hostname, point that
hostname at `instance_public_ip`, apply Terraform, then run
`sudo set-static-log-analysis-dashboard-auth` and
`sudo create-static-log-analysis-dashboard-admin` through Session Manager.
This creates only the local SQLite-backed dashboard login; analysis data stays
in managed CockroachDB. There is no second supported dashboard deployment path.

The module assumes the account still has a default VPC with a public subnet.
That tradeoff is intentional for the hackathon package; it avoids owning a VPC,
NAT gateway, load balancer, container registry, or orchestration cluster.
