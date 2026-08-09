// Package runtime assembles the deployable service. It owns the role selection
// ADR 0001 defines, the operator-facing configuration surface, the process
// worker cadence, and the shutdown ordering operations.md requires.
//
// Everything here is validated before anything is opened. A contradiction
// between the configured scope and an immutable boundary is a deployment fault,
// and startup is the only place it can be reported as one: downstream every
// record would instead look individually invalid, and isolating an individually
// invalid record destroys its durable payload.
package runtime

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/enrich"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/otlpreceiver"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// DatabaseDSNEnvVar carries the one secret in this configuration surface. It is
// deliberately not a flag: a flag default appears in `--help`, in the process
// table, and in shell history, and a rotated credential would leave copies in
// all three.
const DatabaseDSNEnvVar = "STATIC_LOG_ANALYSIS_DATABASE_DSN"

// PolicyBaseline names the service-aware redaction policy from security.md.
// Selection is by name so a deployment cannot silently run an ad-hoc policy
// whose version the journal manifest and store boundary would then disagree on.
const PolicyBaseline = "baseline"

var ErrInvalidConfig = errors.New("runtime: invalid configuration")

// Role is one runnable half of the service. ADR 0001 fixes the set: one
// artifact, several roles, so production may scale ingestion separately from
// processing without a second build.
type Role string

const (
	RoleAll     Role = "all"
	RoleIngest  Role = "ingest"
	RoleProcess Role = "process"
	RoleOutbox  Role = "outbox"
	// RoleSource pulls from a source that does not push to us. It is
	// deliberately not part of RoleAll: a source needs log groups configured,
	// and folding it into the default role would make every replica require
	// CloudWatch configuration it does not use.
	RoleSource Role = "source"
)

func (r Role) Ingests() bool   { return r == RoleAll || r == RoleIngest }
func (r Role) Processes() bool { return r == RoleAll || r == RoleProcess }
func (r Role) Publishes() bool { return r == RoleOutbox }

// Polls reports whether this role pulls from a source adapter. Such a replica
// opens a journal and writes to it, exactly as an ingest replica does, but binds
// no listener: nothing pushes to it.
func (r Role) Polls() bool { return r == RoleSource }

// WritesJournal reports whether this role owns a journal volume. Both the push
// and the pull ingestion paths do, and so does the combined role.
func (r Role) WritesJournal() bool { return r.Ingests() || r.Processes() || r.Polls() }

func (r Role) valid() bool {
	switch r {
	case RoleAll, RoleIngest, RoleProcess, RoleOutbox, RoleSource:
		return true
	default:
		return false
	}
}

type JournalConfig struct {
	Dir string
	// MaxBytes has no default. operations.md derives journal capacity from
	// measured peak bytes per second, and a default would silently substitute a
	// number nobody measured for the one thing standing between a database
	// outage and acknowledged loss.
	MaxBytes     uint64
	MinFreeBytes uint64
}

// WorkerConfig is the process role's cadence. Interval and IdleInterval are
// separate so a worker with nothing to claim waits instead of spinning, while a
// worker that just drained a cohort comes back promptly.
type WorkerConfig struct {
	Cohort       int
	Interval     time.Duration
	IdleInterval time.Duration
	BackoffMin   time.Duration
	BackoffMax   time.Duration
}

// OutboxConfig is the publisher role's surface: which queues it drains onto,
// how much it takes at once, and how long a message that keeps failing keeps
// its place.
//
// The retry schedule and the loop cadence are separate settings because they
// answer different questions. Cadence is how often this replica asks the outbox
// for work; the retry schedule is how long an individual message waits after a
// failed delivery, and it is the one that has to survive a queue outage.
type OutboxConfig struct {
	QueueURL           string
	DeadLetterQueueURL string
	// EndpointURL points the AWS client at a local SQS-compatible endpoint.
	// Declaring it is what distinguishes a deliberate local deployment from a
	// queue URL that silently lost its region.
	EndpointURL    string
	Batch          int
	ClaimTTL       time.Duration
	PublishTimeout time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	MaxAttempts    int64
	Cadence        WorkerConfig
}

// SourceConfig is the pull-based source role's surface.
type SourceConfig struct {
	// Account is the adapter's authenticated account. It is part of cw:v1
	// identity and is never read back from an API answer.
	Account string
	// Groups are the log groups this replica reads, each with the service and
	// environment identity the operator declares for it. CloudWatch events carry
	// no authenticated service identity, so this is the only place it can come
	// from.
	Groups []SourceGroup
	// SourceInstance and CredentialIdentity describe this adapter replica and
	// the principal it reads as. Both are retained for audit.
	SourceInstance     string
	CredentialIdentity string
	// EndpointURL points the AWS client at a local CloudWatch Logs endpoint.
	EndpointURL string
	// CheckpointDir is this replica's non-shared checkpoint volume. It is
	// separate from the journal because the two have different lifetimes: a
	// journal is drained and may be rebuilt, and a checkpoint that was rebuilt
	// with it would reread the whole lookback window.
	CheckpointDir string
	Lookback      time.Duration
	MaxLookback   time.Duration
	MaxWindow     time.Duration
	PageLimit     int
	MaxPages      int
	Cadence       WorkerConfig
}

// EnrichmentConfig is the deployment-metadata surface.
//
// Deployment identity is part of grouping, not decoration, so this is not
// optional decoration either — but it is optional to configure, because a
// deployment that cannot supply it must still be able to run. Without it every
// record groups under unknown-deployment with enrichment pending.
type EnrichmentConfig struct {
	// Deployments are operator declarations, each
	// service=environment=deployment_id=version.
	Deployments []string
	// Budget bounds one provider call. It never bounds a record's wait, which
	// is always zero.
	Budget time.Duration
	// TTL is how long a resolved deployment stays fresh before it is refreshed
	// in the background. The stale answer keeps being used meanwhile.
	TTL time.Duration
	// RetryAfter is the pause after a failed resolution.
	RetryAfter time.Duration
	// MaxEntries bounds the cache.
	MaxEntries int
}

// CapacityConfig is the pre-journal shedding surface.
//
// It is opt-in and off by default. Shedding drops data deliberately, and an
// operator who has declared no eligible source has declared that nothing may be
// dropped; inventing a threshold for them would be the service choosing to lose
// records nobody agreed to lose.
type CapacityConfig struct {
	// NearCapacityPercent is the journal utilization at or above which eligible
	// records may be shed. Zero disables shedding entirely.
	NearCapacityPercent uint8
	// EligibleSources are the sources whose explicitly unprotected DEBUG and
	// INFO records may be shed. Nothing else is ever eligible.
	EligibleSources []string
}

// SourceGroup is one configured log group and the identity it carries.
type SourceGroup struct {
	LogGroup    string
	Service     string
	Environment string
}

type Config struct {
	Role            Role
	Scope           persistence.Scope
	Classification  string
	WorkerOwner     string
	Journal         JournalConfig
	DatabaseDSN     string
	RedactionPolicy string
	ForbiddenValues []string
	Limits          admission.Limits
	Ingress         otlpreceiver.Config
	Worker          WorkerConfig
	Outbox          OutboxConfig
	Source          SourceConfig
	Enrichment      EnrichmentConfig
	// AdminListen serves health, readiness, and metrics. It is separate from the
	// ingress listener because whoever may export logs must not thereby be able
	// to read this replica's operational state.
	AdminListen string
	// Capacity wires operations.md's utilization table to a live journal.
	Capacity        CapacityConfig
	ShutdownTimeout time.Duration
}

// Parse turns a command line and the process environment into a validated
// configuration. args excludes the program name and begins with the role, which
// is positional so `log-analysis ingest` reads the way ADR 0001 writes it.
func Parse(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return Config{}, fmt.Errorf("%w: first argument must be a role: %s, %s, %s, %s, or %s",
			ErrInvalidConfig, RoleAll, RoleIngest, RoleProcess, RoleOutbox, RoleSource)
	}
	config := Config{Role: Role(args[0])}
	if !config.Role.valid() {
		return Config{}, fmt.Errorf("%w: unknown role %q", ErrInvalidConfig, args[0])
	}

	flags := flag.NewFlagSet("log-analysis "+args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&config.Scope.Region, "region", "", "regional boundary this replica serves")
	flags.StringVar(&config.Scope.TenantID, "tenant-id", "", "tenant this replica serves")
	flags.StringVar(&config.Classification, "classification", "SENSITIVE", "highest data classification permitted in this deployment")
	flags.StringVar(&config.WorkerOwner, "worker-owner", pipeline.DefaultWorkerOwner, "journal claim owner recorded with claimed records")
	flags.StringVar(&config.Journal.Dir, "journal-dir", "", "directory of this replica's non-shared journal volume")
	flags.Uint64Var(&config.Journal.MaxBytes, "journal-max-bytes", 0, "journal capacity, sized from measured peak input")
	flags.Uint64Var(&config.Journal.MinFreeBytes, "journal-min-free-bytes", 1<<30, "filesystem headroom below which the journal refuses to grow")
	flags.StringVar(&config.RedactionPolicy, "redaction-policy", PolicyBaseline, "named redaction policy")
	forbidden := flags.String("redaction-forbidden-values", "", "comma-separated service-specific values that must never be persisted")

	flags.Int64Var(&config.Limits.MaxCompressedBytes, "max-compressed-bytes", admission.DefaultMaxCompressedBytes, "maximum compressed request")
	flags.Int64Var(&config.Limits.MaxUncompressedBytes, "max-uncompressed-bytes", admission.DefaultMaxUncompressedBytes, "maximum uncompressed request")
	flags.IntVar(&config.Limits.MaxRecords, "max-records", admission.DefaultMaxRecords, "maximum records per request")
	flags.IntVar(&config.Limits.MaxNestingDepth, "max-nesting-depth", admission.DefaultMaxNestingDepth, "maximum attribute nesting")
	flags.IntVar(&config.Limits.MaxNormalizedBytes, "max-normalized-bytes", admission.DefaultMaxNormalizedBytes, "maximum safe normalized record")
	flags.Int64Var(&config.Limits.MaxMaterializedBytes, "max-materialized-bytes", admission.DefaultMaxMaterializedBytes, "maximum projected aggregate materialization")
	flags.IntVar(&config.Limits.MaxStructuralNodes, "max-structural-nodes", admission.DefaultMaxStructuralNodes, "maximum raw structural nodes")

	flags.StringVar(&config.Ingress.GRPCListen, "otlp-grpc-listen", ":4317", "OTLP/gRPC listen address")
	flags.StringVar(&config.Ingress.HTTPListen, "otlp-http-listen", ":4318", "OTLP/HTTP listen address")
	trustSource := flags.String("otlp-trust-source", "", "how a caller is authenticated: mutual_tls or static_local")
	flags.StringVar(&config.Ingress.Trust.SourceAccount, "otlp-source-account", "", "authenticated source account")
	flags.StringVar(&config.Ingress.Trust.SourceInstance, "otlp-source-instance", "", "collector instance identity (static_local only)")
	flags.StringVar(&config.Ingress.Trust.CredentialIdentity, "otlp-credential-identity", "", "credential principal (static_local only)")
	allowedEnvironments := flags.String("otlp-allowed-environments", "", "comma-separated environments a caller may claim")
	allowedServices := flags.String("otlp-allowed-services", "", "comma-separated service identities a caller may claim")
	flags.StringVar(&config.Ingress.Trust.TLS.CertificateFile, "otlp-tls-cert", "", "server certificate (mutual_tls)")
	flags.StringVar(&config.Ingress.Trust.TLS.KeyFile, "otlp-tls-key", "", "server private key (mutual_tls)")
	flags.StringVar(&config.Ingress.Trust.TLS.ClientCAFile, "otlp-client-ca", "", "CA that client certificates are verified against (mutual_tls)")
	// The listener bounds. static_local has no client certificate by design, so
	// on that socket these are the only limit on what an unauthenticated caller
	// can hold. Every one of them defaults inside otlpreceiver rather than here,
	// so a zero reaching it means "use the documented default" and never "no
	// bound at all".
	flags.DurationVar(&config.Ingress.ReadHeaderTimeout, "otlp-read-header-timeout", 0, "bound on receiving request headers (0 uses the default)")
	flags.DurationVar(&config.Ingress.ReadTimeout, "otlp-read-timeout", 0, "bound on receiving a whole request including its body (0 uses the default)")
	flags.DurationVar(&config.Ingress.WriteTimeout, "otlp-write-timeout", 0, "bound on writing one response (0 uses the default)")
	flags.DurationVar(&config.Ingress.IdleTimeout, "otlp-idle-timeout", 0, "bound on an idle keep-alive connection (0 uses the default)")
	flags.IntVar(&config.Ingress.MaxHeaderBytes, "otlp-max-header-bytes", 0, "bound on the request header block (0 uses the default)")
	flags.IntVar(&config.Ingress.MaxInFlight, "otlp-max-in-flight", 0, "exports admitted at once across both transports (0 uses the default)")
	maxStreams := flags.Uint("otlp-max-concurrent-streams", 0, "concurrent gRPC streams per connection (0 uses the default)")

	flags.IntVar(&config.Worker.Cohort, "process-cohort", persistence.MaxProcessBatch, "records claimed and persisted per cycle")
	flags.DurationVar(&config.Worker.Interval, "process-interval", 250*time.Millisecond, "pause after a cycle that did work")
	flags.DurationVar(&config.Worker.IdleInterval, "process-idle-interval", time.Second, "pause after a cycle with nothing to claim")
	flags.DurationVar(&config.Worker.BackoffMin, "process-backoff-min", time.Second, "first pause after a failed cycle")
	flags.DurationVar(&config.Worker.BackoffMax, "process-backoff-max", 30*time.Second, "longest pause after repeated failures")
	flags.StringVar(&config.Outbox.QueueURL, "outbox-queue-url", "", "regional SQS Standard queue assignments are published to")
	flags.StringVar(&config.Outbox.DeadLetterQueueURL, "outbox-dead-letter-queue-url", "", "queue a message that can never be published is moved to")
	flags.StringVar(&config.Outbox.EndpointURL, "outbox-endpoint-url", "", "SQS-compatible endpoint to use instead of AWS")
	flags.IntVar(&config.Outbox.Batch, "outbox-batch", outbox.DefaultBatch, "outbox messages claimed and published per cycle")
	flags.DurationVar(&config.Outbox.ClaimTTL, "outbox-claim-ttl", outbox.DefaultClaimTTL, "how long a publisher's claim on a message lasts")
	flags.DurationVar(&config.Outbox.PublishTimeout, "outbox-publish-timeout", outbox.DefaultPublishTimeout, "bound on one delivery attempt")
	flags.DurationVar(&config.Outbox.RetryMin, "outbox-retry-min", outbox.DefaultBackoffMin, "first pause after a failed delivery")
	flags.DurationVar(&config.Outbox.RetryMax, "outbox-retry-max", outbox.DefaultBackoffMax, "longest pause between delivery attempts")
	flags.Int64Var(&config.Outbox.MaxAttempts, "outbox-max-attempts", outbox.DefaultMaxAttempts, "failed deliveries before a message is dead-lettered")
	flags.DurationVar(&config.Outbox.Cadence.Interval, "outbox-interval", 250*time.Millisecond, "pause after a cycle that claimed messages")
	flags.DurationVar(&config.Outbox.Cadence.IdleInterval, "outbox-idle-interval", time.Second, "pause after a cycle with nothing to publish")
	flags.DurationVar(&config.Outbox.Cadence.BackoffMin, "outbox-backoff-min", time.Second, "first pause after a failed cycle")
	flags.DurationVar(&config.Outbox.Cadence.BackoffMax, "outbox-backoff-max", 30*time.Second, "longest pause after repeated failed cycles")

	flags.StringVar(&config.Source.Account, "cloudwatch-account", "", "authenticated AWS account the source adapter reads")
	sourceGroups := flags.String("cloudwatch-log-groups", "", "comma-separated log groups as group=service=environment")
	flags.StringVar(&config.Source.SourceInstance, "cloudwatch-source-instance", "", "identity of this adapter replica, retained for audit")
	flags.StringVar(&config.Source.CredentialIdentity, "cloudwatch-credential-identity", "", "IAM principal this adapter reads as, retained for audit")
	flags.StringVar(&config.Source.EndpointURL, "cloudwatch-endpoint-url", "", "CloudWatch Logs-compatible endpoint to use instead of AWS")
	flags.StringVar(&config.Source.CheckpointDir, "cloudwatch-checkpoint-dir", "", "directory of this replica's non-shared checkpoint volume")
	flags.DurationVar(&config.Source.Lookback, "cloudwatch-lookback", 0, "overlap reread on every cycle (0 uses the adapter default)")
	flags.DurationVar(&config.Source.MaxLookback, "cloudwatch-max-lookback", 0, "furthest back a recovering replica reads (0 uses the adapter default)")
	flags.DurationVar(&config.Source.MaxWindow, "cloudwatch-max-window", 0, "widest single query window (0 uses the adapter default)")
	flags.IntVar(&config.Source.PageLimit, "cloudwatch-page-limit", 0, "events requested per page (0 uses the adapter default)")
	flags.IntVar(&config.Source.MaxPages, "cloudwatch-max-pages", 0, "pages walked per group per cycle (0 uses the adapter default)")
	flags.DurationVar(&config.Source.Cadence.Interval, "cloudwatch-interval", 250*time.Millisecond, "pause after a cycle that read events")
	flags.DurationVar(&config.Source.Cadence.IdleInterval, "cloudwatch-idle-interval", 5*time.Second, "pause after a cycle with nothing new")
	flags.DurationVar(&config.Source.Cadence.BackoffMin, "cloudwatch-backoff-min", time.Second, "first pause after a failed cycle")
	flags.DurationVar(&config.Source.Cadence.BackoffMax, "cloudwatch-backoff-max", 30*time.Second, "longest pause after repeated failed cycles")

	nearCapacity := flags.Uint("shed-near-capacity-percent", 0, "journal utilization at or above which eligible records may be shed (0 disables shedding)")
	eligibleSources := flags.String("shed-eligible-sources", "", "comma-separated sources whose unprotected DEBUG and INFO may be shed")
	flags.StringVar(&config.AdminListen, "admin-listen", ":9464", "health, readiness, and metrics listen address (empty disables it)")
	deployments := flags.String("enrichment-deployments", "", "comma-separated service=environment=deployment_id=version")
	flags.DurationVar(&config.Enrichment.Budget, "enrichment-budget", 0, "bound on one metadata lookup (0 uses the default)")
	flags.DurationVar(&config.Enrichment.TTL, "enrichment-ttl", 0, "how long a resolved deployment stays fresh (0 uses the default)")
	flags.DurationVar(&config.Enrichment.RetryAfter, "enrichment-retry-after", 0, "pause after a failed lookup (0 uses the default)")
	flags.IntVar(&config.Enrichment.MaxEntries, "enrichment-max-entries", 0, "bound on cached deployments (0 uses the default)")

	flags.DurationVar(&config.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "time allowed to drain before a forced stop")

	if err := flags.Parse(args[1:]); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if flags.NArg() > 0 {
		return Config{}, fmt.Errorf("%w: unexpected argument %q", ErrInvalidConfig, flags.Arg(0))
	}
	config.ForbiddenValues = splitList(*forbidden)
	config.Ingress.Trust.Source = otlpreceiver.TrustSource(*trustSource)
	config.Ingress.Trust.Region = config.Scope.Region
	config.Ingress.Trust.AllowedEnvironments = splitList(*allowedEnvironments)
	config.Ingress.Trust.AllowedServices = splitList(*allowedServices)
	config.Ingress.Limits = config.Limits
	if *maxStreams > math.MaxUint32 {
		return Config{}, fmt.Errorf("%w: -otlp-max-concurrent-streams must fit in 32 bits", ErrInvalidConfig)
	}
	config.Ingress.MaxConcurrentStreams = uint32(*maxStreams)
	groups, err := parseSourceGroups(*sourceGroups)
	if err != nil {
		return Config{}, err
	}
	config.Source.Groups = groups
	config.Enrichment.Deployments = splitList(*deployments)
	if *nearCapacity > 100 {
		return Config{}, fmt.Errorf("%w: -shed-near-capacity-percent must be between 0 and 100", ErrInvalidConfig)
	}
	config.Capacity.NearCapacityPercent = uint8(*nearCapacity)
	config.Capacity.EligibleSources = splitList(*eligibleSources)
	if getenv != nil {
		config.DatabaseDSN = getenv(DatabaseDSNEnvVar)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate refuses everything the service cannot run correctly on. It is the
// operator's whole feedback loop: after this returns nil, a failure is a real
// failure rather than a typo.
func (c Config) Validate() error {
	if !c.Role.valid() {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidConfig, c.Role)
	}
	if trimmed(c.Scope.Region) == "" || trimmed(c.Scope.TenantID) == "" {
		return fmt.Errorf("%w: -region and -tenant-id are the regional boundary and are required", ErrInvalidConfig)
	}
	if !validClassification(c.Classification) {
		return fmt.Errorf("%w: -classification must be PUBLIC, INTERNAL, SENSITIVE, or RESTRICTED", ErrInvalidConfig)
	}
	if c.RedactionPolicy != PolicyBaseline {
		return fmt.Errorf("%w: -redaction-policy %q is not a known policy", ErrInvalidConfig, c.RedactionPolicy)
	}
	if _, err := c.Policy(); err != nil {
		return fmt.Errorf("%w: -redaction-forbidden-values were not accepted by the policy", ErrInvalidConfig)
	}
	if c.WorkerOwner == "" || strings.TrimSpace(c.WorkerOwner) != c.WorkerOwner || len(c.WorkerOwner) > journal.MaxOwnerBytes {
		return fmt.Errorf("%w: -worker-owner must be a non-empty unpadded identifier", ErrInvalidConfig)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("%w: -shutdown-timeout must be positive; a forced stop is the fallback, not the plan", ErrInvalidConfig)
	}
	if err := c.validateJournal(); err != nil {
		return err
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	if err := c.validateDatabase(); err != nil {
		return err
	}
	if err := c.validateWorker(); err != nil {
		return err
	}
	if err := c.validateOutbox(); err != nil {
		return err
	}
	if err := c.validateSource(); err != nil {
		return err
	}
	if err := c.validateEnrichment(); err != nil {
		return err
	}
	if err := c.validateCapacity(); err != nil {
		return err
	}
	return c.validateIngress()
}

// validateOutbox refuses every queue configuration the publisher could not
// publish through, and does it before the process opens anything.
//
// The regional check is the load-bearing one. security.md and architecture.md
// forbid moving log-derived content across a regional boundary, and an
// assignment carries incident, service, and investigation identity. A queue URL
// naming another region is therefore refused here rather than discovered once
// per message, where every message would instead look individually invalid.
// exact reports whether a value is non-empty and carries no surrounding
// whitespace. Padding matters here because these values are part of record
// identity and of the audit trail, where " prod" and "prod" must not be two
// different things.
func exact(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

// parseSourceGroups reads the repeated group=service=environment form.
//
// Service and environment are operator declarations rather than anything read
// from a CloudWatch event, because a CloudWatch event carries no authenticated
// service identity and a claim inside a message is untrusted. Requiring all
// three in one token is what makes it impossible to configure a group whose
// identity nobody chose.
func parseSourceGroups(raw string) ([]SourceGroup, error) {
	var groups []SourceGroup
	seen := map[string]bool{}
	for _, token := range splitList(raw) {
		parts := strings.Split(token, "=")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: -cloudwatch-log-groups entry %q must be group=service=environment", ErrInvalidConfig, token)
		}
		group := SourceGroup{LogGroup: parts[0], Service: parts[1], Environment: parts[2]}
		if !exact(group.LogGroup) || !exact(group.Service) || !exact(group.Environment) {
			return nil, fmt.Errorf("%w: -cloudwatch-log-groups entry %q has an empty or padded component", ErrInvalidConfig, token)
		}
		if seen[group.LogGroup] {
			// Two entries for one group would give it two identities, and which
			// one a record got would depend on map order.
			return nil, fmt.Errorf("%w: -cloudwatch-log-groups names %q twice", ErrInvalidConfig, group.LogGroup)
		}
		seen[group.LogGroup] = true
		groups = append(groups, group)
	}
	return groups, nil
}

// validateCapacity refuses a shedding configuration that would drop data
// nobody meant to drop, or that declares an intent it cannot carry out.
func (c Config) validateCapacity() error {
	sources := c.Capacity.EligibleSources
	if len(sources) > 0 && c.Capacity.NearCapacityPercent == 0 {
		return fmt.Errorf("%w: -shed-eligible-sources names sources but -shed-near-capacity-percent is 0, so nothing would ever be shed",
			ErrInvalidConfig)
	}
	if c.Capacity.NearCapacityPercent > 0 && len(sources) == 0 {
		return fmt.Errorf("%w: -shed-near-capacity-percent is set but -shed-eligible-sources is empty, so nothing would ever be shed",
			ErrInvalidConfig)
	}
	for _, source := range sources {
		switch model.SourceType(source) {
		case model.SourceTypeOTLP, model.SourceTypeCloudWatch:
		default:
			return fmt.Errorf("%w: -shed-eligible-sources names unknown source %q", ErrInvalidConfig, source)
		}
	}
	return nil
}

// CapacityPolicy is the admission policy this configuration adds up to.
func (c Config) CapacityPolicy() admission.CapacityPolicy {
	sources := make([]model.SourceType, 0, len(c.Capacity.EligibleSources))
	for _, source := range c.Capacity.EligibleSources {
		sources = append(sources, model.SourceType(source))
	}
	return admission.CapacityPolicy{
		NearCapacityPercent: c.Capacity.NearCapacityPercent, EligibleSources: sources,
	}
}

// validateEnrichment refuses a declaration the provider could not be built
// from, at the point an operator can still fix it.
func (c Config) validateEnrichment() error {
	if len(c.Enrichment.Deployments) > 0 && !c.Role.WritesJournal() {
		// Only a role that normalizes records has any use for deployment
		// identity. Holding the declaration elsewhere reads as though that
		// replica were enriching when nothing in it ever will.
		return fmt.Errorf("%w: -enrichment-deployments is set but role %q normalizes no records",
			ErrInvalidConfig, c.Role)
	}
	if _, err := enrich.NewStatic(c.Enrichment.Deployments); err != nil {
		return fmt.Errorf("%w: -enrichment-deployments: %v", ErrInvalidConfig, err)
	}
	for name, value := range map[string]time.Duration{
		"-enrichment-budget": c.Enrichment.Budget, "-enrichment-ttl": c.Enrichment.TTL,
		"-enrichment-retry-after": c.Enrichment.RetryAfter,
	} {
		if value < 0 {
			return fmt.Errorf("%w: %s must not be negative; 0 means the documented default", ErrInvalidConfig, name)
		}
	}
	if c.Enrichment.MaxEntries < 0 {
		return fmt.Errorf("%w: -enrichment-max-entries must not be negative; 0 means the documented default", ErrInvalidConfig)
	}
	return nil
}

func (c Config) validateSource() error {
	if !c.Role.Polls() {
		// A replica that does not poll must not hold source configuration it
		// cannot act on: it reads as though this replica were retrieving logs
		// when nothing in it ever will.
		if len(c.Source.Groups) > 0 || trimmed(c.Source.Account) != "" {
			return fmt.Errorf("%w: -cloudwatch-account and -cloudwatch-log-groups are set but role %q polls no source",
				ErrInvalidConfig, c.Role)
		}
		return nil
	}
	if !exact(c.Source.Account) {
		return fmt.Errorf("%w: -cloudwatch-account is required; it is part of record identity and is never read back from an API answer",
			ErrInvalidConfig)
	}
	if len(c.Source.Groups) == 0 {
		return fmt.Errorf("%w: -cloudwatch-log-groups is required; a source replica with no groups would report healthy and read nothing",
			ErrInvalidConfig)
	}
	if !exact(c.Source.SourceInstance) || !exact(c.Source.CredentialIdentity) {
		return fmt.Errorf("%w: -cloudwatch-source-instance and -cloudwatch-credential-identity are required; both are retained for audit",
			ErrInvalidConfig)
	}
	if !exact(c.Source.CheckpointDir) {
		return fmt.Errorf("%w: -cloudwatch-checkpoint-dir is required; without a durable checkpoint every restart rereads the whole lookback window",
			ErrInvalidConfig)
	}
	if c.Source.CheckpointDir == c.Journal.Dir {
		// They have different lifetimes. A checkpoint rebuilt alongside a
		// rebuilt journal would rediscover the whole window.
		return fmt.Errorf("%w: -cloudwatch-checkpoint-dir and -journal-dir must be different volumes", ErrInvalidConfig)
	}
	if _, err := parseEndpoint(c.Source.EndpointURL); err != nil {
		return fmt.Errorf("%w: -cloudwatch-endpoint-url: %v", ErrInvalidConfig, err)
	}
	// The envelope authorizes exactly the services and environments the groups
	// declare. A group naming a service outside the allow-set would authorize
	// less than it declares, so the allow-sets are derived from the groups and
	// the ingress allow-sets are not consulted: this role binds no listener.
	if c.Source.Cadence.Interval <= 0 || c.Source.Cadence.IdleInterval <= 0 {
		return fmt.Errorf("%w: -cloudwatch-interval and -cloudwatch-idle-interval must be positive, or the poller busy-spins", ErrInvalidConfig)
	}
	if c.Source.Cadence.BackoffMin <= 0 || c.Source.Cadence.BackoffMax < c.Source.Cadence.BackoffMin {
		return fmt.Errorf("%w: -cloudwatch-backoff-min must be positive and no greater than -cloudwatch-backoff-max", ErrInvalidConfig)
	}
	return nil
}

// SourceAllowedServices and SourceAllowedEnvironments are the authorization the
// configured groups add up to. They are derived rather than configured
// separately so an allow-set can never be narrower than the groups it must
// admit.
func (c Config) SourceAllowedServices() []string {
	return distinct(func(g SourceGroup) string { return g.Service }, c.Source.Groups)
}

func (c Config) SourceAllowedEnvironments() []string {
	return distinct(func(g SourceGroup) string { return g.Environment }, c.Source.Groups)
}

func distinct(pick func(SourceGroup) string, groups []SourceGroup) []string {
	seen := map[string]bool{}
	var values []string
	for _, group := range groups {
		value := pick(group)
		if seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	return values
}

func (c Config) validateOutbox() error {
	if !c.Role.Publishes() {
		return nil
	}
	out := c.Outbox
	if trimmed(out.QueueURL) == "" || trimmed(out.DeadLetterQueueURL) == "" {
		return fmt.Errorf("%w: -outbox-queue-url and -outbox-dead-letter-queue-url are both required; without a dead-letter queue a message that can never be published would be retried forever",
			ErrInvalidConfig)
	}
	if out.QueueURL == out.DeadLetterQueueURL {
		return fmt.Errorf("%w: -outbox-queue-url and -outbox-dead-letter-queue-url are the same queue, so a poisoned message would be republished to itself", ErrInvalidConfig)
	}
	endpoint, err := parseEndpoint(out.EndpointURL)
	if err != nil {
		return fmt.Errorf("%w: -outbox-endpoint-url: %v", ErrInvalidConfig, err)
	}
	for flagName, raw := range map[string]string{"-outbox-queue-url": out.QueueURL, "-outbox-dead-letter-queue-url": out.DeadLetterQueueURL} {
		if err := validateQueueURL(raw, c.Scope.Region, endpoint); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidConfig, flagName, err)
		}
	}
	if out.Batch < 1 || out.Batch > persistence.MaxClaimBatch {
		return fmt.Errorf("%w: -outbox-batch must be between 1 and %d", ErrInvalidConfig, persistence.MaxClaimBatch)
	}
	if out.ClaimTTL <= 0 || out.PublishTimeout <= 0 {
		return fmt.Errorf("%w: -outbox-claim-ttl and -outbox-publish-timeout must be positive", ErrInvalidConfig)
	}
	if out.ClaimTTL <= out.PublishTimeout {
		// A lease shorter than one attempt means every message is published
		// under a claim that expired while it was in flight, so every delivery
		// becomes a duplicate a successor also delivers.
		return fmt.Errorf("%w: -outbox-claim-ttl must be longer than -outbox-publish-timeout, or no delivery fits inside its own claim", ErrInvalidConfig)
	}
	if out.RetryMin <= 0 || out.RetryMax < out.RetryMin {
		return fmt.Errorf("%w: -outbox-retry-min must be positive and no greater than -outbox-retry-max", ErrInvalidConfig)
	}
	if out.MaxAttempts < 1 {
		return fmt.Errorf("%w: -outbox-max-attempts must allow at least one delivery", ErrInvalidConfig)
	}
	if out.Cadence.Interval <= 0 || out.Cadence.IdleInterval <= 0 {
		return fmt.Errorf("%w: -outbox-interval and -outbox-idle-interval must be positive, or the publisher busy-spins", ErrInvalidConfig)
	}
	if out.Cadence.BackoffMin <= 0 || out.Cadence.BackoffMax < out.Cadence.BackoffMin {
		return fmt.Errorf("%w: -outbox-backoff-min must be positive and no greater than -outbox-backoff-max", ErrInvalidConfig)
	}
	return nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	if trimmed(raw) == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%q is not an absolute http or https endpoint", raw)
	}
	return parsed, nil
}

// validateQueueURL enforces the shape of a queue URL without asking the network
// anything, so a misdeployed replica is refused even while SQS is unreachable.
func validateQueueURL(raw, region string, endpoint *url.URL) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" {
		return fmt.Errorf("%q is not a queue URL", raw)
	}
	if endpoint != nil {
		if parsed.Host != endpoint.Host {
			return fmt.Errorf("%q is not on the declared endpoint %q", raw, endpoint.Host)
		}
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%q is not https; a queue reached in plaintext must be declared with -outbox-endpoint-url", raw)
	}
	// The only host shape accepted without a declared endpoint is AWS's own, so
	// that the region in the URL can be checked against this replica's region.
	queueRegion, ok := awsQueueRegion(parsed.Host)
	if !ok {
		return fmt.Errorf("%q is not an Amazon SQS queue URL; a non-AWS endpoint must be declared with -outbox-endpoint-url", raw)
	}
	if queueRegion != region {
		return fmt.Errorf("%q is in region %q but this replica serves %q; assignments must not cross a regional boundary", raw, queueRegion, region)
	}
	return nil
}

// awsQueueRegion reads the region out of an Amazon SQS endpoint host.
func awsQueueRegion(host string) (string, bool) {
	if name, ok := strings.CutSuffix(host, ".amazonaws.com"); ok {
		if region, found := strings.CutPrefix(name, "sqs."); found && region != "" && !strings.Contains(region, ".") {
			return region, true
		}
		// The legacy <region>.queue.amazonaws.com form is still served.
		if region, found := strings.CutSuffix(name, ".queue"); found && region != "" && !strings.Contains(region, ".") {
			return region, true
		}
	}
	return "", false
}

// validateJournal applies to every role. Even the process role owns the journal
// volume it drains, and architecture.md permits no two processes on one journal.
func (c Config) validateJournal() error {
	if c.Role.Publishes() {
		return nil
	}
	if trimmed(c.Journal.Dir) == "" {
		return fmt.Errorf("%w: -journal-dir is required; this replica owns one non-shared journal volume", ErrInvalidConfig)
	}
	if c.Journal.MaxBytes == 0 || c.Journal.MinFreeBytes == 0 {
		return fmt.Errorf("%w: -journal-max-bytes and -journal-min-free-bytes must be positive; capacity is sized from measured peak input", ErrInvalidConfig)
	}
	return nil
}

func (c Config) validateLimits() error {
	// The admission package reads an unset limit as "use the default". An
	// operator who writes 0 means zero, which is not a limit the service can
	// honour, so an explicit non-positive value is refused here.
	if c.Limits.MaxCompressedBytes < 1 || c.Limits.MaxUncompressedBytes < 1 || c.Limits.MaxRecords < 1 ||
		c.Limits.MaxNestingDepth < 1 || c.Limits.MaxNormalizedBytes < 1 || c.Limits.MaxMaterializedBytes < 1 ||
		c.Limits.MaxStructuralNodes < 1 {
		return fmt.Errorf("%w: every admission limit must be positive", ErrInvalidConfig)
	}
	if err := c.Limits.Validate(); err != nil {
		// identity-and-admission.md permits configuring limits downward only.
		return fmt.Errorf("%w: admission limits are only configurable downward from the documented defaults", ErrInvalidConfig)
	}
	return nil
}

func (c Config) validateDatabase() error {
	if !c.Role.Processes() && !c.Role.Publishes() {
		// security.md requires least privilege per role, and this role opens no
		// store: operations.md requires ingestion to keep running through a
		// database outage, which a replica that had to reach CockroachDB could
		// not do. Accepting the credential anyway would leave a replica holding
		// a secret nothing in it can use, and would hide that the operator
		// believes this replica talks to CockroachDB when it never will.
		if c.DatabaseDSN != "" {
			return fmt.Errorf("%w: %s is set but role %q opens no database; a replica must not hold a credential it cannot use",
				ErrInvalidConfig, DatabaseDSNEnvVar, c.Role)
		}
		return nil
	}
	if c.DatabaseDSN == "" {
		return fmt.Errorf("%w: %s must be set for role %q", ErrInvalidConfig, DatabaseDSNEnvVar, c.Role)
	}
	// The parse error is discarded rather than wrapped: pgx echoes the
	// connection string it failed on, and this error is printed and shipped.
	if _, err := pgxpool.ParseConfig(c.DatabaseDSN); err != nil {
		return fmt.Errorf("%w: %s is not a valid CockroachDB connection string", ErrInvalidConfig, DatabaseDSNEnvVar)
	}
	return nil
}

func (c Config) validateWorker() error {
	if !c.Role.Processes() {
		return nil
	}
	if c.Worker.Cohort < 1 || c.Worker.Cohort > persistence.MaxProcessBatch {
		return fmt.Errorf("%w: -process-cohort must be between 1 and %d", ErrInvalidConfig, persistence.MaxProcessBatch)
	}
	if c.Worker.Interval <= 0 || c.Worker.IdleInterval <= 0 {
		return fmt.Errorf("%w: -process-interval and -process-idle-interval must be positive, or the worker busy-spins", ErrInvalidConfig)
	}
	if c.Worker.BackoffMin <= 0 || c.Worker.BackoffMax < c.Worker.BackoffMin {
		return fmt.Errorf("%w: -process-backoff-min must be positive and no greater than -process-backoff-max", ErrInvalidConfig)
	}
	return nil
}

func (c Config) validateIngress() error {
	if !c.Role.Ingests() {
		return nil
	}
	if c.Ingress.Trust.Source == "" {
		return fmt.Errorf("%w: -otlp-trust-source must be chosen explicitly (%s or %s); a receiver nobody chose to leave unauthenticated must not be reachable by default",
			ErrInvalidConfig, otlpreceiver.TrustSourceMutualTLS, otlpreceiver.TrustSourceStaticLocal)
	}
	if _, err := otlpreceiver.NewAuthenticator(c.Ingress.Trust); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	grpcAddr, err := normalizeListen(c.Ingress.GRPCListen)
	if err != nil {
		return fmt.Errorf("%w: -otlp-grpc-listen: %v", ErrInvalidConfig, err)
	}
	httpAddr, err := normalizeListen(c.Ingress.HTTPListen)
	if err != nil {
		return fmt.Errorf("%w: -otlp-http-listen: %v", ErrInvalidConfig, err)
	}
	// Port 0 asks the operating system for a free port, so two of them are two
	// different ports and not a conflict.
	if grpcAddr == httpAddr && !strings.HasSuffix(grpcAddr, ":0") {
		return fmt.Errorf("%w: -otlp-grpc-listen and -otlp-http-listen are the same address; one transport would never bind", ErrInvalidConfig)
	}
	return c.validateIngressBounds()
}

// validateIngressBounds refuses a bound an operator got wrong rather than
// quietly substituting the default for it. Zero is the documented "use the
// default" value; a negative one is a mistake, and silently treating it as the
// default would hide that the operator believes a different bound is in force.
func (c Config) validateIngressBounds() error {
	for _, bound := range []struct {
		flag  string
		value time.Duration
	}{
		{"-otlp-read-header-timeout", c.Ingress.ReadHeaderTimeout},
		{"-otlp-read-timeout", c.Ingress.ReadTimeout},
		{"-otlp-write-timeout", c.Ingress.WriteTimeout},
		{"-otlp-idle-timeout", c.Ingress.IdleTimeout},
	} {
		if bound.value < 0 {
			return fmt.Errorf("%w: %s must not be negative; 0 means the documented default", ErrInvalidConfig, bound.flag)
		}
	}
	if c.Ingress.MaxHeaderBytes < 0 {
		return fmt.Errorf("%w: -otlp-max-header-bytes must not be negative; 0 means the documented default", ErrInvalidConfig)
	}
	if c.Ingress.MaxInFlight < 0 {
		return fmt.Errorf("%w: -otlp-max-in-flight must not be negative; 0 means the documented default", ErrInvalidConfig)
	}
	// A read bound shorter than the header bound would end the request before
	// the headers it is waiting for could arrive, so every export would fail.
	if c.Ingress.ReadTimeout > 0 && c.Ingress.ReadHeaderTimeout > c.Ingress.ReadTimeout {
		return fmt.Errorf("%w: -otlp-read-header-timeout (%s) exceeds -otlp-read-timeout (%s); no request could finish being read",
			ErrInvalidConfig, c.Ingress.ReadHeaderTimeout, c.Ingress.ReadTimeout)
	}
	return nil
}

// Policy builds the redaction policy this deployment runs. Its version becomes
// part of the journal manifest and the store boundary, so it is derived here
// once and reused rather than rebuilt per component.
func (c Config) Policy() (*redact.Policy, error) {
	if c.RedactionPolicy != PolicyBaseline {
		return nil, fmt.Errorf("%w: unknown redaction policy %q", ErrInvalidConfig, c.RedactionPolicy)
	}
	if len(c.ForbiddenValues) == 0 {
		return redact.MinimalPolicy(), nil
	}
	return redact.MinimalPolicy().WithForbiddenValues(c.ForbiddenValues...)
}

// Describe is the startup line an operator reads to confirm what this process
// believes it is. It never contains the DSN, which carries a password.
func (c Config) Describe() string {
	fields := []string{
		"role=" + string(c.Role), "region=" + c.Scope.Region, "tenant=" + c.Scope.TenantID,
		"classification=" + c.Classification, "redaction_policy=" + c.RedactionPolicy,
		"journal_dir=" + c.Journal.Dir, fmt.Sprintf("journal_max_bytes=%d", c.Journal.MaxBytes),
		"database=" + describeDatabase(c.DatabaseDSN),
	}
	if c.Role.Ingests() {
		fields = append(fields, "otlp_grpc="+c.Ingress.GRPCListen, "otlp_http="+c.Ingress.HTTPListen,
			"trust_source="+string(c.Ingress.Trust.Source))
	}
	if c.Role.Processes() {
		fields = append(fields, fmt.Sprintf("process_cohort=%d", c.Worker.Cohort), "process_interval="+c.Worker.Interval.String())
	}
	return strings.Join(fields, " ")
}

// describeDatabase reports only whether a DSN was supplied. Even the host is
// omitted: it is not what an operator is checking here, and every additional
// component copied out of the string is another way for the password beside it
// to be copied by a later edit.
func describeDatabase(dsn string) string {
	if dsn == "" {
		return "unconfigured"
	}
	return "configured"
}

func normalizeListen(address string) (string, error) {
	if trimmed(address) == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("%q is not a host:port address", address)
	}
	if port == "" {
		return "", fmt.Errorf("%q has no port", address)
	}
	return net.JoinHostPort(host, port), nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, strings.TrimSpace(part))
	}
	return result
}

func trimmed(value string) string { return strings.TrimSpace(value) }

func validClassification(value string) bool {
	switch value {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	default:
		return false
	}
}
