package persistence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCheckedLeaseDeadlineRejectsOverflow(t *testing.T) {
	now := time.Unix(0, 1<<63-1).UTC()
	if _, err := safeDeadline(now, time.Nanosecond); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want categorical overflow rejection, got %v", err)
	}
}

// stubValidator reports a fixed policy version and accepts everything. It only
// has to make the batch-wide configuration comparison reachable.
type stubValidator struct{ version string }

func (stubValidator) ValidateRecord(model.NormalizedLog) error { return nil }
func (stubValidator) ValidateValue(model.SafeValue) error      { return nil }
func (stubValidator) ValidateText(string) error                { return nil }
func (v stubValidator) Version() string                        { return v.version }

// A batch this Store cannot serve at all must be distinguishable from a
// malformed record, because a caller isolating record-local poison destroys the
// durable payload it isolates.
func TestBatchWideConfigurationFailureIsDistinctFromRecordInvalidity(t *testing.T) {
	store := &Store{
		// The connection is never reached: configuration is decided first.
		pool: new(pgxpool.Pool), validator: stubValidator{version: "redact:test:v1"},
		scope: Scope{Region: "us-east-1", TenantID: "tenant-a"}, classification: "SENSITIVE",
	}
	served := ProcessInput{Scope: store.scope}
	served.Record.Redaction.PolicyVersion = "redact:test:v1"

	foreignScope := served
	foreignScope.Scope = Scope{Region: "eu-west-1", TenantID: "tenant-a"}
	if _, err := store.ProcessBatch(context.Background(), []ProcessInput{foreignScope}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("foreign scope error=%v, want a configuration error", err)
	}

	stalePolicy := served
	stalePolicy.Record.Redaction.PolicyVersion = "redact:test:v0"
	if _, err := store.ProcessBatch(context.Background(), []ProcessInput{stalePolicy}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("stale policy version error=%v, want a configuration error", err)
	}
	if _, err := store.ProcessBatch(context.Background(), []ProcessInput{stalePolicy}); errors.Is(err, ErrInvalidInput) {
		t.Fatal("a configuration error was also reported as record-local invalidity")
	}

	// The record itself carries no durable identity, which remains record-local.
	if _, err := store.ProcessBatch(context.Background(), []ProcessInput{served}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unusable record error=%v, want record-local invalidity", err)
	}
}
