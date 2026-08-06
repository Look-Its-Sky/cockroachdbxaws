package identity_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/identity"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

const (
	instance = "collector-us-east-1-0"
	uid      = "0194f0a0-0000-7000-8000-000000000001"
)

func TestOTLPV1MatchesTheDocumentedDerivation(t *testing.T) {
	// SHA-256("otlp:v1" || trusted_source_instance || log.record.uid)
	digest := sha256.Sum256([]byte("otlp:v1" + instance + uid))
	want := hex.EncodeToString(digest[:])

	got, err := identity.OTLPV1(instance, uid)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestTheSameInputsAlwaysProduceTheSameIdentity(t *testing.T) {
	first, err := identity.OTLPV1(instance, uid)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}

	// This is the whole point: a retried record must hash to the value it
	// hashed to before the process restarted, and before this release.
	for i := 0; i < 100; i++ {
		again, err := identity.OTLPV1(instance, uid)
		if err != nil {
			t.Fatalf("deriving: %v", err)
		}
		if again != first {
			t.Fatalf("derivation is not stable: %s then %s", first, again)
		}
	}
}

func TestIdentityHasTheShapeItsVersionDeclares(t *testing.T) {
	got, err := identity.OTLPV1(instance, uid)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}

	// A record whose identifier does not match its version's shape is rejected
	// by validation, so the deriver has to produce that shape.
	record := model.NormalizedLog{RecordID: got, RecordIDVersion: model.RecordIDVersionOTLPV1}
	if err := record.Validate(); err != nil {
		var validation *model.ValidationError
		if ok := asValidation(err, &validation); ok && validation.Has("record_id") {
			t.Fatalf("derived identity %s does not have the shape its version declares: %v", got, err)
		}
	}
}

func TestADifferentSourceInstanceProducesADifferentIdentity(t *testing.T) {
	first, _ := identity.OTLPV1(instance, uid)
	second, _ := identity.OTLPV1("collector-us-east-1-1", uid)

	// A record uid is a claim, not an authorization. Two sources that chose the
	// same uid must not collide, or one could overwrite the other's history.
	if first == second {
		t.Fatalf("want distinct identities per source instance, both were %s", first)
	}
}

func TestADifferentRecordUIDProducesADifferentIdentity(t *testing.T) {
	first, _ := identity.OTLPV1(instance, uid)
	second, _ := identity.OTLPV1(instance, "0194f0a0-0000-7000-8000-000000000002")

	if first == second {
		t.Fatalf("want distinct identities per record, both were %s", first)
	}
}

func TestUnusableInputsAreRefused(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		uid      string
		want     string
		because  string
	}{
		{
			name:     "missing source instance",
			instance: "",
			uid:      uid,
			want:     "source instance",
			because:  "without it a uid alone is forgeable by another source",
		},
		{
			name:     "whitespace source instance",
			instance: "   ",
			uid:      uid,
			want:     "source instance",
			because:  "a blank instance is a missing instance",
		},
		{
			name:     "missing uid",
			instance: instance,
			uid:      "",
			want:     "record uid",
			because:  "a producer without a uid uses the derived identity version",
		},
		{
			name:     "uid that is not a uuid",
			instance: instance,
			uid:      "record-1",
			want:     "record uid",
			because:  "a non-conforming uid cannot be shown to be stable across a retry",
		},
		{
			name:     "uid that is not version 7",
			instance: instance,
			uid:      "9f8b7c6d-5e4f-4a3b-8c9d-0e1f2a3b4c5d",
			want:     "record uid",
			because:  "the contract requires a UUIDv7-compatible identifier",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := identity.OTLPV1(test.instance, test.uid)

			if err == nil {
				t.Fatalf("want a refusal because %s, got %s", test.because, got)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want the error to name %s, got %v", test.want, err)
			}
			if got != "" {
				t.Errorf("want no identity alongside a refusal, got %s", got)
			}
		})
	}
}

func asValidation(err error, target **model.ValidationError) bool {
	validation, ok := err.(*model.ValidationError)
	if ok {
		*target = validation
	}
	return ok
}
