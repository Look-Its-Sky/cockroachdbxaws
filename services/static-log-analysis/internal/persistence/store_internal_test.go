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

func TestSingleRegionTopologyClassificationRejectsCrossRegionShapes(t *testing.T) {
	value := func(text string) *string { return &text }
	tests := []struct {
		name      string
		primary   *string
		secondary *string
		count     int
		region    *string
		survival  *string
	}{
		{name: "two regions", primary: value("aws-us-east-2"), count: 2, region: value("aws-us-east-2"), survival: value("zone")},
		{name: "secondary region", primary: value("aws-us-east-2"), secondary: value("aws-us-west-2"), count: 1, region: value("aws-us-east-2"), survival: value("zone")},
		{name: "region differs from primary", primary: value("aws-us-east-2"), count: 1, region: value("aws-us-west-2"), survival: value("zone")},
		{name: "region survival", primary: value("aws-us-east-2"), count: 1, region: value("aws-us-east-2"), survival: value("region")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySingleRegionTopology(test.primary, test.secondary, test.count, test.region, test.survival); got != topologyIncompatible {
				t.Fatalf("cross-region topology classified as %v", got)
			}
		})
	}
}

func TestSingleRegionTopologyClassificationAcceptsPortableAndOneRegionManagedDatabases(t *testing.T) {
	value := func(text string) *string { return &text }
	if got := classifySingleRegionTopology(nil, nil, 0, nil, nil); got != topologyPortable {
		t.Fatalf("portable topology=%v", got)
	}
	if got := classifySingleRegionTopology(value("aws-us-east-2"), nil, 1, value("aws-us-east-2"), value("zone")); got != topologyCockroachRegional {
		t.Fatalf("one-region managed topology=%v", got)
	}
}

func TestCatalogChecksumsCannotCrossTopologyOrAcceptAnUnknownVersion(t *testing.T) {
	if !catalogChecksumIsExpected(topologyPortable, expectedPortableCatalogChecksumV2625) {
		t.Fatal("reviewed portable checksum rejected")
	}
	if !catalogChecksumIsExpected(topologyCockroachRegional, expectedRegionalCatalogChecksumV2625) {
		t.Fatal("reviewed one-region managed checksum rejected")
	}
	if !catalogChecksumIsExpected(topologyCockroachRegional, expectedLockedRegionalCatalogChecksumV2625) {
		t.Fatal("reviewed schema-locked managed checksum rejected")
	}
	if catalogChecksumIsExpected(topologyPortable, expectedRegionalCatalogChecksumV2625) {
		t.Fatal("managed locality checksum accepted as portable")
	}
	if catalogChecksumIsExpected(topologyCockroachRegional, "unknown") {
		t.Fatal("unknown catalog checksum accepted")
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
