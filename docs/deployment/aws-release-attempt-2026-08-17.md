# AWS release attempt — 2026-08-17

**Release branch:** `integration`

**Application candidate:** `7537e11db29322b48939498decf38ca7a66017ee`

**Outcome:** Infrastructure converged; application cutover not accepted

This is a sanitized public release record. Account IDs, role ARNs, instance
IDs, addresses, hostnames, queue URLs, tenant identifiers, database locations,
and SSM command IDs are intentionally omitted.

## Source integration

The remote was refreshed before release work. The published `integration`
branch already contained all commits from both `main` and `agent_space`; a
rebase onto `main` was therefore a no-op. No separate Manan-authored branch or
commit was present in the remote refs at the time of the refresh.

The release safeguard was committed and pushed on `integration` as
`7537e11`. The branch and `origin/integration` matched before AWS work began.

## Release gates

The final commit passed:

- static-analysis formatting, vet, integration-tag vet, generated-protobuf,
  and unit/fixture/property gates;
- the serialized Docker-backed race/integration suite with real CockroachDB
  and LocalStack;
- the agent environment's formatting, vet, test, and build gates;
- the dashboard's 22 tests, typecheck, and production Next.js build;
- the production Compose render and pinned Terraform validation; and
- a zero-change post-apply Terraform refresh plan.

## Terraform safety finding and fix

The first live refresh plan was rejected because it proposed replacing both
EC2 hosts and their Elastic IP associations. The replacement trigger was the
module's `most_recent = true` Amazon Linux AMI lookup: a new regional AMI had
turned a routine application release into a destructive host replacement.

The infrastructure now requires an explicit region-specific
`machine_image_id`. A regression test rejects a moving AMI lookup and requires
both hosts to use the pinned input. The ignored operator configuration pins the
AMI already used by the deployed instances.

After the fix, the reviewed plan contained only:

```text
2 to add, 1 to change, 0 to destroy
```

The two additions were the exact-queue App Runner agent runtime role and its
inline consumer-only policy. The one change was the analysis instance's stored
bootstrap metadata; it did not replace or restart the host. The saved plan was
applied, the role was verified to exist, and a fresh plan reported zero drift.

## Application deployment result

The state-preserving deployment helper fetched the pinned application commit
and successfully built the static-analysis images. It also built the dashboard
admin/migration image. The dashboard application image did not complete its
`next build`: SSM reached its 3,600-second command timeout while Next.js still
reported `Creating an optimized production build`.

The helper restarts systemd only after every build succeeds, so it did not
perform its intended atomic restart. A soft reboot did not restore timely SSM
command execution. A subsequent EC2 stop/start preserved the encrypted EBS
root disk, Docker volumes, Elastic IP association, and Terraform identity. A
post-recovery Terraform refresh still reported zero drift.

Post-recovery evidence was mixed and is not sufficient for release acceptance:

- SSM eventually restored a fresh heartbeat and command execution;
- the outbox readiness endpoint returned success;
- the host configuration contained the intended `7537e11` release ref;
- the CloudWatch readiness endpoint reset its connection during verification;
- the public dashboard HTTPS endpoint timed out; and
- later SSM diagnostics remained severely delayed.

Therefore this record does **not** claim that `7537e11` is the active complete
AWS application release. It also does not claim that the dashboard-visible
services are healthy.

## Agent deployment result

The Terraform-owned App Runner runtime role and exact-queue policy are now
applied. The agent application was not launched. The required ignored root
`.env` was absent, so no production agent database DSN, read-only analysis DSN,
model credentials, API token, tenant/classification boundary, or dashboard CORS
origin was available. Local-model loopback configuration was deliberately not
substituted because App Runner cannot reach host-local endpoints.

Database grants, the separate agent database migration, secret provisioning,
network reachability, immutable image publication, App Runner creation, and
end-to-end assignment consumption remain required before the agent can be
called deployed.

## Required next deployment milestone

Do not repeat the on-host dashboard build as the normal release path. The next
AWS deployment change must:

1. build and test static-analysis and dashboard images off-host;
2. push both images under the same immutable commit tag to a private registry;
3. make the analysis host pull by immutable digest;
4. verify both images before changing the running Compose release;
5. perform a bounded restart with an automatic health rollback; and
6. keep the prior digests available for immediate rollback.

Before that cutover, recover and verify the current CloudWatch container and
dashboard through a responsive management channel. Do not purge journals,
queues, checkpoints, dashboard auth data, or either database during recovery.

For the agent, provision production secrets through a managed secret system
rather than committing or printing them, apply the documented least-privilege
database grants, and run the investigation-only acceptance sequence in
`docs/agent-sqs-deployment.md`. Automated remediation remains disabled.

## Follow-up implementation

The immutable-image milestone is now implemented in the repository. Terraform
owns three private ECR repositories with immutable tags and scanning, and the
analysis instance can authenticate to ECR globally only for the AWS-required
authorization token while layer and manifest reads are scoped to those exact
repositories. Production Compose has no application `build` stanza and
requires digest references for the analysis, dashboard, and dashboard-admin
artifacts.

The off-host publisher builds all three artifacts from one clean commit and
resolves their registry digests. The host cutover helper pulls and inspects all
three before migrations or restart, admits only the configured repositories,
uses a bounded health gate, and restores the prior digest selection on failure.
This follow-up describes implemented code, not live release evidence; the ECR
apply, image publication, bootstrap refresh, cutover, and public dashboard
verification still need to succeed before the AWS application is accepted.
