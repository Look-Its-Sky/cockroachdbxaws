package incident_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

func TestIncidentFamiliesAreIsolatedBySourceAccount(t *testing.T) {
	engine := incident.New()
	baseFactory := builders.NewFactory()
	otherEnvelope := baseFactory.Envelope(t)
	otherEnvelope.SourceAccount = "000000000002"
	otherFactory := builders.NewFactory(builders.WithEnvelope(otherEnvelope))

	first := observe(t, engine, baseFactory.Record(t))
	second := observe(t, engine, otherFactory.Record(t, builders.WithRecordID(strings.Repeat("b", 64))))

	if first.IncidentID == second.IncidentID {
		t.Fatalf("accounts %q and %q merged into incident %s",
			builders.DefaultSourceAccount, otherEnvelope.SourceAccount, first.IncidentID)
	}
}

func TestPersistenceDecisionWaitsForOwnFinalizationAndUsesStableEpisode(t *testing.T) {
	for _, order := range []struct {
		name  string
		first time.Duration
		last  time.Duration
	}{{name: "T10 then T9", first: 10 * time.Minute, last: 9 * time.Minute}, {name: "T9 then T10", first: 9 * time.Minute, last: 10 * time.Minute}} {
		t.Run(order.name, func(t *testing.T) {
			engine := incident.New()
			factory := builders.NewFactory()
			origin := fakeclock.Origin.Add(-builders.DefaultEventLag)
			records := []model.NormalizedLog{
				factory.Record(t, builders.WithObservedTime(origin.Add(order.first).Add(builders.DefaultEventLag)), builders.WithEventTime(origin.Add(order.first))),
				factory.Record(t, builders.WithObservedTime(origin.Add(order.last).Add(builders.DefaultEventLag)), builders.WithEventTime(origin.Add(order.last))),
			}
			for _, record := range records {
				observe(t, engine, record)
				if _, ok := engine.PersistenceDecision(record.RecordID); ok {
					t.Fatalf("provisional record %s received a persistence decision", record.RecordID)
				}
			}
			engine.Advance(origin.Add(10*time.Minute + incident.DefaultAllowedLateness + time.Nanosecond))
			var key int64
			for _, record := range records {
				decision, ok := engine.PersistenceDecision(record.RecordID)
				if !ok {
					t.Fatalf("finalized record %s has no persistence decision", record.RecordID)
				}
				if decision.EpisodeStart() != origin.Add(9*time.Minute) {
					t.Fatalf("arrival order changed episode start: %+v", decision)
				}
				if err := incident.ValidatePersistenceDecision(record, decision); err != nil {
					t.Fatalf("shared decision rejected: %v", err)
				}
				if key == 0 {
					key = decision.GenerationKey()
				} else if decision.GenerationKey() != key {
					t.Fatalf("one M1 generation produced two persistence keys: %d and %d", key, decision.GenerationKey())
				}
			}
		})
	}
}

func TestPersistenceDecisionUsesExactReopenBoundary(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	first := factory.Record(t)
	boundary := first.EventTime.Add(incident.DefaultReopenWindow)
	second := factory.Record(t, builders.WithObservedTime(boundary.Add(builders.DefaultEventLag)), builders.WithEventTime(boundary))
	observe(t, engine, first)
	observe(t, engine, second)
	engine.Advance(boundary.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	firstDecision, firstOK := engine.PersistenceDecision(first.RecordID)
	secondDecision, secondOK := engine.PersistenceDecision(second.RecordID)
	if !firstOK || !secondOK || firstDecision.GenerationKey() == secondDecision.GenerationKey() ||
		firstDecision.EpisodeStart() != first.EventTime || secondDecision.EpisodeStart() != second.EventTime {
		t.Fatalf("exact reopen boundary did not produce two immutable episodes: first=%+v/%v second=%+v/%v",
			firstDecision, firstOK, secondDecision, secondOK)
	}
}

func TestGlobalRecordIDIsDeduplicatedAcrossAccountClaims(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	firstRecord := factory.Record(t)
	first := observe(t, engine, firstRecord)

	otherEnvelope := factory.Envelope(t)
	otherEnvelope.SourceAccount = "000000000002"
	claimedAgain := factory.Record(t,
		builders.WithSourceEnvelope(otherEnvelope),
		builders.WithRecordID(firstRecord.RecordID),
	)
	duplicate := observe(t, engine, claimedAgain)

	if !duplicate.Duplicate || duplicate.IncidentID != first.IncidentID {
		t.Fatalf("global record_id was not deduplicated once: first=%+v duplicate=%+v", first, duplicate)
	}
	if got := engine.Incident(first.IncidentID).Current.OccurrenceCount; got != 1 {
		t.Fatalf("global replay contributed %d occurrences", got)
	}
}

func TestIncompleteServiceIdentityNeverRequestsAnInvestigation(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	for occurrence := 1; occurrence <= incident.DefaultInvestigationThreshold+1; occurrence++ {
		update := observe(t, engine, factory.Record(t, builders.WithoutEnvironment()))
		if update.InvestigationRequested || update.ActiveInvestigation {
			t.Fatalf("incomplete identity requested service-specific investigation at occurrence %d: %+v", occurrence, update)
		}
	}
}

func TestConcurrentDuplicateObserveContributesExactlyOnce(t *testing.T) {
	engine := incident.New()
	record := builders.NewFactory().Record(t)
	const observers = 64

	updates := make(chan incident.Update, observers)
	errors := make(chan error, observers)
	var wg sync.WaitGroup
	for i := 0; i < observers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			update, err := engine.Observe(record)
			if err != nil {
				errors <- err
				return
			}
			updates <- update
		}()
	}
	wg.Wait()
	close(updates)
	close(errors)
	for err := range errors {
		t.Fatalf("observe: %v", err)
	}

	firstContributions := 0
	var incidentID string
	for update := range updates {
		incidentID = update.IncidentID
		if !update.Duplicate {
			firstContributions++
		}
	}
	if firstContributions != 1 {
		t.Fatalf("want exactly one non-duplicate contribution, got %d", firstContributions)
	}
	if got := engine.Incident(incidentID).Current.OccurrenceCount; got != 1 {
		t.Fatalf("want one occurrence after concurrent replay, got %d", got)
	}
}

func TestAReplayedRecordContributesOnce(t *testing.T) {
	engine := incident.New()
	record := builders.NewFactory().Record(t)

	first := observe(t, engine, record)
	second := observe(t, engine, record)

	if first.OccurrenceCount != 1 {
		t.Fatalf("want the first occurrence counted, got %d", first.OccurrenceCount)
	}
	if !second.Duplicate {
		t.Fatal("want replay reported as a duplicate")
	}
	if second.OccurrenceCount != 1 || second.WindowCount != 1 {
		t.Fatalf("want replay to leave both counts at one, got occurrence=%d window=%d",
			second.OccurrenceCount, second.WindowCount)
	}
}

func TestFifthOrdinaryErrorRequestsOneInvestigationWhenItsHorizonFinalizes(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	deadline := fakeclock.Origin.Add(-builders.DefaultEventLag).Add(incident.DefaultAllowedLateness)
	var incidentID string

	for occurrence := 1; occurrence <= incident.DefaultInvestigationThreshold; occurrence++ {
		update := observe(t, engine, factory.Record(t))
		incidentID = update.IncidentID
		if occurrence < incident.DefaultInvestigationThreshold && update.InvestigationRequested {
			t.Fatalf("occurrence %d requested an investigation before five-in-five", occurrence)
		}
		if update.InvestigationRequested {
			t.Fatalf("provisional occurrence %d requested before its event-time horizon finalized", occurrence)
		}
	}
	if changed := engine.Advance(deadline.Add(-time.Nanosecond)); investigationRequests(changed) != 0 {
		t.Fatalf("five-in-five launched before the finalization deadline: %+v", changed)
	}
	if snapshot := engine.Incident(incidentID); snapshot.ActiveInvestigation {
		t.Fatalf("five-in-five became active before its finalization deadline: %+v", snapshot)
	}
	changed := engine.Advance(deadline)
	if investigationRequests(changed) != 0 {
		t.Fatalf("five-in-five launched at the still-provisional exact deadline: %+v", changed)
	}
	changed = engine.Advance(deadline.Add(time.Nanosecond))
	if investigationRequests(changed) != 1 {
		t.Fatalf("want one request just after the finalization deadline, got %+v", changed)
	}
	if repeated := engine.Advance(deadline.Add(2 * time.Nanosecond)); investigationRequests(repeated) != 0 {
		t.Fatalf("finalized five-in-five requested more than once: %+v", repeated)
	}
}

func TestFurtherErrorsUpdateOneIncidentWithoutAnotherAgent(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	var incidentID string
	requests := 0

	for occurrence := 1; occurrence <= 100; occurrence++ {
		update := observe(t, engine, factory.Record(t))
		if incidentID == "" {
			incidentID = update.IncidentID
		} else if update.IncidentID != incidentID {
			t.Fatalf("occurrence %d moved from incident %s to %s", occurrence, incidentID, update.IncidentID)
		}
		if update.InvestigationRequested {
			requests++
		}
	}
	requests += investigationRequests(engine.Advance(
		fakeclock.Origin.Add(-builders.DefaultEventLag).Add(incident.DefaultAllowedLateness + time.Nanosecond),
	))

	if requests != 1 {
		t.Fatalf("want one investigation request through 100 errors, got %d", requests)
	}
	final := engine.Incident(incidentID)
	if final.GenerationCount != 1 || final.Current.OccurrenceCount != 100 {
		t.Fatalf("want one generation with 100 errors, got %+v", final)
	}
	if !final.ActiveInvestigation {
		t.Fatal("want the investigation to remain active while later errors accumulate")
	}
}

func TestDeploymentChangeCreatesLinkedGenerationAndUpdatesActiveContext(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	for occurrence := 0; occurrence < incident.DefaultInvestigationThreshold; occurrence++ {
		observe(t, engine, factory.Record(t))
	}
	changedAtDeadline := engine.Advance(
		fakeclock.Origin.Add(-builders.DefaultEventLag).Add(incident.DefaultAllowedLateness + time.Nanosecond),
	)
	if investigationRequests(changedAtDeadline) != 1 || changedAtDeadline[0].ContextVersion != 1 {
		t.Fatalf("want initial investigation context version 1, got %+v", changedAtDeadline)
	}

	changed := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(fakeclock.Origin.Add(3*time.Minute)),
		builders.WithEventTime(fakeclock.Origin.Add(3*time.Minute)),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	))

	if changed.Generation != 2 || changed.GenerationCount != 2 {
		t.Fatalf("want a linked second generation, got %+v", changed)
	}
	if changed.InvestigationRequested {
		t.Fatal("deployment change launched a second agent while one was active")
	}
	if !changed.ContextUpdated || changed.ContextVersion != 2 {
		t.Fatalf("want the active context updated to version 2, got %+v", changed)
	}
	if changed.DeploymentID != "paymentservice-bbbbbbb" {
		t.Fatalf("want new deployment in context, got %q", changed.DeploymentID)
	}
}

func TestWindowIsExactlyHalfOpen(t *testing.T) {
	watermark := fakeclock.Origin.Add(10 * time.Minute)
	lower := watermark.Add(-incident.DefaultInvestigationWindow)

	tests := []struct {
		name string
		at   time.Time
		in   bool
	}{
		{name: "one nanosecond before lower bound", at: lower.Add(-time.Nanosecond), in: false},
		{name: "exact lower bound", at: lower, in: false},
		{name: "one nanosecond inside", at: lower.Add(time.Nanosecond), in: true},
		{name: "exact watermark", at: watermark, in: true},
		{name: "after watermark", at: watermark.Add(time.Nanosecond), in: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := incident.InHalfOpenWindow(test.at, watermark, incident.DefaultInvestigationWindow); got != test.in {
				t.Fatalf("want membership %v for %s, got %v", test.in, test.at, got)
			}
		})
	}
}

func TestARecordAtTheLowerWindowBoundaryDoesNotSatisfyFiveInFive(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	watermark := fakeclock.Origin.Add(10 * time.Minute)
	lower := watermark.Add(-incident.DefaultInvestigationWindow)

	observe(t, engine, at(t, factory, lower))
	for occurrence := 1; occurrence <= incident.DefaultInvestigationThreshold; occurrence++ {
		update := observe(t, engine, at(t, factory, watermark))
		if update.InvestigationRequested {
			t.Fatalf("occurrence %d requested before its horizon finalized", occurrence)
		}
	}
	deadline := watermark.Add(incident.DefaultAllowedLateness)
	if changed := engine.Advance(deadline.Add(-time.Nanosecond)); investigationRequests(changed) != 0 {
		t.Fatalf("lower-bound fixture launched before finalization: %+v", changed)
	}
	if changed := engine.Advance(deadline); investigationRequests(changed) != 0 {
		t.Fatalf("lower-bound fixture launched at its still-provisional exact deadline: %+v", changed)
	}
	if changed := engine.Advance(deadline.Add(time.Nanosecond)); investigationRequests(changed) != 1 {
		t.Fatalf("want five finalized records strictly inside the window to trigger, got %+v", changed)
	}
}

func TestWatermarkLeapDoesNotLoseAFinalizedFiveInFiveCluster(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	for occurrence := 0; occurrence < incident.DefaultInvestigationThreshold; occurrence++ {
		observe(t, engine, at(t, factory, start))
	}

	finalizer := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(start.Add(10*time.Minute)),
		builders.WithEventTime(start.Add(10*time.Minute)),
		builders.SeverityWarn(),
	))
	if !finalizer.InvestigationRequested {
		t.Fatalf("watermark leap skipped the earlier finalized five-in-five cluster: %+v", finalizer)
	}
}

func TestGenerationBecomesQuietAtExactlyFifteenMinutes(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))

	if changed := engine.Advance(start.Add(incident.DefaultQuietPeriod - time.Nanosecond)); len(changed) != 0 {
		t.Fatalf("generation became quiet early: %+v", changed)
	}
	changed := engine.Advance(start.Add(incident.DefaultQuietPeriod))
	if len(changed) != 1 || changed[0].DetectionStatus != incident.DetectionQuiet {
		t.Fatalf("want exactly one quiet transition at 15 minutes, got %+v", changed)
	}
	if got := engine.Incident(first.IncidentID).Current.DetectionStatus; got != incident.DetectionQuiet {
		t.Fatalf("want snapshot quiet, got %s", got)
	}
}

func TestWatermarkUsesAllowedLatenessAndNeverMovesBackward(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	firstTime := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, at(t, factory, firstTime))
	want := firstTime.Add(-incident.DefaultAllowedLateness)
	if !first.Watermark.Equal(want) {
		t.Fatalf("want watermark %s, got %s", want, first.Watermark)
	}

	outOfOrder := observe(t, engine, at(t, factory, firstTime.Add(-time.Minute)))
	if !outOfOrder.Watermark.Equal(want) {
		t.Fatalf("out-of-order record moved watermark backward from %s to %s", want, outOfOrder.Watermark)
	}
	if outOfOrder.Late {
		t.Fatal("record newer than the watermark was classified late")
	}

	late := observe(t, engine, at(t, factory, want.Add(-time.Nanosecond)))
	if !late.Late {
		t.Fatalf("record behind watermark %s was not classified late: %+v", want, late)
	}
}

func TestEngineWatermarkWindowIsExactlyHalfOpen(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	maximum := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, at(t, factory, maximum))
	watermark := maximum.Add(-incident.DefaultAllowedLateness)
	lower := watermark.Add(-incident.DefaultInvestigationWindow)
	if first.WatermarkWindowCount != 0 {
		t.Fatalf("record newer than watermark should remain live but unsettled, got %+v", first)
	}

	atLower := observe(t, engine, at(t, factory, lower))
	if atLower.WatermarkWindowCount != 0 {
		t.Fatalf("exact lower bound was included in watermark window: %+v", atLower)
	}
	inside := observe(t, engine, at(t, factory, lower.Add(time.Nanosecond)))
	if inside.WatermarkWindowCount != 1 {
		t.Fatalf("record one nanosecond inside watermark window was excluded: %+v", inside)
	}
	atWatermark := observe(t, engine, at(t, factory, watermark))
	if atWatermark.WatermarkWindowCount != 2 {
		t.Fatalf("exact watermark was excluded: %+v", atWatermark)
	}
	if atWatermark.Late || atWatermark.EvidenceOnly {
		t.Fatalf("arrival exactly at the strict watermark frontier was not provisional: %+v", atWatermark)
	}
}

func TestEvidenceOnlyRecordsNeverSatisfyThresholdOrLaunch(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	maximum := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(maximum),
		builders.WithEventTime(maximum),
		builders.SeverityWarn(),
	))
	finalizedTime := maximum.Add(-incident.DefaultAllowedLateness - time.Nanosecond)

	for occurrence := 1; occurrence <= incident.DefaultInvestigationThreshold; occurrence++ {
		update := observe(t, engine, at(t, factory, finalizedTime))
		if !update.EvidenceOnly {
			t.Fatalf("finalized occurrence %d was not labeled evidence-only: %+v", occurrence, update)
		}
		if update.InvestigationRequested || update.ActiveInvestigation {
			t.Fatalf("evidence-only occurrence %d affected investigation state: %+v", occurrence, update)
		}
	}

	if changed := engine.Advance(maximum.Add(incident.DefaultAllowedLateness)); investigationRequests(changed) != 0 {
		t.Fatalf("evidence-only records affected the finalized threshold: %+v", changed)
	}
	snapshot := engine.Incident(first.IncidentID)
	if snapshot.ActiveInvestigation {
		t.Fatalf("evidence-only records launched an investigation: %+v", snapshot)
	}
	if snapshot.Current.OccurrenceCount != incident.DefaultInvestigationThreshold+1 {
		t.Fatalf("retained evidence was not visible in occurrence evidence: %+v", snapshot)
	}
}

func TestLateArrivalDoesNotReactivateAQuietGeneration(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))
	latest := start.Add(10 * time.Minute)
	observe(t, engine, at(t, factory, latest))
	engine.Advance(latest.Add(incident.DefaultQuietPeriod))

	lateRecord := factory.Record(t,
		builders.WithObservedTime(latest.Add(incident.DefaultQuietPeriod+time.Minute)),
		builders.WithEventTime(latest.Add(-incident.DefaultAllowedLateness-time.Nanosecond)),
	)
	late := observe(t, engine, lateRecord)
	if !late.Late || !late.EvidenceOnly {
		t.Fatalf("want quiet-generation late arrival retained as evidence only, got %+v", late)
	}
	if late.DetectionStatus != incident.DetectionQuiet {
		t.Fatalf("late arrival reactivated quiet generation: %+v", late)
	}
	if got := engine.Incident(first.IncidentID); got.GenerationCount != 1 || got.Current.DetectionStatus != incident.DetectionQuiet {
		t.Fatalf("late arrival changed current generation: %+v", got)
	}
}

func TestLateEarlierDeploymentDoesNotCreateASpuriousCurrentGeneration(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	var active incident.Update
	for i := 0; i < incident.DefaultInvestigationThreshold; i++ {
		active = observe(t, engine, at(t, factory, start))
	}
	changedAtDeadline := engine.Advance(start.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	if investigationRequests(changedAtDeadline) != 1 {
		t.Fatalf("setup did not activate investigation: %+v", changedAtDeadline)
	}

	newDeploymentTime := start.Add(10 * time.Minute)
	changed := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(newDeploymentTime),
		builders.WithEventTime(newDeploymentTime),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	))
	if changed.Generation != 2 || changed.ContextVersion != 2 {
		t.Fatalf("setup did not create linked deployment generation: %+v", changed)
	}

	lateOld := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(newDeploymentTime.Add(time.Minute)),
		builders.WithEventTime(newDeploymentTime.Add(-incident.DefaultAllowedLateness-time.Nanosecond)),
	))
	if !lateOld.Late || lateOld.Generation != 1 {
		t.Fatalf("want late evidence attached to earlier deployment generation, got %+v", lateOld)
	}
	if lateOld.ContextUpdated || lateOld.ContextVersion != 2 {
		t.Fatalf("late prior deployment changed active context: %+v", lateOld)
	}
	if got := engine.Incident(active.IncidentID); got.GenerationCount != 2 || got.Current.Number != 2 {
		t.Fatalf("late prior deployment changed current generation: %+v", got)
	}
}

func TestWithinLatenessEarlierDeploymentDoesNotBecomeCurrent(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))
	deploymentBTime := start.Add(10 * time.Minute)
	observe(t, engine, factory.Record(t,
		builders.WithObservedTime(deploymentBTime),
		builders.WithEventTime(deploymentBTime),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	))

	withinLateness := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(deploymentBTime.Add(time.Minute)),
		builders.WithEventTime(start.Add(9*time.Minute)),
	))
	if withinLateness.Late {
		t.Fatalf("9m is newer than the 8m watermark: %+v", withinLateness)
	}
	if withinLateness.Generation != 1 || withinLateness.EvidenceOnly {
		t.Fatalf("out-of-order provisional event did not contribute to generation 1: %+v", withinLateness)
	}
	if got := engine.Incident(first.IncidentID); got.GenerationCount != 2 || got.Current.Number != 2 ||
		got.Current.DeploymentID != "paymentservice-bbbbbbb" {
		t.Fatalf("A@0, B@10m, A@9m rewrote current lifecycle: %+v", got)
	}
}

func TestWithinLatenessCurrentDeploymentParticipatesNormally(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))
	deploymentBTime := start.Add(10 * time.Minute)
	observe(t, engine, factory.Record(t,
		builders.WithObservedTime(deploymentBTime),
		builders.WithEventTime(deploymentBTime),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	))

	withinLateness := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(deploymentBTime.Add(time.Minute)),
		builders.WithEventTime(start.Add(9*time.Minute)),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	))
	if withinLateness.Late || withinLateness.EvidenceOnly || withinLateness.Generation != 2 {
		t.Fatalf("B@9m did not participate normally after A@0, B@10m: %+v", withinLateness)
	}
	if withinLateness.OccurrenceCount != 2 || withinLateness.WindowCount != 2 {
		t.Fatalf("B@9m was missed by current generation/window counts: %+v", withinLateness)
	}
	snapshot := engine.Incident(first.IncidentID)
	if snapshot.GenerationCount != 2 || snapshot.Current.Number != 2 ||
		!snapshot.Current.FirstOccurrence.Equal(start.Add(9*time.Minute)) {
		t.Fatalf("B episode did not expand earlier within its valid boundary: %+v", snapshot)
	}
}

func TestWithinLatenessCurrentDeploymentCanCompleteFiveInFive(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	observe(t, engine, at(t, factory, start))
	deploymentBTime := start.Add(10 * time.Minute)
	current := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(deploymentBTime),
		builders.WithEventTime(deploymentBTime),
		builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
	))

	for occurrence := 2; occurrence <= incident.DefaultInvestigationThreshold; occurrence++ {
		eventTime := start.Add(9*time.Minute + time.Duration(occurrence)*time.Second)
		current = observe(t, engine, factory.Record(t,
			builders.WithObservedTime(deploymentBTime.Add(time.Duration(occurrence)*time.Second)),
			builders.WithEventTime(eventTime),
			builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5"),
		))
		if current.EvidenceOnly {
			t.Fatalf("B occurrence %d was treated as historical evidence: %+v", occurrence, current)
		}
		if occurrence < incident.DefaultInvestigationThreshold && current.InvestigationRequested {
			t.Fatalf("B occurrence %d requested investigation early: %+v", occurrence, current)
		}
	}
	if current.InvestigationRequested || current.OccurrenceCount != incident.DefaultInvestigationThreshold ||
		current.WindowCount != incident.DefaultInvestigationThreshold {
		t.Fatalf("within-lateness B records did not remain provisional at five-in-five: %+v", current)
	}
	if changed := engine.Advance(deploymentBTime.Add(incident.DefaultAllowedLateness + time.Nanosecond)); investigationRequests(changed) != 1 {
		t.Fatalf("within-lateness B records failed to launch when finalized: %+v", changed)
	}
}

func TestDeploymentEpisodeIncludesUniqueRecordsAtItsCreatingTimestamp(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))
	deploymentB := builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5")
	observe(t, engine, factory.Record(t,
		builders.WithObservedTime(start), builders.WithEventTime(start), deploymentB,
	))
	observe(t, engine, factory.Record(t,
		builders.WithObservedTime(start.Add(time.Minute)), builders.WithEventTime(start.Add(time.Minute)), deploymentB,
	))

	sameTimestamp := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(start.Add(2*time.Minute)), builders.WithEventTime(start), deploymentB,
	))
	if sameTimestamp.EvidenceOnly || sameTimestamp.Generation != 2 || sameTimestamp.OccurrenceCount != 3 {
		t.Fatalf("unique B@0 did not remain in the B episode after B@1: %+v", sameTimestamp)
	}
	if got := engine.Incident(first.IncidentID); got.GenerationCount != 2 || got.Current.Number != 2 {
		t.Fatalf("same-timestamp B record changed generation lifecycle: %+v", got)
	}
}

func TestSameTimestampDeploymentRecordsCanCompleteFiveInFive(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	observe(t, engine, at(t, factory, start))
	deploymentB := builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5")
	current := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(start), builders.WithEventTime(start), deploymentB,
	))
	current = observe(t, engine, factory.Record(t,
		builders.WithObservedTime(start.Add(time.Minute)), builders.WithEventTime(start.Add(time.Minute)), deploymentB,
	))

	for occurrence := 3; occurrence <= incident.DefaultInvestigationThreshold; occurrence++ {
		current = observe(t, engine, factory.Record(t,
			builders.WithObservedTime(start.Add(time.Duration(occurrence)*time.Minute)),
			builders.WithEventTime(start),
			deploymentB,
		))
		if current.EvidenceOnly {
			t.Fatalf("unique B@0 occurrence %d became evidence-only: %+v", occurrence, current)
		}
	}
	if current.InvestigationRequested || current.OccurrenceCount != incident.DefaultInvestigationThreshold ||
		current.WindowCount != incident.DefaultInvestigationThreshold {
		t.Fatalf("same-timestamp B records did not remain provisional at five-in-five: %+v", current)
	}
	if changed := engine.Advance(start.Add(time.Minute + incident.DefaultAllowedLateness + time.Nanosecond)); investigationRequests(changed) != 1 {
		t.Fatalf("same-timestamp B records failed to launch when finalized: %+v", changed)
	}
}

func TestSameTimestampTieBreakIsPermutationInvariant(t *testing.T) {
	base := fakeclock.Origin
	factory := builders.NewFactory()
	records := []model.NormalizedLog{
		deploymentRecord(t, factory, base, "a-deployment"),
		deploymentRecord(t, factory, base, "b-deployment"),
		deploymentRecord(t, factory, base, "a-deployment"),
	}

	for permutation, order := range allOrders(len(records)) {
		engine := incident.New()
		var incidentID string
		for _, index := range order {
			incidentID = observe(t, engine, records[index]).IncidentID
		}
		assertTopology(
			t, permutation, engine.Incident(incidentID),
			[]string{"a-deployment", "b-deployment"}, []int{2, 1}, false,
		)
	}
}

func TestUnseenDeploymentWithinLatenessCreatesLinkedHistoricalGeneration(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	latest := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, at(t, factory, latest))
	deploymentB := builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5")

	historical := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(latest.Add(time.Minute)),
		builders.WithEventTime(latest.Add(-time.Minute)),
		deploymentB,
	))
	if historical.Late || historical.EvidenceOnly || historical.Generation != 1 ||
		historical.OccurrenceCount != 1 || historical.WindowCount != 1 {
		t.Fatalf("first B@9m inside the 8m watermark was not admitted as a linked episode: %+v", historical)
	}
	snapshot := engine.Incident(first.IncidentID)
	if snapshot.GenerationCount != 2 || snapshot.Current.Number != 2 ||
		snapshot.Current.DeploymentID != builders.DefaultDeploymentID {
		t.Fatalf("historical B@9m replaced event-time-current A@10m: %+v", snapshot)
	}
}

func TestHistoricalProvisionalContributorsCanCompleteFiveInFive(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	latest := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, at(t, factory, latest))
	deploymentB := builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5")
	var current incident.Update
	for occurrence := 1; occurrence <= incident.DefaultInvestigationThreshold; occurrence++ {
		current = observe(t, engine, factory.Record(t,
			builders.WithObservedTime(latest.Add(time.Duration(occurrence)*time.Second)),
			builders.WithEventTime(latest.Add(-time.Minute)),
			deploymentB,
		))
		if current.EvidenceOnly {
			t.Fatalf("historical provisional B@9m occurrence %d was marked evidence-only: %+v", occurrence, current)
		}
		if occurrence < incident.DefaultInvestigationThreshold && current.InvestigationRequested {
			t.Fatalf("B@9m occurrence %d requested investigation early: %+v", occurrence, current)
		}
	}
	if current.InvestigationRequested || current.OccurrenceCount != incident.DefaultInvestigationThreshold ||
		current.WindowCount != incident.DefaultInvestigationThreshold {
		t.Fatalf("unseen within-lateness deployment did not remain provisional: %+v", current)
	}
	if changed := engine.Advance(latest.Add(incident.DefaultAllowedLateness)); investigationRequests(changed) != 1 {
		t.Fatalf("unseen within-lateness deployment failed to launch when finalized: %+v", changed)
	}
	if got := engine.Incident(first.IncidentID); got.GenerationCount != 2 || got.Current.Number != 2 {
		t.Fatalf("B threshold changed event-time-current generation: %+v", got)
	}
}

func TestEpisodeReconstructionIsPermutationInvariant(t *testing.T) {
	base := fakeclock.Origin
	factory := builders.NewFactory()
	records := []model.NormalizedLog{
		deploymentRecord(t, factory, base.Add(9*time.Minute), "deployment-b"),
		deploymentRecord(t, factory, base.Add(9*time.Minute+30*time.Second), "deployment-c"),
		deploymentRecord(t, factory, base.Add(9*time.Minute+45*time.Second), "deployment-b"),
		deploymentRecord(t, factory, base.Add(10*time.Minute), builders.DefaultDeploymentID),
	}
	wantDeployments := []string{"deployment-b", "deployment-c", "deployment-b", builders.DefaultDeploymentID}

	for permutation, order := range allOrders(len(records)) {
		engine := incident.New()
		var incidentID string
		for _, index := range order {
			update := observe(t, engine, records[index])
			incidentID = update.IncidentID
		}
		assertTopology(t, permutation, engine.Incident(incidentID), wantDeployments, []int{1, 1, 1, 1}, false)
	}
}

func TestExactAllowedLatenessBoundaryIsPermutationInvariant(t *testing.T) {
	base := fakeclock.Origin
	deadline := base.Add(incident.DefaultAllowedLateness)
	factory := builders.NewFactory()
	records := make([]model.NormalizedLog, 0, incident.DefaultInvestigationThreshold+1)
	for occurrence := 0; occurrence < incident.DefaultInvestigationThreshold; occurrence++ {
		records = append(records, deploymentRecord(t, factory, base, builders.DefaultDeploymentID))
	}
	records = append(records, factory.Record(t,
		builders.WithObservedTime(deadline),
		builders.WithEventTime(deadline),
		builders.SeverityWarn(),
	))

	for permutation, order := range allOrders(len(records)) {
		engine := incident.New()
		var incidentID string
		requests := 0
		for _, index := range order {
			update := observe(t, engine, records[index])
			incidentID = update.IncidentID
			requests += boolInt(update.InvestigationRequested)
			if update.EvidenceOnly {
				t.Fatalf("permutation %d order %v: exact-frontier record became evidence-only: %+v",
					permutation, order, update)
			}
		}
		if requests != 0 {
			t.Fatalf("permutation %d order %v: launched before frontier passed T", permutation, order)
		}
		assertTopology(
			t, permutation, engine.Incident(incidentID),
			[]string{builders.DefaultDeploymentID}, []int{incident.DefaultInvestigationThreshold + 1}, false,
		)

		requests += investigationRequests(engine.Advance(deadline))
		if requests != 0 || engine.Incident(incidentID).ActiveInvestigation {
			t.Fatalf("permutation %d order %v: exact deadline was treated as finalized", permutation, order)
		}
		requests += investigationRequests(engine.Advance(deadline.Add(time.Nanosecond)))
		if requests != 1 {
			t.Fatalf("permutation %d order %v: want one launch after frontier passed T, got %d",
				permutation, order, requests)
		}
		assertTopology(
			t, permutation, engine.Incident(incidentID),
			[]string{builders.DefaultDeploymentID}, []int{incident.DefaultInvestigationThreshold + 1}, true,
		)
	}
}

func TestIdleFinalizationFreezesEpisodeTopology(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	latest := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, at(t, factory, latest))

	engine.Advance(latest.Add(incident.DefaultAllowedLateness))
	finalizedArrival := observe(t, engine, deploymentRecord(
		t, factory, latest.Add(-time.Minute), "deployment-b",
	))
	if !finalizedArrival.EvidenceOnly || finalizedArrival.Generation != 0 {
		t.Fatalf("arrival behind the idle finalization frontier changed topology: %+v", finalizedArrival)
	}

	snapshot := engine.Incident(first.IncidentID)
	if snapshot.GenerationCount != 1 || snapshot.Current.DeploymentID != builders.DefaultDeploymentID {
		t.Fatalf("finalized deployment topology was rewritten: %+v", snapshot)
	}
}

func TestThresholdDecisionIsPermutationInvariantAcrossInterleavedDeployments(t *testing.T) {
	for _, test := range []struct {
		name              string
		firstBCount       int
		wantInvestigation bool
	}{
		{name: "four B records stay below threshold", firstBCount: 4},
		{name: "five B records satisfy threshold", firstBCount: 5, wantInvestigation: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := fakeclock.Origin
			factory := builders.NewFactory()
			var records []model.NormalizedLog
			for i := 0; i < test.firstBCount; i++ {
				records = append(records, deploymentRecord(
					t, factory, base.Add(9*time.Minute+time.Duration(i)*5*time.Second), "deployment-b",
				))
			}
			records = append(records,
				deploymentRecord(t, factory, base.Add(9*time.Minute+30*time.Second), "deployment-c"),
				deploymentRecord(t, factory, base.Add(9*time.Minute+45*time.Second), "deployment-b"),
				deploymentRecord(t, factory, base.Add(10*time.Minute), builders.DefaultDeploymentID),
			)

			wantCounts := []int{test.firstBCount, 1, 1, 1}
			wantDeployments := []string{"deployment-b", "deployment-c", "deployment-b", builders.DefaultDeploymentID}
			for permutation, order := range sampledOrders(len(records), 48) {
				engine := incident.New()
				requests := 0
				var incidentID string
				for _, index := range order {
					update := observe(t, engine, records[index])
					incidentID = update.IncidentID
					if update.InvestigationRequested {
						requests++
					}
				}
				requests += investigationRequests(engine.Advance(base.Add(12 * time.Minute)))
				if want := boolInt(test.wantInvestigation); requests != want {
					t.Fatalf("permutation %d order %v: want %d investigation requests, got %d", permutation, order, want, requests)
				}
				assertTopology(t, permutation, engine.Incident(incidentID), wantDeployments, wantCounts, test.wantInvestigation)
			}
		})
	}
}

func TestWithinLatenessOlderEventDoesNotReactivateQuietGeneration(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	latest := fakeclock.Origin.Add(10 * time.Minute)
	first := observe(t, engine, at(t, factory, latest))
	engine.Advance(latest.Add(incident.DefaultQuietPeriod))

	older := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(latest.Add(incident.DefaultQuietPeriod+time.Minute)),
		builders.WithEventTime(latest.Add(-time.Minute)),
	))
	if older.Late {
		t.Fatalf("event within allowed-lateness band was classified late: %+v", older)
	}
	if !older.EvidenceOnly || older.DetectionStatus != incident.DetectionQuiet {
		t.Fatalf("older arrival reactivated quiet generation: %+v", older)
	}
	if got := engine.Incident(first.IncidentID).Current.DetectionStatus; got != incident.DetectionQuiet {
		t.Fatalf("quiet lifecycle changed to %s", got)
	}
}

func TestLateEvidenceSelectsDeploymentEpisodeByEventTime(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))
	engine.Advance(start.Add(incident.DefaultQuietPeriod))
	secondTime := start.Add(incident.DefaultReopenWindow)
	second := observe(t, engine, at(t, factory, secondTime))
	if second.Generation != 2 {
		t.Fatalf("setup did not create second same-deployment episode: %+v", second)
	}
	secondFirst := engine.Incident(first.IncidentID).Current.FirstOccurrence

	late := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(secondTime.Add(time.Minute)),
		builders.WithEventTime(start.Add(time.Minute)),
	))
	if !late.Late || !late.EvidenceOnly || late.Generation != 1 || late.OccurrenceCount != 2 {
		t.Fatalf("late first-episode evidence selected wrong deployment generation: %+v", late)
	}
	snapshot := engine.Incident(first.IncidentID)
	if snapshot.Current.Number != 2 || !snapshot.Current.FirstOccurrence.Equal(secondFirst) {
		t.Fatalf("late gen1 evidence shifted gen2 episode boundary: before=%s after=%+v", secondFirst, snapshot.Current)
	}
}

func TestVeryLateRecordIsEvidenceOnly(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	first := observe(t, engine, at(t, factory, start))
	recent := start.Add(incident.DefaultStateRetention + 10*time.Minute)
	observe(t, engine, at(t, factory, recent))

	veryLate := observe(t, engine, factory.Record(t,
		builders.WithObservedTime(recent.Add(time.Minute)),
		builders.WithEventTime(start.Add(-time.Nanosecond)),
	))
	if !veryLate.Late || !veryLate.EvidenceOnly {
		t.Fatalf("want very late occurrence retained as evidence only, got %+v", veryLate)
	}
	if got := engine.Incident(first.IncidentID).Current.OccurrenceCount; got != 1 {
		t.Fatalf("very late evidence changed current occurrence count to %d", got)
	}
}

func TestRecurrenceUsesTheExactTwoHourGenerationBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		offset     time.Duration
		generation int
	}{
		{name: "one nanosecond inside reopens same generation", offset: incident.DefaultReopenWindow - time.Nanosecond, generation: 1},
		{name: "exact boundary creates linked generation", offset: incident.DefaultReopenWindow, generation: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := incident.New()
			factory := builders.NewFactory()
			start := fakeclock.Origin
			first := observe(t, engine, at(t, factory, start))
			engine.Advance(start.Add(incident.DefaultQuietPeriod))

			recurrence := observe(t, engine, at(t, factory, start.Add(test.offset)))
			if recurrence.IncidentID != first.IncidentID {
				t.Fatalf("recurrence left family %s for %s", first.IncidentID, recurrence.IncidentID)
			}
			if recurrence.Generation != test.generation {
				t.Fatalf("want generation %d, got %+v", test.generation, recurrence)
			}
			if recurrence.DetectionStatus != incident.DetectionActive {
				t.Fatalf("want recurrence active, got %s", recurrence.DetectionStatus)
			}
		})
	}
}

// One hot incident family is exactly the shape an attacker controls. Retaining
// every 256 KiB record for the process lifetime is an unbounded memory and CPU
// cost which peaks precisely while the system is under incident load, so state
// is bounded by the retention time-and-windows.md allows.
func TestRetainedStateIsBoundedAsAFamilyGrowsPastRetention(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	const step = time.Minute
	const records = 600 // ten hours, several times DefaultStateRetention
	for i := 0; i < records; i++ {
		when := start.Add(time.Duration(i) * step)
		observe(t, engine, factory.Record(t, builders.WithObservedTime(when), builders.WithEventTime(when)))
		engine.Advance(when)
	}

	// Only the retention horizon plus the still-provisional allowed-lateness band
	// can affect any later decision. Two extra records absorb the boundary.
	bound := int((incident.DefaultStateRetention+incident.DefaultAllowedLateness)/step) + 2
	retained := engine.Retained()
	if retained.Records > bound {
		t.Fatalf("retained %d records after %d arrivals, want at most %d", retained.Records, records, bound)
	}
	if retained.Seen > bound {
		t.Fatalf("retained %d record identities, want at most %d", retained.Seen, bound)
	}
	if retained.Families != 1 {
		t.Fatalf("retained %d families, want the one live family", retained.Families)
	}
}

// A family which stopped receiving records cannot be reached by any later
// arrival, so it must not be held for the process lifetime either.
func TestIdleFamilyIsCompactedAfterItsRetentionHorizonPasses(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	observe(t, engine, at(t, factory, start))
	if retained := engine.Retained(); retained.Families != 1 || retained.Records != 1 {
		t.Fatalf("setup retained %+v", retained)
	}

	engine.Advance(start.Add(incident.DefaultStateRetention))
	if retained := engine.Retained(); retained.Records != 1 {
		t.Fatalf("idle family was compacted inside its retention horizon: %+v", retained)
	}
	engine.Advance(start.Add(incident.DefaultStateRetention + incident.DefaultAllowedLateness + time.Nanosecond))
	if retained := engine.Retained(); retained.Families != 0 || retained.Records != 0 || retained.Seen != 0 {
		t.Fatalf("idle family survived its retention horizon: %+v", retained)
	}
}

// Compacting an identity out of the in-memory map is safe: records_seen in
// CockroachDB is the durable global gate for duplicate delivery and outlives
// this process. The engine map is only a local fast path, so a compacted
// identity may legitimately be observed again here; M4 still counts it once.
func TestCompactedRecordIdentityDefersToTheDurableGate(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	record := at(t, factory, start)
	if first := observe(t, engine, record); first.Duplicate {
		t.Fatalf("first arrival reported as a duplicate: %+v", first)
	}
	engine.Advance(start.Add(incident.DefaultStateRetention + incident.DefaultAllowedLateness + time.Nanosecond))
	if retained := engine.Retained(); retained.Seen != 0 {
		t.Fatalf("identity was not compacted: %+v", retained)
	}

	replay := observe(t, engine, record)
	if replay.Duplicate {
		t.Fatalf("compacted identity was answered from stale in-memory state: %+v", replay)
	}
	// The replay must not resurrect the compacted episode or its investigation.
	if replay.Generation != 1 || replay.ActiveInvestigation {
		t.Fatalf("compacted replay rebuilt prior lifecycle state: %+v", replay)
	}
}

func at(t *testing.T, factory *builders.Factory, when time.Time) model.NormalizedLog {
	t.Helper()
	return factory.Record(t, builders.WithObservedTime(when), builders.WithEventTime(when))
}

func observe(t *testing.T, engine *incident.Engine, record model.NormalizedLog) incident.Update {
	t.Helper()
	update, err := engine.Observe(record)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return update
}

func deploymentRecord(t *testing.T, factory *builders.Factory, eventTime time.Time, deploymentID string) model.NormalizedLog {
	t.Helper()
	return factory.Record(t,
		builders.WithObservedTime(fakeclock.Origin.Add(10*time.Minute)),
		builders.WithEventTime(eventTime),
		builders.WithDeployment(deploymentID, "test-version"),
	)
}

func assertTopology(
	t *testing.T,
	permutation int,
	snapshot incident.Snapshot,
	wantDeployments []string,
	wantCounts []int,
	wantInvestigation bool,
) {
	t.Helper()
	if snapshot.GenerationCount != len(wantDeployments) || len(snapshot.Generations) != len(wantDeployments) {
		t.Fatalf("permutation %d: want %d generations, got %+v", permutation, len(wantDeployments), snapshot)
	}
	for i, generation := range snapshot.Generations {
		if generation.Number != i+1 || generation.DeploymentID != wantDeployments[i] ||
			generation.OccurrenceCount != wantCounts[i] {
			t.Fatalf("permutation %d generation %d: want deployment=%s count=%d, got %+v",
				permutation, i+1, wantDeployments[i], wantCounts[i], generation)
		}
	}
	if snapshot.Current.Number != len(wantDeployments) ||
		snapshot.Current.DeploymentID != wantDeployments[len(wantDeployments)-1] {
		t.Fatalf("permutation %d: wrong event-time current generation: %+v", permutation, snapshot)
	}
	if snapshot.ActiveInvestigation != wantInvestigation {
		t.Fatalf("permutation %d: want investigation=%v, got %+v", permutation, wantInvestigation, snapshot)
	}
	wantContextVersion := 0
	if wantInvestigation {
		wantContextVersion = len(wantDeployments)
	}
	if snapshot.ContextVersion != wantContextVersion {
		t.Fatalf("permutation %d: want context version=%d, got %+v", permutation, wantContextVersion, snapshot)
	}
}

func investigationRequests(updates []incident.Update) int {
	requests := 0
	for _, update := range updates {
		if update.InvestigationRequested {
			requests++
		}
	}
	return requests
}

func allOrders(size int) [][]int {
	values := make([]int, size)
	for i := range values {
		values[i] = i
	}
	var orders [][]int
	var visit func(int)
	visit = func(index int) {
		if index == len(values) {
			orders = append(orders, append([]int(nil), values...))
			return
		}
		for i := index; i < len(values); i++ {
			values[index], values[i] = values[i], values[index]
			visit(index + 1)
			values[index], values[i] = values[i], values[index]
		}
	}
	visit(0)
	return orders
}

func sampledOrders(size, count int) [][]int {
	orders := make([][]int, 0, count)
	for seed := 0; seed < count; seed++ {
		order := make([]int, size)
		for i := range order {
			order[i] = i
		}
		state := uint64(seed + 1)
		for i := len(order) - 1; i > 0; i-- {
			state = state*6364136223846793005 + 1442695040888963407
			j := int(state % uint64(i+1))
			order[i], order[j] = order[j], order[i]
		}
		orders = append(orders, order)
	}
	return orders
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// A worker still holding a journal claim needs the record's payload to obtain a
// persistence decision. Compacting it would leave that claim unresolvable and
// retried forever, so in-flight identities are exempt until the worker releases
// them.
func TestPinnedInFlightRecordsSurviveCompaction(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	record := at(t, factory, start)
	observe(t, engine, record)
	engine.Pin([]string{record.RecordID})

	beyond := start.Add(incident.DefaultStateRetention + incident.DefaultAllowedLateness + time.Hour)
	engine.Advance(beyond)
	if retained := engine.Retained(); retained.Records != 1 || retained.Families != 1 {
		t.Fatalf("an in-flight record was compacted: %+v", retained)
	}
	if _, ok := engine.PersistenceDecision(record.RecordID); !ok {
		t.Fatal("an in-flight record lost its persistence decision")
	}

	// Once the worker has committed and released it, ordinary retention applies.
	engine.Pin(nil)
	engine.Advance(beyond.Add(time.Nanosecond))
	if retained := engine.Retained(); retained.Records != 0 || retained.Families != 0 {
		t.Fatalf("released record survived compaction: %+v", retained)
	}
}
