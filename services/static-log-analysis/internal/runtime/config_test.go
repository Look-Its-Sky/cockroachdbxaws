package runtime_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/otlpreceiver"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/runtime"
)

const secretPassword = "sup3r-s3cret-password"

const (
	validQueueURL          = "https://sqs.us-east-1.amazonaws.com/000000000000/agent-assignments"
	validDeadLetterQueue   = "https://sqs.us-east-1.amazonaws.com/000000000000/agent-assignments-dlq"
	foreignRegionQueueURL  = "https://sqs.eu-central-1.amazonaws.com/000000000000/agent-assignments"
	localEndpointURL       = "http://127.0.0.1:4566"
	localEndpointQueueURL  = "http://127.0.0.1:4566/000000000000/agent-assignments"
	localEndpointDeadQueue = "http://127.0.0.1:4566/000000000000/agent-assignments-dlq"
)

// validArgs is a complete, deployable configuration for every role. Each test
// removes or corrupts exactly one thing so the failure it asserts is the only
// difference from a configuration that starts.
func validArgs(role string, overrides ...string) []string {
	args := []string{role,
		"-region=us-east-1", "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=/var/lib/log-analysis/journal", "-journal-max-bytes=1073741824",
		"-otlp-trust-source=static_local", "-otlp-source-account=aws-account-a",
		"-otlp-source-instance=collector-a", "-otlp-credential-identity=workload-a",
		"-otlp-allowed-environments=production", "-otlp-allowed-services=paymentservice",
		"-outbox-queue-url=" + validQueueURL, "-outbox-dead-letter-queue-url=" + validDeadLetterQueue,
	}
	return append(args, overrides...)
}

func env(dsn string) func(string) string {
	return func(name string) string {
		if name == runtime.DatabaseDSNEnvVar {
			return dsn
		}
		return ""
	}
}

const validDSN = "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable"

func parse(t *testing.T, args []string, getenv func(string) string) (runtime.Config, error) {
	t.Helper()
	return runtime.Parse(args, getenv, io.Discard)
}

func TestParseAcceptsEveryDocumentedRole(t *testing.T) {
	// Each role is given the environment it is entitled to. A role that opens no
	// database is refused a database credential, so handing one to every role
	// here would be testing a configuration an operator is not allowed to make.
	for _, role := range []string{"all", "ingest", "process", "outbox", "source", "cloudwatch"} {
		dsn := validDSN
		args := validArgs(role)
		if role == "ingest" {
			dsn = ""
		}
		if role == "source" {
			// A source replica polls rather than listens and holds no database
			// credential, so it needs its own complete configuration.
			dsn, args = "", sourceArgs(t)
		}
		if role == "cloudwatch" {
			args = cloudWatchArgs(t)
		}
		config, err := parse(t, args, env(dsn))
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		if string(config.Role) != role {
			t.Fatalf("role %q parsed as %q", role, config.Role)
		}
	}
}

func TestCloudWatchRoleCombinesPollingAndProcessingWithoutOpeningIngress(t *testing.T) {
	config, err := parse(t, cloudWatchArgs(t), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Role.Polls() || !config.Role.Processes() {
		t.Fatalf("cloudwatch role traits: polls=%v processes=%v", config.Role.Polls(), config.Role.Processes())
	}
	if config.Role.Ingests() || config.Role.Publishes() {
		t.Fatalf("cloudwatch role unexpectedly listens or publishes: ingests=%v publishes=%v", config.Role.Ingests(), config.Role.Publishes())
	}
	if _, err := parse(t, cloudWatchArgs(t), env("")); !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("cloudwatch role started without the database needed to drain its journal: %v", err)
	}
}

func TestSourceOnlyRoleStillRefusesADatabaseCredential(t *testing.T) {
	if _, err := parse(t, sourceArgs(t), env(validDSN)); !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("source-only role accepted a database credential it cannot use: %v", err)
	}
}

func TestParseRefusesAnUnknownRole(t *testing.T) {
	if _, err := parse(t, validArgs("publisher"), env(validDSN)); err == nil {
		t.Fatal("an unknown role started")
	}
	if _, err := parse(t, []string{"-region=us-east-1"}, env(validDSN)); err == nil {
		t.Fatal("a missing role started")
	}
}

// The DSN is the one secret in this configuration surface. A flag default would
// put it in process arguments, in shell history, and in `--help` output.
func TestDatabaseDSNComesFromTheEnvironmentAndIsNotAFlag(t *testing.T) {
	if _, err := parse(t, validArgs("all", "-database-dsn="+validDSN), env("")); err == nil {
		t.Fatal("the DSN was accepted as a flag")
	}
	config, err := parse(t, validArgs("all"), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	if config.DatabaseDSN != validDSN {
		t.Fatalf("DSN was not read from %s", runtime.DatabaseDSNEnvVar)
	}
	if _, err := parse(t, validArgs("all"), env("")); err == nil {
		t.Fatal("a role that needs the database started without a DSN")
	}
}

// An operator-facing error is printed and often shipped to a log aggregator, so
// a rejected DSN must not carry itself into that error. The driver's own parse
// error echoes the connection string, and masking inside a dependency is not a
// property this service may rely on, so no part of the DSN is repeated at all.
func TestRejectedDatabaseDSNNeverAppearsInTheError(t *testing.T) {
	dsn := "postgresql://root:" + secretPassword + "@db-primary.internal.example:notaport/defaultdb"
	_, err := parse(t, validArgs("all"), env(dsn))
	if err == nil {
		t.Fatal("an unparseable DSN started")
	}
	for _, fragment := range []string{secretPassword, dsn, "db-primary.internal.example"} {
		if strings.Contains(err.Error(), fragment) {
			t.Fatalf("the DSN leaked %q into an operator-facing error: %v", fragment, err)
		}
	}
}

func TestIngestOnlyRoleDoesNotNeedTheDatabase(t *testing.T) {
	if _, err := parse(t, validArgs("ingest"), env("")); err != nil {
		t.Fatalf("ingest requires a database it never uses: %v", err)
	}
}

func TestProcessOnlyRoleDoesNotNeedIngressConfiguration(t *testing.T) {
	args := []string{"process", "-region=us-east-1", "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=/var/lib/journal", "-journal-max-bytes=1073741824"}
	if _, err := parse(t, args, env(validDSN)); err != nil {
		t.Fatalf("process requires ingress it never opens: %v", err)
	}
}

// A receiver whose trust source defaulted would be an unauthenticated receiver
// nobody chose. Selection is mandatory for any role that listens.
func TestIngressRolesRequireAnExplicitTrustSource(t *testing.T) {
	for _, role := range []string{"all", "ingest"} {
		// Everything a static_local receiver needs is present. The only thing
		// missing is the operator's decision about who may be trusted.
		complete := validArgs(role)
		args := make([]string, 0, len(complete))
		for _, arg := range complete {
			if !strings.HasPrefix(arg, "-otlp-trust-source=") {
				args = append(args, arg)
			}
		}
		if _, err := parse(t, args, env(validDSN)); err == nil {
			t.Fatalf("role %q opened a receiver with no configured trust source", role)
		}
	}
}

func TestParseRefusesEveryInvalidScopeOrBoundary(t *testing.T) {
	cases := map[string][]string{
		"empty region":         {"-region="},
		"empty tenant":         {"-tenant-id="},
		"blank tenant":         {"-tenant-id=   "},
		"unknown class":        {"-classification=TOP_SECRET"},
		"prohibited class":     {"-classification=PROHIBITED"},
		"empty journal dir":    {"-journal-dir="},
		"zero journal bytes":   {"-journal-max-bytes=0"},
		"zero min free":        {"-journal-min-free-bytes=0"},
		"unknown policy":       {"-redaction-policy=permissive"},
		"empty worker owner":   {"-worker-owner="},
		"padded worker owner":  {"-worker-owner= owner "},
		"negative shutdown":    {"-shutdown-timeout=-1s"},
		"zero shutdown":        {"-shutdown-timeout=0s"},
		"bad grpc listen":      {"-otlp-grpc-listen=not-an-address"},
		"bad http listen":      {"-otlp-http-listen=not-an-address"},
		"identical listeners":  {"-otlp-grpc-listen=:4317", "-otlp-http-listen=:4317"},
		"no allowed service":   {"-otlp-allowed-services="},
		"no allowed env":       {"-otlp-allowed-environments="},
		"empty source account": {"-otlp-source-account="},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, validArgs("all", overrides...), env(validDSN)); err == nil {
				t.Fatalf("%s started", name)
			}
		})
	}
}

// The admission package already owns which limit combinations are coherent.
// Startup surfaces that judgement instead of re-deciding it.
func TestParseRefusesAdmissionLimitsTheAdmissionPackageRefuses(t *testing.T) {
	cases := map[string][]string{
		"zero records":     {"-max-records=0"},
		"negative depth":   {"-max-nesting-depth=-1"},
		"zero normalized":  {"-max-normalized-bytes=0"},
		"compressed above": {"-max-compressed-bytes=33554432"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, validArgs("all", overrides...), env(validDSN)); err == nil {
				t.Fatalf("%s started", name)
			}
		})
	}
}

func TestParseRefusesWorkerCadenceTheProcessBoundaryCannotHonour(t *testing.T) {
	cases := map[string][]string{
		"zero cohort":      {"-process-cohort=0"},
		"oversized cohort": {"-process-cohort=101"},
		"zero interval":    {"-process-interval=0s"},
		"zero idle":        {"-process-idle-interval=0s"},
		"inverted backoff": {"-process-backoff-min=30s", "-process-backoff-max=1s"},
		"zero backoff":     {"-process-backoff-min=0s"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, validArgs("all", overrides...), env(validDSN)); err == nil {
				t.Fatalf("%s started", name)
			}
		})
	}
	if persistence.MaxProcessBatch != 100 {
		t.Fatalf("the oversized-cohort case no longer straddles MaxProcessBatch=%d", persistence.MaxProcessBatch)
	}
}

func TestDefaultsMatchTheDocumentedPortsAndAdmissionLimits(t *testing.T) {
	config, err := parse(t, validArgs("all"), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	if config.Ingress.GRPCListen != ":4317" || config.Ingress.HTTPListen != ":4318" {
		t.Fatalf("listen defaults are %q and %q", config.Ingress.GRPCListen, config.Ingress.HTTPListen)
	}
	want := admission.Limits{
		MaxCompressedBytes: admission.DefaultMaxCompressedBytes, MaxUncompressedBytes: admission.DefaultMaxUncompressedBytes,
		MaxRecords: admission.DefaultMaxRecords, MaxNestingDepth: admission.DefaultMaxNestingDepth,
		MaxNormalizedBytes: admission.DefaultMaxNormalizedBytes, MaxMaterializedBytes: admission.DefaultMaxMaterializedBytes,
		MaxStructuralNodes: admission.DefaultMaxStructuralNodes,
	}
	if config.Limits != want {
		t.Fatalf("admission limit defaults drifted: %+v", config.Limits)
	}
	if config.Scope != (persistence.Scope{Region: "us-east-1", TenantID: "tenant-a"}) {
		t.Fatalf("scope=%+v", config.Scope)
	}
	if config.Ingress.Trust.Source != otlpreceiver.TrustSourceStaticLocal {
		t.Fatalf("trust source=%q", config.Ingress.Trust.Source)
	}
	if config.Worker.Cohort < 1 || config.Worker.Cohort > persistence.MaxProcessBatch ||
		config.Worker.Interval <= 0 || config.Worker.IdleInterval <= 0 ||
		config.Worker.BackoffMin <= 0 || config.Worker.BackoffMax < config.Worker.BackoffMin {
		t.Fatalf("worker defaults are not a usable cadence: %+v", config.Worker)
	}
	if config.ShutdownTimeout <= 0 {
		t.Fatalf("shutdown timeout default=%s", config.ShutdownTimeout)
	}
}

// The publisher's whole job is to move committed assignments onto a queue. A
// replica that could not name both queues, or that named a queue in another
// region, must be refused before it publishes anything: security.md and
// architecture.md forbid regional movement of log-derived content, and a
// dead-letter queue nobody configured means a poisoned message would be retried
// forever.
func TestOutboxRoleRefusesEveryQueueConfigurationItCouldNotPublishThrough(t *testing.T) {
	cases := map[string][]string{
		"no queue":                      {"-outbox-queue-url="},
		"no dead-letter queue":          {"-outbox-dead-letter-queue-url="},
		"one queue serving both roles":  {"-outbox-dead-letter-queue-url=" + validQueueURL},
		"queue in another region":       {"-outbox-queue-url=" + foreignRegionQueueURL},
		"dead-letter in another region": {"-outbox-dead-letter-queue-url=" + foreignRegionQueueURL},
		"queue that is not a URL":       {"-outbox-queue-url=agent-assignments"},
		"queue on a plaintext endpoint": {"-outbox-queue-url=http://sqs.us-east-1.amazonaws.com/0/q"},
		"local queue without a declared endpoint": {"-outbox-queue-url=" + localEndpointQueueURL,
			"-outbox-dead-letter-queue-url=" + localEndpointDeadQueue},
		"endpoint that contradicts the queue": {"-outbox-endpoint-url=" + localEndpointURL},
		"batch larger than one claim":         {fmt.Sprintf("-outbox-batch=%d", persistence.MaxClaimBatch+1)},
		"zero batch":                          {"-outbox-batch=0"},
		"zero claim lease":                    {"-outbox-claim-ttl=0s"},
		"zero publish timeout":                {"-outbox-publish-timeout=0s"},
		"inverted retry schedule":             {"-outbox-retry-min=5m", "-outbox-retry-max=1s"},
		"no attempts at all":                  {"-outbox-max-attempts=0"},
		"zero idle interval":                  {"-outbox-idle-interval=0s"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, validArgs("outbox", overrides...), env(validDSN)); err == nil {
				t.Fatalf("an outbox replica with %s started", name)
			}
		})
	}
}

// A local SQS-compatible endpoint is how this runs without AWS. Declaring it
// explicitly is what makes the region check on a production queue URL safe to
// enforce.
func TestOutboxRoleAcceptsADeclaredLocalEndpoint(t *testing.T) {
	config, err := parse(t, validArgs("outbox",
		"-outbox-endpoint-url="+localEndpointURL,
		"-outbox-queue-url="+localEndpointQueueURL,
		"-outbox-dead-letter-queue-url="+localEndpointDeadQueue), env(validDSN))
	if err != nil {
		t.Fatalf("a declared local endpoint was refused: %v", err)
	}
	if config.Outbox.EndpointURL != localEndpointURL || config.Outbox.QueueURL != localEndpointQueueURL {
		t.Fatalf("outbox=%+v", config.Outbox)
	}
}

// The publisher role opens no journal and no listener, so it must not require
// either. runtime.md's role table is the contract.
func TestOutboxRoleNeedsNeitherAJournalNorIngressConfiguration(t *testing.T) {
	args := []string{"outbox", "-region=us-east-1", "-tenant-id=tenant-a",
		"-outbox-queue-url=" + validQueueURL, "-outbox-dead-letter-queue-url=" + validDeadLetterQueue}
	if _, err := parse(t, args, env(validDSN)); err != nil {
		t.Fatalf("outbox requires a journal and ingress it never opens: %v", err)
	}
	// It does hold a database credential: the outbox lives in CockroachDB.
	if _, err := parse(t, args, env("")); err == nil {
		t.Fatal("a publisher started without the database that holds the outbox")
	}
}

func TestOutboxDefaultsAreAUsablePublisherSchedule(t *testing.T) {
	config, err := parse(t, validArgs("outbox"), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	got := config.Outbox
	if got.Batch < 1 || got.Batch > persistence.MaxClaimBatch || got.ClaimTTL <= 0 || got.PublishTimeout <= 0 ||
		got.RetryMin <= 0 || got.RetryMax < got.RetryMin || got.MaxAttempts < 1 {
		t.Fatalf("publisher defaults are not usable: %+v", got)
	}
	// A claim lease must outlast one delivery attempt, or every message would be
	// published under a lease that expired while it was in flight.
	if got.ClaimTTL <= got.PublishTimeout {
		t.Fatalf("claim lease %s does not cover one %s publish attempt", got.ClaimTTL, got.PublishTimeout)
	}
	if got.Cadence.Interval <= 0 || got.Cadence.IdleInterval <= 0 ||
		got.Cadence.BackoffMin <= 0 || got.Cadence.BackoffMax < got.Cadence.BackoffMin {
		t.Fatalf("publisher cadence defaults are not usable: %+v", got.Cadence)
	}
}

func TestForbiddenRedactionValuesReachThePolicy(t *testing.T) {
	config, err := parse(t, validArgs("all", "-redaction-forbidden-values=alpha,beta"), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := config.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.ValidateText("this contains alpha") == nil {
		t.Fatal("a configured forbidden value was not enforced by the built policy")
	}
}

// mutual_tls authenticates the caller, so the caller's identity may not also be
// asserted by the operator: two disagreeing sources of one truth is exactly the
// ambiguity the trusted envelope exists to remove.
func TestMutualTLSRefusesOperatorSuppliedCallerIdentity(t *testing.T) {
	base := []string{"all", "-region=us-east-1", "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=/var/lib/journal", "-journal-max-bytes=1073741824",
		"-otlp-trust-source=mutual_tls", "-otlp-source-account=aws-account-a",
		"-otlp-allowed-environments=production", "-otlp-allowed-services=paymentservice",
		"-otlp-tls-cert=/tls/server.crt", "-otlp-tls-key=/tls/server.key", "-otlp-client-ca=/tls/ca.crt"}
	if _, err := parse(t, base, env(validDSN)); err != nil {
		t.Fatalf("a complete mutual_tls configuration was refused: %v", err)
	}
	for _, override := range [][]string{
		{"-otlp-credential-identity=workload-a"},
		{"-otlp-source-instance=collector-a"},
	} {
		if _, err := parse(t, append(append([]string{}, base...), override...), env(validDSN)); err == nil {
			t.Fatalf("mutual_tls accepted operator-declared %v", override)
		}
	}
	for _, missing := range []string{"-otlp-tls-cert=", "-otlp-tls-key=", "-otlp-client-ca="} {
		if _, err := parse(t, append(append([]string{}, base...), missing), env(validDSN)); err == nil {
			t.Fatalf("mutual_tls started without %s", missing)
		}
	}
}

func TestStaticLocalTrustRefusesTLSMaterialItWouldNotVerify(t *testing.T) {
	if _, err := parse(t, validArgs("all", "-otlp-client-ca=/tls/ca.crt"), env(validDSN)); err == nil {
		t.Fatal("static_local accepted a client CA it never verifies against")
	}
}

func TestParsedConfigurationRedactsTheDSNWhenDescribed(t *testing.T) {
	config, err := parse(t, validArgs("all"), env("postgresql://root:"+secretPassword+"@127.0.0.1:26257/defaultdb?sslmode=disable"))
	if err != nil {
		t.Fatal(err)
	}
	described := config.Describe()
	if strings.Contains(described, secretPassword) {
		t.Fatalf("the DSN password reached the startup description: %s", described)
	}
	if !strings.Contains(described, "us-east-1") || !strings.Contains(described, "all") {
		t.Fatalf("the startup description omits the scope an operator needs: %s", described)
	}
}

func TestShutdownTimeoutIsParsedAsADuration(t *testing.T) {
	config, err := parse(t, validArgs("all", "-shutdown-timeout=45s"), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	if config.ShutdownTimeout != 45*time.Second {
		t.Fatalf("shutdown timeout=%s", config.ShutdownTimeout)
	}
}

// TestAnIngestReplicaRefusesADatabaseCredentialItMustNotHold pins least
// privilege per role at the point an operator can still act on it.
//
// The ingest role opens no store: operations.md requires ingestion to keep
// running through a database outage, and security.md requires least privilege
// per role. Accepting the credential anyway would leave a replica holding a
// secret it has no use for, and would hide that the operator believes this
// replica talks to CockroachDB when nothing in it ever will.
func TestAnIngestReplicaRefusesADatabaseCredentialItMustNotHold(t *testing.T) {
	_, err := runtime.Parse(serverArgs(t, "ingest"), func(string) string { return validDSN }, io.Discard)
	if !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("an ingest replica accepted a database credential it never uses: %v", err)
	}
}

// TestAnIngestReplicaStillStartsWithoutOne is the other half: the refusal above
// must be about the credential being present, not about it being checked.
func TestAnIngestReplicaStillStartsWithoutOne(t *testing.T) {
	if _, err := runtime.Parse(serverArgs(t, "ingest"), func(string) string { return "" }, io.Discard); err != nil {
		t.Fatalf("an ingest replica requires a database credential it must not hold: %v", err)
	}
}

func sourceArgs(t *testing.T, overrides ...string) []string {
	t.Helper()
	args := []string{"source",
		"-region=us-east-1", "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + t.TempDir(), "-journal-max-bytes=67108864",
		"-cloudwatch-account=000000000000",
		"-cloudwatch-log-groups=/aws/ecs/payments=paymentservice=production",
		"-cloudwatch-source-instance=cw-adapter-a",
		"-cloudwatch-credential-identity=workload-a",
		"-cloudwatch-checkpoint-dir=" + t.TempDir(),
	}
	return append(args, overrides...)
}

func cloudWatchArgs(t *testing.T, overrides ...string) []string {
	t.Helper()
	args := sourceArgs(t)
	args[0] = "cloudwatch"
	return append(args, overrides...)
}

func TestASourceReplicaParsesItsGroupsWithTheIdentityTheOperatorDeclared(t *testing.T) {
	config, err := parse(t, sourceArgs(t), env(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Source.Groups) != 1 {
		t.Fatalf("groups=%+v", config.Source.Groups)
	}
	group := config.Source.Groups[0]
	if group.LogGroup != "/aws/ecs/payments" || group.Service != "paymentservice" || group.Environment != "production" {
		t.Fatalf("group=%+v", group)
	}
	// The allow-sets are derived rather than configured, so they can never be
	// narrower than the groups they must admit.
	if got := config.SourceAllowedServices(); len(got) != 1 || got[0] != "paymentservice" {
		t.Fatalf("allowed services=%v", got)
	}
	if got := config.SourceAllowedEnvironments(); len(got) != 1 || got[0] != "production" {
		t.Fatalf("allowed environments=%v", got)
	}
}

func TestASourceReplicaRefusesAConfigurationItCouldNotReadWith(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		override []string
	}{
		{"no account", []string{"-cloudwatch-account="}},
		{"no groups", []string{"-cloudwatch-log-groups="}},
		{"no checkpoint volume", []string{"-cloudwatch-checkpoint-dir="}},
		{"no audit identity", []string{"-cloudwatch-source-instance="}},
		{"malformed group", []string{"-cloudwatch-log-groups=/aws/ecs/payments=paymentservice"}},
		{"padded group component", []string{"-cloudwatch-log-groups=/aws/ecs/payments= paymentservice=production"}},
		{"one group named twice", []string{"-cloudwatch-log-groups=/a=s=e,/a=s2=e"}},
		{"idle interval of zero", []string{"-cloudwatch-idle-interval=0"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parse(t, sourceArgs(t, testCase.override...), env("")); !errors.Is(err, runtime.ErrInvalidConfig) {
				t.Fatalf("a source replica accepted %s: %v", testCase.name, err)
			}
		})
	}
}

// TestACheckpointVolumeMustNotBeTheJournalVolume pins the one they are most
// likely to be pointed at together. They have different lifetimes: a journal is
// drained and may be rebuilt, and a checkpoint rebuilt with it would reread the
// whole lookback window.
func TestACheckpointVolumeMustNotBeTheJournalVolume(t *testing.T) {
	shared := t.TempDir()
	args := []string{"source",
		"-region=us-east-1", "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + shared, "-journal-max-bytes=67108864",
		"-cloudwatch-account=000000000000",
		"-cloudwatch-log-groups=/aws/ecs/payments=paymentservice=production",
		"-cloudwatch-source-instance=cw-adapter-a", "-cloudwatch-credential-identity=workload-a",
		"-cloudwatch-checkpoint-dir=" + shared,
	}
	if _, err := parse(t, args, env("")); !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("one volume was accepted as both journal and checkpoints: %v", err)
	}
}

// TestARoleThatPollsNothingRefusesSourceConfiguration is the same least-surprise
// rule the database credential follows: a replica must not hold configuration it
// cannot act on, because it reads as though this replica were retrieving logs
// when nothing in it ever will.
func TestARoleThatPollsNothingRefusesSourceConfiguration(t *testing.T) {
	args := append(validArgs("ingest"), "-cloudwatch-account=000000000000",
		"-cloudwatch-log-groups=/aws/ecs/payments=paymentservice=production")
	if _, err := parse(t, args, env("")); !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("an ingest replica accepted source configuration it never uses: %v", err)
	}
}

// TestSheddingMustBeConfiguredCompletelyOrNotAtAll pins that a half-configured
// policy is refused. Shedding drops data deliberately, so a configuration that
// looks like it sheds but does not — or that names a threshold with nothing
// eligible — is a misunderstanding an operator has to be told about.
func TestSheddingMustBeConfiguredCompletelyOrNotAtAll(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		overrides []string
		wantErr   bool
	}{
		{"neither", nil, false},
		{"both", []string{"-shed-near-capacity-percent=85", "-shed-eligible-sources=otlp"}, false},
		{"threshold with nothing eligible", []string{"-shed-near-capacity-percent=85"}, true},
		{"eligible with no threshold", []string{"-shed-eligible-sources=otlp"}, true},
		{"unknown source", []string{"-shed-near-capacity-percent=85", "-shed-eligible-sources=syslog"}, true},
		{"impossible percentage", []string{"-shed-near-capacity-percent=101", "-shed-eligible-sources=otlp"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parse(t, validArgs("all", testCase.overrides...), env(validDSN))
			if testCase.wantErr && !errors.Is(err, runtime.ErrInvalidConfig) {
				t.Fatalf("%s was accepted: %v", testCase.name, err)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("%s was refused: %v", testCase.name, err)
			}
		})
	}
}

// TestTheCapacityPolicyIsWhatWasConfigured keeps the derived policy honest, so
// a flag an operator set cannot silently mean something else.
func TestTheCapacityPolicyIsWhatWasConfigured(t *testing.T) {
	config, err := parse(t, validArgs("all", "-shed-near-capacity-percent=90", "-shed-eligible-sources=otlp,cloudwatch"), env(validDSN))
	if err != nil {
		t.Fatal(err)
	}
	policy := config.CapacityPolicy()
	if policy.NearCapacityPercent != 90 {
		t.Fatalf("threshold=%d", policy.NearCapacityPercent)
	}
	if len(policy.EligibleSources) != 2 {
		t.Fatalf("eligible sources=%v", policy.EligibleSources)
	}
}
