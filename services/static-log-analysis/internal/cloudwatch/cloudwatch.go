// Package cloudwatch is the pull-based, regional Amazon CloudWatch Logs source
// adapter.
//
// It owns source authentication, cursors, native identity extraction, and
// transport retries. It does not own rules, grouping, incident lifecycle, or
// redaction policy: it applies the policy it is given, and everything it emits
// has already been through it.
//
// The load-bearing property of this package is stated in source-adapters.md:
//
//	A checkpoint update and journal acknowledgement cannot be one transaction
//	across CloudWatch and local storage. Recovery therefore intentionally
//	rereads an overlap; stable record IDs make the replay harmless.
//
// So the ordering is fixed. Events are read, redacted, mapped, and handed to
// the ingestion sink; only once the sink reports a durable journal write does
// the checkpoint advance. A crash in between loses the checkpoint, not the
// records, and the next cycle rereads an overlap whose events hash to the
// identifiers they already had.
package cloudwatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

// Group is one CloudWatch log group inside one account and region.
//
// Account and Region are the adapter's own authenticated boundary. They are
// never read back from an API answer, because an identifier built from an
// unauthenticated locator is forgeable by whatever produced the locator.
type Group struct {
	Account  string
	Region   string
	LogGroup string
}

// Stream is one log stream inside a group. A checkpoint is kept per stream.
type Stream struct {
	Group Group
	Name  string
}

// Event is one native CloudWatch log event exactly as the source reported it.
// Message is raw and unredacted; nothing in this type may be persisted or
// logged before it has been through the redaction policy.
type Event struct {
	// StreamName is the log stream the event was written to. CloudWatch reports
	// it per event, because one group-level read spans many streams.
	StreamName string
	// EventID is the native CloudWatch event identifier. It is the whole reason
	// replay is harmless, so an event without one cannot be admitted: content
	// hashing is prohibited because two legitimate identical messages occur.
	EventID string
	// Timestamp is the producer's event time.
	Timestamp time.Time
	// IngestionTime is when CloudWatch accepted the event, which is the
	// adapter's observed time. The gap between the two is why a bounded
	// lookback overlap exists.
	IngestionTime time.Time
	Message       string
}

// Query asks the source for one page of events from one log group.
//
// Start is inclusive and End exclusive, both in event time. CloudWatch filters
// on event time rather than ingestion time, which is precisely why an event
// written late can appear behind a position the adapter has already passed.
type Query struct {
	LogGroup  string
	Start     time.Time
	End       time.Time
	NextToken string
	Limit     int
}

// Page is one page of events.
//
// Region and LogGroup are echoed so the adapter can check that the answer
// belongs to the boundary it asked about. Without the echo, a misconfigured
// endpoint would have another region's events mapped under this region's
// account, service identity, and record identity.
type Page struct {
	Region    string
	LogGroup  string
	Events    []Event
	NextToken string
}

// LogsAPI is the narrow source API this adapter needs. The AWS SDK
// implementation lives in the cwaws subpackage; nothing here imports it.
type LogsAPI interface {
	// Region reports the region the client's endpoint and credentials are bound
	// to. The adapter refuses to read from a client outside its own region.
	Region() string
	// FilterEvents returns one page. An empty page with no next token is a
	// valid answer meaning "nothing new", and MUST NOT be reported as an error.
	FilterEvents(ctx context.Context, query Query) (Page, error)
}

// Ack is the ingestion service's answer.
type Ack struct {
	// Acknowledged is true only when every accepted record reached the durable
	// analysis journal. An adapter may not declare delivery complete, or move a
	// checkpoint, until it is.
	Acknowledged bool
	Accepted     int
	// Rejected counts records ingestion refused permanently and record-locally.
	// They never reach the journal and never will, so the checkpoint advances
	// past them rather than replaying them forever.
	Rejected int
}

// Sink is the ingestion boundary the adapter delivers to.
//
// It is deliberately not internal/pipeline.Service.Ingest: that entry point
// takes OTLP bytes and derives otlp:v1 identity from a producer-assigned record
// UID, which would discard the native CloudWatch event ID that identity
// version cw:v1 is defined over. See
// docs/static-log-analysis/implementation/cloudwatch-source.md.
type Sink interface {
	IngestRecords(ctx context.Context, envelope model.TrustedEnvelope, records []model.NormalizedLog) (Ack, error)
}

var (
	// ErrThrottled marks a source API refusal a later attempt can succeed at.
	// An implementation of LogsAPI must wrap AWS throttling in it, because a
	// permanent failure reported as retryable replays forever and a retryable
	// failure reported as permanent discards work a retry would have saved.
	ErrThrottled = errors.New("cloudwatch: source api throttled")
	// ErrSourceUnavailable marks any other source API failure. It is strictly
	// distinct from a valid empty result: an empty result advances nothing and
	// is not an error at all.
	ErrSourceUnavailable = errors.New("cloudwatch: source api unavailable")
	// ErrRegionalBoundary means the adapter was asked to read, or was answered
	// with, something outside its configured account, region, or log group.
	// It is never record-local: quarantine deletes durable payloads, and a
	// misconfigured endpoint would destroy every record for a deployment typo.
	ErrRegionalBoundary = errors.New("cloudwatch: regional boundary would be crossed")
	// ErrInvalidConfig means the adapter cannot be built as configured.
	ErrInvalidConfig = errors.New("cloudwatch: invalid configuration")
	// ErrUnusableRecord is a record-local refusal: this one event cannot be
	// turned into a canonical record, and its siblings are unaffected.
	ErrUnusableRecord = errors.New("cloudwatch: event cannot be mapped to a record")
	// ErrNotAcknowledged means the ingestion sink did not report a durable
	// journal write, so no checkpoint may advance.
	ErrNotAcknowledged = errors.New("cloudwatch: ingestion did not acknowledge a durable write")
)

// Locator renders the region-local ARN of a stream. It contains no credentials
// and is safe to store as a regional log reference.
func (s Stream) Locator() string {
	return "arn:aws:logs:" + s.Group.Region + ":" + s.Group.Account +
		":log-group:" + s.Group.LogGroup + ":log-stream:" + s.Name
}

func (g Group) valid() bool {
	return trimmed(g.Account) && trimmed(g.Region) && trimmed(g.LogGroup)
}

func trimmed(value string) bool {
	return strings.TrimSpace(value) != "" && strings.TrimSpace(value) == value
}
