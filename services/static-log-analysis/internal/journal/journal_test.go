package journal_test

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

type overridingClock struct {
	clock.Clock
	now time.Time
}

func (c *overridingClock) Now() time.Time { return c.now }

type harness struct {
	t       *testing.T
	dir     string
	clock   *fakeclock.Clock
	cfg     journal.Config
	factory *builders.Factory
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c := fakeclock.NewAtOrigin()
	h := &harness{t: t, dir: filepath.Join(t.TempDir(), "journal"), clock: c, factory: builders.NewFactory(builders.WithClock(c))}
	h.cfg = journal.Config{Dir: h.dir, Owner: "replica-a", TenantID: "tenant-a", Region: builders.DefaultRegion, Classification: "SENSITIVE", Clock: c, Validator: redact.MinimalPolicy(), MaxBytes: 64 << 20, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }}
	return h
}
func (h *harness) open() *journal.Journal {
	h.t.Helper()
	j, err := journal.Open(h.cfg)
	if err != nil {
		h.t.Fatalf("open: %v", err)
	}
	return j
}
func (h *harness) one() model.NormalizedLog { return h.factory.Record(h.t) }

func TestSynchronizedAtomicAppendSurvivesCleanReopen(t *testing.T) {
	h := newHarness(t)
	r := h.one()
	j := h.open()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	defer j.Close()
	claimed, err := j.Claim(1, "worker-a")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	if !reflect.DeepEqual(claimed[0].Record, r) {
		t.Fatalf("record changed across reopen")
	}
	wantReplay, err := journal.NewReplayIdentity(r, h.cfg.TenantID, h.cfg.Classification, journal.PriorityHigh)
	if err != nil || claimed[0].Replay.Version() != wantReplay.Version() ||
		claimed[0].Replay.Digest() != wantReplay.Digest() || claimed[0].Replay.Priority() != wantReplay.Priority() {
		t.Fatalf("claim did not expose exact sealed replay identity: got=%+v want=%+v err=%v", claimed[0].Replay, wantReplay, err)
	}
}

func TestRecoverRetainedPagesEverySafeStateWithoutChangingIt(t *testing.T) {
	h := newHarness(t)
	records := h.factory.Batch(t, 3)
	j := h.open()
	admissions := make([]journal.Admission, len(records))
	for i := range records {
		admissions[i] = journal.Admission{Record: records[i], Priority: journal.PriorityHigh}
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	claims, err := j.Claim(2, "worker-a")
	if err != nil || len(claims) != 2 {
		t.Fatalf("claim: records=%d err=%v", len(claims), err)
	}
	if err := j.MarkCommitted(claims[0].Record.RecordID, claims[0].Token); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	defer j.Close()

	states := map[journal.State]int{}
	cursor := ""
	for {
		page, next, err := j.RecoverRetained(cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, recovered := range page {
			if err := recovered.Record.Validate(); err != nil {
				t.Fatalf("recovered unsafe record: %v", err)
			}
			states[recovered.State]++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if states[journal.StatePending] != 1 || states[journal.StateClaimed] != 1 || states[journal.StateCommitted] != 1 {
		t.Fatalf("unexpected recovered states: %+v", states)
	}
	stats, err := j.Stats()
	if err != nil || stats.Pending != 1 || stats.Claimed != 1 || stats.Committed != 1 {
		t.Fatalf("recovery mutated journal: stats=%+v err=%v", stats, err)
	}
}

func TestDuplicateIdentityAllowsNewTransportAttemptButRejectsContentConflict(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	first := h.one()
	if err := j.AppendBatch(first.BatchID, []journal.Admission{{Record: first, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	retry := first
	retry.BatchID = h.factory.Record(t).BatchID
	retry.Source.ReceivedAt = h.clock.Now()
	if err := j.AppendBatch(retry.BatchID, []journal.Admission{{Record: retry, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatalf("semantic retry: %v", err)
	}
	conflict := retry
	conflict.BatchID = h.factory.Record(t).BatchID
	conflict.Body = model.SafeString("materially different safe body")
	if err := j.AppendBatch(conflict.BatchID, []journal.Admission{{Record: conflict, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrDuplicateConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	stats, _ := j.Stats()
	if stats.Pending != 1 {
		t.Fatalf("want one canonical record, got %+v", stats)
	}
}

func TestClaimExpiryExactBoundaryAndStaleToken(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	r := h.one()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	first, err := j.Claim(1, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultClaimTTL)
	second, err := j.Claim(1, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("want exact-expiry reclaim, got %+v", second)
	}
	if err := j.MarkCommitted(r.RecordID, first[0].Token); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("want stale token, got %v", err)
	}
	if err := j.MarkCommitted(r.RecordID, second[0].Token); err != nil {
		t.Fatal(err)
	}
}

func TestClaimBoundsExpiryCleanupAndClaimingToOneTransitionBudget(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	records := h.factory.Batch(t, journal.MaxTransitionRecords+1)
	admissions := make([]journal.Admission, 0, len(records))
	for _, record := range records {
		admissions = append(admissions, journal.Admission{Record: record, Priority: journal.PriorityNormal})
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	if claimed, err := j.Claim(journal.MaxTransitionRecords, "worker-a"); err != nil || len(claimed) != journal.MaxTransitionRecords {
		t.Fatalf("first claims: %v count=%d", err, len(claimed))
	}
	if claimed, err := j.Claim(1, "worker-b"); err != nil || len(claimed) != 1 {
		t.Fatalf("last claim: %v count=%d", err, len(claimed))
	}
	h.clock.Advance(journal.DefaultClaimTTL)
	// Cleaning 1,000 expired claims consumes the entire per-call transition
	// budget, so Claim(1) must not perform a 1,001st record transition.
	if claimed, err := j.Claim(1, "worker-c"); err != nil || len(claimed) != 0 {
		t.Fatalf("bounded expiry cleanup: %v count=%d", err, len(claimed))
	}
	stats, err := j.Stats()
	if err != nil || stats.Pending != journal.MaxTransitionRecords || stats.Claimed != 1 {
		t.Fatalf("cleanup exceeded budget: %v %+v", err, stats)
	}
	claimed, err := j.Claim(1, "worker-d")
	if err != nil || len(claimed) != 1 || claimed[0].Attempt != 2 {
		t.Fatalf("next bounded reclaim: %v %+v", err, claimed)
	}
}

func TestDatabaseCommitJournalUpdateGapReplaysAfterReopen(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	r := h.one()
	_ = j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityCritical}})
	claim, _ := j.Claim(1, "worker")
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultClaimTTL)
	j = h.open()
	defer j.Close()
	replay, err := j.Claim(1, "worker-retry")
	if err != nil || len(replay) != 1 {
		t.Fatalf("replay: %v %+v", err, replay)
	}
	if replay[0].Record.RecordID != claim[0].Record.RecordID {
		t.Fatal("record identity changed")
	}
	if err := j.MarkCommitted(r.RecordID, replay[0].Token); err != nil {
		t.Fatal(err)
	}
}

func TestQuarantinePersistsSafeTombstoneMetadataAndRejectsStaleClaim(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	r := h.one()
	_ = j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}})
	claim, _ := j.Claim(1, "worker")
	if err := j.Quarantine(r.RecordID, claim[0].Token, journal.QuarantineDeterministicProcessing); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkCommitted(r.RecordID, claim[0].Token); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("want stale, got %v", err)
	}
	stats, _ := j.Stats()
	if stats.Quarantined != 1 || stats.Pending+stats.Claimed+stats.Committed != 0 {
		t.Fatalf("unexpected states: %+v", stats)
	}
	_ = j.Close()
	j = h.open()
	defer j.Close()
	stats, _ = j.Stats()
	if stats.Quarantined != 1 {
		t.Fatalf("quarantine lost: %+v", stats)
	}
}

func TestQuarantinedRecordAcknowledgesExactReplayWithoutResurrection(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	record := h.one()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatal(err)
	}
	claims, err := j.Claim(1, "worker")
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v %+v", err, claims)
	}
	if err := j.Quarantine(record.RecordID, claims[0].Token, journal.QuarantineDeterministicProcessing); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j = h.open()
	defer j.Close()
	replay := record
	replay.BatchID = h.one().BatchID
	replay.Source.ReceivedAt = h.clock.Now().Add(time.Second)
	if err := j.AppendBatch(replay.BatchID, []journal.Admission{{Record: replay, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatalf("exact replay of quarantined record: %v", err)
	}
	stats, err := j.Stats()
	if err != nil || stats.Quarantined != 1 || stats.Pending+stats.Claimed+stats.Committed != 0 {
		t.Fatalf("replay resurrected quarantined payload: %v %+v", err, stats)
	}
	if claimed, err := j.Claim(1, "worker"); err != nil || len(claimed) != 0 {
		t.Fatalf("quarantined replay became claimable: %v %+v", err, claimed)
	}

	conflict := replay
	conflict.BatchID = h.one().BatchID
	conflict.Source.ReceivedAt = h.clock.Now().Add(2 * time.Second)
	conflict.Body = model.SafeString("changed content under a quarantined identity")
	if err := j.AppendBatch(conflict.BatchID, []journal.Admission{{Record: conflict, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrDuplicateConflict) {
		t.Fatalf("changed quarantined replay was not rejected: %v", err)
	}
}

func TestCompactionUsesStrictSafetyDelayBoundaryAndReleasesAccounting(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	r := h.one()
	_ = j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}})
	claim, _ := j.Claim(1, "worker")
	_ = j.MarkCommitted(r.RecordID, claim[0].Token)
	before, _ := j.Stats()
	if before.ByPriority[journal.PriorityNormal] != 1 {
		t.Fatalf("priority accounting missing: %+v", before)
	}
	h.clock.Advance(journal.DefaultSafetyDelay)
	if n, err := j.Compact(10); err != nil || n != 0 {
		t.Fatalf("deadline retained: %d %v", n, err)
	}
	h.clock.Advance(time.Nanosecond)
	if n, err := j.Compact(10); err != nil || n != 1 {
		t.Fatalf("after deadline removed: %d %v", n, err)
	}
	after, _ := j.Stats()
	if after.AccountedBytes >= before.AccountedBytes || after.Committed != 0 || after.ByPriority[journal.PriorityNormal] != 0 {
		t.Fatalf("compaction accounting/state: before=%+v after=%+v", before, after)
	}
	if n, err := j.Compact(10); err != nil || n != 0 {
		t.Fatalf("idempotent compact: %d %v", n, err)
	}
}

func TestMultiRecordSharedBatchQuarantineAndCompactionReopensCleanly(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	records := h.factory.Batch(t, 4)
	ad := make([]journal.Admission, 4)
	for i, r := range records {
		ad[i] = journal.Admission{Record: r, Priority: journal.PriorityNormal}
	}
	if err := j.AppendBatch(records[0].BatchID, ad); err != nil {
		t.Fatal(err)
	}
	// Two records also belong to a second transport attempt. Removing either
	// one must update both staged batch memberships without losing the first
	// update made by the same Pebble mutation.
	h.clock.Advance(time.Nanosecond)
	retryBatch := h.factory.Record(t).BatchID
	retries := make([]journal.Admission, 2)
	for i := range retries {
		retry := records[i]
		retry.BatchID = retryBatch
		retry.Source.ReceivedAt = h.clock.Now()
		retries[i] = journal.Admission{Record: retry, Priority: journal.PriorityNormal}
	}
	if err := j.AppendBatch(retryBatch, retries); err != nil {
		t.Fatal(err)
	}
	claims, err := j.Claim(4, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Quarantine(claims[0].Record.RecordID, claims[0].Token, journal.QuarantineDeterministicProcessing); err != nil {
		t.Fatal(err)
	}
	commits := make([]journal.CommitClaim, 0, 3)
	for _, claim := range claims[1:] {
		commits = append(commits, journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token})
	}
	if err := j.MarkCommittedBatch(commits); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
	if n, err := j.Compact(1); err != nil || n != 1 {
		t.Fatalf("limit-one partial compact: %d %v", n, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	if n, err := j.Compact(2); err != nil || n != 2 {
		t.Fatalf("same-mutation shared-batch compact: %d %v", n, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	defer j.Close()
	stats, err := j.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.Committed != 0 {
		t.Fatalf("unexpected reopened state: %+v", stats)
	}
}

func TestMarkCommittedBatchAllOrNoneIdempotentAndReopens(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	records := h.factory.Batch(t, 3)
	admissions := make([]journal.Admission, 0, len(records))
	for i, record := range records {
		admissions = append(admissions, journal.Admission{Record: record, Priority: journal.Priority(i)})
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	claims, err := j.Claim(3, "worker")
	if err != nil {
		t.Fatal(err)
	}
	valid := []journal.CommitClaim{{RecordID: claims[0].Record.RecordID, Token: claims[0].Token}, {RecordID: claims[1].Record.RecordID, Token: claims[1].Token}}
	stale := append([]journal.CommitClaim(nil), valid...)
	last := len(stale[1].Token) - 1
	if stale[1].Token[last] == 'a' {
		stale[1].Token = stale[1].Token[:last] + "b"
	} else {
		stale[1].Token = stale[1].Token[:last] + "a"
	}
	if err := j.MarkCommittedBatch(stale); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("want stale all-or-none refusal, got %v", err)
	}
	stats, _ := j.Stats()
	if stats.Claimed != 3 || stats.Committed != 0 {
		t.Fatalf("stale batch partially committed: %+v", stats)
	}
	duplicate := []journal.CommitClaim{valid[0], valid[0]}
	if err := j.MarkCommittedBatch(duplicate); !errors.Is(err, journal.ErrInvalidInput) {
		t.Fatalf("want duplicate refusal, got %v", err)
	}
	if err := j.MarkCommittedBatch(valid[:1]); err != nil {
		t.Fatal(err)
	}
	// A retry may mix an already committed member with a still-current claim.
	if err := j.MarkCommittedBatch(valid); err != nil {
		t.Fatalf("mixed idempotent batch: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	defer j.Close()
	if err := j.MarkCommittedBatch(valid); err != nil {
		t.Fatalf("idempotence after reopen: %v", err)
	}
	stats, _ = j.Stats()
	if stats.Committed != 2 || stats.Claimed != 1 || stats.ByPriority[journal.PriorityCritical] != 1 || stats.ByPriority[journal.PriorityHigh] != 1 || stats.ByPriority[journal.PriorityNormal] != 1 {
		t.Fatalf("unexpected persisted states/accounting: %+v", stats)
	}
}

func TestMarkCommittedBatchExpiredMemberRejectsWholeBatch(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	records := h.factory.Batch(t, 2)
	if err := j.AppendBatch(records[0].BatchID, []journal.Admission{{Record: records[0], Priority: journal.PriorityHigh}, {Record: records[1], Priority: journal.PriorityLow}}); err != nil {
		t.Fatal(err)
	}
	claims, _ := j.Claim(2, "worker")
	h.clock.Advance(journal.DefaultClaimTTL)
	commits := []journal.CommitClaim{{RecordID: claims[0].Record.RecordID, Token: claims[0].Token}, {RecordID: claims[1].Record.RecordID, Token: claims[1].Token}}
	if err := j.MarkCommittedBatch(commits); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("want expired batch refusal, got %v", err)
	}
	stats, _ := j.Stats()
	if stats.Claimed != 2 || stats.Committed != 0 {
		t.Fatalf("expired batch partially committed: %+v", stats)
	}
}

func TestImmutableBoundaryRejectsContradictionsAfterDrain(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	r := h.one()
	_ = j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}})
	claim, _ := j.Claim(1, "worker")
	_ = j.MarkCommitted(r.RecordID, claim[0].Token)
	h.clock.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
	_, _ = j.Compact(1)
	_ = j.Close()
	tests := []func(*journal.Config){func(c *journal.Config) { c.Region = "us-west-2" }, func(c *journal.Config) { c.TenantID = "tenant-b" }, func(c *journal.Config) { c.Classification = "RESTRICTED" }, func(c *journal.Config) { c.Owner = "replica-b" }, func(c *journal.Config) { c.Validator = versionOnlyValidator{"9.9"} }}
	for i, change := range tests {
		cfg := h.cfg
		change(&cfg)
		if _, err := journal.Open(cfg); err == nil {
			t.Fatalf("boundary contradiction %d accepted", i)
		}
	}
}

func TestImmutableBoundaryPersistsOnEmptyVolume(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	changed := h.cfg
	changed.Region = "us-west-2"
	if _, err := journal.Open(changed); !errors.Is(err, journal.ErrBoundaryMismatch) {
		t.Fatalf("empty volume boundary changed: %v", err)
	}
}

type versionOnlyValidator struct{ version string }

func (v versionOnlyValidator) Version() string                        { return v.version }
func (versionOnlyValidator) ValidateRecord(model.NormalizedLog) error { return nil }

func TestTypedNilDependenciesAndInvalidInitialTimeCreateNoFilesystem(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*journal.Config)
	}{
		{name: "clock", change: func(c *journal.Config) { var typedNil *fakeclock.Clock; c.Clock = typedNil }},
		{name: "validator", change: func(c *journal.Config) { var typedNil *redact.Policy; c.Validator = typedNil }},
		{name: "clock-time", change: func(c *journal.Config) { c.Clock = &overridingClock{Clock: c.Clock, now: time.Time{}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			test.change(&h.cfg)
			if _, err := journal.Open(h.cfg); !errors.Is(err, journal.ErrInvalidConfig) {
				t.Fatalf("want invalid config, got %v", err)
			}
			if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid dependency touched filesystem: %v", err)
			}
		})
	}
}

func TestConfigurationCategoriesAreBoundedClosedAndOpaque(t *testing.T) {
	unsafe := "secret category value"
	for _, change := range []func(*journal.Config){
		func(c *journal.Config) { c.Owner = strings.Repeat("o", journal.MaxOwnerBytes+1) },
		func(c *journal.Config) { c.Region = unsafe },
		func(c *journal.Config) { c.TenantID = strings.Repeat("t", journal.MaxBoundaryBytes+1) },
		func(c *journal.Config) { c.Classification = "PROHIBITED" },
		func(c *journal.Config) {
			c.Validator = versionOnlyValidator{strings.Repeat("v", journal.MaxBoundaryBytes+1)}
		},
	} {
		h := newHarness(t)
		change(&h.cfg)
		_, err := journal.Open(h.cfg)
		if !errors.Is(err, journal.ErrInvalidConfig) {
			t.Fatalf("want invalid config, got %v", err)
		}
		if contains(err.Error(), unsafe) {
			t.Fatalf("configuration error leaked category: %v", err)
		}
	}
}

func TestInvalidClockTimeLatchesEveryLiveTransition(t *testing.T) {
	for _, operation := range []string{"claim", "commit", "quarantine", "compact"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t)
			wrapped := &overridingClock{Clock: h.clock, now: h.clock.Now()}
			h.cfg.Clock = wrapped
			j := h.open()
			defer j.Close()
			r := h.one()
			if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); err != nil {
				t.Fatal(err)
			}
			var claim journal.ClaimedRecord
			if operation != "claim" {
				claimed, err := j.Claim(1, "worker")
				if err != nil {
					t.Fatal(err)
				}
				claim = claimed[0]
			}
			wrapped.now = time.Time{}
			var err error
			switch operation {
			case "claim":
				_, err = j.Claim(1, "worker")
			case "commit":
				err = j.MarkCommitted(r.RecordID, claim.Token)
			case "quarantine":
				err = j.Quarantine(r.RecordID, claim.Token, journal.QuarantineUnsupportedData)
			case "compact":
				_, err = j.Compact(1)
			}
			if !errors.Is(err, journal.ErrCorruption) || j.Ready() {
				t.Fatalf("invalid clock did not latch %s: err=%v ready=%v", operation, err, j.Ready())
			}
		})
	}
}

func TestCrossRegionAndPolicyMismatchAdmissionRejected(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	r := h.one()
	r.Region = "us-west-2"
	r.Source.Region = "us-west-2"
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrInvalidInput) {
		t.Fatalf("cross region: %v", err)
	}
	r = h.one()
	r.Redaction.PolicyVersion = "2.1"
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrInvalidInput) {
		t.Fatalf("policy mismatch: %v", err)
	}
}

func TestRawReferenceRequiresLosslessOrderedCompleteTimesAndBoundary(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	base := h.one()
	base.RawReference = &model.RegionalLogReference{
		SourceType:     model.SourceTypeCloudWatch,
		Region:         h.cfg.Region,
		Locator:        "safe-locator",
		From:           h.clock.Now().Add(-time.Minute),
		To:             h.clock.Now(),
		Classification: "SENSITIVE",
		ExpiresAt:      h.clock.Now().Add(time.Hour),
	}
	if err := j.AppendBatch(base.BatchID, []journal.Admission{{Record: base, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatalf("valid reference: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*model.RegionalLogReference)
	}{
		{name: "zero-from", change: func(r *model.RegionalLogReference) { r.From = time.Time{} }},
		{name: "zero-to", change: func(r *model.RegionalLogReference) { r.To = time.Time{} }},
		{name: "zero-expiry", change: func(r *model.RegionalLogReference) { r.ExpiresAt = time.Time{} }},
		{name: "reversed-range", change: func(r *model.RegionalLogReference) { r.From = r.To.Add(time.Nanosecond) }},
		{name: "nonfuture-expiry", change: func(r *model.RegionalLogReference) { r.ExpiresAt = r.To }},
		{name: "cross-region", change: func(r *model.RegionalLogReference) { r.Region = "us-west-2" }},
		{name: "prohibited", change: func(r *model.RegionalLogReference) { r.Classification = "PROHIBITED" }},
		{name: "empty-source", change: func(r *model.RegionalLogReference) { r.SourceType = "" }},
		{name: "unknown-source", change: func(r *model.RegionalLogReference) { r.SourceType = model.SourceType("future-source") }},
		{name: "unknown-classification", change: func(r *model.RegionalLogReference) { r.Classification = "SECRET" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := h.one()
			ref := *base.RawReference
			test.change(&ref)
			record.RawReference = &ref
			if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrInvalidInput) {
				t.Fatalf("want invalid input, got %v", err)
			}
		})
	}
}

func TestCommitRejectsSafetyDeadlineOverflowBeforePersistence(t *testing.T) {
	h := newHarness(t)
	wrapped := &overridingClock{Clock: h.clock, now: h.clock.Now()}
	h.cfg.Clock = wrapped
	j := h.open()
	record := h.one()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := j.Claim(1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	wrapped.now = time.Unix(0, math.MaxInt64).UTC()
	if err := j.MarkCommitted(record.RecordID, claimed[0].Token); !errors.Is(err, journal.ErrCorruption) {
		t.Fatalf("want checked safety deadline refusal, got %v", err)
	}
	if j.Ready() {
		t.Fatal("overflowing clock did not latch journal unready")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// The rejected transition must not have persisted committed state.
	wrapped.now = h.clock.Now()
	j = h.open()
	defer j.Close()
	stats, err := j.Stats()
	if err != nil || stats.Claimed != 1 || stats.Committed != 0 {
		t.Fatalf("overflow transition became visible: %v %+v", err, stats)
	}
}

func TestOriginalBatchReplayAfterPartialCompactionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	records := h.factory.Batch(t, 3)
	admissions := make([]journal.Admission, 0, len(records))
	for _, record := range records {
		admissions = append(admissions, journal.Admission{Record: record, Priority: journal.PriorityNormal})
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	claims, _ := j.Claim(3, "worker")
	commits := make([]journal.CommitClaim, 0, len(claims))
	for _, claim := range claims {
		commits = append(commits, journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token})
	}
	if err := j.MarkCommittedBatch(commits); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
	if count, err := j.Compact(1); err != nil || count != 1 {
		t.Fatalf("partial compact: %v count=%d", err, count)
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatalf("exact original replay after partial compact: %v", err)
	}
	stats, _ := j.Stats()
	if stats.Pending != 0 || stats.Committed != 2 {
		t.Fatalf("replay resurrected compacted member: %+v", stats)
	}
	changedMembership := admissions[:2]
	if err := j.AppendBatch(records[0].BatchID, changedMembership); !errors.Is(err, journal.ErrDuplicateConflict) {
		t.Fatalf("changed original membership accepted: %v", err)
	}
	removed := -1
	for i := range records {
		found := false
		for _, claim := range claims {
			if claim.Record.RecordID == records[i].RecordID {
				found = true
				break
			}
		}
		if found && (removed == -1 || records[i].RecordID < records[removed].RecordID) {
			removed = i
		}
	}
	conflict := append([]journal.Admission(nil), admissions...)
	conflict[removed].Record.Body = model.SafeString("different safe content")
	if err := j.AppendBatch(records[0].BatchID, conflict); !errors.Is(err, journal.ErrDuplicateConflict) {
		t.Fatalf("changed compacted member accepted: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	defer j.Close()
	stats, err := j.Stats()
	if err != nil || stats.Committed != 2 || stats.Pending != 0 {
		t.Fatalf("reopen after replay: %v %+v", err, stats)
	}
}

func TestCrossBatchRedeliveryAfterPartialCompactionPreservesOriginalReplay(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	records := h.factory.Batch(t, 3)
	admissions := make([]journal.Admission, 0, len(records))
	for _, record := range records {
		admissions = append(admissions, journal.Admission{Record: record, Priority: journal.PriorityNormal})
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	claims, err := j.Claim(len(records), "worker")
	if err != nil {
		t.Fatal(err)
	}
	commits := make([]journal.CommitClaim, 0, len(claims))
	for _, claim := range claims {
		commits = append(commits, journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token})
	}
	if err := j.MarkCommittedBatch(commits); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
	if count, err := j.Compact(1); err != nil || count != 1 {
		t.Fatalf("partial compact: %v count=%d", err, count)
	}
	removed := 0
	for i := 1; i < len(records); i++ {
		if records[i].RecordID < records[removed].RecordID {
			removed = i
		}
	}

	conflict := records[removed]
	conflict.BatchID = h.one().BatchID
	conflict.Source.ReceivedAt = h.clock.Now()
	conflict.Body = model.SafeString("conflicting content after compaction")
	if err := j.AppendBatch(conflict.BatchID, []journal.Admission{{Record: conflict, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrDuplicateConflict) {
		t.Fatalf("conflicting cross-batch redelivery accepted: %v", err)
	}
	if !j.Ready() {
		t.Fatal("identity conflict incorrectly latched journal unready")
	}

	retry := records[removed]
	retry.BatchID = h.one().BatchID
	retry.Source.ReceivedAt = h.clock.Now()
	if err := j.AppendBatch(retry.BatchID, []journal.Admission{{Record: retry, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatalf("same-semantic cross-batch redelivery rejected: %v", err)
	}
	beforeReplay, err := j.Stats()
	if err != nil || beforeReplay.Pending != 1 || beforeReplay.Committed != 2 {
		t.Fatalf("cross-batch state: %v %+v", err, beforeReplay)
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatalf("original batch replay after cross-batch redelivery: %v", err)
	}
	afterReplay, err := j.Stats()
	if err != nil || afterReplay.Pending != beforeReplay.Pending || afterReplay.Committed != beforeReplay.Committed || afterReplay.AccountedBytes != beforeReplay.AccountedBytes {
		t.Fatalf("original replay changed durable state: %v before=%+v after=%+v", err, beforeReplay, afterReplay)
	}
	if !j.Ready() {
		t.Fatal("exact original replay latched journal unready")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = h.open()
	defer j.Close()
	stats, err := j.Stats()
	if err != nil || stats.Pending != 1 || stats.Committed != 2 {
		t.Fatalf("reopen after cross-batch replay: %v %+v", err, stats)
	}
}

func TestCheckedDurationAdditionExactUnixNanoBoundary(t *testing.T) {
	max := time.Unix(0, math.MaxInt64).UTC()
	for _, test := range []struct {
		name string
		now  time.Time
		ok   bool
	}{
		{name: "exact", now: max.Add(-time.Nanosecond), ok: true},
		{name: "overflow", now: max, ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Clock = fakeclock.New(test.now)
			h.cfg.ClaimTTL = time.Nanosecond
			h.cfg.SafetyDelay = time.Nanosecond
			j, err := journal.Open(h.cfg)
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				_ = j.Close()
			} else if !errors.Is(err, journal.ErrInvalidConfig) {
				t.Fatalf("want overflow refusal, got %v", err)
			}
		})
	}
}

func TestCapacityRejectsWholeAppendAndDuplicateDoesNotDoubleCount(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	r := h.one()
	_ = j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}})
	stats, _ := j.Stats()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatal(err)
	}
	again, _ := j.Stats()
	if again.AccountedBytes != stats.AccountedBytes {
		t.Fatalf("duplicate changed accounting: %d -> %d", stats.AccountedBytes, again.AccountedBytes)
	}
	_ = j.Close()
	h.cfg.MaxBytes = stats.AccountedBytes
	j = h.open()
	defer j.Close()
	another := h.one()
	if err := j.AppendBatch(another.BatchID, []journal.Admission{{Record: another, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrCapacity) {
		t.Fatalf("want capacity, got %v", err)
	}
	after, _ := j.Stats()
	if after.Pending != 1 {
		t.Fatalf("partial append: %+v", after)
	}
}

func TestCapacityExactByteAndFreeSpaceBoundariesAndOverflow(t *testing.T) {
	measure := newHarness(t)
	r := measure.one()
	j := measure.open()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatal(err)
	}
	stats, _ := j.Stats()
	_ = j.Close()
	exact := newHarness(t)
	r = exact.one()
	exact.cfg.MaxBytes = stats.AccountedBytes
	exact.cfg.FreeSpace = func(string) (uint64, error) { return exact.cfg.MinFreeBytes + stats.AccountedBytes, nil }
	j = exact.open()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); err != nil {
		t.Fatalf("exact boundaries: %v", err)
	}
	claim, err := j.Claim(1, "worker")
	if err != nil || len(claim) != 1 {
		t.Fatalf("reserved transition: %v %+v", err, claim)
	}
	afterClaim, _ := j.Stats()
	if afterClaim.AccountedBytes > exact.cfg.MaxBytes {
		t.Fatalf("transition exceeded admitted maximum: %+v", afterClaim)
	}
	_ = j.Close()
	over := newHarness(t)
	r = over.one()
	over.cfg.FreeSpace = func(string) (uint64, error) { return over.cfg.MinFreeBytes + stats.AccountedBytes - 1, nil }
	j = over.open()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrCapacity) {
		t.Fatalf("one byte below free boundary: %v", err)
	}
	empty, _ := j.Stats()
	if empty.Pending != 0 || empty.AccountedBytes != 0 {
		t.Fatalf("capacity failure wrote partial state: %+v", empty)
	}
	_ = j.Close()
	overflow := newHarness(t)
	overflow.cfg.EntryOverhead = math.MaxUint64
	r = overflow.one()
	j = overflow.open()
	defer j.Close()
	if err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}}); !errors.Is(err, journal.ErrCapacity) {
		t.Fatalf("want overflow-safe capacity refusal, got %v", err)
	}
}

func TestTransitionReserveCoversExactMaximumOwnerAndPersistedExpiry(t *testing.T) {
	measure := newHarness(t)
	record := measure.one()
	j := measure.open()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityCritical}}); err != nil {
		t.Fatal(err)
	}
	appendStats, _ := j.Stats()
	_ = j.Close()

	h := newHarness(t)
	h.cfg.MaxBytes = appendStats.AccountedBytes
	h.cfg.FreeSpace = func(string) (uint64, error) { return h.cfg.MinFreeBytes + appendStats.AccountedBytes, nil }
	record = h.one()
	j = h.open()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityCritical}}); err != nil {
		t.Fatalf("exact-capacity append: %v", err)
	}
	maxOwner := strings.Repeat("w", journal.MaxOwnerBytes)
	first, err := j.Claim(1, maxOwner)
	if err != nil || len(first) != 1 {
		t.Fatalf("maximum owner claim: %v %+v", err, first)
	}
	claimedStats, _ := j.Stats()
	if claimedStats.AccountedBytes > appendStats.AccountedBytes {
		t.Fatalf("reserve failed exact maximum owner: append=%d claim=%d", appendStats.AccountedBytes, claimedStats.AccountedBytes)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultClaimTTL)
	j = h.open()
	defer j.Close()
	second, err := j.Claim(1, maxOwner)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("persisted expiry attempt-two reclaim: %v %+v", err, second)
	}
	if err := j.MarkCommitted(record.RecordID, second[0].Token); err != nil {
		t.Fatal(err)
	}
}

func TestReplayPriorityConflictMaxBatchRefsAndPartialCommit(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxBatchRefs = 2
	j := h.open()
	defer j.Close()
	records := h.factory.Batch(t, 2)
	if err := j.AppendBatch(records[0].BatchID, []journal.Admission{{Record: records[0], Priority: journal.PriorityHigh}, {Record: records[1], Priority: journal.PriorityLow}}); err != nil {
		t.Fatal(err)
	}
	priorityConflict := records[0]
	priorityConflict.BatchID = h.factory.Record(t).BatchID
	priorityConflict.Source.ReceivedAt = h.clock.Now().Add(time.Nanosecond)
	if err := j.AppendBatch(priorityConflict.BatchID, []journal.Admission{{Record: priorityConflict, Priority: journal.PriorityLow}}); !errors.Is(err, journal.ErrDuplicateConflict) {
		t.Fatalf("priority conflict accepted: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		retry := records[0]
		retry.BatchID = h.factory.Record(t).BatchID
		retry.Source.ReceivedAt = h.clock.Now().Add(time.Duration(attempt+1) * time.Nanosecond)
		err := j.AppendBatch(retry.BatchID, []journal.Admission{{Record: retry, Priority: journal.PriorityHigh}})
		if attempt == 0 && err != nil {
			t.Fatalf("second live batch ref: %v", err)
		}
		if attempt == 1 && !errors.Is(err, journal.ErrCapacity) {
			t.Fatalf("MaxBatchRefs not enforced: %v", err)
		}
	}
	claims, err := j.Claim(2, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.MarkCommittedBatch([]journal.CommitClaim{{RecordID: claims[0].Record.RecordID, Token: claims[0].Token}}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(journal.DefaultClaimTTL)
	replay, err := j.Claim(2, "replay-worker")
	if err != nil || len(replay) != 1 || replay[0].Attempt != 2 {
		t.Fatalf("partial replay: %v %+v", err, replay)
	}
}

func TestBatchWorkLimitsExactAndOver(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		h := newHarness(t)
		j := h.open()
		defer j.Close()
		record := h.one()
		exact := make([]journal.Admission, journal.MaxAppendRecords)
		for i := range exact {
			exact[i] = journal.Admission{Record: record, Priority: journal.PriorityHigh}
		}
		if err := j.AppendBatch(record.BatchID, exact); err != nil {
			t.Fatalf("exact append limit: %v", err)
		}
		if err := j.AppendBatch("over-limit", make([]journal.Admission, journal.MaxAppendRecords+1)); !errors.Is(err, journal.ErrInvalidInput) {
			t.Fatalf("append over limit: %v", err)
		}
	})

	t.Run("transitions", func(t *testing.T) {
		h := newHarness(t)
		j := h.open()
		defer j.Close()
		records := h.factory.Batch(t, journal.MaxTransitionRecords)
		admissions := make([]journal.Admission, 0, len(records))
		for _, record := range records {
			admissions = append(admissions, journal.Admission{Record: record, Priority: journal.PriorityNormal})
		}
		if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Claim(journal.MaxTransitionRecords+1, "worker"); !errors.Is(err, journal.ErrInvalidInput) {
			t.Fatalf("claim over limit: %v", err)
		}
		claims, err := j.Claim(journal.MaxTransitionRecords, "worker")
		if err != nil || len(claims) != journal.MaxTransitionRecords {
			t.Fatalf("exact claim limit: %v count=%d", err, len(claims))
		}
		if err := j.MarkCommittedBatch(make([]journal.CommitClaim, journal.MaxTransitionRecords+1)); !errors.Is(err, journal.ErrInvalidInput) {
			t.Fatalf("commit over limit: %v", err)
		}
		commits := make([]journal.CommitClaim, 0, len(claims))
		for _, claim := range claims {
			commits = append(commits, journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token})
		}
		if err := j.MarkCommittedBatch(commits); err != nil {
			t.Fatalf("exact commit limit: %v", err)
		}
		h.clock.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
		if _, err := j.Compact(journal.MaxTransitionRecords + 1); !errors.Is(err, journal.ErrInvalidInput) {
			t.Fatalf("compact over limit: %v", err)
		}
		if count, err := j.Compact(journal.MaxTransitionRecords); err != nil || count != journal.MaxTransitionRecords {
			t.Fatalf("exact compact limit: %v count=%d", err, count)
		}
	})
}

func TestTransitionIdentifiersAreBoundedAndOpaque(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	record := h.one()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	claim, err := j.Claim(1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	unsafe := "bad\ncontrol"
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "commit-record", run: func() error { return j.MarkCommitted(strings.Repeat("a", 65), claim[0].Token) }},
		{name: "commit-token", run: func() error { return j.MarkCommitted(record.RecordID, unsafe) }},
		{name: "quarantine-record", run: func() error { return j.Quarantine(unsafe, claim[0].Token, journal.QuarantineUnsupportedData) }},
		{name: "quarantine-token", run: func() error {
			return j.Quarantine(record.RecordID, strings.Repeat("t", 1024), journal.QuarantineUnsupportedData)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if !errors.Is(err, journal.ErrInvalidInput) || contains(err.Error(), unsafe) {
				t.Fatalf("want opaque invalid input, got %v", err)
			}
		})
	}
}

func TestOwnerAndConcurrentOpenRefuseStartupCategorically(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	if _, err := journal.Open(h.cfg); !errors.Is(err, journal.ErrLocked) {
		t.Fatalf("want lock, got %v", err)
	}
	_ = j.Close()
	other := h.cfg
	other.Owner = "replica-b"
	if _, err := journal.Open(other); !errors.Is(err, journal.ErrOwnerMismatch) {
		t.Fatalf("want owner mismatch, got %v", err)
	}
}

func TestConcurrentAppendsAreDeterministicAndRaceSafe(t *testing.T) {
	h := newHarness(t)
	j := h.open()
	defer j.Close()
	records := make([]model.NormalizedLog, 16)
	for i := range records {
		records[i] = h.factory.Record(t)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(records))
	for _, r := range records {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stats, _ := j.Stats()
	if stats.Pending != 16 {
		t.Fatalf("want 16 pending, got %+v", stats)
	}
}

func TestFinalProhibitedContentValidationIsMandatoryAndOpaque(t *testing.T) {
	h := newHarness(t)
	bad := h.cfg
	bad.Validator = nil
	if _, err := journal.Open(bad); !errors.Is(err, journal.ErrInvalidConfig) {
		t.Fatalf("want invalid config, got %v", err)
	}
	j := h.open()
	defer j.Close()
	r := h.one()
	secret := "password=hunter2"
	r.Body = model.SafeString(secret)
	err := j.AppendBatch(r.BatchID, []journal.Admission{{Record: r, Priority: journal.PriorityNormal}})
	if !errors.Is(err, journal.ErrInvalidInput) {
		t.Fatalf("want rejection, got %v", err)
	}
	if contains(err.Error(), secret) || contains(err.Error(), r.RecordID) {
		t.Fatalf("error leaked input: %v", err)
	}
}
func contains(s, part string) bool {
	return len(part) > 0 && len(s) >= len(part) && func() bool {
		for i := 0; i+len(part) <= len(s); i++ {
			if s[i:i+len(part)] == part {
				return true
			}
		}
		return false
	}()
}

func TestProcessTerminationAfterSynchronizedACKReplays(t *testing.T) {
	if os.Getenv("JOURNAL_CRASH_HELPER") == "1" {
		t.Skip("helper is selected by its dedicated test name")
	}
	for _, phase := range []string{"pending", "claimed", "committed", "quarantined", "compacted"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestJournalCrashAppendHelper$")
			cmd.Env = append(os.Environ(), "JOURNAL_CRASH_HELPER=1", "JOURNAL_CRASH_DIR="+h.dir, "JOURNAL_CRASH_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper: %v: %s", err, output)
			}
			if string(output) != "ACK\n" {
				t.Fatalf("ACK must follow synchronized transition, got %q", output)
			}
			j := h.open()
			defer j.Close()
			stats, err := j.Stats()
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "pending":
				if stats.Pending != 3 {
					t.Fatalf("want atomic three-record replay, got %+v", stats)
				}
				claimed, err := j.Claim(3, "parent")
				if err != nil || len(claimed) != 3 {
					t.Fatalf("replay: %v %+v", err, claimed)
				}
			case "claimed":
				if stats.Claimed != 3 {
					t.Fatalf("want claims persisted, got %+v", stats)
				}
				h.clock.Advance(journal.DefaultClaimTTL)
				claimed, err := j.Claim(3, "parent")
				if err != nil || len(claimed) != 3 {
					t.Fatalf("expired replay: %v %+v", err, claimed)
				}
			case "committed":
				if stats.Committed != 3 {
					t.Fatalf("want commits persisted, got %+v", stats)
				}
			case "quarantined":
				if stats.Quarantined != 3 || stats.Pending+stats.Claimed+stats.Committed != 0 {
					t.Fatalf("want safe quarantine metadata only, got %+v", stats)
				}
			case "compacted":
				if stats.Pending+stats.Claimed+stats.Committed+stats.Quarantined != 0 || stats.AccountedBytes != 0 {
					t.Fatalf("want durable compacted state, got %+v", stats)
				}
			}
		})
	}
}

func TestJournalCrashAppendHelper(t *testing.T) {
	if os.Getenv("JOURNAL_CRASH_HELPER") != "1" {
		return
	}
	c := fakeclock.NewAtOrigin()
	factory := builders.NewFactory(builders.WithClock(c))
	cfg := journal.Config{Dir: os.Getenv("JOURNAL_CRASH_DIR"), Owner: "replica-a", TenantID: "tenant-a", Region: builders.DefaultRegion, Classification: "SENSITIVE", Clock: c, Validator: redact.MinimalPolicy(), MaxBytes: 64 << 20, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }}
	j, err := journal.Open(cfg)
	if err != nil {
		os.Exit(20)
	}
	records := factory.Batch(t, 3)
	admissions := make([]journal.Admission, 0, 3)
	for _, r := range records {
		admissions = append(admissions, journal.Admission{Record: r, Priority: journal.PriorityHigh})
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		os.Exit(21)
	}
	phase := os.Getenv("JOURNAL_CRASH_PHASE")
	var claims []journal.ClaimedRecord
	if phase != "pending" {
		claims, err = j.Claim(3, "crash-worker")
		if err != nil || len(claims) != 3 {
			os.Exit(22)
		}
	}
	if phase == "committed" || phase == "compacted" {
		commits := make([]journal.CommitClaim, 0, len(claims))
		for _, claim := range claims {
			commits = append(commits, journal.CommitClaim{RecordID: claim.Record.RecordID, Token: claim.Token})
		}
		if j.MarkCommittedBatch(commits) != nil {
			os.Exit(23)
		}
	}
	if phase == "quarantined" {
		for _, claim := range claims {
			if j.Quarantine(claim.Record.RecordID, claim.Token, journal.QuarantineDeterministicProcessing) != nil {
				os.Exit(24)
			}
		}
	}
	if phase == "compacted" {
		c.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
		if n, err := j.Compact(3); err != nil || n != 3 {
			os.Exit(25)
		}
	}
	fmt.Println("ACK")
	os.Exit(0)
}

// A restarted worker still owns whatever it claimed before it died. Without a
// way to re-adopt those claims the records stall for a full claim TTL even
// though the owner is back and healthy, which operations.md counts against the
// required 30-minute outage tolerance.
func TestAdoptClaimsReturnsOnlyThisOwnersUnexpiredClaims(t *testing.T) {
	h := newHarness(t)
	records := h.factory.Batch(t, 3)
	j := h.open()
	defer j.Close()
	admissions := make([]journal.Admission, len(records))
	for i := range records {
		admissions[i] = journal.Admission{Record: records[i], Priority: journal.PriorityHigh}
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	mine, err := j.Claim(2, "worker-a")
	if err != nil || len(mine) != 2 {
		t.Fatalf("claim: %v %+v", err, mine)
	}
	other, err := j.Claim(1, "worker-b")
	if err != nil || len(other) != 1 {
		t.Fatalf("other claim: %v %+v", err, other)
	}

	adopted, err := j.AdoptClaims("worker-a", journal.MaxTransitionRecords)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(adopted) != 2 {
		t.Fatalf("adopted %d claims, want this owner's two", len(adopted))
	}
	tokens := map[string]string{}
	for _, claim := range mine {
		tokens[claim.Record.RecordID] = claim.Token
	}
	for _, claim := range adopted {
		if claim.Owner != "worker-a" {
			t.Fatalf("adopted a claim owned by %q", claim.Owner)
		}
		if tokens[claim.Record.RecordID] != claim.Token {
			t.Fatalf("adopted claim %s carried a different token", claim.Record.RecordID)
		}
	}
	// Adoption observes; it must not itself be a state transition.
	stats, err := j.Stats()
	if err != nil || stats.Claimed != 3 || stats.Pending != 0 {
		t.Fatalf("adoption changed journal state: stats=%+v err=%v", stats, err)
	}
	// An adopted token is immediately usable, so the record does not wait out a
	// claim TTL it never needed.
	if err := j.MarkCommittedBatch([]journal.CommitClaim{{RecordID: adopted[0].Record.RecordID, Token: adopted[0].Token}}); err != nil {
		t.Fatalf("commit an adopted claim: %v", err)
	}

	h.clock.Advance(journal.DefaultClaimTTL)
	expired, err := j.AdoptClaims("worker-a", journal.MaxTransitionRecords)
	if err != nil || len(expired) != 0 {
		t.Fatalf("adopted %d expired claims: %v", len(expired), err)
	}
}

// A worker still retrying a record during a database outage must be able to
// hold its claim instead of letting it expire and churn every claim TTL.
func TestRenewClaimsExtendsExpiryAndRetiresTheOldToken(t *testing.T) {
	h := newHarness(t)
	record := h.one()
	j := h.open()
	defer j.Close()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := j.Claim(1, "worker-a")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	original := claimed[0]

	h.clock.Advance(journal.DefaultClaimTTL - time.Nanosecond)
	renewed, err := j.RenewClaims([]journal.CommitClaim{{RecordID: original.Record.RecordID, Token: original.Token}})
	if err != nil || len(renewed) != 1 {
		t.Fatalf("renew: %v %+v", err, renewed)
	}
	if !renewed[0].ExpiresAt.After(original.ExpiresAt) {
		t.Fatalf("renewal did not extend expiry: %s to %s", original.ExpiresAt, renewed[0].ExpiresAt)
	}
	if renewed[0].Attempt != original.Attempt {
		t.Fatalf("renewal counted a new attempt: %d to %d", original.Attempt, renewed[0].Attempt)
	}

	// Past the original expiry the claim is still held, so no other worker can
	// take the record and the renewed token still commits.
	h.clock.Advance(2 * time.Nanosecond)
	stolen, err := j.Claim(1, "worker-b")
	if err != nil || len(stolen) != 0 {
		t.Fatalf("renewed claim was stolen: %v %+v", err, stolen)
	}
	if err := j.MarkCommittedBatch([]journal.CommitClaim{{RecordID: original.Record.RecordID, Token: original.Token}}); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("retired token error=%v, want a stale claim", err)
	}
	if err := j.MarkCommittedBatch([]journal.CommitClaim{{RecordID: original.Record.RecordID, Token: renewed[0].Token}}); err != nil {
		t.Fatalf("commit a renewed claim: %v", err)
	}
}

func TestRenewClaimsRefusesAStaleOrExpiredClaim(t *testing.T) {
	h := newHarness(t)
	record := h.one()
	j := h.open()
	defer j.Close()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := j.Claim(1, "worker-a")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	forged := claimed[0].Token[:len(claimed[0].Token)-1] + "0"
	if forged == claimed[0].Token {
		forged = claimed[0].Token[:len(claimed[0].Token)-1] + "1"
	}
	if _, err := j.RenewClaims([]journal.CommitClaim{{RecordID: record.RecordID, Token: forged}}); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("forged token error=%v, want a stale claim", err)
	}
	h.clock.Advance(journal.DefaultClaimTTL)
	if _, err := j.RenewClaims([]journal.CommitClaim{{RecordID: record.RecordID, Token: claimed[0].Token}}); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("expired renewal error=%v, want a stale claim", err)
	}
	if _, err := j.RenewClaims(nil); !errors.Is(err, journal.ErrInvalidInput) {
		t.Fatalf("empty renewal error=%v, want invalid input", err)
	}
}

// A lost MarkCommittedBatch response leaves a worker still holding a claim the
// journal has already retired. MarkCommittedBatch treats that as satisfied
// rather than stale, and renewal has to agree: otherwise the one member whose
// response was lost rejects the renewal of every healthy sibling in its cohort,
// and the worker forfeits claims the journal still holds for it.
func TestRenewClaimsIsIdempotentForAnAlreadyCommittedMember(t *testing.T) {
	h := newHarness(t)
	first := h.one()
	second := h.factory.Record(t, builders.WithRecordID(strings.Repeat("c", 64)), builders.WithBatchID(first.BatchID))
	j := h.open()
	defer j.Close()
	if err := j.AppendBatch(first.BatchID, []journal.Admission{
		{Record: first, Priority: journal.PriorityHigh},
		{Record: second, Priority: journal.PriorityHigh},
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := j.Claim(2, "worker-a")
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	cohort := []journal.CommitClaim{
		{RecordID: claimed[0].Record.RecordID, Token: claimed[0].Token},
		{RecordID: claimed[1].Record.RecordID, Token: claimed[1].Token},
	}
	if err := j.MarkCommittedBatch(cohort[:1]); err != nil {
		t.Fatalf("commit the first member: %v", err)
	}

	h.clock.Advance(journal.DefaultClaimTTL / 2)
	renewed, err := j.RenewClaims(cohort)
	if err != nil {
		t.Fatalf("one already-committed member rejected the whole renewal: %v", err)
	}
	if len(renewed) != 1 || renewed[0].Record.RecordID != cohort[1].RecordID {
		t.Fatalf("want only the still-claimed sibling renewed, got %+v", renewed)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != 1 || stats.Claimed != 1 || stats.Pending != 0 {
		t.Fatalf("renewal disturbed the committed member: stats=%+v err=%v", stats, err)
	}
	if err := j.MarkCommittedBatch([]journal.CommitClaim{
		{RecordID: renewed[0].Record.RecordID, Token: renewed[0].Token},
	}); err != nil {
		t.Fatalf("commit the renewed sibling: %v", err)
	}
}

// A committed record whose token does not match is still a stale claim: the
// idempotent path must not become a way to renew someone else's work.
func TestRenewClaimsRefusesACommittedRecordWithAnotherToken(t *testing.T) {
	h := newHarness(t)
	record := h.one()
	j := h.open()
	defer j.Close()
	if err := j.AppendBatch(record.BatchID, []journal.Admission{{Record: record, Priority: journal.PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := j.Claim(1, "worker-a")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	if err := j.MarkCommitted(record.RecordID, claimed[0].Token); err != nil {
		t.Fatal(err)
	}
	forged := claimed[0].Token[:len(claimed[0].Token)-1] + "0"
	if forged == claimed[0].Token {
		forged = claimed[0].Token[:len(claimed[0].Token)-1] + "1"
	}
	if _, err := j.RenewClaims([]journal.CommitClaim{{RecordID: record.RecordID, Token: forged}}); !errors.Is(err, journal.ErrStaleClaim) {
		t.Fatalf("forged token against a committed record error=%v, want a stale claim", err)
	}
}
