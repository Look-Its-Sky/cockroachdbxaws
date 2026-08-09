//go:build integration

package persistence

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

func TestInvestigationCandidateElectsExactlyAtFiveInHalfOpenWindow(t *testing.T) {
	store, pool := integrationStore(t)
	clock := fakeclock.NewAtOrigin()
	factory := builders.NewFactory(builders.WithClock(clock))
	idSource := testids.New(testids.WithClock(clock))
	records := make([]model.NormalizedLog, 6)
	for i := range records {
		records[i] = factory.Record(t, builders.WithEventTime(fakeclock.Origin.Add(time.Duration(i)*time.Second)))
	}
	inputs := candidateInputs(t, idSource, records)
	for i := 0; i < 4; i++ {
		result, err := store.Process(context.Background(), inputs[i])
		if err != nil || result.InvestigationCreated {
			t.Fatalf("record %d elected early: result=%+v err=%v", i+1, result, err)
		}
		snapshot, err := store.Incident(context.Background(), inputs[i].Scope, inputs[i].IncidentID)
		if err != nil || snapshot.ContextVersion != 0 || snapshot.ActiveInvestigationID != "" {
			t.Fatalf("candidate mutated context before election: snapshot=%+v err=%v", snapshot, err)
		}
	}
	fifth, err := store.Process(context.Background(), inputs[4])
	if err != nil || !fifth.InvestigationCreated {
		t.Fatalf("fifth did not elect: result=%+v err=%v", fifth, err)
	}
	var generationContext int64
	if err := pool.QueryRow(context.Background(), `SELECT context_version FROM incident_generations
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4`, inputs[4].Scope.Region,
		inputs[4].Scope.TenantID, fifth.IncidentID, fifth.Generation).Scan(&generationContext); err != nil || generationContext != 1 {
		t.Fatalf("winner did not advance generation context: version=%d err=%v", generationContext, err)
	}
	sixth, err := store.Process(context.Background(), inputs[5])
	if err != nil || sixth.InvestigationCreated || sixth.InvestigationID != fifth.InvestigationID {
		t.Fatalf("later candidate was not a no-op: fifth=%+v sixth=%+v err=%v", fifth, sixth, err)
	}
	contextValue, err := store.InvestigationContext(context.Background(), inputs[4].Scope, fifth.InvestigationID, 1)
	if err != nil || contextValue.Version != 1 {
		t.Fatalf("claimable context: value=%+v err=%v", contextValue, err)
	}
}

func TestInvestigationCandidateWindowBoundaryAndGenerationIsolation(t *testing.T) {
	tests := []struct {
		name      string
		offsets   []time.Duration
		deploy    []string
		wantAgent bool
	}{
		{name: "inside", offsets: []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 5*time.Minute - time.Nanosecond}, wantAgent: true},
		{name: "exact lower boundary excluded", offsets: []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 5 * time.Minute}},
		{name: "outside", offsets: []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 5*time.Minute + time.Nanosecond}},
		{name: "three plus two deployments", offsets: []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}, deploy: []string{"deploy-a", "deploy-a", "deploy-a", "deploy-b", "deploy-b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := integrationStore(t)
			clock := fakeclock.NewAtOrigin()
			factory := builders.NewFactory(builders.WithClock(clock))
			ids := testids.New(testids.WithClock(clock))
			records := make([]model.NormalizedLog, len(tc.offsets))
			for i, offset := range tc.offsets {
				opts := []builders.RecordOption{builders.WithEventTime(fakeclock.Origin.Add(offset))}
				if len(tc.deploy) > 0 {
					opts = append(opts, builders.WithDeployment(tc.deploy[i], "v1"))
				}
				records[i] = factory.Record(t, opts...)
			}
			inputs := candidateInputs(t, ids, records)
			created := 0
			for i := len(inputs) - 1; i >= 0; i-- { // reverse arrival proves late completion
				result, err := store.Process(context.Background(), inputs[i])
				if err != nil {
					t.Fatal(err)
				}
				if result.InvestigationCreated {
					created++
				}
			}
			if got := created == 1; got != tc.wantAgent {
				t.Fatalf("created=%d wantAgent=%v", created, tc.wantAgent)
			}
		})
	}
}

func TestConcurrentSatisfiedCandidatesElectOneInvestigation(t *testing.T) {
	store, _ := integrationStore(t)
	clock := fakeclock.NewAtOrigin()
	factory := builders.NewFactory(builders.WithClock(clock))
	ids := testids.New(testids.WithClock(clock))
	records := make([]model.NormalizedLog, 6)
	for i := range records {
		records[i] = factory.Record(t, builders.WithEventTime(fakeclock.Origin.Add(time.Duration(i)*time.Second)))
	}
	inputs := candidateInputs(t, ids, records)
	for i := 0; i < 4; i++ {
		if _, err := store.Process(context.Background(), inputs[i]); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan ProcessResult, 2)
	errs := make(chan error, 2)
	for _, input := range inputs[4:] {
		input := input
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := store.Process(context.Background(), input)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	created := 0
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.InvestigationCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("concurrent candidates created %d investigations", created)
	}
}

func TestInvestigationCandidateDoesNotWaitForEnrichmentAvailability(t *testing.T) {
	store, _ := integrationStore(t)
	clock := fakeclock.NewAtOrigin()
	factory := builders.NewFactory(builders.WithClock(clock))
	ids := testids.New(testids.WithClock(clock))
	records := make([]model.NormalizedLog, 5)
	for i := range records {
		records[i] = factory.Record(t, builders.WithEventTime(fakeclock.Origin.Add(time.Duration(i)*time.Second)))
		records[i].Service.Status = model.EnrichmentPending
		if err := records[i].Validate(); err != nil {
			t.Fatal(err)
		}
	}
	inputs := candidateInputs(t, ids, records)
	created := 0
	for _, input := range inputs {
		result, err := store.Process(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if result.InvestigationCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("pending enrichment created %d investigations, want 1", created)
	}
}

func candidateInputs(t *testing.T, ids *testids.Source, records []model.NormalizedLog) []ProcessInput {
	t.Helper()
	engine := incident.New()
	for _, record := range records {
		if _, err := engine.Observe(record); err != nil {
			t.Fatal(err)
		}
	}
	latest := records[0].EventTime
	for _, record := range records[1:] {
		if record.EventTime.After(latest) {
			latest = record.EventTime
		}
	}
	engine.Advance(latest.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	inputs := make([]ProcessInput, len(records))
	for i, record := range records {
		decision, ok := engine.PersistenceDecision(record.RecordID)
		if !ok {
			t.Fatalf("record %d did not finalize", i)
		}
		input := preparedInput(t, ids, record, true)
		applyDecision(&input, decision)
		setAssignmentBody(t, &input)
		input.InvestigationCandidate = input.Investigation
		input.Investigation = nil
		input.ContextVersion = 0
		inputs[i] = input
	}
	return inputs
}
