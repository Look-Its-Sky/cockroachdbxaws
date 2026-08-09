// Package persistence owns the explicit CockroachDB boundary for incidents,
// investigations, immutable contexts, and the transactional outbox.
package persistence

import (
	"errors"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

const (
	MaxProcessBatch    = 100
	MaxClaimBatch      = 100
	MaxScopeBytes      = 128
	MaxOwnerBytes      = 128
	MaxProjectionBytes = 256 << 10
	MaxContextBytes    = 256 << 10
)

var (
	ErrInvalidInput = errors.New("persistence: invalid input")
	// ErrConfiguration means this Store cannot serve the batch at all: the
	// scope, redaction-policy version, or classification boundary disagrees with
	// the one it was constructed for. It is deliberately distinct from
	// ErrInvalidInput so a caller never mistakes a deployment error for a
	// malformed record and destroys durable data trying to isolate it.
	ErrConfiguration      = errors.New("persistence: store configuration mismatch")
	ErrUnavailable        = errors.New("persistence: unavailable")
	ErrIncompatibleSchema = errors.New("persistence: incompatible schema")
	ErrStaleClaim         = errors.New("persistence: stale claim")
	ErrImmutable          = errors.New("persistence: immutable value exists")
	ErrIdentityConflict   = errors.New("persistence: generation identity conflict")
)

type Topology string

const TopologySingleRegion Topology = "single-region"

type RecordValidator interface {
	ValidateRecord(model.NormalizedLog) error
	ValidateValue(model.SafeValue) error
	ValidateText(string) error
	Version() string
}

type Config struct {
	Validator      RecordValidator
	Scope          Scope
	Classification string
	Topology       Topology
}

type Scope struct {
	Region   string
	TenantID string
}

// Boundary is the immutable configuration a Store is bound to. It is the
// database-side counterpart of the journal's directory manifest.
type Boundary struct {
	Scope          Scope
	Classification string
	PolicyVersion  string
}

type EvidenceInput struct {
	EvidenceID     string
	Version        int64
	Classification string
	Provenance     string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type InvestigationInput struct {
	InvestigationID string
	TriggerReason   string
	ContextVersion  int64
	Context         model.SafeValue
	Classification  string
	Outbox          queue.Message
}

type ContextInput struct {
	Version        int64
	Snapshot       model.SafeValue
	Classification string
	CreatedAt      time.Time
}

// ProcessInput is fully prepared before entering the retryable transaction.
// In particular, all IDs, timestamps, context snapshots, and outbox bytes are
// caller supplied and are therefore reused if CockroachDB retries the callback.
type ProcessInput struct {
	Scope  Scope
	Record model.NormalizedLog
	// Decision is emitted by M1 only when this record's topology is immutable.
	// Provisional journal work has no decision and is not eligible for Process.
	Decision incident.PersistenceDecision
	Replay   journal.ReplayIdentity

	ProcessedAt        time.Time
	FingerprintVersion string
	Fingerprint        string
	IncidentID         string
	Generation         int64
	DeploymentID       string
	EpisodeStart       time.Time
	DetectionStatus    string
	Severity           string
	QuietAt            time.Time
	ReopenUntil        time.Time
	RuleTrigger        string
	ContextVersion     int64
	Late               bool
	EvidenceOnly       bool

	Evidence      *EvidenceInput
	Investigation *InvestigationInput
	// InvestigationCandidate carries fully deterministic assignment material
	// for the transactional ordinary-error policy. CockroachDB elects it only
	// when the persisted event-time window first reaches five occurrences.
	InvestigationCandidate *InvestigationInput
	ContextUpdate          *ContextInput
}

type ProcessResult struct {
	RecordID             string
	Duplicate            bool
	IncidentID           string
	Generation           int64
	InvestigationID      string
	InvestigationCreated bool
}

type IncidentSnapshot struct {
	Scope                 Scope
	IncidentID            string
	LatestGeneration      int64
	OccurrenceCount       int64
	ActiveInvestigationID string
	ContextVersion        int64
}

const OccurrenceProjectionSchemaVersion = "1.0"

// OccurrenceProjection is the bounded, safe, versioned record context retained
// after the journal compacts. Transport attempt and credential fields are
// deliberately absent; deterministic diagnostic context remains.
type OccurrenceProjection struct {
	SchemaVersion      string                     `json:"schema_version"`
	Region             string                     `json:"region"`
	EventTime          time.Time                  `json:"event_time"`
	ObservedTime       time.Time                  `json:"observed_time"`
	TimestampInferred  bool                       `json:"timestamp_inferred"`
	TimestampReason    string                     `json:"timestamp_inference_reason,omitempty"`
	SeverityNumber     int32                      `json:"severity_number"`
	SeverityText       string                     `json:"severity_text,omitempty"`
	SeverityClass      model.SeverityClass        `json:"severity_class"`
	Body               model.SafeValue            `json:"body,omitempty"`
	EventName          string                     `json:"event_name,omitempty"`
	Attributes         map[string]model.SafeValue `json:"attributes,omitempty"`
	ResourceAttributes map[string]model.SafeValue `json:"resource_attributes,omitempty"`
	ScopeAttributes    map[string]model.SafeValue `json:"scope_attributes,omitempty"`
	Service            model.ServiceIdentity      `json:"service"`
	Deployment         model.DeploymentIdentity   `json:"deployment"`
	Correlation        model.CorrelationIdentity  `json:"correlation"`
	Exception          *model.NormalizedException `json:"exception,omitempty"`
	Redaction          model.RedactionMetadata    `json:"redaction"`
}

type OccurrenceSnapshot struct {
	Scope        Scope
	RecordID     string
	IncidentID   string
	Generation   int64
	Late         bool
	EvidenceOnly bool
	Projection   OccurrenceProjection
}

type Lease struct {
	Scope           Scope
	IncidentID      string
	InvestigationID string
	Owner           string
	Token           string
	ExpiresAt       time.Time
	RenewalSequence int64
}

type OutboxClaim struct {
	Scope     Scope
	Message   queue.Message
	Token     string
	Owner     string
	Attempt   int64
	ExpiresAt time.Time
}

type OutboxFailure string

const (
	OutboxFailureQueueUnavailable OutboxFailure = "queue_unavailable"
	OutboxFailureTimeout          OutboxFailure = "timeout"
	OutboxFailureLostAck          OutboxFailure = "lost_ack"
	OutboxFailureRejected         OutboxFailure = "rejected"
)

type InvestigationContext struct {
	Scope           Scope
	InvestigationID string
	Version         int64
	Snapshot        model.SafeValue
	Classification  string
	PolicyVersion   string
	CreatedAt       time.Time
}
