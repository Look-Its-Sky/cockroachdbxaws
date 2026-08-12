# Operations dashboard

The operations dashboard is a small, read-only Next.js application for the
hackathon deployment. It is publicly reachable over HTTPS but never publicly
readable. Better Auth validates an email/password session and the server then
requires the account to have the `admin` role.

The dashboard authentication database is a local SQLite file on the analysis
EC2 instance. It stores only dashboard users, password credentials, sessions,
verification records, and authorization fields. It does not replace
CockroachDB: incident families, investigations, reports, and outbox state stay
in the external CockroachDB database.

## Safe data contract

The browser may receive only:

- categorical worker readiness;
- bounded, unlabelled process metrics;
- journal byte and record counts;
- counts of incident families, investigations, and outbox states;
- SQS visible, in-flight, and dead-letter counts; and
- at most ten recent investigation identifiers with service, environment,
  severity, state, trigger reason, and queue time.

It must never receive a database connection string, AWS credential, raw log,
log body, stack trace, fingerprint, context snapshot, report body, queue
payload, customer identifier, token, or secret. The Go service enforces this
boundary through its local-only `/overviewz` projection. The dashboard has no
CockroachDB credential.

The instance role grants the dashboard only `sqs:GetQueueAttributes`. It cannot
receive, hide, delete, redrive, or publish queue messages. Dashboard collection
is partial: an unavailable source is rendered as degraded and does not suppress
the other sources.

## Authentication and authorization

Public sign-up is disabled in code. The EC2 setup helper generates the Better
Auth signing secret locally, and a separate root-only helper creates an account
with the `admin` role. Both the proxy and the protected server page participate
in access control: the proxy provides an optimistic cookie check, while the
page validates the session against SQLite and checks the role before reading
operational data. Authentication endpoints are rate-limited in the single
dashboard process.

For the hackathon demo, the sign-in form maps the temporary username `admin`
to a private internal email identity. The password minimum is temporarily nine
characters to support the requested shared demo credential. This is not the
production target: remove the alias, rotate the credential, and restore the
minimum to at least 12 characters before treating the dashboard as durable
operator access.

The SQLite file is held in the `dashboard-auth` Docker volume on the encrypted
EC2 root EBS volume. The Better Auth secret is held in
`/etc/static-log-analysis/dashboard.env`, owned by `root:root` with mode `0600`.
Neither belongs in Terraform state, Git, EC2 user data, or a browser bundle.
Back up the encrypted volume if dashboard accounts must survive instance
replacement; otherwise rerun setup and create a new administrator.

## Network boundary

Caddy is the only public dashboard entry point. It obtains and renews the TLS
certificate for `dashboard_hostname`, redirects HTTP to HTTPS, and proxies to
Next.js on the private Compose network. Ports 9464, 9465, 26257, and 3000 stay
closed to the internet. The dashboard and its proxy are an optional Compose
profile, so missing dashboard authentication configuration cannot stop log
processing.

The dashboard does not change the regional boundary. It runs on the analysis
host, reads same-region SQS metadata through the instance profile, and calls
the Go process over the local Compose network.
