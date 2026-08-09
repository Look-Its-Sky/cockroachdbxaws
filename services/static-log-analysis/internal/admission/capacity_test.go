package admission_test

import (
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

func TestOnlyExplicitlyUnprotectedDebugAndInfoMayShedAtThreshold(t *testing.T) {
	policy := admission.CapacityPolicy{NearCapacityPercent: 80, EligibleSources: []model.SourceType{model.SourceTypeOTLP}}
	state := admission.CapacityState{Used: 80, Capacity: 100, CanPersist: true}
	unprotected := admission.Classification{Protected: false, Certain: true}

	for _, severity := range []model.SeverityClass{model.SeverityClassDebug, model.SeverityClassInfo} {
		got := policy.Decide(admission.Record{Source: model.SourceTypeOTLP, Severity: severity}, unprotected, state)
		if got.Action != admission.ActionShed || !got.Acknowledged || got.Counter != admission.CounterDropped {
			t.Fatalf("%s must shed with an explicit dropped result, got %+v", severity, got)
		}
	}

	tests := []struct {
		name           string
		record         admission.Record
		classification admission.Classification
	}{
		{"below threshold", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassInfo}, unprotected},
		{"ineligible source", admission.Record{Source: model.SourceTypeCloudWatch, Severity: model.SeverityClassInfo}, unprotected},
		{"warning", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassWarn}, unprotected},
		{"error", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassError}, unprotected},
		{"fatal", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassFatal}, unprotected},
		{"protected", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassInfo}, admission.Classification{Protected: true, Certain: true}},
		{"uncertain", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassInfo}, admission.Classification{Protected: true, Certain: false}},
		{"security", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassInfo, Security: true}, unprotected},
		{"event triggering", admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassInfo, EventTriggering: true}, unprotected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testState := state
			if test.name == "below threshold" {
				testState.Used = 79
			}
			got := policy.Decide(test.record, test.classification, testState)
			if got.Action != admission.ActionAdmit || got.Acknowledged {
				t.Fatalf("must remain admitted, got %+v", got)
			}
		})
	}
}

func TestProtectedInputReceivesRetryableBackpressureNotFalseAcknowledgement(t *testing.T) {
	policy := admission.CapacityPolicy{NearCapacityPercent: 80, EligibleSources: []model.SourceType{model.SourceTypeOTLP}}
	got := policy.Decide(
		admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassError},
		admission.Classification{Protected: true, Certain: true},
		admission.CapacityState{Used: 100, Capacity: 100, CanPersist: false},
	)

	if got.Action != admission.ActionBackpressure || got.Acknowledged || !got.Retryable || got.Counter != admission.CounterBackpressured {
		t.Fatalf("protected input must receive explicit retryable backpressure, got %+v", got)
	}
}

func TestCapacityBoundaryDoesNotOverflow(t *testing.T) {
	policy := admission.CapacityPolicy{NearCapacityPercent: 80, EligibleSources: []model.SourceType{model.SourceTypeOTLP}}
	record := admission.Record{Source: model.SourceTypeOTLP, Severity: model.SeverityClassInfo}
	class := admission.Classification{Certain: true}
	for _, state := range []admission.CapacityState{
		{Used: ^uint64(0), Capacity: ^uint64(0), CanPersist: true},
		{Used: 1, Capacity: 0, CanPersist: false},
	} {
		got := policy.Decide(record, class, state)
		if state.Capacity == 0 && got.Action != admission.ActionBackpressure {
			t.Fatalf("unknown capacity must fail closed when persistence is unavailable, got %+v", got)
		}
	}
}

func TestCapacityResultsProduceDeterministicCounters(t *testing.T) {
	counters := admission.Counters{}
	counters = counters.With(admission.CapacityResult{Counter: admission.CounterDropped})
	counters = counters.With(admission.CapacityResult{Counter: admission.CounterBackpressured})
	counters = counters.With(admission.CapacityResult{Counter: admission.CounterDropped})
	counters = counters.With(admission.CapacityResult{})
	if counters.Dropped != 2 || counters.Backpressured != 1 {
		t.Fatalf("want dropped=2 backpressured=1, got %+v", counters)
	}
}
