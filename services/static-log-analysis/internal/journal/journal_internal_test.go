package journal

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/gen/internalv1"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/golden"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/batchrepr"
	"github.com/cockroachdb/pebble/v2/vfs"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func internalConfig(dir string, c *fakeclock.Clock) Config {
	return Config{Dir: dir, Owner: "owner", TenantID: "tenant", Region: builders.DefaultRegion, Classification: "SENSITIVE", Clock: c, Validator: redact.MinimalPolicy(), MaxBytes: 64 << 20, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }}
}

func TestBinaryKeyEncodingGolden(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	actual := "batch " + hex.EncodeToString(batchKey("a/b")) + "\n" + "record " + hex.EncodeToString(recordKey("ab/c")) + "\n" + "pending " + hex.EncodeToString(pendingKey(PriorityHigh, at, "record")) + "\n" + "claim " + hex.EncodeToString(claimKey(at, "record")) + "\n" + "batch_ref " + hex.EncodeToString(batchRefKey("a", "b/c")) + "\n" + "batch_original_ref " + hex.EncodeToString(batchOriginalRefKey("a", "b/c")) + "\n"
	golden.String(t, "journal_keys.hex", actual)
}

func TestIncompatibleFutureDirectoryFormatRefusesStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig(dir, c)
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := pebble.Open(dir, &pebble.Options{Logger: discardLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	payload := binary.BigEndian.AppendUint16(nil, FormatMajor)
	payload = binary.BigEndian.AppendUint16(payload, FormatMinor+1)
	if err := db.Set(keyFormat, checked([]byte("JFM1"), payload), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(cfg); !errors.Is(err, ErrIncompatibleFormat) {
		t.Fatalf("want incompatible format, got %v", err)
	}
}

func TestCorruptionRefusesReopenWithoutSkippingRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig(dir, c)
	factory := builders.NewFactory(builders.WithClock(c))
	r := factory.Record(t)
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendBatch(r.BatchID, []Admission{{Record: r, Priority: PriorityNormal}}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	db, err := pebble.Open(dir, &pebble.Options{Logger: discardLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	value, closer, err := db.Get(recordKey(r.RecordID))
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), value...)
	_ = closer.Close()
	damaged[len(damaged)-1] ^= 0xff
	if err := db.Set(recordKey(r.RecordID), damaged, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(cfg); !errors.Is(err, ErrCorruption) {
		t.Fatalf("want corruption refusal, got %v", err)
	}
}

func TestDetectedCorruptionLatchesUnreadyAndStopsAcceptingAndClaiming(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig(dir, c)
	r := builders.NewFactory(builders.WithClock(c)).Record(t)
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.AppendBatch(r.BatchID, []Admission{{Record: r, Priority: PriorityNormal}}); err != nil {
		t.Fatal(err)
	}
	value, found, err := get(j.db, recordKey(r.RecordID))
	if err != nil || !found {
		t.Fatal("record missing")
	}
	value[len(value)-1] ^= 1
	if err := j.db.Set(recordKey(r.RecordID), value, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Claim(1, "worker"); !errors.Is(err, ErrCorruption) {
		t.Fatalf("want corruption detection, got %v", err)
	}
	if j.Ready() {
		t.Fatal("corrupt journal remained ready")
	}
	another := builders.NewFactory(builders.WithClock(c)).Record(t)
	if err := j.AppendBatch(another.BatchID, []Admission{{Record: another, Priority: PriorityNormal}}); !errors.Is(err, ErrNotHealthy) {
		t.Fatalf("want acceptance stopped, got %v", err)
	}
}

func TestLiveCorruptFormatIsNotSilentlyRepairedByAppend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig(dir, c)
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	damaged := formatValue()
	damaged[len(damaged)-1] ^= 1
	if err := j.db.Set(keyFormat, damaged, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	record := builders.NewFactory(builders.WithClock(c)).Record(t)
	if err := j.AppendBatch(record.BatchID, []Admission{{Record: record, Priority: PriorityNormal}}); !errors.Is(err, ErrCorruption) {
		t.Fatalf("want format corruption, got %v", err)
	}
	if j.Ready() {
		t.Fatal("format corruption did not latch unready")
	}
	if got := mustGet(t, j.db, keyFormat); !reflect.DeepEqual(got, damaged) {
		t.Fatal("append silently repaired corrupt format")
	}
}

func TestPriorityAccountingCorruptionRefusesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig(dir, c)
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := builders.NewFactory(builders.WithClock(c)).Record(t)
	if err := j.AppendBatch(r.BatchID, []Admission{{Record: r, Priority: PriorityHigh}}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	db, err := pebble.Open(dir, &pebble.Options{Logger: discardLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Set(keyPriority, priorityValue([4]uint64{}), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(cfg); !errors.Is(err, ErrCorruption) {
		t.Fatalf("want priority corruption refusal, got %v", err)
	}
}

func TestDurableEnvelopeRoundTripValidatesConfiguredBoundary(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	r := builders.NewFactory(builders.WithClock(c)).Record(t)
	encoded, err := encodeDurable(r, "tenant-a", "SENSITIVE")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeDurable(encoded, "tenant-a", "SENSITIVE")
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RecordID != r.RecordID {
		t.Fatal("identity changed")
	}
	if _, err := decodeDurable(encoded, "tenant-b", "SENSITIVE"); err == nil {
		t.Fatal("want tenant contradiction rejected")
	}
	forged := r
	forged.Body = model.SafeValue{Kind: model.SafeValueKind("unknown")}
	if _, err := encodeDurable(forged, "tenant-a", "SENSITIVE"); err == nil {
		t.Fatal("want unknown safe value rejected")
	}
}

func TestDurableEnvelopeRejectsInvalidCommonTimestamp(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	r := builders.NewFactory(builders.WithClock(c)).Record(t)
	encoded, err := encodeDurable(r, "tenant-a", "SENSITIVE")
	if err != nil {
		t.Fatal(err)
	}
	var message internalv1.DurableNormalizedLog
	if err := proto.Unmarshal(encoded, &message); err != nil {
		t.Fatal(err)
	}
	message.CreatedAt.Nanos = 1_000_000_000
	forged, err := proto.Marshal(&message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeDurable(forged, "tenant-a", "SENSITIVE"); err == nil {
		t.Fatal("want invalid common timestamp rejected")
	}
}

func TestStoredOptionalTimesPreserveMinimumUnixNano(t *testing.T) {
	minimum := time.Unix(0, math.MinInt64).UTC()
	record := storedRecord{
		state:       StateCommitted,
		priority:    PriorityCritical,
		received:    minimum,
		committedAt: minimum,
		attempt:     1,
		claimToken:  "claim:" + strings.Repeat("a", 64),
		envelope:    []byte("encoded"),
	}
	decoded, err := decodeStored(encodeStored(record))
	if err != nil || !decoded.received.Equal(minimum) || !decoded.committedAt.Equal(minimum) {
		t.Fatalf("minimum UnixNano did not round trip: %v %#v", err, decoded)
	}
}

func TestDurableProtobufDeterministicCompleteRoundTripAndCompatibility(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	r := builders.NewFactory(builders.WithClock(c)).Record(t)
	r.Body = model.SafeString("")
	r.Attributes = map[string]model.SafeValue{
		"bool-zero":   model.SafeBool(false),
		"double-zero": model.SafeDouble(0),
		"int-zero":    model.SafeInt(0),
		"list-empty":  model.SafeSlice(),
		"list-nested": model.SafeSlice(model.SafeString("safe"), model.SafeMap(map[string]model.SafeValue{"nested": model.SafeInt(7)})),
		"map-empty":   model.SafeMap(map[string]model.SafeValue{}),
		"string-zero": model.SafeString(""),
		"withheld":    model.Withheld("categorical_reason"),
	}
	r.ResourceAttributes = map[string]model.SafeValue{"resource": model.SafeBool(true)}
	r.ScopeAttributes = map[string]model.SafeValue{"scope": model.SafeDouble(1.25)}
	r.Exception = &model.NormalizedException{Type: "SafeError", SafeMessage: "safe message", StackFrames: []model.StackFrame{{Function: "safeFunction", Module: "safe/module", File: "safe.go", InApplication: true}}}
	r.Correlation = model.CorrelationIdentity{TraceID: "00112233445566778899aabbccddeeff", SpanID: "0011223344556677"}
	r.RawReference = &model.RegionalLogReference{SourceType: model.SourceTypeCloudWatch, Region: r.Region, Locator: "safe-locator", From: c.Now().Add(-time.Minute), To: c.Now(), Classification: "SENSITIVE", ExpiresAt: c.Now().Add(time.Hour)}
	r.Redaction.RuleIDs = []string{"rule.a", "rule.b"}
	r.Redaction.WithheldFields = []string{"attributes.7"}
	r.ObservedTimeInferred = true
	r.ObservedTimeInferenceReason = "observed_time_missing_event_time_used"

	first, err := encodeDurable(r, "tenant", "SENSITIVE")
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeDurable(r, "tenant", "SENSITIVE")
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("protobuf encoding is not deterministic: %v", err)
	}
	decoded, err := decodeDurable(first, "tenant", "SENSITIVE")
	expected := r
	expected.Attributes = make(map[string]model.SafeValue, len(r.Attributes))
	for key, value := range r.Attributes {
		expected.Attributes[key] = value
	}
	// Proto maps have one canonical empty representation, so a present empty
	// map and nil map intentionally decode to the same SafeMap value.
	expected.Attributes["map-empty"] = model.SafeMap(nil)
	expected.Attributes["list-empty"] = model.SafeValue{Kind: model.SafeKindSlice, Slice: []model.SafeValue{}}
	if err != nil || !reflect.DeepEqual(decoded, expected) {
		t.Fatalf("complete round trip: %v\nwant=%#v\ngot=%#v", err, expected, decoded)
	}

	// A same-major future minor and unknown wire fields are accepted. Pebble
	// retains the original envelope bytes, so a reader never rewrites and drops
	// unknown identity-relevant data.
	var future internalv1.DurableNormalizedLog
	if err := proto.Unmarshal(first, &future); err != nil {
		t.Fatal(err)
	}
	future.SchemaVersion = "1.9"
	future.Log.SchemaVersion = "1.9"
	envelopeUnknown := protowire.AppendTag(nil, 127, protowire.BytesType)
	envelopeUnknown = protowire.AppendString(envelopeUnknown, "future-envelope")
	logUnknown := protowire.AppendTag(nil, 126, protowire.BytesType)
	logUnknown = protowire.AppendString(logUnknown, "future-log")
	future.ProtoReflect().SetUnknown(envelopeUnknown)
	future.Log.ProtoReflect().SetUnknown(logUnknown)
	compatible, err := proto.MarshalOptions{Deterministic: true}.Marshal(&future)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = decodeDurable(compatible, "tenant", "SENSITIVE")
	if err != nil || decoded.SchemaVersion != "1.9" {
		t.Fatalf("same-major compatible payload: %v %#v", err, decoded)
	}
	knownDigest, err := semanticDigestEnvelope(first)
	if err != nil {
		t.Fatal(err)
	}
	futureDigest, err := semanticDigestEnvelope(compatible)
	if err != nil || knownDigest == futureDigest {
		t.Fatalf("unknown identity-relevant fields were dropped from digest: %v", err)
	}
	if _, err := decodeDurable(append(first, 0xff), "tenant", "SENSITIVE"); err == nil {
		t.Fatal("malformed protobuf accepted")
	}

	nilMaps := r
	nilMaps.Attributes = nil
	nilMaps.ResourceAttributes = nil
	nilMaps.ScopeAttributes = nil
	emptyMaps := nilMaps
	emptyMaps.Attributes = map[string]model.SafeValue{}
	emptyMaps.ResourceAttributes = map[string]model.SafeValue{}
	emptyMaps.ScopeAttributes = map[string]model.SafeValue{}
	nilEncoded, _ := encodeDurable(nilMaps, "tenant", "SENSITIVE")
	emptyEncoded, _ := encodeDurable(emptyMaps, "tenant", "SENSITIVE")
	if !reflect.DeepEqual(nilEncoded, emptyEncoded) {
		t.Fatal("nil and empty protobuf maps must have one canonical encoding")
	}
}

func TestEncodedNormalizedRecordBoundaryExactAndOver(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	r := builders.NewFactory(builders.WithClock(c)).Record(t)
	low, high := 0, MaxEncodedRecordBytes
	for low < high {
		mid := low + (high-low+1)/2
		r.Body = model.SafeString(strings.Repeat("x", mid))
		if encodedNormalizedSize(r) <= MaxEncodedRecordBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	r.Body = model.SafeString(strings.Repeat("x", low))
	if got := encodedNormalizedSize(r); got != MaxEncodedRecordBytes {
		t.Fatalf("test fixture could not reach exact boundary: got %d", got)
	}
	dir := filepath.Join(t.TempDir(), "journal")
	cfg := internalConfig(dir, c)
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.AppendBatch(r.BatchID, []Admission{{Record: r, Priority: PriorityNormal}}); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}
	over := builders.NewFactory(builders.WithClock(c)).Record(t)
	over.Body = model.SafeString(strings.Repeat("x", low+1))
	if got := encodedNormalizedSize(over); got != MaxEncodedRecordBytes+1 {
		t.Fatalf("want exactly one byte over, got %d", got)
	}
	if err := j.AppendBatch(over.BatchID, []Admission{{Record: over, Priority: PriorityNormal}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("one byte over accepted: %v", err)
	}
}

func TestCorruptionMatrixRefusesReopen(t *testing.T) {
	tests := []struct {
		name    string
		maxRefs int
		mutate  func(*testing.T, *pebble.DB, Config, []model.NormalizedLog)
	}{
		{
			name: "unsorted-batch-members",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				batch := mustDecodeBatch(t, db, records[0].BatchID)
				batch.original[0], batch.original[1] = batch.original[1], batch.original[0]
				mustSet(t, db, batchKey(records[0].BatchID), encodeBatch(batch))
			},
		},
		{
			name: "duplicate-batch-members",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				batch := mustDecodeBatch(t, db, records[0].BatchID)
				batch.original[1] = batch.original[0]
				mustSet(t, db, batchKey(records[0].BatchID), encodeBatch(batch))
			},
		},
		{
			name:    "excessive-batch-refs",
			maxRefs: 1,
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				stored, err := decodeStored(mustGet(t, db, recordKey(records[0].RecordID)))
				if err != nil {
					t.Fatal(err)
				}
				batch := storedBatch{original: []storedBatchMember{{recordID: records[0].RecordID, priority: stored.priority, semantic: stored.semantic}}, live: []string{records[0].RecordID}}
				mustSet(t, db, batchKey("second-batch"), encodeBatch(batch))
				mustSet(t, db, batchRefKey(records[0].RecordID, "second-batch"), nil)
				mustSet(t, db, batchOriginalRefKey(records[0].RecordID, "second-batch"), nil)
			},
		},
		{
			name: "missing-original-batch-reference",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				mustDelete(t, db, batchOriginalRefKey(records[0].RecordID, records[0].BatchID))
			},
		},
		{
			name: "record-and-quarantine",
			mutate: func(t *testing.T, db *pebble.DB, cfg Config, records []model.NormalizedLog) {
				mustSet(t, db, quarantineKey(records[0].RecordID), encodeQuarantine(records[0].RecordID, QuarantineUnsupportedData, cfg.Clock.Now(), 1))
			},
		},
		{
			name: "invalid-quarantine-attempt",
			mutate: func(t *testing.T, db *pebble.DB, cfg Config, _ []model.NormalizedLog) {
				id := strings.Repeat("a", 64)
				mustSet(t, db, quarantineKey(id), encodeQuarantine(id, QuarantineUnsupportedData, cfg.Clock.Now(), 0))
			},
		},
		{
			name: "invalid-quarantine-time",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, _ []model.NormalizedLog) {
				id := strings.Repeat("a", 64)
				mustSet(t, db, quarantineKey(id), encodeQuarantine(id, QuarantineUnsupportedData, time.Time{}, 1))
			},
		},
		{
			name: "invalid-quarantine-key-identity",
			mutate: func(t *testing.T, db *pebble.DB, cfg Config, records []model.NormalizedLog) {
				otherID := strings.Repeat("a", 64)
				mustSet(t, db, quarantineKey(otherID), encodeQuarantine(records[0].RecordID, QuarantineUnsupportedData, cfg.Clock.Now(), 1))
			},
		},
		{
			name: "reserve-bit-corruption",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				value := mustGet(t, db, transitionReserveKey(records[0].RecordID))
				value[len(value)-1] ^= 1
				mustSet(t, db, transitionReserveKey(records[0].RecordID), value)
			},
		},
		{
			name: "duplicate-state-index",
			mutate: func(t *testing.T, db *pebble.DB, cfg Config, records []model.NormalizedLog) {
				mustSet(t, db, claimKey(cfg.Clock.Now().Add(time.Minute), records[0].RecordID), nil)
			},
		},
		{
			name: "wrong-index-timestamp",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				mustDelete(t, db, pendingKey(PriorityHigh, records[0].Source.ReceivedAt, records[0].RecordID))
				mustSet(t, db, pendingKey(PriorityHigh, records[0].Source.ReceivedAt.Add(time.Nanosecond), records[0].RecordID), nil)
			},
		},
		{
			name: "nonempty-index-value",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				mustSet(t, db, pendingKey(PriorityHigh, records[0].Source.ReceivedAt, records[0].RecordID), []byte("unexpected"))
			},
		},
		{
			name: "stale-batch-reference",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				mustSet(t, db, batchRefKey(records[0].RecordID, "missing-batch"), nil)
			},
		},
		{
			name: "stored-received-time",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				stored, err := decodeStored(mustGet(t, db, recordKey(records[0].RecordID)))
				if err != nil {
					t.Fatal(err)
				}
				mustDelete(t, db, pendingKey(stored.priority, stored.received, records[0].RecordID))
				stored.received = stored.received.Add(time.Nanosecond)
				mustSet(t, db, recordKey(records[0].RecordID), encodeStored(stored))
				mustSet(t, db, pendingKey(stored.priority, stored.received, records[0].RecordID), nil)
			},
		},
		{
			name: "envelope",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, records []model.NormalizedLog) {
				stored, err := decodeStored(mustGet(t, db, recordKey(records[0].RecordID)))
				if err != nil {
					t.Fatal(err)
				}
				stored.envelope = append(stored.envelope, 0xff)
				mustSet(t, db, recordKey(records[0].RecordID), encodeStored(stored))
			},
		},
		{
			name: "accounting",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, _ []model.NormalizedLog) {
				usage, ok := decodeUsage(mustGet(t, db, keyUsage))
				if !ok {
					t.Fatal("usage did not decode")
				}
				mustSet(t, db, keyUsage, usageValue(usage+1))
			},
		},
		{
			name: "priority-accounting",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, _ []model.NormalizedLog) {
				mustSet(t, db, keyPriority, priorityValue([4]uint64{}))
			},
		},
		{
			name: "boundary-manifest",
			mutate: func(t *testing.T, db *pebble.DB, _ Config, _ []model.NormalizedLog) {
				value := mustGet(t, db, keyBoundary)
				value[len(value)-1] ^= 1
				mustSet(t, db, keyBoundary, value)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "journal")
			c := fakeclock.NewAtOrigin()
			cfg := internalConfig(dir, c)
			if test.maxRefs != 0 {
				cfg.MaxBatchRefs = test.maxRefs
			}
			records := builders.NewFactory(builders.WithClock(c)).Batch(t, 2)
			j, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.AppendBatch(records[0].BatchID, []Admission{{Record: records[0], Priority: PriorityHigh}, {Record: records[1], Priority: PriorityLow}}); err != nil {
				t.Fatal(err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := pebble.Open(dir, &pebble.Options{Logger: discardLogger{}})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, db, cfg, records)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(cfg); !errors.Is(err, ErrCorruption) {
				if reopened != nil {
					_ = reopened.Close()
				}
				t.Fatalf("want corruption refusal, got %v", err)
			}
		})
	}
}

func mustGet(t *testing.T, db *pebble.DB, key []byte) []byte {
	t.Helper()
	value, closer, err := db.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	return append([]byte(nil), value...)
}

func mustSet(t *testing.T, db *pebble.DB, key, value []byte) {
	t.Helper()
	if err := db.Set(key, value, pebble.Sync); err != nil {
		t.Fatal(err)
	}
}

func mustDelete(t *testing.T, db *pebble.DB, key []byte) {
	t.Helper()
	if err := db.Delete(key, pebble.Sync); err != nil {
		t.Fatal(err)
	}
}

func TestCrashablePebbleSyncedAtomicAppendSurvivesPowerLoss(t *testing.T) {
	filesystem := vfs.NewCrashableMem()
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig("/journal", c)
	options := &pebble.Options{FS: filesystem, Logger: discardLogger{}}
	j, err := openWithOptions(cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	records := builders.NewFactory(builders.WithClock(c)).Batch(t, 3)
	admissions := make([]Admission, 0, len(records))
	for _, record := range records {
		admissions = append(admissions, Admission{Record: record, Priority: PriorityHigh})
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	crashed := filesystem.CrashClone(vfs.CrashCloneCfg{UnsyncedDataPercent: 0})
	// Clone first: closing the abandoned process would flush writes and is not
	// part of the simulated power loss.
	_ = j.db.Close()
	_ = j.lock.Close()
	j.closed = true

	reopened, err := openWithOptions(cfg, &pebble.Options{FS: crashed, Logger: discardLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats, err := reopened.Stats()
	if err != nil || stats.Pending != 3 {
		t.Fatalf("synced all-or-none replay: %v %+v", err, stats)
	}
}

func TestAtomicAppendBatchContainsFormatAndEveryRecordAndUsesSync(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig("/journal", c)
	j, err := openWithOptions(cfg, &pebble.Options{FS: vfs.NewMem(), Logger: discardLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	originalCommit := j.commit
	var sawFormat bool
	var recordWrites int
	j.commit = func(batch *pebble.Batch, options *pebble.WriteOptions) error {
		if options == nil || !options.Sync {
			t.Fatal("append did not request Pebble Sync")
		}
		reader := batchrepr.Read(batch.Repr())
		for {
			_, key, value, ok, err := reader.Next()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if reflect.DeepEqual(key, keyFormat) && reflect.DeepEqual(value, formatValue()) {
				sawFormat = true
			}
			if len(key) >= 2 && key[0] == keyVersion && key[1] == nsRecord {
				recordWrites++
			}
		}
		return originalCommit(batch, options)
	}
	records := builders.NewFactory(builders.WithClock(c)).Batch(t, 2)
	if err := j.AppendBatch(records[0].BatchID, []Admission{{Record: records[0], Priority: PriorityHigh}, {Record: records[1], Priority: PriorityLow}}); err != nil {
		t.Fatal(err)
	}
	if !sawFormat || recordWrites != len(records) {
		t.Fatalf("atomic append contents: format=%v records=%d", sawFormat, recordWrites)
	}
}

func TestInjectedSynchronizedWriteFailureLatchesUnhealthy(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig("/journal", c)
	j, err := openWithOptions(cfg, &pebble.Options{FS: vfs.NewMem(), Logger: discardLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	j.commit = func(*pebble.Batch, *pebble.WriteOptions) error {
		return errors.New("injected synchronized commit failure")
	}
	record := builders.NewFactory(builders.WithClock(c)).Record(t)
	if err := j.AppendBatch(record.BatchID, []Admission{{Record: record, Priority: PriorityHigh}}); !errors.Is(err, ErrCorruption) {
		t.Fatalf("want categorical write failure, got %v", err)
	}
	if j.Ready() {
		t.Fatal("write failure did not latch journal unhealthy")
	}
	if _, found, err := get(j.db, recordKey(record.RecordID)); err != nil || found {
		t.Fatalf("failed append became visible: found=%v err=%v", found, err)
	}
	if err := j.AppendBatch(record.BatchID, []Admission{{Record: record, Priority: PriorityHigh}}); !errors.Is(err, ErrNotHealthy) {
		t.Fatalf("unhealthy journal accepted another write: %v", err)
	}
}

func TestTouchedTransitionCorruptionLatchesUnready(t *testing.T) {
	t.Run("claim-pending-index-value", func(t *testing.T) {
		j, cfg, record := openPendingRecord(t)
		defer j.Close()
		mustSet(t, j.db, pendingKey(PriorityHigh, record.Source.ReceivedAt, record.RecordID), []byte("corrupt"))
		assertCorruptAndUnready(t, j, func() error {
			_, err := j.Claim(1, "worker")
			return err
		})
		_ = cfg
	})

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Journal, model.NormalizedLog)
	}{
		{name: "missing-reserve", mutate: func(t *testing.T, j *Journal, record model.NormalizedLog) {
			mustDelete(t, j.db, transitionReserveKey(record.RecordID))
		}},
		{name: "corrupt-reserve", mutate: func(t *testing.T, j *Journal, record model.NormalizedLog) {
			value := mustGet(t, j.db, transitionReserveKey(record.RecordID))
			value[len(value)-1] ^= 1
			mustSet(t, j.db, transitionReserveKey(record.RecordID), value)
		}},
	} {
		t.Run("claim-"+test.name, func(t *testing.T) {
			j, _, record := openPendingRecord(t)
			defer j.Close()
			test.mutate(t, j, record)
			assertCorruptAndUnready(t, j, func() error {
				_, err := j.Claim(1, "worker")
				return err
			})
		})
	}

	t.Run("expiry-claim-index-value", func(t *testing.T) {
		j, cfg, record := openPendingRecord(t)
		defer j.Close()
		claimed, err := j.Claim(1, "worker")
		if err != nil {
			t.Fatal(err)
		}
		mustSet(t, j.db, claimKey(claimed[0].ExpiresAt, record.RecordID), []byte("corrupt"))
		cfg.Clock.(*fakeclock.Clock).Advance(DefaultClaimTTL)
		assertCorruptAndUnready(t, j, func() error {
			_, err := j.Claim(1, "worker-2")
			return err
		})
	})

	for _, operation := range []string{"commit", "quarantine"} {
		t.Run(operation+"-claim-index", func(t *testing.T) {
			j, _, record := openPendingRecord(t)
			defer j.Close()
			claimed, err := j.Claim(1, "worker")
			if err != nil {
				t.Fatal(err)
			}
			mustDelete(t, j.db, claimKey(claimed[0].ExpiresAt, record.RecordID))
			assertCorruptAndUnready(t, j, func() error {
				if operation == "commit" {
					return j.MarkCommitted(record.RecordID, claimed[0].Token)
				}
				return j.Quarantine(record.RecordID, claimed[0].Token, QuarantineUnsupportedData)
			})
		})
	}

	t.Run("compact-committed-index-value", func(t *testing.T) {
		j, cfg, record := openPendingRecord(t)
		defer j.Close()
		claimed, _ := j.Claim(1, "worker")
		if err := j.MarkCommitted(record.RecordID, claimed[0].Token); err != nil {
			t.Fatal(err)
		}
		committedAt := cfg.Clock.Now()
		mustSet(t, j.db, committedKey(committedAt, record.RecordID), []byte("corrupt"))
		cfg.Clock.(*fakeclock.Clock).Advance(DefaultSafetyDelay + time.Nanosecond)
		assertCorruptAndUnready(t, j, func() error {
			_, err := j.Compact(1)
			return err
		})
	})

	t.Run("append-existing-batch-unsorted", func(t *testing.T) {
		j, cfg, records, admissions := openPendingBatch(t, 2)
		defer j.Close()
		batch := mustDecodeBatch(t, j.db, records[0].BatchID)
		sort.Slice(batch.original, func(i, k int) bool { return batch.original[i].recordID > batch.original[k].recordID })
		mustSet(t, j.db, batchKey(records[0].BatchID), encodeBatch(batch))
		assertCorruptAndUnready(t, j, func() error { return j.AppendBatch(records[0].BatchID, admissions) })
		_ = cfg
	})

	t.Run("append-existing-batch-duplicate", func(t *testing.T) {
		j, _, records, admissions := openPendingBatch(t, 2)
		defer j.Close()
		batch := mustDecodeBatch(t, j.db, records[0].BatchID)
		batch.original[1] = batch.original[0]
		mustSet(t, j.db, batchKey(records[0].BatchID), encodeBatch(batch))
		assertCorruptAndUnready(t, j, func() error { return j.AppendBatch(records[0].BatchID, admissions) })
	})

	t.Run("append-existing-batch-missing-reverse-ref", func(t *testing.T) {
		j, _, records, admissions := openPendingBatch(t, 2)
		defer j.Close()
		mustDelete(t, j.db, batchRefKey(records[0].RecordID, records[0].BatchID))
		assertCorruptAndUnready(t, j, func() error { return j.AppendBatch(records[0].BatchID, admissions) })
	})

	t.Run("append-existing-batch-missing-original-ref", func(t *testing.T) {
		j, _, records, admissions := openPendingBatch(t, 2)
		defer j.Close()
		mustDelete(t, j.db, batchOriginalRefKey(records[0].RecordID, records[0].BatchID))
		assertCorruptAndUnready(t, j, func() error { return j.AppendBatch(records[0].BatchID, admissions) })
	})

	t.Run("append-cross-batch-missing-original-ref", func(t *testing.T) {
		j, cfg, records, _ := openPendingBatch(t, 2)
		originalBatchID := records[0].BatchID
		mustDelete(t, j.db, batchOriginalRefKey(records[0].RecordID, originalBatchID))
		retry := records[0]
		retry.BatchID = "cross-batch-retry"
		assertCorruptAndUnready(t, j, func() error {
			return j.AppendBatch(retry.BatchID, []Admission{{Record: retry, Priority: PriorityHigh}})
		})
		if _, found, err := get(j.db, batchKey(retry.BatchID)); err != nil || found {
			t.Fatalf("rejected cross-batch append became durable: found=%v err=%v", found, err)
		}
		mustSet(t, j.db, batchOriginalRefKey(records[0].RecordID, originalBatchID), nil)
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(cfg)
		if err != nil {
			t.Fatalf("reopen after restoring injected corruption: %v", err)
		}
		defer reopened.Close()
		if _, found, err := get(reopened.db, batchKey(retry.BatchID)); err != nil || found {
			t.Fatalf("rejected cross-batch append visible after reopen: found=%v err=%v", found, err)
		}
	})
}

func openPendingRecord(t *testing.T) (*Journal, Config, model.NormalizedLog) {
	t.Helper()
	j, cfg, records, _ := openPendingBatch(t, 1)
	return j, cfg, records[0]
}

func mustDecodeBatch(t *testing.T, db *pebble.DB, batchID string) storedBatch {
	t.Helper()
	batch, err := decodeBatch(mustGet(t, db, batchKey(batchID)))
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func openPendingBatch(t *testing.T, count int) (*Journal, Config, []model.NormalizedLog, []Admission) {
	t.Helper()
	c := fakeclock.NewAtOrigin()
	cfg := internalConfig(filepath.Join(t.TempDir(), "journal"), c)
	records := builders.NewFactory(builders.WithClock(c)).Batch(t, count)
	admissions := make([]Admission, 0, count)
	for _, record := range records {
		admissions = append(admissions, Admission{Record: record, Priority: PriorityHigh})
	}
	j, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendBatch(records[0].BatchID, admissions); err != nil {
		t.Fatal(err)
	}
	return j, cfg, records, admissions
}

func assertCorruptAndUnready(t *testing.T, j *Journal, operation func() error) {
	t.Helper()
	if err := operation(); !errors.Is(err, ErrCorruption) {
		t.Fatalf("want touched corruption, got %v", err)
	}
	if j.Ready() {
		t.Fatal("touched corruption did not latch unready")
	}
}
