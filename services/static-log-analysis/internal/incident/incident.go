// Package incident implements deterministic, in-memory incident transitions.
// Persistence later executes the same decisions inside a database transaction.
package incident

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/fingerprint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

const (
	DefaultInvestigationThreshold = 5
	DefaultInvestigationWindow    = 5 * time.Minute
	DefaultAllowedLateness        = 2 * time.Minute
	DefaultQuietPeriod            = 15 * time.Minute
	DefaultReopenWindow           = 2 * time.Hour
	DefaultStateRetention         = DefaultInvestigationWindow + DefaultAllowedLateness + DefaultReopenWindow
	// DefaultReinvestigationInterval is the interval after which
	// time-and-windows.md allows a completed report to be reinvestigated. Past
	// it, an active investigation no longer suppresses anything, so the family
	// shell which only carries that suppression can be released.
	DefaultReinvestigationInterval = 24 * time.Hour
)

const PersistenceDecisionVersion = "incident:persistence:v1"

var ErrInvalidPersistenceDecision = errors.New("incident: invalid persistence decision")

// PersistenceDecision is the immutable bridge from finalized M1 topology to
// storage. A normal record has no decision while its own ordering position is
// provisional, so a journal worker must leave it pending and retry later.
type PersistenceDecision struct {
	version          string
	incidentID       string
	recordID         string
	deploymentID     string
	episodeStart     time.Time
	generationKey    int64
	finalizedThrough time.Time
	evidenceOnly     bool
	// The lifecycle fields below are M1's own reconstruction of the finalized
	// generation. Storage consumes them rather than re-deriving them from a
	// single record, which cannot see the generation's other contributions and
	// silently disagreed with the engine about lateness, quiet, and status.
	detectionStatus DetectionStatus
	quietAt         time.Time
	reopenUntil     time.Time
	late            bool
}

func (d PersistenceDecision) Version() string                  { return d.version }
func (d PersistenceDecision) IncidentID() string               { return d.incidentID }
func (d PersistenceDecision) RecordID() string                 { return d.recordID }
func (d PersistenceDecision) DeploymentID() string             { return d.deploymentID }
func (d PersistenceDecision) EpisodeStart() time.Time          { return d.episodeStart }
func (d PersistenceDecision) GenerationKey() int64             { return d.generationKey }
func (d PersistenceDecision) FinalizedThrough() time.Time      { return d.finalizedThrough }
func (d PersistenceDecision) EvidenceOnly() bool               { return d.evidenceOnly }
func (d PersistenceDecision) DetectionStatus() DetectionStatus { return d.detectionStatus }
func (d PersistenceDecision) QuietAt() time.Time               { return d.quietAt }
func (d PersistenceDecision) ReopenUntil() time.Time           { return d.reopenUntil }

// Late reports the engine's own classification: this record's event position was
// strictly behind the family watermark when it was observed.
func (d PersistenceDecision) Late() bool { return d.late }

type DetectionStatus string

const (
	DetectionActive DetectionStatus = "active"
	DetectionQuiet  DetectionStatus = "quiet"
)

type GenerationSnapshot struct {
	Number               int
	DeploymentID         string
	OccurrenceCount      int
	WindowCount          int
	WatermarkWindowCount int
	DetectionStatus      DetectionStatus
	FirstOccurrence      time.Time
	LatestOccurrence     time.Time
}

type Snapshot struct {
	IncidentID          string
	GenerationCount     int
	Generations         []GenerationSnapshot
	Current             GenerationSnapshot
	ActiveInvestigation bool
	ContextVersion      int
	Watermark           time.Time
}

type Update struct {
	IncidentID             string
	Generation             int
	GenerationCount        int
	DeploymentID           string
	OccurrenceCount        int
	WindowCount            int
	WatermarkWindowCount   int
	DetectionStatus        DetectionStatus
	Duplicate              bool
	InvestigationRequested bool
	ActiveInvestigation    bool
	ContextVersion         int
	ContextUpdated         bool
	Watermark              time.Time
	Late                   bool
	EvidenceOnly           bool // retained outside episode reconstruction and threshold contributions
}

type Engine struct {
	mu       sync.Mutex
	families map[string]*family
	seen     map[string]recordLocation
	// prunedSeen counts identities removed since seen was last rebuilt. Go maps
	// never release bucket memory on delete, so a burst would otherwise keep its
	// peak allocation for the process lifetime.
	prunedSeen int
	// pinned holds identities a worker still has in flight.
	pinned map[string]struct{}
}

// Pin exempts identities a worker is still holding from compaction. A record
// whose payload has been released can never produce a persistence decision, so
// compacting one which is still claimed would leave its journal claim
// unresolvable and retried forever. The set is replaced on every call, so an
// identity stops being exempt as soon as the worker stops holding it.
func (e *Engine) Pin(recordIDs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(recordIDs) == 0 {
		e.pinned = nil
		return
	}
	pinned := make(map[string]struct{}, len(recordIDs))
	for _, recordID := range recordIDs {
		pinned[recordID] = struct{}{}
	}
	e.pinned = pinned
}

type recordLocation struct {
	incidentID string
	generation int
	late       bool
	evidence   bool
}

type evidenceRecord struct {
	record model.NormalizedLog
	attach bool
	// late is the classification made when the record arrived. It is stored
	// rather than re-derived, because the family watermark keeps moving and a
	// record's own arrival position cannot change afterwards.
	late bool
}

type family struct {
	id       string
	events   map[string]model.NormalizedLog
	evidence map[string]evidenceRecord
	// ordered is every retained contributing record in reconstruction order.
	ordered             []model.NormalizedLog
	generations         []*generation
	currentGeneration   *generation
	activeInvestigation bool
	contextVersion      int
	maxEventTime        time.Time
	watermark           time.Time
	finalizedThrough    time.Time
	advancedTo          time.Time
	// lastActivity is the newest observed-time-bounded activity this family has
	// ever seen. It is monotone, so a lifecycle deadline measured from it cannot
	// be pushed backwards by a late arrival.
	lastActivity time.Time
	// prunedGenerations counts the leading episodes released from memory.
	// Display ordinals and the agent-visible context version are offset by it so
	// compaction can never move either backwards while the family is retained.
	prunedGenerations int
}

type generation struct {
	number           int
	deploymentID     string
	status           DetectionStatus
	firstOccurrence  time.Time
	latestOccurrence time.Time
	lastActivity     time.Time
	maxEventTime     time.Time
	records          map[string]model.NormalizedLog // threshold-contributing records
	evidence         map[string]model.NormalizedLog // retained, non-contributing evidence
	// times and evidenceTimes are the sorted event times of the two maps above.
	// Window counts answer by binary search, so one hot episode does not make
	// every later observation proportional to its whole history.
	times         []time.Time
	evidenceTimes []time.Time
	// eligible holds the sorted event times which may contribute to the
	// threshold. satisfiedAt is the earliest endpoint whose half-open window
	// contains a whole threshold; time-and-windows.md requires retaining it so a
	// later watermark jump cannot skip a cluster which already qualified.
	eligible    []time.Time
	satisfiedAt time.Time
}

func New() *Engine {
	return &Engine{families: map[string]*family{}, seen: map[string]recordLocation{}}
}

func (e *Engine) Observe(record model.NormalizedLog) (Update, error) {
	result, err := fingerprint.Error(record)
	if err != nil {
		return Update{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if location, duplicate := e.seen[record.RecordID]; duplicate {
		f := e.families[location.incidentID]
		return e.update(f, generationAt(f, location.generation), true, false, false, location.late, location.evidence), nil
	}

	id := incidentID(record, result)
	f := e.families[id]
	if f == nil {
		f = &family{
			id:       id,
			events:   map[string]model.NormalizedLog{},
			evidence: map[string]evidenceRecord{},
		}
		e.families[id] = f
	}

	if f.maxEventTime.IsZero() || record.EventTime.After(f.maxEventTime) {
		f.maxEventTime = record.EventTime
		f.watermark = record.EventTime.Add(-DefaultAllowedLateness)
		f.finalizeThrough(f.watermark)
	}
	if activity := boundedActivity(record); activity.After(f.lastActivity) {
		f.lastActivity = activity
	}
	late := !f.watermark.IsZero() && record.EventTime.Before(f.watermark)
	veryLate := record.EventTime.Before(f.watermark.Add(-DefaultStateRetention))
	quietEvidence := !f.advancedTo.IsZero() &&
		!f.advancedTo.Before(boundedActivity(record).Add(DefaultQuietPeriod))
	finalized := !f.finalizedThrough.IsZero() && record.EventTime.Before(f.finalizedThrough)

	if finalized || quietEvidence {
		target := f.generationForEvidence(deploymentID(record), record.EventTime)
		attach := !veryLate && target != nil
		evidence := evidenceRecord{record: record, attach: attach, late: late}
		f.evidence[record.RecordID] = evidence
		// Evidence never changes episode membership, so only its own attachment
		// has to be resolved. Reconstructing the family here would repeat its
		// whole history for a record which by definition cannot rewrite it.
		e.attachEvidence(f, evidence)
		return e.update(f, f.generationContaining(record.RecordID), false, false, false, late, true), nil
	}

	oldTopology := f.topology()
	oldContext := f.contextVersion
	f.events[record.RecordID] = record
	if f.insert(record) {
		// The record sorts after everything already retained, so the left-to-right
		// episode fold over the earlier records is unchanged and only its tail can
		// move. A full reconstruction would produce the same topology at a cost
		// proportional to the whole family.
		e.extend(f, record)
	} else {
		e.rebuild(f)
	}
	requested := false
	if !f.activeInvestigation && e.thresholdGeneration(f, f.finalizedThrough) != nil {
		f.activeInvestigation = true
		requested = true
	}
	if f.activeInvestigation {
		// Context version is a deterministic projection of the reconstructed
		// topology, so the same event set produces the same agent-visible version
		// regardless of which arrival happened to cross the threshold.
		f.contextVersion = f.generationCount()
	}
	contextUpdated := !requested && f.activeInvestigation &&
		(oldTopology != f.topology() || oldContext != f.contextVersion)
	target := f.generationContaining(record.RecordID)
	return e.update(f, target, false, requested, contextUpdated, false, false), nil
}

func (e *Engine) Advance(now time.Time) []Update {
	e.mu.Lock()
	defer e.mu.Unlock()

	ids := make([]string, 0, len(e.families))
	for id := range e.families {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var changed []Update
	for _, id := range ids {
		f := e.families[id]
		old := make(map[int]DetectionStatus, len(f.generations))
		for _, g := range f.generations {
			old[g.number] = g.status
		}
		if f.advancedTo.IsZero() || now.After(f.advancedTo) {
			f.advancedTo = now
		}
		if !f.maxEventTime.IsZero() {
			deadlineFrontier := now.Add(-DefaultAllowedLateness)
			maximumFrontier := strictSuccessor(f.maxEventTime)
			if deadlineFrontier.After(maximumFrontier) {
				deadlineFrontier = maximumFrontier
			}
			f.finalizeThrough(deadlineFrontier)
		}
		// Only detection status depends on the advancing frontier. Episode
		// membership is a function of the retained records alone, so replaying the
		// whole family on every cycle would buy nothing.
		f.refreshStatuses()
		requestedGeneration := (*generation)(nil)
		if !f.activeInvestigation {
			requestedGeneration = e.thresholdGeneration(f, f.finalizedThrough)
			if requestedGeneration != nil {
				f.activeInvestigation = true
				f.contextVersion = f.generationCount()
				changed = append(changed, e.update(f, requestedGeneration, false, true, false, false, false))
			}
		}
		for _, g := range f.generations {
			if old[g.number] == DetectionActive && g.status == DetectionQuiet {
				if g == requestedGeneration {
					continue
				}
				changed = append(changed, e.update(f, g, false, false, false, false, false))
			}
		}
		// Compaction runs only after this cycle's frontier has been evaluated, so
		// a cluster which became finalized on this very cycle can still launch
		// before its evidence is released.
		e.compact(f, id, now)
	}
	e.compactSeen()
	return changed
}

// compact releases in-memory state which can no longer affect any decision.
// Without it one hot family retains every 256 KiB record for the lifetime of
// the process and makes each later observation proportional to that history,
// which is exactly the wrong behaviour while the system is under incident load.
func (e *Engine) compact(f *family, id string, now time.Time) {
	if e.prune(f, now) {
		e.rebuild(f)
	}
	if len(f.events) != 0 || len(f.evidence) != 0 {
		return
	}
	// A family with nothing retained has no state left to protect: any arrival
	// old enough to have belonged to it is already evidence-only. The one thing
	// its shell still carries is the relaunch suppression of an active
	// investigation, and nothing in this milestone can clear that flag, so
	// holding every investigated shell forever is a slow leak keyed on
	// distinct-fingerprint cardinality. time-and-windows.md permits
	// reinvestigation once the configured interval has expired; past that
	// deadline the shell suppresses nothing an operator would want suppressed.
	if f.activeInvestigation && now.Before(f.lastActivity.Add(DefaultReinvestigationInterval)) {
		return
	}
	delete(e.families, id)
}

// prune drops records behind the family's retention horizon and reports whether
// anything was released. It releases payloads only; which episodes that empties,
// and which of those may then be released without renumbering the rest, is
// decided by the rebuild it triggers.
func (e *Engine) prune(f *family, now time.Time) bool {
	horizon := f.retentionHorizon(now)
	if horizon.IsZero() {
		return false
	}
	// Reconstruction order is event-time major, so everything behind the horizon
	// is a prefix; an in-flight record inside it is kept in place.
	cut := sort.Search(len(f.ordered), func(i int) bool { return !f.ordered[i].EventTime.Before(horizon) })
	pruned := 0
	kept := 0
	for i, record := range f.ordered {
		if i < cut && !e.isPinned(record.RecordID) {
			delete(f.events, record.RecordID)
			e.forget(record.RecordID)
			pruned++
			continue
		}
		f.ordered[kept] = record
		kept++
	}
	for i := kept; i < len(f.ordered); i++ {
		f.ordered[i] = model.NormalizedLog{}
	}
	f.ordered = f.ordered[:kept]
	for recordID, evidence := range f.evidence {
		if evidence.record.EventTime.Before(horizon) && !e.isPinned(recordID) {
			delete(f.evidence, recordID)
			e.forget(recordID)
			pruned++
		}
	}
	if pruned == 0 {
		return false
	}
	if pruned > len(f.events) {
		// The map kept its peak bucket array through the deletes above.
		events := make(map[string]model.NormalizedLog, len(f.events))
		for recordID, record := range f.events {
			events[recordID] = record
		}
		f.events = events
	}
	return true
}

func (e *Engine) isPinned(recordID string) bool {
	_, pinned := e.pinned[recordID]
	return pinned
}

// forget removes one identity from the local duplicate-suppression map. This is
// safe because it is not the durable gate: `records_seen` in CockroachDB is
// unique on `record_id` and outlives this process, so a compacted identity
// delivered again still contributes exactly one occurrence. The in-memory map
// only avoids re-deriving topology for identities still inside the retention
// horizon, and nothing inside that horizon is ever removed here.
func (e *Engine) forget(recordID string) {
	delete(e.seen, recordID)
	e.prunedSeen++
}

// compactSeen rebuilds the identity map once deletions have left most of its
// bucket array empty, because Go maps never shrink in place.
func (e *Engine) compactSeen() {
	const minimumCompaction = 1024
	if e.prunedSeen < minimumCompaction || e.prunedSeen <= len(e.seen) {
		return
	}
	seen := make(map[string]recordLocation, len(e.seen))
	for recordID, location := range e.seen {
		seen[recordID] = location
	}
	e.seen = seen
	e.prunedSeen = 0
}

func strictSuccessor(eventTime time.Time) time.Time {
	successor := eventTime.Add(time.Nanosecond)
	if successor.After(eventTime) {
		return successor
	}
	return eventTime
}

// Retention reports how much in-memory state the engine is holding. It exists
// because the only honest way to test a retention bound is to assert on the
// retained state itself; a timing assertion would be a flake.
type Retention struct {
	Families int
	Records  int
	Evidence int
	Seen     int
}

// Retained returns the current in-memory retention counts.
func (e *Engine) Retained() Retention {
	e.mu.Lock()
	defer e.mu.Unlock()
	retained := Retention{Families: len(e.families), Seen: len(e.seen)}
	for _, f := range e.families {
		retained.Records += len(f.events)
		retained.Evidence += len(f.evidence)
	}
	return retained
}

func (e *Engine) Incident(id string) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.families[id]
	if f == nil {
		return Snapshot{}
	}
	generations := make([]GenerationSnapshot, 0, len(f.generations))
	for _, g := range f.generations {
		generations = append(generations, generationSnapshot(g, f.watermark))
	}
	return Snapshot{
		IncidentID:          f.id,
		GenerationCount:     f.generationCount(),
		Generations:         generations,
		Current:             generationSnapshot(f.current(), f.watermark),
		ActiveInvestigation: f.activeInvestigation,
		ContextVersion:      f.contextVersion,
		Watermark:           f.watermark,
	}
}

// PersistenceDecision returns a decision only after this record's own event
// position is strictly behind the irreversible finalization frontier. Evidence
// must additionally be attached to an already-frozen generation.
func (e *Engine) PersistenceDecision(recordID string) (PersistenceDecision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	location, ok := e.seen[recordID]
	if !ok {
		return PersistenceDecision{}, false
	}
	f := e.families[location.incidentID]
	if f == nil || f.finalizedThrough.IsZero() {
		return PersistenceDecision{}, false
	}
	var record model.NormalizedLog
	if location.evidence {
		evidence, found := f.evidence[recordID]
		if !found || !evidence.attach {
			return PersistenceDecision{}, false
		}
		record = evidence.record
	} else {
		var found bool
		record, found = f.events[recordID]
		if !found {
			return PersistenceDecision{}, false
		}
	}
	// Strict event-time finalization also freezes every tie-break position at
	// this timestamp: the deadline frontier advances by one nanosecond.
	if !record.EventTime.Before(f.finalizedThrough) {
		return PersistenceDecision{}, false
	}
	g := f.generationContaining(recordID)
	if g == nil || !g.firstOccurrence.Before(f.finalizedThrough) {
		return PersistenceDecision{}, false
	}
	key, err := GenerationKey(g.deploymentID, g.firstOccurrence)
	if err != nil {
		return PersistenceDecision{}, false
	}
	// Quiet is measured from the generation's bounded activity, so a
	// future-skewed record cannot hold the incident open past its observed time.
	quietAt := g.lastActivity.Add(DefaultQuietPeriod)
	return PersistenceDecision{
		version:          PersistenceDecisionVersion,
		incidentID:       f.id,
		recordID:         recordID,
		deploymentID:     g.deploymentID,
		episodeStart:     g.firstOccurrence,
		generationKey:    key,
		finalizedThrough: f.finalizedThrough,
		evidenceOnly:     location.evidence,
		detectionStatus:  g.status,
		quietAt:          quietAt,
		reopenUntil:      quietAt.Add(DefaultReopenWindow),
		late:             location.late,
	}, true
}

// ValidatePersistenceDecision checks the complete record-bound, versioned
// decision before a storage retry callback begins.
func ValidatePersistenceDecision(record model.NormalizedLog, decision PersistenceDecision) error {
	if record.Validate() != nil || decision.version != PersistenceDecisionVersion ||
		decision.recordID != record.RecordID || decision.finalizedThrough.IsZero() ||
		decision.finalizedThrough.Location() != time.UTC || !record.EventTime.Before(decision.finalizedThrough) ||
		decision.deploymentID != deploymentID(record) {
		return ErrInvalidPersistenceDecision
	}
	wantIncidentID, err := DeterministicID(record)
	if err != nil || wantIncidentID != decision.incidentID {
		return ErrInvalidPersistenceDecision
	}
	wantKey, err := GenerationKey(decision.deploymentID, decision.episodeStart)
	if err != nil || wantKey != decision.generationKey {
		return ErrInvalidPersistenceDecision
	}
	if decision.detectionStatus != DetectionActive && decision.detectionStatus != DetectionQuiet {
		return ErrInvalidPersistenceDecision
	}
	if decision.quietAt.IsZero() || decision.quietAt.Location() != time.UTC ||
		!decision.reopenUntil.Equal(decision.quietAt.Add(DefaultReopenWindow)) {
		return ErrInvalidPersistenceDecision
	}
	// Only a record the frontier had already passed can be late. A provisional
	// record participates on time by construction, so a late non-evidence
	// decision would mean the caller reconstructed lateness itself.
	if decision.late && !decision.evidenceOnly {
		return ErrInvalidPersistenceDecision
	}
	if !decision.evidenceOnly {
		if record.EventTime.Before(decision.episodeStart) {
			return ErrInvalidPersistenceDecision
		}
		// A contributing record's own bounded activity must be covered by the
		// generation quiet time it is being persisted with.
		if decision.quietAt.Before(boundedActivity(record).Add(DefaultQuietPeriod)) {
			return ErrInvalidPersistenceDecision
		}
	}
	return nil
}

// GenerationKey derives the immutable opaque storage key for one finalized
// deployment episode. It is not the mutable in-memory display ordinal.
func GenerationKey(deployment string, episodeStart time.Time) (int64, error) {
	if strings.TrimSpace(deployment) == "" || episodeStart.IsZero() || episodeStart.Location() != time.UTC {
		return 0, ErrInvalidPersistenceDecision
	}
	hash := sha256.New()
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(deployment)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(deployment))
	var instant [8]byte
	binary.BigEndian.PutUint64(instant[:], uint64(episodeStart.UnixNano()))
	_, _ = hash.Write(instant[:])
	key := int64(binary.BigEndian.Uint64(hash.Sum(nil)[:8]) & ((1 << 63) - 1))
	if key == 0 {
		key = 1
	}
	return key, nil
}

func InHalfOpenWindow(event, watermark time.Time, duration time.Duration) bool {
	if duration <= 0 || event.After(watermark) {
		return false
	}
	return event.After(watermark.Add(-duration))
}

// sortsBefore is the total order episode reconstruction folds over. Equal event
// times are ordered by deployment and then global record_id, which groups
// same-deployment records and makes topology independent of arrival order.
func sortsBefore(left, right model.NormalizedLog) bool {
	if !left.EventTime.Equal(right.EventTime) {
		return left.EventTime.Before(right.EventTime)
	}
	leftDeployment, rightDeployment := deploymentID(left), deploymentID(right)
	if leftDeployment != rightDeployment {
		return leftDeployment < rightDeployment
	}
	return left.RecordID < right.RecordID
}

// insert places one record in the family's reconstruction order and reports
// whether it landed at the end. The order is maintained rather than recomputed
// because sorting the whole history on every arrival is the dominant cost of a
// hot family.
func (f *family) insert(record model.NormalizedLog) bool {
	index := sort.Search(len(f.ordered), func(i int) bool { return !sortsBefore(f.ordered[i], record) })
	f.ordered = append(f.ordered, model.NormalizedLog{})
	copy(f.ordered[index+1:], f.ordered[index:])
	f.ordered[index] = record
	return index == len(f.ordered)-1
}

// extend folds one record which sorts after every retained record into the
// existing episodes. It is exactly the last step a full reconstruction would
// take, so both paths agree by construction.
func (e *Engine) extend(f *family, record model.NormalizedLog) {
	deployment := deploymentID(record)
	activity := boundedActivity(record)
	g := f.currentGeneration
	created := false
	if g == nil || g.deploymentID != deployment || !activity.Before(g.lastActivity.Add(DefaultReopenWindow)) {
		g = newGeneration(f.generationCount()+1, deployment)
		f.generations = append(f.generations, g)
		f.currentGeneration = g
		created = true
	}
	addContribution(g, record)
	f.refreshStatuses()
	if created {
		// A new final episode can become the attachment target for evidence at
		// the same event time, so attachment is resolved again rather than left
		// pointing at the previous episode of the same deployment.
		e.attachAllEvidence(f)
	}
	e.seen[record.RecordID] = recordLocation{incidentID: f.id, generation: g.number}
}

// rebuild reconstructs every episode from the retained records. It is used when
// an out-of-order arrival or compaction can move an earlier fold step.
//
// An episode's identity is assigned once, when the episode begins, and adopted
// here rather than re-derived. Storage keys `incident_generations` on
// (deployment_id, episode_start), so an episode start recomputed from whichever
// records happened to survive compaction would give one real episode a second
// durable generation, splitting its occurrences, evidence, quiet/reopen bounds
// and its election index across both identities.
func (e *Engine) rebuild(f *family) {
	f.generations = f.foldEpisodes()
	f.attachRetainedEvidence()
	f.prunedGenerations += f.releaseLeadingEmptyEpisodes()
	for index, g := range f.generations {
		g.number = f.prunedGenerations + index + 1
	}
	f.currentGeneration = nil
	if len(f.generations) > 0 {
		f.currentGeneration = f.generations[len(f.generations)-1]
	}
	f.refreshStatuses()
	e.indexLocations(f)
}

// foldEpisodes folds the retained records into episodes, adopting the identity
// each record's episode was already assigned. An episode which lost every
// retained record keeps an identity-only shell in place rather than vanishing,
// so releasing it out of the middle can never renumber the live episodes after
// it.
func (f *family) foldEpisodes() []*generation {
	var generations []*generation
	for _, record := range f.ordered {
		deployment := deploymentID(record)
		activity := boundedActivity(record)
		var g *generation
		if len(generations) > 0 {
			g = generations[len(generations)-1]
		}
		if g == nil || g.deploymentID != deployment ||
			!activity.Before(g.lastActivity.Add(DefaultReopenWindow)) {
			g = newGeneration(0, deployment)
			generations = append(generations, g)
		}
		addContribution(g, record)
	}
	generations = append(generations, f.adoptAssignedIdentities(generations)...)
	// Episodes are contiguous ranges of the reconstruction order, so ordering
	// them by their assigned start reproduces the fold order and also places a
	// shell whose records are gone back among the episodes it sits between.
	sort.SliceStable(generations, func(i, j int) bool {
		if !generations[i].firstOccurrence.Equal(generations[j].firstOccurrence) {
			return generations[i].firstOccurrence.Before(generations[j].firstOccurrence)
		}
		return generations[i].deploymentID < generations[j].deploymentID
	})
	return generations
}

// adoptAssignedIdentities gives each freshly folded episode the identity its
// records already carried, and returns identity-only shells for the episodes
// which lost every retained record to compaction. An episode is matched to the
// earliest not-yet-adopted assignment any of its records held, so a fold which
// merges or splits provisional episodes still walks the assignments in order.
// A prior episode whose records are all still retained was absorbed rather than
// released, so it leaves no shell behind.
func (f *family) adoptAssignedIdentities(folded []*generation) []*generation {
	prior := f.generations
	if len(prior) == 0 {
		return nil
	}
	assigned := make(map[string]int, len(f.ordered))
	for index, p := range prior {
		for recordID := range p.records {
			assigned[recordID] = index
		}
	}
	adopted := make([]bool, len(prior))
	cursor := 0
	for _, g := range folded {
		index := -1
		for recordID := range g.records {
			if at, ok := assigned[recordID]; ok && at >= cursor && (index < 0 || at < index) {
				index = at
			}
		}
		if index < 0 {
			continue
		}
		g.adopt(prior[index])
		adopted[index] = true
		cursor = index + 1
	}
	var shells []*generation
	for index, p := range prior {
		if adopted[index] || f.retainsAnyOf(p) {
			continue
		}
		shells = append(shells, shellOf(p))
	}
	return shells
}

// retainsAnyOf reports whether any of an episode's records survived compaction.
func (f *family) retainsAnyOf(g *generation) bool {
	for recordID := range g.records {
		if _, ok := f.events[recordID]; ok {
			return true
		}
	}
	return false
}

// releaseLeadingEmptyEpisodes drops the leading run of episodes which retain
// neither records nor evidence and reports how many were released. Only a
// leading run may go: dropping an episode out of the middle would renumber
// every episode after it, and a display ordinal which moves backwards takes the
// agent-visible context version with it.
func (f *family) releaseLeadingEmptyEpisodes() int {
	released := 0
	for released < len(f.generations) {
		g := f.generations[released]
		if len(g.records) != 0 || len(g.evidence) != 0 {
			break
		}
		released++
	}
	f.generations = f.generations[released:]
	return released
}

// indexLocations republishes the local identity index from the rebuilt
// episodes. A display ordinal is only known once compaction has released whole
// episodes, so the index cannot be written while the fold is still running.
func (e *Engine) indexLocations(f *family) {
	for _, g := range f.generations {
		for recordID := range g.records {
			e.seen[recordID] = recordLocation{incidentID: f.id, generation: g.number}
		}
	}
	e.indexEvidence(f)
}

// refreshStatuses recomputes detection status from the current episodes and the
// advancing observed-time frontier. Status is always derived from scratch so an
// episode which grows past its quiet instant becomes active again.
func (f *family) refreshStatuses() {
	for i, g := range f.generations {
		g.status = DetectionActive
		if i < len(f.generations)-1 || (!f.advancedTo.IsZero() &&
			!f.advancedTo.Before(g.lastActivity.Add(DefaultQuietPeriod))) {
			g.status = DetectionQuiet
		}
	}
}

// attachAllEvidence resolves every retained evidence record against the current
// episodes and republishes their identities.
func (e *Engine) attachAllEvidence(f *family) {
	f.attachRetainedEvidence()
	e.indexEvidence(f)
}

// attachRetainedEvidence resolves every retained evidence record against the
// current episodes. Attachment is recomputed rather than accumulated so an
// episode can never hold the same evidence twice.
func (f *family) attachRetainedEvidence() {
	for _, g := range f.generations {
		if len(g.evidence) != 0 {
			g.evidence = map[string]model.NormalizedLog{}
			g.evidenceTimes = g.evidenceTimes[:0]
		}
	}
	for _, evidence := range f.evidence {
		if !evidence.attach {
			continue
		}
		if g := f.generationForEvidence(deploymentID(evidence.record), evidence.record.EventTime); g != nil {
			g.evidence[evidence.record.RecordID] = evidence.record
			g.evidenceTimes = insertTime(g.evidenceTimes, evidence.record.EventTime)
		}
	}
}

// indexEvidence republishes the identity of every retained evidence record.
// Lateness is the classification made when the record arrived, not a value
// re-derived from a watermark which has since moved on.
func (e *Engine) indexEvidence(f *family) {
	for recordID, evidence := range f.evidence {
		location := recordLocation{incidentID: f.id, evidence: true, late: evidence.late}
		if g := f.generationContaining(recordID); g != nil {
			location.generation = g.number
		}
		e.seen[recordID] = location
	}
}

// attachEvidence resolves one evidence record's episode and records its
// identity. Lateness is the classification made when the record arrived, not a
// value re-derived from a watermark which has since moved on.
func (e *Engine) attachEvidence(f *family, evidence evidenceRecord) {
	location := recordLocation{incidentID: f.id, evidence: true, late: evidence.late}
	if evidence.attach {
		if g := f.generationForEvidence(deploymentID(evidence.record), evidence.record.EventTime); g != nil {
			g.evidence[evidence.record.RecordID] = evidence.record
			g.evidenceTimes = insertTime(g.evidenceTimes, evidence.record.EventTime)
			location.generation = g.number
		}
	}
	e.seen[evidence.record.RecordID] = location
}

func newGeneration(number int, deployment string) *generation {
	return &generation{
		number:       number,
		deploymentID: deployment,
		status:       DetectionActive,
		records:      map[string]model.NormalizedLog{},
		evidence:     map[string]model.NormalizedLog{},
	}
}

// adopt carries the episode start assigned when the episode began onto the
// generation being rebuilt for it. Only a contribution older than the assigned
// start may still lower it, which is legitimate while the episode is
// provisional; by the time a decision exists, the finalization frontier has
// already made an older contribution evidence-only. Everything else about the
// episode is a function of its current membership and is re-derived.
func (g *generation) adopt(prior *generation) {
	if !prior.firstOccurrence.IsZero() &&
		(g.firstOccurrence.IsZero() || prior.firstOccurrence.Before(g.firstOccurrence)) {
		g.firstOccurrence = prior.firstOccurrence
	}
}

// shellOf is the identity of an episode whose retained records have all been
// released. It carries no payload; it exists so the episodes after it keep
// their display ordinals until the whole leading run can be released together.
// Its lifecycle bounds come along because a later same-deployment recurrence
// still measures its reopen window from them.
func shellOf(prior *generation) *generation {
	g := newGeneration(0, prior.deploymentID)
	g.firstOccurrence = prior.firstOccurrence
	g.latestOccurrence = prior.latestOccurrence
	g.maxEventTime = prior.maxEventTime
	g.lastActivity = prior.lastActivity
	return g
}

func (f *family) current() *generation { return f.currentGeneration }

// generationCount includes episodes whose payloads have been compacted away, so
// the agent-visible ordinal of a live generation never changes.
func (f *family) generationCount() int { return f.prunedGenerations + len(f.generations) }

// retentionHorizon is the oldest event time whose payload can still affect a
// decision. time-and-windows.md retains state through
// window + allowed_lateness + reopen_window; anything strictly older is already
// in the very-late band, which is evidence-only and cannot attach to or rewrite
// any episode. An idle family's horizon still advances with processing time, so
// a family that stopped receiving records is compacted rather than held for the
// process lifetime.
func (f *family) retentionHorizon(now time.Time) time.Time {
	frontier := f.watermark
	if idle := now.Add(-DefaultAllowedLateness); !now.IsZero() && idle.After(frontier) {
		frontier = idle
	}
	if frontier.IsZero() {
		return time.Time{}
	}
	return frontier.Add(-DefaultStateRetention)
}

func (f *family) finalizeThrough(eventTime time.Time) {
	if f.finalizedThrough.IsZero() || eventTime.After(f.finalizedThrough) {
		f.finalizedThrough = eventTime
	}
}

func (f *family) generationForEvidence(deployment string, eventTime time.Time) *generation {
	var earliest *generation
	for i := len(f.generations) - 1; i >= 0; i-- {
		g := f.generations[i]
		if g.deploymentID != deployment {
			continue
		}
		earliest = g
		if !eventTime.Before(g.firstOccurrence) {
			return g
		}
	}
	return earliest
}

func (f *family) generationContaining(recordID string) *generation {
	for _, g := range f.generations {
		if _, ok := g.records[recordID]; ok {
			return g
		}
		if _, ok := g.evidence[recordID]; ok {
			return g
		}
	}
	return nil
}

func (f *family) topology() string {
	parts := make([]string, 0, len(f.generations))
	for _, g := range f.generations {
		parts = append(parts, g.deploymentID)
	}
	return strings.Join(parts, "\x00")
}

func (e *Engine) thresholdGeneration(f *family, finalizedThrough time.Time) *generation {
	for _, g := range f.generations {
		if generationSatisfiesThreshold(g, finalizedThrough) {
			return g
		}
	}
	return nil
}

// generationSatisfiesThreshold answers from the retained qualifying endpoint.
// Every record inside a window ending at that endpoint is at or before it, so
// an endpoint strictly behind the frontier means the whole cluster is too.
func generationSatisfiesThreshold(g *generation, finalizedThrough time.Time) bool {
	return !g.satisfiedAt.IsZero() && g.satisfiedAt.Before(finalizedThrough)
}

func (e *Engine) update(f *family, g *generation, duplicate, requested, contextUpdated, late, evidenceOnly bool) Update {
	update := Update{
		IncidentID:             f.id,
		GenerationCount:        f.generationCount(),
		Duplicate:              duplicate,
		InvestigationRequested: requested,
		ActiveInvestigation:    f.activeInvestigation,
		ContextVersion:         f.contextVersion,
		ContextUpdated:         contextUpdated,
		Watermark:              f.watermark,
		Late:                   late,
		EvidenceOnly:           evidenceOnly,
	}
	if g == nil {
		return update
	}
	update.Generation = g.number
	update.DeploymentID = g.deploymentID
	update.OccurrenceCount = occurrenceCount(g)
	update.WindowCount = countInWindow(g, g.maxEventTime)
	update.WatermarkWindowCount = countInWindow(g, f.watermark)
	update.DetectionStatus = g.status
	return update
}

func generationAt(f *family, number int) *generation {
	index := number - 1 - f.prunedGenerations
	if index < 0 || index >= len(f.generations) {
		return nil
	}
	return f.generations[index]
}

// addContribution folds one record into an episode. Records reach it in
// reconstruction order, so the sorted projections below are appends.
func addContribution(g *generation, record model.NormalizedLog) {
	g.records[record.RecordID] = record
	g.times = append(g.times, record.EventTime)
	if investigationEligible(record) {
		g.eligible = append(g.eligible, record.EventTime)
		if g.satisfiedAt.IsZero() {
			lower := record.EventTime.Add(-DefaultInvestigationWindow)
			first := sort.Search(len(g.eligible), func(i int) bool { return g.eligible[i].After(lower) })
			if len(g.eligible)-first >= DefaultInvestigationThreshold {
				// Endpoints are folded in increasing order, so the first endpoint
				// to qualify is the earliest one.
				g.satisfiedAt = record.EventTime
			}
		}
	}
	if g.firstOccurrence.IsZero() || record.EventTime.Before(g.firstOccurrence) {
		g.firstOccurrence = record.EventTime
	}
	if g.latestOccurrence.IsZero() || record.EventTime.After(g.latestOccurrence) {
		g.latestOccurrence = record.EventTime
	}
	if g.maxEventTime.IsZero() || record.EventTime.After(g.maxEventTime) {
		g.maxEventTime = record.EventTime
	}
	activity := boundedActivity(record)
	if g.lastActivity.IsZero() || activity.After(g.lastActivity) {
		g.lastActivity = activity
	}
}

func investigationEligible(record model.NormalizedLog) bool {
	return record.SeverityClass == model.SeverityClassError &&
		record.Service.Status == model.EnrichmentAvailable &&
		strings.TrimSpace(record.Service.Name) != "" &&
		strings.TrimSpace(record.Service.Environment) != ""
}

func generationSnapshot(g *generation, watermark time.Time) GenerationSnapshot {
	if g == nil {
		return GenerationSnapshot{}
	}
	return GenerationSnapshot{
		Number:               g.number,
		DeploymentID:         g.deploymentID,
		OccurrenceCount:      occurrenceCount(g),
		WindowCount:          countInWindow(g, g.maxEventTime),
		WatermarkWindowCount: countInWindow(g, watermark),
		DetectionStatus:      g.status,
		FirstOccurrence:      g.firstOccurrence,
		LatestOccurrence:     g.latestOccurrence,
	}
}

func occurrenceCount(g *generation) int { return len(g.records) + len(g.evidence) }

func countInWindow(g *generation, watermark time.Time) int {
	return countInSortedWindow(g.times, watermark) + countInSortedWindow(g.evidenceTimes, watermark)
}

// countInSortedWindow counts the members of a sorted slice inside the half-open
// interval (watermark - window, watermark].
func countInSortedWindow(times []time.Time, watermark time.Time) int {
	if watermark.IsZero() {
		return 0
	}
	lower := watermark.Add(-DefaultInvestigationWindow)
	first := sort.Search(len(times), func(i int) bool { return times[i].After(lower) })
	past := sort.Search(len(times), func(i int) bool { return times[i].After(watermark) })
	if past < first {
		return 0
	}
	return past - first
}

// insertTime keeps a sorted event-time projection sorted under an out-of-order
// insertion.
func insertTime(times []time.Time, when time.Time) []time.Time {
	index := sort.Search(len(times), func(i int) bool { return times[i].After(when) })
	times = append(times, time.Time{})
	copy(times[index+1:], times[index:])
	times[index] = when
	return times
}

func boundedActivity(record model.NormalizedLog) time.Time {
	if record.EventTime.After(record.ObservedTime) {
		return record.ObservedTime
	}
	return record.EventTime
}

func deploymentID(record model.NormalizedLog) string {
	if strings.TrimSpace(record.Deployment.ID) == "" {
		return model.UnknownDeployment
	}
	return record.Deployment.ID
}

func incidentID(record model.NormalizedLog, result fingerprint.Result) string {
	key := fmt.Sprintf("incident:v1\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		record.Source.SourceAccount, record.Region, record.Service.Name, record.Service.Environment, result.Version, result.Digest)
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// DeterministicID returns the canonical M1 family identity for a normalized
// record. Storage boundaries use this function to reject caller-forged family
// identifiers instead of maintaining a second copy of the identity algorithm.
func DeterministicID(record model.NormalizedLog) (string, error) {
	result, err := fingerprint.Error(record)
	if err != nil {
		return "", err
	}
	return incidentID(record, result), nil
}
