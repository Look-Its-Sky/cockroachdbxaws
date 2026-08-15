package incident_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

// episodeIdentity is the pair storage keys `incident_generations` on.
type episodeIdentity struct {
	deployment   string
	episodeStart time.Time
	key          int64
}

func identityOf(decision incident.PersistenceDecision) episodeIdentity {
	return episodeIdentity{
		deployment:   decision.DeploymentID(),
		episodeStart: decision.EpisodeStart(),
		key:          decision.GenerationKey(),
	}
}

func (i episodeIdentity) equal(other episodeIdentity) bool {
	return i.deployment == other.deployment && i.episodeStart.Equal(other.episodeStart) && i.key == other.key
}

func (i episodeIdentity) String() string {
	return fmt.Sprintf("(%s,%s,%d)", i.deployment, i.episodeStart.Format(time.RFC3339Nano), i.key)
}

// A PersistenceDecision is the immutable bridge from finalized M1 topology to
// storage. `incident_generations` is keyed on
// (region, tenant_id, incident_id, deployment_id, episode_start), so if an
// already-issued decision's episode identity ever changes, one real episode
// acquires a second durable generation and its occurrences, evidence,
// quiet/reopen bounds and election index split across both. Retention
// compaction releases payloads; it must never re-derive an identity from
// whichever records happen to survive.
func TestIssuedEpisodeIdentityIsImmutableAcrossRetentionCompaction(t *testing.T) {
	gapSets := [][]time.Duration{
		{0, 1*time.Hour + 59*time.Minute, 3*time.Hour + 58*time.Minute, 5*time.Hour + 50*time.Minute},
		{0, 30 * time.Minute, 1 * time.Hour, 90 * time.Minute, 2 * time.Hour},
		{0, 10 * time.Minute, 20 * time.Minute, 3 * time.Hour, 6 * time.Hour},
		{0, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 8 * time.Hour, 10 * time.Hour},
		{0, time.Minute, 2 * time.Minute, 4 * time.Hour, 4*time.Hour + time.Minute, 9 * time.Hour},
	}
	for index, gaps := range gapSets {
		t.Run(fmt.Sprintf("set_%d", index), func(t *testing.T) {
			engine := incident.New()
			factory := builders.NewFactory()
			origin := fakeclock.Origin.Add(-builders.DefaultEventLag)
			var records []model.NormalizedLog
			for _, gap := range gaps {
				records = append(records, at(t, factory, origin.Add(gap)))
				observe(t, engine, records[len(records)-1])
			}

			issued := map[string]episodeIdentity{}
			// A decision issued before any Advance must survive the very first
			// Advance which prunes, not only later ones.
			collectIdentities(t, engine, records, issued, "before the first Advance")
			last := gaps[len(gaps)-1]
			for step := time.Duration(0); step <= last+80*time.Hour; step += 30 * time.Minute {
				engine.Advance(origin.Add(step))
				collectIdentities(t, engine, records, issued, fmt.Sprintf("advance=%s", step))
			}
		})
	}
}

// Compaction is not the only path that re-folds a family: an out-of-order
// arrival re-folds it too, and it runs against the survivors of every earlier
// compaction. The identity assigned when the episode began has to survive that
// fold as well.
func TestOutOfOrderArrivalAfterCompactionKeepsTheAssignedEpisodeIdentity(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	origin := fakeclock.Origin.Add(-builders.DefaultEventLag)
	var records []model.NormalizedLog
	for _, gap := range []time.Duration{0, 30 * time.Minute, time.Hour, 90 * time.Minute, 2 * time.Hour} {
		records = append(records, at(t, factory, origin.Add(gap)))
		observe(t, engine, records[len(records)-1])
	}

	issued := map[string]episodeIdentity{}
	collectIdentities(t, engine, records, issued, "before the compacting advance")
	engine.Advance(origin.Add(2*time.Hour + 30*time.Minute))
	collectIdentities(t, engine, records, issued, "after the compacting advance")
	if retained := engine.Retained(); retained.Records >= len(records) {
		t.Fatalf("setup did not compact anything: %+v", retained)
	}

	// Two arrivals share one event time, so the second sorts before the first
	// and forces a full re-fold rather than a tail extension.
	later := origin.Add(2*time.Hour + 30*time.Minute)
	observe(t, engine, factory.Record(t, builders.WithRecordID(strings.Repeat("f", 64)),
		builders.WithObservedTime(later.Add(builders.DefaultEventLag)), builders.WithEventTime(later)))
	observe(t, engine, factory.Record(t, builders.WithRecordID(strings.Repeat("1", 64)),
		builders.WithObservedTime(later.Add(builders.DefaultEventLag)), builders.WithEventTime(later)))

	collectIdentities(t, engine, records, issued, "after the out-of-order arrival")
}

func collectIdentities(
	t *testing.T,
	engine *incident.Engine,
	records []model.NormalizedLog,
	issued map[string]episodeIdentity,
	stage string,
) {
	t.Helper()
	for _, record := range records {
		decision, ok := engine.PersistenceDecision(record.RecordID)
		if !ok {
			continue
		}
		current := identityOf(decision)
		previous, already := issued[record.RecordID]
		if already && !previous.equal(current) {
			t.Fatalf("%s: record %s changed its issued episode identity from %s to %s",
				stage, record.RecordID[:8], previous, current)
		}
		if err := incident.ValidatePersistenceDecision(record, decision); err != nil {
			t.Fatalf("%s: record %s issued a decision storage rejects: %v", stage, record.RecordID[:8], err)
		}
		issued[record.RecordID] = current
	}
}

// Compaction releases an episode out of the middle of a family whenever an
// in-flight record holds an older one alive. The live episodes after it must
// keep their display ordinals: an ordinal which moves backwards makes the
// agent-visible context version move backwards with it.
func TestCompactingAMiddleEpisodeDoesNotRenumberTheEpisodesAfterIt(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	start := fakeclock.Origin
	deploymentB := builders.WithDeployment("paymentservice-bbbbbbb", "2026.3.5")

	first := at(t, factory, start)
	observe(t, engine, first)
	middle := start.Add(10 * time.Minute)
	observe(t, engine, factory.Record(t, builders.WithObservedTime(middle), builders.WithEventTime(middle), deploymentB))
	latest := start.Add(5 * time.Hour)
	third := observe(t, engine, at(t, factory, latest))
	if third.Generation != 3 || third.GenerationCount != 3 {
		t.Fatalf("setup did not create three episodes: %+v", third)
	}

	// The first episode stays alive because a worker still holds its record;
	// the second is entirely behind the retention horizon.
	engine.Pin([]string{first.RecordID})
	engine.Advance(latest.Add(2 * time.Minute))

	snapshot := engine.Incident(third.IncidentID)
	if snapshot.GenerationCount != 3 || snapshot.Current.Number != 3 {
		t.Fatalf("compacting the middle episode renumbered the live ones: %+v", snapshot)
	}
	if snapshot.Current.DeploymentID != builders.DefaultDeploymentID ||
		!snapshot.Current.FirstOccurrence.Equal(latest) {
		t.Fatalf("the live episode is no longer the third one: %+v", snapshot.Current)
	}
}

// A family whose whole shell is released cannot carry its ordinals forward, and
// keeping every shell for the life of the process is the unbounded growth
// compaction exists to remove. What must hold is the durable property: the
// recurrence is a new episode with its own (deployment_id, episode_start), so
// it can never be confused with a released one. See time-and-windows.md.
func TestReleasedFamilyRestartsOrdinalsWithoutReusingADurableEpisodeIdentity(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	origin := fakeclock.Origin.Add(-builders.DefaultEventLag)
	first := at(t, factory, origin)
	observe(t, engine, first)
	secondTime := origin.Add(2*time.Hour + 30*time.Minute)
	second := at(t, factory, secondTime)
	if update := observe(t, engine, second); update.Generation != 2 || update.GenerationCount != 2 {
		t.Fatalf("setup did not create a second episode: %+v", update)
	}
	released := map[string]episodeIdentity{}
	collectIdentities(t, engine, []model.NormalizedLog{first, second}, released, "before release")
	if len(released) == 0 {
		t.Fatal("setup issued no decision to compare against")
	}

	engine.Advance(secondTime.Add(10 * time.Hour))
	if retained := engine.Retained(); retained.Families != 0 {
		t.Fatalf("the idle family was not released: %+v", retained)
	}

	recurrenceTime := secondTime.Add(10 * time.Hour)
	recurrence := at(t, factory, recurrenceTime)
	update := observe(t, engine, recurrence)
	if update.Generation != 1 || update.GenerationCount != 1 {
		t.Fatalf("a released family did not restart its ordinals: %+v", update)
	}
	engine.Advance(recurrenceTime.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	decision, ok := engine.PersistenceDecision(recurrence.RecordID)
	if !ok {
		t.Fatal("the recurrence never received a persistence decision")
	}
	current := identityOf(decision)
	for recordID, previous := range released {
		if previous.equal(current) {
			t.Fatalf("the recurrence reused the durable episode identity %s of released record %s",
				current, recordID[:8])
		}
	}
}

// activeInvestigation is set by the engine and there is no completion signal to
// clear it, so an investigated family's shell used to survive every horizon.
// The shell only exists to suppress a relaunch, and time-and-windows.md allows
// reinvestigation once the configured interval has expired, so past that point
// the shell protects nothing and must be released with everything else.
func TestInvestigatedFamilyShellsAreReleasedAfterTheReinvestigationInterval(t *testing.T) {
	engine := incident.New()
	factory := builders.NewFactory()
	const families = 40
	start := fakeclock.Origin
	for family := 0; family < families; family++ {
		envelope := factory.Envelope(t)
		envelope.SourceAccount = fmt.Sprintf("%012d", family+1)
		for occurrence := 0; occurrence < incident.DefaultInvestigationThreshold; occurrence++ {
			observe(t, engine, factory.Record(t, builders.WithSourceEnvelope(envelope),
				builders.WithObservedTime(start), builders.WithEventTime(start)))
		}
	}
	engine.Advance(start.Add(incident.DefaultAllowedLateness + time.Nanosecond))
	if retained := engine.Retained(); retained.Families != families {
		t.Fatalf("setup did not create %d investigated families: %+v", families, retained)
	}

	engine.Advance(start.Add(incident.DefaultReinvestigationInterval - time.Nanosecond))
	if retained := engine.Retained(); retained.Families != families {
		t.Fatalf("suppression shells were released before reinvestigation became possible: %+v", retained)
	}
	engine.Advance(start.Add(incident.DefaultReinvestigationInterval))
	if retained := engine.Retained(); retained.Families != 0 || retained.Records != 0 ||
		retained.Evidence != 0 || retained.Seen != 0 {
		t.Fatalf("investigated family shells outlived every lifecycle deadline: %+v", retained)
	}
}
