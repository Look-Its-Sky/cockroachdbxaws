// Package cwgen produces deterministic Amazon CloudWatch Logs events.
//
// Real CloudWatch cannot be asked for an event that arrives ninety seconds
// late, for the same event on two overlapping pages, or for a page boundary in
// a chosen place. Those are the cases the adapter's checkpoint, overlap, and
// identity behaviour are decided by, so the harness generates them directly.
//
// Everything is derived from an injected clock and a counter, so the same
// scenario produces identical events on every run.
package cwgen

import (
	"fmt"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

// Defaults for the payment failure scenario the vertical slice is built around,
// expressed as the CloudWatch deployment of the same service.
const (
	DefaultAccount     = "123456789012"
	DefaultRegion      = "us-east-1"
	DefaultLogGroup    = "/aws/ecs/paymentservice"
	DefaultLogStream   = "ecs/paymentservice/0e1f2a3b"
	DefaultService     = "paymentservice"
	DefaultEnvironment = "production"
)

// eventIDWidth is the width of a native CloudWatch event ID, which is a large
// decimal number rendered as a fixed-width string.
const eventIDWidth = 56

// Producer builds CloudWatch events.
type Producer struct {
	clock  clock.Clock
	stream string
	// counter makes every event ID distinct without randomness, so a scenario
	// that asks for a duplicate gets one deliberately rather than by accident.
	counter uint64
	// idBase keeps generated identifiers in the shape CloudWatch uses.
	idBase uint64
}

// Option configures a Producer.
type Option func(*Producer)

// WithClock supplies the clock ingestion times are read from.
func WithClock(c clock.Clock) Option { return func(p *Producer) { p.clock = c } }

// ForStream sets the log stream every event from this producer is written to.
func ForStream(name string) Option { return func(p *Producer) { p.stream = name } }

// WithIDBase distinguishes two producers whose events must not collide.
func WithIDBase(base uint64) Option { return func(p *Producer) { p.idBase = base } }

// New returns a producer. Without options it uses a fake clock at
// fakeclock.Origin and the default stream.
func New(opts ...Option) *Producer {
	p := &Producer{clock: fakeclock.NewAtOrigin(), stream: DefaultLogStream, idBase: 1}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Clock returns the clock the producer reads times from.
func (p *Producer) Clock() clock.Clock { return p.clock }

type eventSettings struct {
	stream        string
	eventID       string
	explicitID    bool
	message       string
	timestamp     time.Time
	ingestion     time.Time
	omitEventTime bool
	omitIngestion bool
}

// EventOption adjusts an event under construction.
type EventOption func(*eventSettings)

// Event returns one CloudWatch log event.
func (p *Producer) Event(opts ...EventOption) cloudwatch.Event {
	now := p.clock.Now()
	settings := &eventSettings{
		stream: p.stream,
		// CloudWatch stamps ingestion time on receipt; a producer stamps event
		// time slightly earlier. The two-second lag keeps the event inside the
		// range where its own timestamp is used as given.
		timestamp: now.Add(-2 * time.Second),
		ingestion: now,
		message:   "charge failed for order",
	}
	for _, opt := range opts {
		opt(settings)
	}
	if !settings.explicitID {
		p.counter++
		settings.eventID = EventID(p.idBase*1_000_000 + p.counter)
	}
	event := cloudwatch.Event{
		StreamName:    settings.stream,
		EventID:       settings.eventID,
		Timestamp:     settings.timestamp,
		IngestionTime: settings.ingestion,
		Message:       settings.message,
	}
	if settings.omitEventTime {
		event.Timestamp = time.Time{}
	}
	if settings.omitIngestion {
		event.IngestionTime = time.Time{}
	}
	return event
}

// PaymentError returns the representative structured payment failure: a JSON
// message with a declared level, which is the only way a CloudWatch event
// carries a severity at all.
func (p *Producer) PaymentError(opts ...EventOption) cloudwatch.Event {
	base := []EventOption{WithMessage(
		`{"level":"error","msg":"charge failed for order 4711: card declined",` +
			`"payment.provider":"acme","http.route":"/api/payments/{id}"}`)}
	return p.Event(append(base, opts...)...)
}

// WithMessage sets the raw message.
func WithMessage(message string) EventOption {
	return func(s *eventSettings) { s.message = message }
}

// WithLevel returns a structured message carrying a declared level.
func WithLevel(level, message string) EventOption {
	return WithMessage(fmt.Sprintf(`{"level":%q,"msg":%q}`, level, message))
}

// WithStream sets the log stream, which is how a scenario spreads events over
// several streams and therefore several checkpoints.
func WithStream(name string) EventOption {
	return func(s *eventSettings) { s.stream = name }
}

// WithEventID sets the native event ID explicitly, which is how the same event
// is made to appear on two overlapping pages.
func WithEventID(id string) EventOption {
	return func(s *eventSettings) {
		s.eventID = id
		s.explicitID = true
	}
}

// AtEventTime sets the producer's event time.
func AtEventTime(t time.Time) EventOption {
	return func(s *eventSettings) { s.timestamp = t }
}

// AtIngestionTime sets the time CloudWatch accepted the event.
func AtIngestionTime(t time.Time) EventOption {
	return func(s *eventSettings) { s.ingestion = t }
}

// LateBy makes an event arrive d after it happened, which is the case a bounded
// lookback overlap exists to recover.
func LateBy(d time.Duration) EventOption {
	return func(s *eventSettings) { s.timestamp = s.ingestion.Add(-d) }
}

// WithoutEventTime removes the producer's event time.
func WithoutEventTime() EventOption {
	return func(s *eventSettings) { s.omitEventTime = true }
}

// WithoutIngestionTime removes the ingestion time.
func WithoutIngestionTime() EventOption {
	return func(s *eventSettings) { s.omitIngestion = true }
}

// EventID renders n in the shape of a native CloudWatch event identifier.
func EventID(n uint64) string {
	rendered := fmt.Sprintf("%d", n)
	if len(rendered) >= eventIDWidth {
		return rendered
	}
	return strings.Repeat("3", eventIDWidth-len(rendered)) + rendered
}

// Sequence returns count events spaced apart by gap, starting at the producer's
// current clock reading.
func (p *Producer) Sequence(count int, gap time.Duration, opts ...EventOption) []cloudwatch.Event {
	start := p.clock.Now()
	events := make([]cloudwatch.Event, 0, count)
	for i := 0; i < count; i++ {
		at := start.Add(time.Duration(i) * gap)
		events = append(events, p.PaymentError(append([]EventOption{
			AtIngestionTime(at), AtEventTime(at.Add(-time.Second)),
		}, opts...)...))
	}
	return events
}

// Duplicate returns the same event again, as an overlapping page or an
// uncommitted checkpoint causes it to be read a second time.
func Duplicate(event cloudwatch.Event) cloudwatch.Event { return event }

// AwkwardMessages returns messages whose handling must be defined rather than
// accidental. Every one of them must either map to a valid, safe record or be
// refused record-locally.
func AwkwardMessages() []string {
	return []string{
		"",
		" ",
		"charge failed",
		`{"level":"error"}`,
		`{"level":42,"msg":"numeric level"}`,
		`{"level":null,"msg":"null level"}`,
		`{"level":{"nested":"error"},"msg":"structured level"}`,
		`{"":"empty key"}`,
		`{"level":"error","level":"info"}`,
		`[1,2,3]`,
		`"just a json string"`,
		`{"msg":"unterminated`,
		"charge failed \xff\xfe",
		"authorization: Bearer abcdefghijklmnop",
		"password=hunter2",
		"visit https://user:secret@example.com/path?token=abc",
		strings.Repeat("p", 200_000),
		"line one\nline two\nline three",
		"‮evil",
		`{"level":"ERROR","msg":"upper case level"}`,
		`{"severity_text":"warn","msg":"severity text field"}`,
	}
}
