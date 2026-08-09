package pipeline_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// switchableResolver is a deployment resolver whose answer an operator's next
// release changes. It is not a stand-in for the enrichment cache — that has its
// own tests against its own provider — it is how this test expresses "a
// deployment changed" at the seam the coordinator actually consumes.
type switchableResolver struct {
	mu         sync.Mutex
	deployment model.DeploymentIdentity
}

func (r *switchableResolver) Deployment(model.ServiceIdentity, string) model.DeploymentIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deployment
}

func (r *switchableResolver) set(id, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployment = model.DeploymentIdentity{ID: id, Version: version, Status: model.EnrichmentAvailable}
}

// TestADeploymentChangeReachesTheCoordinatorAsADistinctIdentity is the
// Milestone 1 behaviour that was untestable through the coordinator until
// enrichment existed: normalization hard-coded unknown-deployment, so no
// accepted OTLP input could produce two deployment identities.
//
// The engine's own grouping across generations is covered in internal/incident.
// What only this test can show is that the identity actually reaches durable
// records through the real ingestion path.
func TestADeploymentChangeReachesTheCoordinatorAsADistinctIdentity(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	resolver := &switchableResolver{}
	resolver.set("deploy-1", "1.0.0")

	service, err := pipeline.New(pipeline.Config{
		Clock: clock, IDs: testids.New(testids.WithClock(clock)), Journal: j, Store: &capturingStore{},
		Policy: policy, Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, Enrichment: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}

	envelope := model.TrustedEnvelope{SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a",
		Region: otlpgen.DefaultRegion, AllowedEnvironments: []string{otlpgen.DefaultEnvironment},
		AllowedServices: []string{otlpgen.DefaultService}, SourceInstance: "collector-a",
		CredentialIdentity: "workload-a"}
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock), testids.WithSeed(11))))

	ingest := func(index int) {
		t.Helper()
		record := producer.PaymentError(otlpgen.WithBody("payment declined"),
			otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(index)*time.Second).UnixNano())))
		payload, err := otlpgen.Encode(producer.Request(record))
		if err != nil {
			t.Fatal(err)
		}
		envelope.ReceivedAt = clock.Now()
		result, err := service.Ingest(context.Background(), envelope, payload, admission.EncodingIdentity)
		if err != nil || !result.Acknowledged {
			t.Fatalf("ingest %d: result=%+v err=%v", index, result, err)
		}
	}

	ingest(0)
	// A new release goes out.
	resolver.set("deploy-2", "2.0.0")
	ingest(1)

	// Both records are durable, and they disagree about their deployment, which
	// is the whole point: before enrichment existed they could not.
	claims, err := j.Claim(10, "enrichment-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claimed %d records, want 2", len(claims))
	}
	seen := map[string]string{}
	for _, claim := range claims {
		if claim.Record.Deployment.Status != model.EnrichmentAvailable {
			t.Fatalf("record %s carries deployment status %q; a resolved deployment must be marked available",
				claim.Record.RecordID, claim.Record.Deployment.Status)
		}
		seen[claim.Record.Deployment.ID] = claim.Record.Deployment.Version
	}
	if len(seen) != 2 {
		t.Fatalf("two releases produced %d deployment identities: %v", len(seen), seen)
	}
	if seen["deploy-1"] != "1.0.0" || seen["deploy-2"] != "2.0.0" {
		t.Fatalf("deployment versions did not survive normalization: %v", seen)
	}
}

// TestWithoutAResolverEveryRecordGroupsUnderUnknownDeploymentAsPending pins the
// documented behaviour for a deployment whose identity is not yet known, and
// keeps enrichment optional rather than required to start.
func TestWithoutAResolverEveryRecordGroupsUnderUnknownDeploymentAsPending(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})

	envelope := model.TrustedEnvelope{SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a",
		Region: otlpgen.DefaultRegion, AllowedEnvironments: []string{otlpgen.DefaultEnvironment},
		AllowedServices: []string{otlpgen.DefaultService}, SourceInstance: "collector-a",
		CredentialIdentity: "workload-a", ReceivedAt: clock.Now()}
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock), testids.WithSeed(12))))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError()))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := service.Ingest(context.Background(), envelope, payload, admission.EncodingIdentity); err != nil || !result.Acknowledged {
		t.Fatalf("ingest: result=%+v err=%v", result, err)
	}

	claims, err := j.Claim(10, "enrichment-test")
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	deployment := claims[0].Record.Deployment
	if deployment.ID != model.UnknownDeployment || deployment.Status != model.EnrichmentPending {
		t.Fatalf("without a resolver a record carried %+v, want unknown-deployment and pending", deployment)
	}
}

// TestIngestionDoesNotWaitForEnrichment is acceptance item 5 stated as a
// property of the ingestion path rather than of the cache: even a resolver that
// takes a long time cannot make a record wait, because the contract forbids it
// and the coordinator relies on that.
//
// A resolver that violates the contract is a defect in that resolver; what this
// pins is that the coordinator calls it exactly once per record on the ingest
// path and does nothing else with it, so a compliant resolver's zero wait is
// the whole cost.
func TestIngestionDoesNotWaitForEnrichment(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	counting := &countingResolver{}
	service, err := pipeline.New(pipeline.Config{
		Clock: clock, IDs: testids.New(testids.WithClock(clock)), Journal: j, Store: &capturingStore{},
		Policy: policy, Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, Enrichment: counting,
	})
	if err != nil {
		t.Fatal(err)
	}

	envelope := model.TrustedEnvelope{SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a",
		Region: otlpgen.DefaultRegion, AllowedEnvironments: []string{otlpgen.DefaultEnvironment},
		AllowedServices: []string{otlpgen.DefaultService}, SourceInstance: "collector-a",
		CredentialIdentity: "workload-a", ReceivedAt: clock.Now()}
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock), testids.WithSeed(13))))
	records := []*otlpgen.Record{producer.PaymentError(), producer.PaymentError(), producer.PaymentError()}
	payload, err := otlpgen.Encode(producer.Request(records...))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := service.Ingest(context.Background(), envelope, payload, admission.EncodingIdentity); err != nil || result.Accepted != 3 {
		t.Fatalf("ingest: result=%+v err=%v", result, err)
	}
	if got := counting.count(); got != 3 {
		t.Fatalf("the resolver was consulted %d times for 3 records; enrichment must be one lookup per record and nothing more", got)
	}
}

type countingResolver struct {
	mu     sync.Mutex
	calls  int
	answer model.DeploymentIdentity
}

func (r *countingResolver) Deployment(model.ServiceIdentity, string) model.DeploymentIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.answer
}

func (r *countingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
