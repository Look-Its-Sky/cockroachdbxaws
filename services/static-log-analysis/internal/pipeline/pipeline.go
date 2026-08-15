// Package pipeline coordinates the production OTLP-to-investigation vertical
// slice. It owns acknowledgement and retry ordering across admission, the local
// journal, deterministic incident decisions, and CockroachDB persistence.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/fingerprint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/normalize"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

const (
	DefaultWorkerOwner = "static-log-analysis"
	DefaultRecoveryMax = journal.MaxTransitionRecords
	requiredRetention  = incident.DefaultInvestigationWindow + incident.DefaultAllowedLateness
)

var (
	ErrInvalidConfig      = errors.New("pipeline: invalid configuration")
	ErrRequestRejected    = errors.New("pipeline: request rejected")
	ErrJournalUnavailable = errors.New("pipeline: journal unavailable")
	// ErrInvalidStoreResult means the Store answered with a result shape that
	// cannot be reconciled with the claims about to be committed.
	ErrInvalidStoreResult = errors.New("pipeline: invalid store result")
	// ErrScopeNotPermitted and ErrJournalRejected are permanent request-level
	// outcomes. They wrap ErrRequestRejected so a transport retries neither.
	ErrScopeNotPermitted = fmt.Errorf("%w: envelope scope is not served by this replica", ErrRequestRejected)
	ErrJournalRejected   = fmt.Errorf("%w: journal permanently refused the batch", ErrRequestRejected)
)

// errRecordPoison marks the one preparation failure a record can never recover
// from. Every other preparation failure is retryable and must keep its claim.
var errRecordPoison = errors.New("pipeline: record cannot be prepared")

type RejectionReason string
type RejectionSubreason string

const (
	RejectionNestingTooDeep    RejectionReason = "nesting_too_deep"
	RejectionClaimNotPermitted RejectionReason = "claim_not_permitted"
	RejectionUnusableIdentity  RejectionReason = "unusable_identity"
	RejectionInvalidRecord     RejectionReason = "invalid_record"
	RejectionRecordTooLarge    RejectionReason = "normalized_record_too_large"
)

const (
	RejectionInvalidMissingTimestamps RejectionSubreason = "missing_timestamps"
	RejectionInvalidMissingContent    RejectionSubreason = "missing_content"
	RejectionInvalidProhibitedContent RejectionSubreason = "prohibited_content"
	RejectionInvalidStructural        RejectionSubreason = "structural_validation"
	RejectionInvalidOther             RejectionSubreason = "other"
)

type RecordRejection struct {
	Index     int
	Reason    RejectionReason
	Subreason RejectionSubreason
}

type IngestResult struct {
	Acknowledged bool
	Accepted     int
	Rejected     []RecordRejection
}

type ProcessResult struct {
	Claimed        int
	Processed      int
	Duplicates     int
	Investigations int
	Quarantined    int
}

type Journal interface {
	AdoptClaims(string, int) ([]journal.ClaimedRecord, error)
	AppendBatch(string, []journal.Admission) error
	Claim(int, string) ([]journal.ClaimedRecord, error)
	ClaimTTL() time.Duration
	Capacity() journal.Capacity
	Manifest() journal.Manifest
	MarkCommittedBatch([]journal.CommitClaim) error
	Quarantine(string, string, journal.QuarantineReason) error
	RecoverRetained(string, int) ([]journal.RecoveredRecord, string, error)
	RenewClaims([]journal.CommitClaim) ([]journal.ClaimedRecord, error)
	Retention() time.Duration
}

// Counters are cumulative, categorical, process-lifetime counts for every
// outcome outside the normal path. A full audit-event subsystem is later work;
// these exist so none of these outcomes is silently swallowed.
type Counters struct {
	// RecoverySkipped counts retained records the rule engine could not observe
	// during startup replay. They are skipped rather than fatal.
	RecoverySkipped uint64
	// ScopeRejections counts envelopes addressed to another region.
	ScopeRejections uint64
	// JournalRejections counts batches the journal permanently refused.
	JournalRejections uint64
	// Quarantined counts records given a terminal categorical outcome.
	Quarantined uint64
	// Shed counts optional records deliberately dropped near capacity. They are
	// acknowledged, so nothing downstream would otherwise record that they
	// existed.
	Shed uint64
	// Backpressured counts batches refused because mandatory data could not be
	// persisted.
	Backpressured uint64
	// DerivedIdentity counts normalized OTLP records that used the deterministic
	// fallback because the producer supplied no native record UID. It is an
	// attempt counter: transport retries are intentionally visible.
	DerivedIdentity  uint64
	RecordRejections RecordRejectionCounters
}

type RecordRejectionCounters struct {
	NestingTooDeep           uint64
	ClaimNotPermitted        uint64
	UnusableIdentity         uint64
	RecordTooLarge           uint64
	InvalidMissingTimestamps uint64
	InvalidMissingContent    uint64
	InvalidProhibitedContent uint64
	InvalidStructural        uint64
	InvalidOther             uint64
}

func (c RecordRejectionCounters) Total() uint64 {
	return c.NestingTooDeep + c.ClaimNotPermitted + c.UnusableIdentity + c.RecordTooLarge +
		c.InvalidMissingTimestamps + c.InvalidMissingContent + c.InvalidProhibitedContent +
		c.InvalidStructural + c.InvalidOther
}

type counters struct {
	recoverySkipped                  atomic.Uint64
	scopeRejections                  atomic.Uint64
	journalRejections                atomic.Uint64
	quarantined                      atomic.Uint64
	shed                             atomic.Uint64
	backpressured                    atomic.Uint64
	derivedIdentity                  atomic.Uint64
	rejectedNestingTooDeep           atomic.Uint64
	rejectedClaimNotPermitted        atomic.Uint64
	rejectedUnusableIdentity         atomic.Uint64
	rejectedRecordTooLarge           atomic.Uint64
	rejectedInvalidMissingTimestamps atomic.Uint64
	rejectedInvalidMissingContent    atomic.Uint64
	rejectedInvalidProhibitedContent atomic.Uint64
	rejectedInvalidStructural        atomic.Uint64
	rejectedInvalidOther             atomic.Uint64
}

type Store interface {
	Boundary() persistence.Boundary
	ProcessBatch(context.Context, []persistence.ProcessInput) ([]persistence.ProcessResult, error)
}

type Config struct {
	Clock          clock.Clock
	IDs            ids.Source
	Journal        Journal
	Store          Store
	Policy         *redact.Policy
	Scope          persistence.Scope
	Classification string
	WorkerOwner    string
	RecoveryMax    int
	Limits         admission.Limits
	// Enrichment supplies deployment identity. It is optional: without it every
	// record groups under unknown-deployment with enrichment pending, which is
	// what data-model.md specifies for a deployment that is not yet known.
	//
	// It MUST NOT block. architecture.md forbids detection waiting on external
	// metadata, and this is the seam where that would happen.
	Enrichment normalize.DeploymentResolver
	// CapacityPolicy and Classifier wire operations.md's utilization table to a
	// live journal. Both are optional and both are required together: without a
	// classifier nothing can be shown to be unprotected, and shedding a record
	// that might feed a rule is the one thing the table forbids.
	//
	// With neither, nothing is ever shed. That is the safe default: an operator
	// who configured no eligible sources has chosen that nothing may be dropped.
	CapacityPolicy admission.CapacityPolicy
	Classifier     *admission.Classifier
}

type Service struct {
	clock          clock.Clock
	ids            ids.Source
	journal        Journal
	store          Store
	policy         *redact.Policy
	normalizer     *normalize.Normalizer
	engine         *incident.Engine
	scope          persistence.Scope
	classification string
	workerOwner    string
	limits         admission.Limits
	capacityPolicy admission.CapacityPolicy
	classifier     *admission.Classifier
	counters       counters

	processMu sync.Mutex
	claimed   map[string]journal.ClaimedRecord
}

// Counters returns a snapshot of the categorical outcome counts.
func (s *Service) Counters() Counters {
	if s == nil {
		return Counters{}
	}
	return Counters{
		RecoverySkipped:   s.counters.recoverySkipped.Load(),
		ScopeRejections:   s.counters.scopeRejections.Load(),
		JournalRejections: s.counters.journalRejections.Load(),
		Quarantined:       s.counters.quarantined.Load(),
		Shed:              s.counters.shed.Load(),
		Backpressured:     s.counters.backpressured.Load(),
		DerivedIdentity:   s.counters.derivedIdentity.Load(),
		RecordRejections: RecordRejectionCounters{
			NestingTooDeep:           s.counters.rejectedNestingTooDeep.Load(),
			ClaimNotPermitted:        s.counters.rejectedClaimNotPermitted.Load(),
			UnusableIdentity:         s.counters.rejectedUnusableIdentity.Load(),
			RecordTooLarge:           s.counters.rejectedRecordTooLarge.Load(),
			InvalidMissingTimestamps: s.counters.rejectedInvalidMissingTimestamps.Load(),
			InvalidMissingContent:    s.counters.rejectedInvalidMissingContent.Load(),
			InvalidProhibitedContent: s.counters.rejectedInvalidProhibitedContent.Load(),
			InvalidStructural:        s.counters.rejectedInvalidStructural.Load(),
			InvalidOther:             s.counters.rejectedInvalidOther.Load(),
		},
	}
}

func New(config Config) (*Service, error) {
	if nilInterface(config.Clock) || nilInterface(config.IDs) || nilInterface(config.Journal) ||
		nilInterface(config.Store) || config.Policy == nil || strings.TrimSpace(config.Scope.Region) == "" ||
		strings.TrimSpace(config.Scope.TenantID) == "" || !validClassification(config.Classification) ||
		config.Journal.Retention() < requiredRetention || config.Limits.Validate() != nil {
		return nil, ErrInvalidConfig
	}
	// The journal directory's manifest and the Store's boundary are both
	// immutable. A coordinator whose scope, classification, or redaction policy
	// contradicts either is misdeployed. Startup is the only place that can say
	// so, because downstream every record would instead look individually
	// invalid and be isolated by destroying its durable payload.
	manifest := config.Journal.Manifest()
	boundary := config.Store.Boundary()
	if manifest.Region != config.Scope.Region || manifest.TenantID != config.Scope.TenantID ||
		manifest.Classification != config.Classification || manifest.PolicyVersion != config.Policy.Version() ||
		boundary.Scope != config.Scope || boundary.Classification != config.Classification ||
		boundary.PolicyVersion != config.Policy.Version() {
		return nil, ErrInvalidConfig
	}
	owner := config.WorkerOwner
	if owner == "" {
		owner = DefaultWorkerOwner
	}
	if strings.TrimSpace(owner) != owner || len(owner) > journal.MaxOwnerBytes {
		return nil, ErrInvalidConfig
	}
	recoveryMax := config.RecoveryMax
	if recoveryMax == 0 {
		recoveryMax = DefaultRecoveryMax
	}
	if recoveryMax < 1 || recoveryMax > journal.MaxTransitionRecords {
		return nil, ErrInvalidConfig
	}
	engine := incident.New()
	skipped := uint64(0)
	cursor := ""
	for {
		recovered, next, err := config.Journal.RecoverRetained(cursor, recoveryMax)
		if err != nil {
			return nil, err
		}
		for _, retained := range recovered {
			// A record the engine cannot observe was still admitted and durably
			// acknowledged by design, for instance because it carries no service
			// identity. Refusing to start would make one such record permanently
			// fatal with no operator recovery short of deleting the volume, so it
			// is skipped here and terminated categorically by Process instead.
			if _, err := engine.Observe(retained.Record); err != nil {
				skipped++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	// A coordinator which restarts still owns whatever it had claimed. Adopting
	// those claims here resumes them on the first cycle instead of leaving them
	// untouchable until the claim TTL lapses, which is dead time the required
	// 30-minute database outage budget cannot spend. Adoption is bounded by the
	// journal's own transition limit, which is above the largest cohort a
	// coordinator can ever hold.
	adopted, err := config.Journal.AdoptClaims(owner, journal.MaxTransitionRecords)
	if err != nil {
		return nil, err
	}
	claimed := make(map[string]journal.ClaimedRecord, len(adopted))
	for _, claim := range adopted {
		claimed[claim.Record.RecordID] = claim
	}
	service := &Service{
		clock: config.Clock, ids: config.IDs, journal: config.Journal, store: config.Store, policy: config.Policy,
		normalizer: normalize.New(normalize.WithIDs(config.IDs), normalize.WithRedactionPolicy(config.Policy),
			normalize.WithDeploymentResolver(config.Enrichment)),
		capacityPolicy: config.CapacityPolicy, classifier: config.Classifier,
		engine: engine, scope: config.Scope, classification: config.Classification, workerOwner: owner,
		limits: config.Limits, claimed: claimed,
	}
	service.counters.recoverySkipped.Store(skipped)
	return service, nil
}

// Ingest acknowledges only after every accepted, redacted normalized record is
// synchronously appended to Pebble. Record-local failures remain partial.
func (s *Service) Ingest(ctx context.Context, envelope model.TrustedEnvelope, payload []byte, encoding admission.Encoding) (IngestResult, error) {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return IngestResult{}, ErrRequestRejected
	}
	// The envelope is authenticated but not necessarily addressed to this
	// replica. A misrouted or forged regional envelope is permanently wrong, so
	// it is rejected and counted here rather than reaching the journal, whose
	// immutable regional boundary would report it as if a retry could help.
	if envelope.Region != s.scope.Region {
		s.counters.scopeRejections.Add(1)
		return IngestResult{}, ErrScopeNotPermitted
	}
	decoded, err := admission.Decode(payload, encoding, s.limits)
	if err != nil {
		return IngestResult{}, ErrRequestRejected
	}
	normalized, err := s.normalizer.MappedRequest(envelope, decoded.Request, decoded.AcceptedOriginalIndexes)
	if err != nil {
		return IngestResult{}, ErrRequestRejected
	}
	for _, record := range normalized.Records {
		if record.IdentityQuality == model.IdentityQualityDerived {
			s.counters.derivedIdentity.Add(1)
		}
	}
	result := IngestResult{}
	for _, rejected := range decoded.Rejected {
		result.Rejected = append(result.Rejected, s.recordRejection(rejected.Index, RejectionNestingTooDeep, ""))
	}
	for _, rejected := range normalized.Rejected {
		result.Rejected = append(result.Rejected, s.recordRejection(rejected.Index,
			normalizationReason(rejected.Err), normalizationSubreason(rejected.Err)))
	}
	maximum := s.limits.MaxNormalizedBytes
	if maximum == 0 {
		maximum = admission.DefaultMaxNormalizedBytes
	}
	admissions := make([]journal.Admission, 0, len(normalized.Records))
	capacity := s.capacity()
	for i, record := range normalized.Records {
		if journal.NormalizedRecordSize(record) > maximum {
			result.Rejected = append(result.Rejected,
				s.recordRejection(normalized.RecordOriginalIndexes[i], RejectionRecordTooLarge, ""))
			continue
		}
		switch s.capacityAction(record, capacity) {
		case admission.ActionShed:
			// Deliberately dropped and acknowledged. operations.md counts these
			// because nothing downstream will ever record that they existed.
			s.counters.shed.Add(1)
			continue
		case admission.ActionBackpressure:
			// Mandatory data that cannot be persisted. Refusing the whole batch
			// retryably is the only answer that never acknowledges a silently
			// dropped mandatory record.
			s.counters.backpressured.Add(1)
			return IngestResult{}, ErrJournalUnavailable
		}
		admissions = append(admissions, journal.Admission{Record: record, Priority: priority(record.SeverityClass)})
	}
	sort.Slice(result.Rejected, func(i, j int) bool { return result.Rejected[i].Index < result.Rejected[j].Index })
	result.Accepted = len(admissions)
	if len(admissions) == 0 {
		result.Acknowledged = true
		return result, nil
	}
	if err := s.journal.AppendBatch(normalized.BatchID, admissions); err != nil {
		switch {
		case errors.Is(err, journal.ErrDuplicateConflict):
			return IngestResult{}, journal.ErrDuplicateConflict
		case errors.Is(err, journal.ErrInvalidInput):
			// The journal refuses this content for a reason no retry can change.
			// Reporting it as unavailability would make the transport replay a
			// poisoned batch forever and starve its valid siblings.
			s.counters.journalRejections.Add(1)
			return IngestResult{}, ErrJournalRejected
		default:
			return IngestResult{}, ErrJournalUnavailable
		}
	}
	result.Acknowledged = true
	return result, nil
}

// IngestRecords is the second ingestion entry point, for sources that produce
// canonical records themselves rather than a transport payload this service
// decodes.
//
// Ingest cannot serve them. It takes OTLP bytes and derives otlp:v1 identity
// from a producer-assigned record UID, so routing a CloudWatch read through it
// would discard the native event ID that cw:v1 is defined over — and that ID is
// the whole reason an overlapping replay is harmless rather than a duplicate
// occurrence.
//
// This entry point is deliberately not a way around anything Ingest enforces.
// The regional envelope check, the record-local size bound, and the rule that
// an acknowledgement means a durable journal write are all the same, and the
// journal applies the same redaction-policy and boundary validation to these
// records as to any other. What is skipped is only decoding and normalization,
// which the source has already done under this deployment's policy.
func (s *Service) IngestRecords(ctx context.Context, envelope model.TrustedEnvelope, records []model.NormalizedLog) (IngestResult, error) {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return IngestResult{}, ErrRequestRejected
	}
	if envelope.Region != s.scope.Region {
		s.counters.scopeRejections.Add(1)
		return IngestResult{}, ErrScopeNotPermitted
	}
	result := IngestResult{}
	if len(records) == 0 {
		// Nothing to write and nothing to retry. A source that read a quiet
		// window may advance its checkpoint.
		result.Acknowledged = true
		return result, nil
	}
	// The journal appends under one batch identity and refuses any record that
	// disagrees with it. Catching that here makes a caller bug a rejected
	// request instead of a whole-append failure that looks like unavailability.
	batchID := records[0].BatchID
	for _, record := range records {
		if record.BatchID != batchID {
			return IngestResult{}, ErrRequestRejected
		}
	}
	maximum := s.limits.MaxNormalizedBytes
	if maximum == 0 {
		maximum = admission.DefaultMaxNormalizedBytes
	}
	admissions := make([]journal.Admission, 0, len(records))
	for i, record := range records {
		if journal.NormalizedRecordSize(record) > maximum {
			// Record-local: no retry makes this record smaller, and failing the
			// whole call would make the source replay a batch whose one bad
			// record can never be accepted.
			result.Rejected = append(result.Rejected, s.recordRejection(i, RejectionRecordTooLarge, ""))
			continue
		}
		admissions = append(admissions, journal.Admission{Record: record, Priority: priority(record.SeverityClass)})
	}
	result.Accepted = len(admissions)
	if len(admissions) == 0 {
		result.Acknowledged = true
		return result, nil
	}
	if err := s.journal.AppendBatch(batchID, admissions); err != nil {
		switch {
		case errors.Is(err, journal.ErrDuplicateConflict):
			return IngestResult{}, journal.ErrDuplicateConflict
		case errors.Is(err, journal.ErrInvalidInput):
			s.counters.journalRejections.Add(1)
			return IngestResult{}, ErrJournalRejected
		default:
			return IngestResult{}, ErrJournalUnavailable
		}
	}
	result.Acknowledged = true
	return result, nil
}

// capacity observes the journal once per batch. Once, rather than per record,
// because the observation is a point in time either way and a batch that
// straddled a threshold would otherwise shed some of its records and not others
// for no reason an operator could explain.
func (s *Service) capacity() admission.CapacityState {
	if s.classifier == nil {
		// Shedding is opt-in. Without a classifier nothing can be shown to be
		// unprotected, and the journal's own refusal remains the backstop.
		return admission.CapacityState{CanPersist: true}
	}
	observed := s.journal.Capacity()
	return admission.CapacityState{Used: observed.Used, Capacity: observed.Total, CanPersist: observed.CanPersist}
}

// capacityAction decides what happens to one record at the observed capacity.
func (s *Service) capacityAction(record model.NormalizedLog, state admission.CapacityState) admission.Action {
	if s.classifier == nil {
		return admission.ActionAdmit
	}
	candidate := admission.Record{
		Source:      record.Source.SourceType,
		Service:     record.Service.Name,
		Environment: record.Service.Environment,
		Severity:    record.SeverityClass,
		EventType:   record.EventName,
	}
	return s.capacityPolicy.Decide(candidate, s.classifier.Classify(candidate), state).Action
}

// Process advances a bounded cohort. Database failure leaves claims available
// for retry; journal commit happens only after the CockroachDB transaction.
func (s *Service) Process(ctx context.Context, limit int) (ProcessResult, error) {
	if s == nil || ctx == nil || limit < 1 || limit > persistence.MaxProcessBatch {
		return ProcessResult{}, ErrInvalidConfig
	}
	s.processMu.Lock()
	defer s.processMu.Unlock()
	now := s.clock.Now()
	result := ProcessResult{}
	if err := s.holdClaims(now); err != nil {
		return result, err
	}
	want := limit - len(s.claimed)
	if want > 0 {
		claims, err := s.journal.Claim(want, s.workerOwner)
		if err != nil {
			return result, err
		}
		result.Claimed = len(claims)
		for _, claim := range claims {
			s.claimed[claim.Record.RecordID] = claim
			if _, err := s.engine.Observe(claim.Record); err != nil {
				// The record was admitted and acknowledged by design but cannot
				// be identified for rule grouping. No retry changes that, so it
				// terminates here retaining only safe quarantine metadata.
				if quarantineErr := s.journal.Quarantine(claim.Record.RecordID, claim.Token, journal.QuarantineUnsupportedData); quarantineErr != nil {
					return result, quarantineErr
				}
				delete(s.claimed, claim.Record.RecordID)
				result.Quarantined++
				s.counters.quarantined.Add(1)
			}
		}
	}
	ids := make([]string, 0, len(s.claimed))
	for recordID := range s.claimed {
		ids = append(ids, recordID)
	}
	sort.Strings(ids)
	// The engine compacts state past its retention horizon. A database outage
	// can outlast that horizon, and a record whose payload were released could
	// never produce a decision, so its claim would be renewed forever. Declaring
	// the in-flight cohort keeps exactly those records alive.
	s.engine.Pin(ids)
	s.engine.Advance(now)
	inputs := make([]persistence.ProcessInput, 0, len(ids))
	eligibleClaims := make([]journal.ClaimedRecord, 0, len(ids))
	for _, recordID := range ids {
		decision, ok := s.engine.PersistenceDecision(recordID)
		if !ok {
			continue
		}
		claim := s.claimed[recordID]
		input, err := s.prepareInput(claim, decision, now)
		if err != nil {
			// Preparation also fails transiently, for example when UUIDv7
			// generation cannot read entropy or the clock. Destroying a valid
			// record's payload for that would remove an incident contribution
			// the five-in-five rule still needs, so only a record which can
			// never be prepared is terminated.
			if !errors.Is(err, errRecordPoison) {
				return result, err
			}
			if quarantineErr := s.journal.Quarantine(recordID, claim.Token, journal.QuarantineUnsupportedData); quarantineErr != nil {
				return result, quarantineErr
			}
			delete(s.claimed, recordID)
			result.Quarantined++
			s.counters.quarantined.Add(1)
			continue
		}
		inputs = append(inputs, input)
		eligibleClaims = append(eligibleClaims, claim)
	}
	if len(inputs) == 0 {
		return result, nil
	}
	persisted, err := s.persistClaims(ctx, inputs, eligibleClaims)
	result.Processed += persisted.Processed
	result.Duplicates += persisted.Duplicates
	result.Investigations += persisted.Investigations
	result.Quarantined += persisted.Quarantined
	return result, err
}

// holdClaims releases claims which already lapsed and renews the ones this
// cohort is still retrying. A CockroachDB outage of the 30 minutes
// operations.md requires outlives several claim TTLs; without renewal the same
// records would lapse, be re-claimed, and have their work re-derived on every
// TTL, and each lapse is a window in which another worker takes them.
func (s *Service) holdClaims(now time.Time) error {
	// Renew while at least half the lease remains, so a worker whose cycle is
	// slower than half a TTL still never lets a claim lapse mid-retry.
	renewBefore := now.Add(s.journal.ClaimTTL() / 2)
	due := make([]journal.CommitClaim, 0, len(s.claimed))
	for recordID, claim := range s.claimed {
		switch {
		case !now.Before(claim.ExpiresAt):
			delete(s.claimed, recordID)
		case !claim.ExpiresAt.After(renewBefore):
			due = append(due, journal.CommitClaim{RecordID: recordID, Token: claim.Token})
		}
	}
	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].RecordID < due[j].RecordID })
	renewed, err := s.journal.RenewClaims(due)
	if err != nil {
		// A stale member means the journal no longer agrees this worker holds
		// that record. It says nothing about the rest of the cohort, whose
		// claims the journal is still holding for this worker, so the batch is
		// retried one record at a time rather than forfeited: a forfeited
		// sibling is not returned to pending either, so it stalls for a whole
		// claim TTL, costs an extra attempt, and loses its compaction pin for
		// that window. Any other failure is a journal problem the caller sees.
		if !errors.Is(err, journal.ErrStaleClaim) {
			return err
		}
		renewed = renewed[:0]
		for _, claim := range due {
			held, individual := s.journal.RenewClaims([]journal.CommitClaim{claim})
			if individual != nil {
				if !errors.Is(individual, journal.ErrStaleClaim) {
					return individual
				}
				// Forgetting this one locally is safe and self-correcting: it
				// returns to pending when its claim lapses and is claimed again.
				delete(s.claimed, claim.RecordID)
				continue
			}
			renewed = append(renewed, held...)
		}
	}
	for _, claim := range renewed {
		s.claimed[claim.Record.RecordID] = claim
	}
	return nil
}

// persistClaims bisects record-local poison so one malformed downstream input
// cannot quarantine valid siblings. Every other failure stops the walk and
// leaves the unprocessed journal claims intact.
func (s *Service) persistClaims(ctx context.Context, inputs []persistence.ProcessInput, claims []journal.ClaimedRecord) (ProcessResult, error) {
	processed, err := s.store.ProcessBatch(ctx, inputs)
	if err != nil {
		// Quarantine deletes the durable payload, so only record-local
		// invalidity may reach it. A store configuration mismatch is a
		// deployment fault which would otherwise destroy every acknowledged
		// record, and a generation identity or immutable-value conflict is the
		// exact evidence an operator has to investigate.
		if !errors.Is(err, persistence.ErrInvalidInput) {
			return ProcessResult{}, err
		}
		if len(inputs) == 1 {
			claim := claims[0]
			if quarantineErr := s.journal.Quarantine(claim.Record.RecordID, claim.Token, journal.QuarantineDeterministicProcessing); quarantineErr != nil {
				return ProcessResult{}, quarantineErr
			}
			delete(s.claimed, claim.Record.RecordID)
			s.counters.quarantined.Add(1)
			// A quarantined record is a reported outcome, not a batch failure,
			// whatever cohort size happened to isolate it.
			return ProcessResult{Quarantined: 1}, nil
		}
		middle := len(inputs) / 2
		left, leftErr := s.persistClaims(ctx, inputs[:middle], claims[:middle])
		if leftErr != nil {
			return left, leftErr
		}
		right, rightErr := s.persistClaims(ctx, inputs[middle:], claims[middle:])
		left.Processed += right.Processed
		left.Duplicates += right.Duplicates
		left.Investigations += right.Investigations
		left.Quarantined += right.Quarantined
		return left, rightErr
	}
	if len(processed) != len(claims) {
		// The results are indexed by input position to attribute duplicates and
		// investigations. A short or long response cannot be reconciled with the
		// claims, and committing them would lose that attribution silently.
		return ProcessResult{}, ErrInvalidStoreResult
	}
	commits := make([]journal.CommitClaim, len(claims))
	for i, claim := range claims {
		commits[i] = journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token}
	}
	if err := s.journal.MarkCommittedBatch(commits); err != nil {
		return ProcessResult{}, err
	}
	result := ProcessResult{Processed: len(claims)}
	for i, claim := range claims {
		delete(s.claimed, claim.Record.RecordID)
		if processed[i].Duplicate {
			result.Duplicates++
		}
		if processed[i].InvestigationCreated {
			result.Investigations++
		}
	}
	return result, nil
}

func (s *Service) prepareInput(claim journal.ClaimedRecord, decision incident.PersistenceDecision, now time.Time) (persistence.ProcessInput, error) {
	// CockroachDB TIMESTAMPTZ round-trips at microsecond precision. The same
	// instant is also encoded into the immutable assignment payload and checked
	// again when an outbox replica claims it, so canonicalize once before either
	// representation is built. Without this, ordinary production time.Now
	// values retain nanoseconds in JSON but lose them in the database, making a
	// valid assignment look corrupt and permanently unclaimable.
	now = now.UTC().Truncate(time.Microsecond)
	record := claim.Record
	fp, err := fingerprint.Error(record)
	if err != nil {
		// A record which cannot be fingerprinted can never join an incident
		// family. This is the only preparation failure a retry cannot fix.
		return persistence.ProcessInput{}, fmt.Errorf("%w: %v", errRecordPoison, err)
	}
	// Detection status, quiet bounds, and lateness belong to the finalized M1
	// generation, which alone can see the episode's other contributions.
	input := persistence.ProcessInput{
		Scope: s.scope, Record: record, Decision: decision, Replay: claim.Replay, ProcessedAt: now,
		FingerprintVersion: fp.Version, Fingerprint: fp.Digest, IncidentID: decision.IncidentID(),
		Generation: decision.GenerationKey(), DeploymentID: decision.DeploymentID(), EpisodeStart: decision.EpisodeStart(),
		DetectionStatus: string(decision.DetectionStatus()), Severity: string(record.SeverityClass),
		QuietAt: decision.QuietAt(), ReopenUntil: decision.ReopenUntil(),
		RuleTrigger: "ordinary_error_v1", Late: decision.Late(), EvidenceOnly: decision.EvidenceOnly(),
	}
	if decision.EvidenceOnly() || record.SeverityClass != model.SeverityClassError ||
		strings.TrimSpace(record.Service.Name) == "" || strings.TrimSpace(record.Service.Environment) == "" {
		return input, nil
	}
	investigationID, err := s.ids.New()
	if err != nil {
		return persistence.ProcessInput{}, err
	}
	messageID, err := s.ids.New()
	if err != nil {
		return persistence.ProcessInput{}, err
	}
	const candidateContextVersion = 1
	contextValue := model.SafeMap(map[string]model.SafeValue{
		"schema_version": model.SafeString("1.0"), "incident_id": model.SafeString(input.IncidentID),
		"generation": model.SafeInt(input.Generation), "record_id": model.SafeString(record.RecordID),
		"rule_id": model.SafeString("ordinary_error_v1"), "threshold": model.SafeInt(5),
		"window_seconds": model.SafeInt(300), "representative_error": record.Body,
	})
	assignment := agent.Assignment{
		SchemaVersion: agent.AssignmentSchemaVersion, MessageID: messageID, MessageType: agent.AssignmentMessageType,
		CreatedAt: now, Region: s.scope.Region, TenantID: s.scope.TenantID, Classification: s.classification,
		Producer: agent.AssignmentProducer, CorrelationID: investigationID, IncidentID: input.IncidentID,
		IncidentGeneration: input.Generation, InvestigationID: investigationID, ServiceID: record.Service.Name,
		Environment: record.Service.Environment, Severity: input.Severity, ContextVersion: candidateContextVersion,
	}
	body, err := agent.EncodeAssignment(assignment)
	if err != nil {
		return persistence.ProcessInput{}, err
	}
	input.InvestigationCandidate = &persistence.InvestigationInput{
		InvestigationID: investigationID, TriggerReason: "five_in_five", ContextVersion: candidateContextVersion,
		Context: contextValue, Classification: s.classification, Outbox: queue.Message{
			MessageID: messageID, DeduplicationKey: "assignment:" + investigationID, Type: agent.AssignmentMessageType,
			Body: body, Attributes: map[string]string{"region": s.scope.Region},
		},
	}
	return input, nil
}

func normalizationReason(err error) RejectionReason {
	switch {
	case errors.Is(err, normalize.ErrClaimNotPermitted):
		return RejectionClaimNotPermitted
	case errors.Is(err, normalize.ErrUnusableIdentity):
		return RejectionUnusableIdentity
	default:
		return RejectionInvalidRecord
	}
}

func normalizationSubreason(err error) RejectionSubreason {
	switch {
	case errors.Is(err, normalize.ErrMissingTimestamps):
		return RejectionInvalidMissingTimestamps
	case errors.Is(err, normalize.ErrMissingContent):
		return RejectionInvalidMissingContent
	case errors.Is(err, normalize.ErrProhibitedContent):
		return RejectionInvalidProhibitedContent
	case errors.Is(err, normalize.ErrStructuralRecord):
		return RejectionInvalidStructural
	default:
		if normalizationReason(err) == RejectionInvalidRecord {
			return RejectionInvalidOther
		}
		return ""
	}
}

func (s *Service) recordRejection(index int, reason RejectionReason, subreason RejectionSubreason) RecordRejection {
	switch reason {
	case RejectionNestingTooDeep:
		s.counters.rejectedNestingTooDeep.Add(1)
	case RejectionClaimNotPermitted:
		s.counters.rejectedClaimNotPermitted.Add(1)
	case RejectionUnusableIdentity:
		s.counters.rejectedUnusableIdentity.Add(1)
	case RejectionRecordTooLarge:
		s.counters.rejectedRecordTooLarge.Add(1)
	case RejectionInvalidRecord:
		switch subreason {
		case RejectionInvalidMissingTimestamps:
			s.counters.rejectedInvalidMissingTimestamps.Add(1)
		case RejectionInvalidMissingContent:
			s.counters.rejectedInvalidMissingContent.Add(1)
		case RejectionInvalidProhibitedContent:
			s.counters.rejectedInvalidProhibitedContent.Add(1)
		case RejectionInvalidStructural:
			s.counters.rejectedInvalidStructural.Add(1)
		default:
			s.counters.rejectedInvalidOther.Add(1)
			subreason = RejectionInvalidOther
		}
	}
	return RecordRejection{Index: index, Reason: reason, Subreason: subreason}
}

func priority(severity model.SeverityClass) journal.Priority {
	switch severity {
	case model.SeverityClassFatal:
		return journal.PriorityCritical
	case model.SeverityClassError:
		return journal.PriorityHigh
	case model.SeverityClassWarn:
		return journal.PriorityNormal
	default:
		return journal.PriorityLow
	}
}

func validClassification(value string) bool {
	switch value {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	default:
		return false
	}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
