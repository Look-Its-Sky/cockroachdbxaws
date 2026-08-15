package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
)

// Defaults for a publisher nobody has tuned. They are deliberately unheroic:
// the outbox exists so that a queue outage costs latency rather than data, and a
// publisher that hammered an unavailable queue would turn one outage into two.
const (
	DefaultBatch          = 25
	DefaultClaimTTL       = 30 * time.Second
	DefaultPublishTimeout = 10 * time.Second
	DefaultBackoffMin     = time.Second
	DefaultBackoffMax     = 5 * time.Minute
	// DefaultMaxAttempts bounds how long a message that keeps failing keeps its
	// place in front of its siblings. With the default backoff it is roughly
	// twenty minutes of trying before the dead-letter queue.
	DefaultMaxAttempts = 8
)

var ErrInvalidConfig = errors.New("outbox: invalid configuration")

// Store is the durable outbox boundary. *persistence.Store satisfies it; the
// interface is declared here because this is the package that consumes it.
//
// Claim ordering, fencing tokens, claim expiry, and the content digest are the
// store's contract, not this package's. The publisher's job is to choose which
// of these three transitions each transport outcome earns.
type Store interface {
	ClaimOutbox(ctx context.Context, scope persistence.Scope, owner string, tokens []string, now time.Time, ttl time.Duration) ([]persistence.OutboxClaim, error)
	MarkOutboxPublished(ctx context.Context, claim persistence.OutboxClaim, publishedAt time.Time) error
	RetryOutbox(ctx context.Context, claim persistence.OutboxClaim, failure persistence.OutboxFailure, now, nextAttemptAt time.Time) error
}

// Config is everything a publisher needs. Nothing here reads wall-clock time or
// entropy on its own: Clock, IDs, and Jitter are injected so a test drives the
// backoff schedule at its exact instants.
type Config struct {
	Scope     persistence.Scope
	Owner     string
	Store     Store
	Transport Transport
	Clock     clock.Clock
	IDs       ids.Source
	Logger    *slog.Logger

	// Batch bounds how many messages one cycle claims. It also bounds how long
	// a cycle can hold the shutdown budget open.
	Batch int
	// ClaimTTL is how long a claim stays this publisher's. A publisher that
	// dies mid-cycle costs this much latency and nothing else.
	ClaimTTL time.Duration
	// PublishTimeout bounds one delivery attempt, so a queue that accepts a
	// connection and then says nothing cannot hold a cycle open forever.
	PublishTimeout time.Duration

	BackoffMin, BackoffMax time.Duration
	// MaxAttempts is the attempt at which a message that has never succeeded is
	// dead-lettered instead of retried again. Zero uses DefaultMaxAttempts.
	MaxAttempts int64
	// Jitter spreads the retry schedule so that a queue recovering from an
	// outage is not met by every publisher's whole backlog at the same instant.
	// It receives the computed backoff and returns the extra delay to add.
	Jitter func(time.Duration) time.Duration
}

// Result counts one cycle's outcomes. Every claimed message lands in exactly
// one of the four terminal counts unless the cycle was cut short.
type Result struct {
	Claimed      int
	Published    int
	DeadLettered int
	Retried      int
	// Unrecorded counts messages the queue acknowledged but whose outbox row
	// could not be updated. They are not lost: the claim lapses and the message
	// is published again under the same identity, which is exactly the case
	// ADR 0004 says consumers deduplicate.
	Unrecorded int
}

// Publisher drains the outbox onto the regional agent queue.
type Publisher struct {
	config Config
}

func New(config Config) (*Publisher, error) {
	if config.Store == nil || config.Transport == nil || config.Clock == nil || config.IDs == nil {
		return nil, fmt.Errorf("%w: a publisher needs a store, a transport, a clock, and an identifier source", ErrInvalidConfig)
	}
	if config.Scope.Region == "" || config.Scope.TenantID == "" || config.Owner == "" {
		return nil, fmt.Errorf("%w: -region, -tenant-id, and an owner identify which outbox this publisher drains", ErrInvalidConfig)
	}
	// security.md and architecture.md: logs, evidence, and correlatable
	// identifiers do not cross regions. A publisher wired to another region's
	// queue would move every assignment across that boundary, so it is refused
	// here rather than once per message.
	if config.Transport.Region() != config.Scope.Region {
		return nil, fmt.Errorf("%w: this replica serves region %q but its queue is in region %q; assignments must not cross a regional boundary",
			ErrInvalidConfig, config.Scope.Region, config.Transport.Region())
	}
	if config.Batch < 1 || config.Batch > persistence.MaxClaimBatch {
		return nil, fmt.Errorf("%w: batch size must be between 1 and %d", ErrInvalidConfig, persistence.MaxClaimBatch)
	}
	if config.ClaimTTL <= 0 || config.PublishTimeout <= 0 {
		return nil, fmt.Errorf("%w: claim lease and publish timeout must be positive", ErrInvalidConfig)
	}
	if config.BackoffMin <= 0 || config.BackoffMax < config.BackoffMin {
		return nil, fmt.Errorf("%w: backoff minimum must be positive and no greater than the maximum", ErrInvalidConfig)
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = DefaultMaxAttempts
	}
	if config.MaxAttempts < 1 {
		return nil, fmt.Errorf("%w: a message must be allowed at least one attempt", ErrInvalidConfig)
	}
	if config.Jitter == nil {
		config.Jitter = fullJitter
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	return &Publisher{config: config}, nil
}

// PublishCycle claims a bounded batch, delivers each message, and records what
// happened to it. It never publishes inside a database transaction, which is
// what makes a lost acknowledgement a duplicate rather than a lost message.
//
// A returned error is the cycle's error, not a message's: a message that fails
// permanently is dead-lettered and counted, and the cycle continues, so one
// poisoned message cannot starve the siblings behind it.
func (p *Publisher) PublishCycle(ctx context.Context) (Result, error) {
	var result Result
	now := p.config.Clock.Now()
	// Identifiers are generated before the claim transaction and reused on every
	// retry inside it, as the database contract requires.
	tokens, err := p.tokens()
	if err != nil {
		return result, err
	}
	claims, err := p.config.Store.ClaimOutbox(ctx, p.config.Scope, p.config.Owner, tokens, now, p.config.ClaimTTL)
	if err != nil {
		return result, fmt.Errorf("outbox: claim: %w", err)
	}
	result.Claimed = len(claims)
	for i, claim := range claims {
		if ctx.Err() != nil {
			// The shutdown budget is spent. The remaining claims are left to
			// lapse rather than published under a context that is about to be
			// torn down: a claim that lapses costs ClaimTTL of latency, and a
			// publish attempt cut off midway costs an unrecorded delivery.
			p.config.Logger.Info("outbox cycle stopped before it finished",
				slog.Int("delivered", i), slog.Int("claimed", len(claims)))
			return result, nil
		}
		if !p.leaseCovers(claim) {
			// The lease would expire while this message was in flight, so the
			// row could be claimed by a successor and this publisher's mark
			// would be refused. Releasing now turns a certain duplicate into an
			// immediate reclaim.
			p.releaseNow(ctx, claims[i:], ReasonTimeout)
			result.Retried += len(claims) - i
			return result, nil
		}
		outcome, err := p.deliver(ctx, claim, &result)
		if err != nil {
			// Only a deployment fault ends the cycle. Every remaining claim is
			// released unattempted so a sibling publisher, or this one after an
			// operator fixes the deployment, finds them pending rather than
			// waiting out the lease.
			p.releaseNow(ctx, claims[i+1:], outcome)
			result.Retried += len(claims) - i - 1
			return result, err
		}
	}
	return result, nil
}

// deliver performs one message's whole lifecycle: the regional check, the
// publish attempt, and the durable transition that attempt earns.
func (p *Publisher) deliver(ctx context.Context, claim persistence.OutboxClaim, result *Result) (Reason, error) {
	err := p.checkBoundary(claim)
	if err == nil {
		publishCtx, cancel := context.WithTimeout(ctx, p.config.PublishTimeout)
		err = p.config.Transport.Publish(publishCtx, claim.Message)
		cancel()
		if err == nil {
			p.markPublished(ctx, claim, result)
			return "", nil
		}
	}
	class, reason := classify(err)
	switch {
	case errors.Is(class, ErrDeploymentFault):
		// Released immediately, like the claims behind it: nothing about this
		// message failed, and how long to wait before trying the queue again is
		// the cycle's decision, not this message's.
		p.retryAfter(ctx, claim, reason, 0)
		result.Retried++
		return reason, fmt.Errorf("outbox: %w: %v", ErrDeploymentFault, err)
	case errors.Is(class, ErrMessageRejected):
		p.deadLetter(ctx, claim, reason, result)
		return reason, nil
	case claim.Attempt >= p.config.MaxAttempts:
		// The message has had its whole schedule and has failed on every
		// delivery. Keeping it would let one message hold a place in front of
		// its siblings forever.
		//
		// The count is only consulted on a failed delivery, never before one.
		// A claim released unattempted has still incremented the row's attempt
		// counter, so escalating on the counter alone would let a repeated
		// deployment fault drain a whole region's committed assignments into
		// the dead-letter queue without any of them ever being offered.
		p.deadLetter(ctx, claim, ReasonAttemptsExhausted, result)
		return ReasonAttemptsExhausted, nil
	default:
		p.config.Logger.Warn("outbox publish failed and will be retried",
			slog.String("message_id", claim.Message.MessageID), slog.String("reason", string(reason)),
			slog.Int64("attempt", claim.Attempt))
		p.retry(ctx, claim, reason)
		result.Retried++
		return reason, nil
	}
}

// checkBoundary refuses to publish anything that does not belong in this
// publisher's region.
//
// A mismatch is categorical rather than a silent skip, and it is a deployment
// fault rather than a dead-letter case: dead-lettering it would move the very
// content the boundary exists to contain into a queue that may be in the wrong
// region too. The store already refuses to claim a row whose payload disagrees
// with its scope, so reaching this check means the store, the queue, or the
// flags disagree with each other, which is not something a message can fix.
func (p *Publisher) checkBoundary(claim persistence.OutboxClaim) error {
	region, tenant := p.config.Scope.Region, p.config.Scope.TenantID
	crossRegion := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{NewFailure(ErrDeploymentFault, ReasonRegionMismatch, "")}, args...)...)
	}
	if claim.Scope.Region != region || claim.Scope.TenantID != tenant {
		return crossRegion("message %s is scoped to region %q and tenant %q, this publisher serves %q and %q",
			claim.Message.MessageID, claim.Scope.Region, claim.Scope.TenantID, region, tenant)
	}
	if claim.Message.Attributes["region"] != region {
		return crossRegion("message %s routes to region %q, this publisher serves %q",
			claim.Message.MessageID, claim.Message.Attributes["region"], region)
	}
	if claim.Message.Type == agent.AssignmentMessageType {
		assignment, err := agent.DecodeAssignment(claim.Message.Body)
		if err != nil {
			// A body the typed catalogue refuses can never be published by this
			// service. The store validates the same thing before it hands out a
			// claim, so this is the second of two locks on the same door.
			return NewFailure(ErrMessageRejected, ReasonRejected, "invalid_assignment")
		}
		if assignment.Region != region || assignment.TenantID != tenant {
			return crossRegion("assignment %s carries region %q and tenant %q, this publisher serves %q and %q",
				claim.Message.MessageID, assignment.Region, assignment.TenantID, region, tenant)
		}
	}
	return nil
}

// markPublished records a delivery. A failure here does not lose the message:
// the claim lapses and the message is published again under the same message
// identity and deduplication key, which is the duplicate ADR 0004 says consumers
// absorb. It is never converted into a retry or a dead letter, because the queue
// has already accepted the message and both of those would misreport that.
func (p *Publisher) markPublished(ctx context.Context, claim persistence.OutboxClaim, result *Result) {
	if err := p.config.Store.MarkOutboxPublished(ctx, claim, p.config.Clock.Now()); err != nil {
		result.Unrecorded++
		p.config.Logger.Error("outbox message was published but not recorded; it will be published again",
			slog.String("message_id", claim.Message.MessageID), slog.String("error", err.Error()))
		return
	}
	result.Published++
}

// deadLetter moves a message that can never be published out of the way. The
// body is unchanged, so the dead-letter queue holds exactly what was committed,
// and the categorical reason travels with it.
//
// A dead-letter send that itself fails is treated as retryable. The alternative
// is to mark a message published that nothing ever received.
func (p *Publisher) deadLetter(ctx context.Context, claim persistence.OutboxClaim, reason Reason, result *Result) {
	sendCtx, cancel := context.WithTimeout(ctx, p.config.PublishTimeout)
	err := p.config.Transport.DeadLetter(sendCtx, claim.Message, reason)
	cancel()
	if err != nil {
		p.config.Logger.Error("outbox dead-letter send failed; the message keeps its place",
			slog.String("message_id", claim.Message.MessageID), slog.String("reason", string(reason)),
			slog.String("error", err.Error()))
		class, retryReason := classify(err)
		delay := p.Backoff(claim.Attempt)
		if errors.Is(class, ErrMessageRejected) {
			// Neither queue will take this message. It stays in the outbox,
			// where retention keeps it for an operator, and it waits the whole
			// ceiling between attempts so that a message nothing can carry costs
			// one attempt per backoff window rather than one per cycle.
			delay = p.config.BackoffMax
		}
		p.retryAfter(ctx, claim, retryReason, delay)
		result.Retried++
		return
	}
	p.config.Logger.Error("outbox message was dead-lettered",
		slog.String("message_id", claim.Message.MessageID), slog.String("reason", string(reason)),
		slog.Int64("attempt", claim.Attempt))
	if err := p.config.Store.MarkOutboxPublished(ctx, claim, p.config.Clock.Now()); err != nil {
		result.Unrecorded++
		p.config.Logger.Error("dead-lettered message was not recorded; it will be delivered again",
			slog.String("message_id", claim.Message.MessageID), slog.String("error", err.Error()))
		return
	}
	result.DeadLettered++
}

// retry returns a message to the pending set with a backed-off next attempt.
func (p *Publisher) retry(ctx context.Context, claim persistence.OutboxClaim, reason Reason) {
	p.retryAfter(ctx, claim, reason, p.Backoff(claim.Attempt))
}

func (p *Publisher) retryAfter(ctx context.Context, claim persistence.OutboxClaim, reason Reason, delay time.Duration) {
	now := p.config.Clock.Now()
	if err := p.config.Store.RetryOutbox(ctx, claim, reason.failure(), now, now.Add(delay)); err != nil {
		// The claim keeps its lease and lapses. Nothing is lost; the message is
		// simply retried at expiry instead of at the computed instant.
		p.config.Logger.Error("outbox retry was not recorded; the claim will lapse instead",
			slog.String("message_id", claim.Message.MessageID), slog.String("error", err.Error()))
	}
}

// releaseNow hands back claims this cycle never attempted. They become
// claimable immediately rather than after a backoff, because nothing about them
// failed: this publisher simply stopped. A deployment fault therefore costs one
// cycle rather than one claim lease per message.
func (p *Publisher) releaseNow(ctx context.Context, claims []persistence.OutboxClaim, reason Reason) {
	for _, claim := range claims {
		p.retryAfter(ctx, claim, reason, 0)
	}
}

// leaseCovers reports whether this claim's lease still covers a whole delivery
// attempt. A publisher that started one without it would hand its row to a
// successor mid-flight and then find its own mark refused.
func (p *Publisher) leaseCovers(claim persistence.OutboxClaim) bool {
	return !p.config.Clock.Now().Add(p.config.PublishTimeout).After(claim.ExpiresAt)
}

// Backoff is the delay before the next attempt on a message that has already
// failed attempt times. It doubles from BackoffMin, is capped at BackoffMax, and
// carries jitter so that a recovering queue is not met by every publisher's
// whole backlog at one instant.
func (p *Publisher) Backoff(attempt int64) time.Duration {
	delay := p.config.BackoffMin
	for i := int64(1); i < attempt && delay < p.config.BackoffMax; i++ {
		delay *= 2
	}
	if delay > p.config.BackoffMax || delay <= 0 {
		delay = p.config.BackoffMax
	}
	jitter := p.config.Jitter(delay)
	if jitter < 0 {
		jitter = 0
	}
	total := delay + jitter
	if total <= 0 {
		// Overflow. A next attempt in the past would be refused by the store and
		// a message would sit claimed until its lease lapsed.
		return p.config.BackoffMax
	}
	return total
}

// fullJitter spreads retries over the second half of the backoff window. It is
// the only place this package reads entropy, and it is injectable so that a test
// asserts an exact instant rather than a range.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)/2 + 1))
}

func (p *Publisher) tokens() ([]string, error) {
	tokens := make([]string, 0, p.config.Batch)
	for i := 0; i < p.config.Batch; i++ {
		token, err := p.config.IDs.New()
		if err != nil {
			return nil, fmt.Errorf("outbox: fencing token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}
