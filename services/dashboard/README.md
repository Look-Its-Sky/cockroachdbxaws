# Static Log Analysis dashboard

This is the single supported operator dashboard for Static Log Analysis. It is
a read-only Next.js application protected by Better Auth and an explicit
`admin` role. Public sign-up is disabled. It never receives the CockroachDB
connection string or SQS message bodies.

Its SQLite database is deliberately narrow: it stores only dashboard accounts,
password credentials, sessions, verification records, and roles. CockroachDB
continues to store all analysis and investigation data.

For local UI development:

```bash
cp .env.example .env.local
openssl rand -base64 48
# Put the generated value in BETTER_AUTH_SECRET in .env.local.
npm install
set -a
. ./.env.local
set +a
npm run auth:migrate
printf '%s\n%s\n' 'admin@example.com' 'replace-with-a-test-password' | npm run auth:create-admin
npm run dev
```

Use a disposable password for local development because commands can be saved
in shell history. Production uses a root-only interactive helper and never puts
the password in an argument or environment file.

The hackathon deployment temporarily accepts nine-character passwords so the
shared demo login can be used. Remove that exception and restore the 12-character
minimum in `lib/auth.ts`, the admin script, and the EC2 helper when moving away
from the demo credential.

Production deployment is part of the existing AWS Terraform and Compose path.
Follow `docs/static-log-analysis/deployment.md`; do not deploy this directory as
a separate public application.
