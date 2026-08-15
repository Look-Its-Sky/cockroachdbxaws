package admission

import "github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"

// CapacityPolicy controls pre-journal shedding. NearCapacityPercent is an
// integer percentage so boundary behavior does not depend on floating point.
type CapacityPolicy struct {
	NearCapacityPercent uint8
	EligibleSources     []model.SourceType
}

// CapacityState is a point-in-time journal capacity observation.
type CapacityState struct {
	Used       uint64
	Capacity   uint64
	CanPersist bool
}

type Action string

const (
	ActionAdmit        Action = "admit"
	ActionShed         Action = "shed"
	ActionBackpressure Action = "backpressure"
)

type Counter string

const (
	CounterNone          Counter = ""
	CounterDropped       Counter = "dropped"
	CounterBackpressured Counter = "backpressured"
)

// CapacityResult is explicit about transport semantics. Admit is deliberately
// not acknowledged: only a later synchronized journal commit may do that.
type CapacityResult struct {
	Action       Action
	Acknowledged bool
	Retryable    bool
	Counter      Counter
}

// Counters is an immutable aggregation of admission outcomes. With returns a
// new value, which keeps retries and tests free from hidden shared state.
type Counters struct {
	Dropped       uint64
	Backpressured uint64
}

func (c Counters) With(result CapacityResult) Counters {
	switch result.Counter {
	case CounterDropped:
		c.Dropped++
	case CounterBackpressured:
		c.Backpressured++
	}
	return c
}

// Decide applies capacity policy without mutating counters or storage state.
// Callers can aggregate the returned Counter exactly once with the result.
func (p CapacityPolicy) Decide(record Record, classification Classification, state CapacityState) CapacityResult {
	if !state.CanPersist {
		if p.mayShed(record, classification) && p.nearCapacity(state) {
			return CapacityResult{Action: ActionShed, Acknowledged: true, Counter: CounterDropped}
		}
		return CapacityResult{Action: ActionBackpressure, Retryable: true, Counter: CounterBackpressured}
	}
	if p.mayShed(record, classification) && p.nearCapacity(state) {
		return CapacityResult{Action: ActionShed, Acknowledged: true, Counter: CounterDropped}
	}
	return CapacityResult{Action: ActionAdmit}
}

func (p CapacityPolicy) mayShed(record Record, classification Classification) bool {
	if classification.Protected || !classification.Certain || record.Security || record.EventTriggering {
		return false
	}
	if record.Severity != model.SeverityClassDebug && record.Severity != model.SeverityClassInfo {
		return false
	}
	for _, source := range p.EligibleSources {
		if source == record.Source {
			return true
		}
	}
	return false
}

func (p CapacityPolicy) nearCapacity(state CapacityState) bool {
	if state.Capacity == 0 || p.NearCapacityPercent == 0 || p.NearCapacityPercent > 100 {
		return false
	}
	// Division avoids overflow from Used*100. The remainder comparison makes
	// the threshold inclusive at exact fractional boundaries.
	whole := state.Used / state.Capacity
	remainder := state.Used % state.Capacity
	if whole >= 1 {
		return true
	}
	percent := uint64(p.NearCapacityPercent)
	threshold := (state.Capacity/100)*percent + ((state.Capacity%100)*percent+99)/100
	return remainder >= threshold
}
