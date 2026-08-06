# Terminology

- **Record**: one source log entry after normalization.
- **Occurrence**: one unique record contributing to a matched failure.
- **Event**: the deterministic output of a rule trigger at a point in time.
- **Incident family**: related failures sharing service, environment, and normalized
  fingerprint across episodes or deployments.
- **Incident generation**: one deployment/episode segment within a family.
- **Investigation**: durable unit of agent work for an incident family.
- **Evidence**: immutable redacted material or a controlled reference supporting a
  finding.
- **Fingerprint**: versioned deterministic identity for a normalized failure shape.
- **Journal**: authoritative regional disk-backed pending-work store owned by the
  analysis service.
- **Collector queue**: short transport queue protecting delivery to analysis.
- **Outbox**: CockroachDB rows atomically committed with incident changes and later
  published to SQS.
- **Quiet**: no matching occurrence during the configured quiet period; not proof
  of resolution.
- **Reopen window**: period during which a returning failure resumes a generation.
- **Effectively once**: retries are allowed, but one record contributes at most once.
