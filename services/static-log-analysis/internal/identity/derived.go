package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

// ErrInvalidDerivedInput means a record without a native identifier does not
// carry enough valid, already-redacted evidence to establish a fallback
// identity. The error deliberately contains no record-derived value.
var ErrInvalidDerivedInput = errors.New("identity: derived identity input is invalid")

// DerivedV1 derives the fallback identity for a safe, normalized OTLP record.
//
// The encoding is length-delimited and map keys are byte-sorted. Transport
// attempt fields (BatchID and Source.ReceivedAt), enrichment, redaction
// metadata, and raw-log references are deliberately excluded: any of those may
// change when the same source record is retried or enriched later.
//
// This function only accepts already-redacted model values. Calling it over raw
// OTLP content would make a digest a persistent secret verifier.
func DerivedV1(record model.NormalizedLog) (string, error) {
	if err := record.Source.Validate(); err != nil ||
		(record.EventTime.IsZero() && record.ObservedTime.IsZero()) ||
		(record.Body.IsZero() && record.EventName == "") ||
		record.Body.Validate() != nil ||
		!validSafeMap(record.Attributes) ||
		!validSafeMap(record.ResourceAttributes) ||
		!validSafeMap(record.ScopeAttributes) {
		return "", ErrInvalidDerivedInput
	}

	encoder := canonicalEncoder{digest: sha256.New()}
	encoder.rawString(model.RecordIDVersionDerivedV1)

	// Authenticated source boundary. ReceivedAt is intentionally absent.
	encoder.string(string(record.Source.SourceType))
	encoder.string(record.Source.SourceAccount)
	encoder.string(record.Source.Region)
	encoder.stringsAsSet(record.Source.AllowedEnvironments)
	encoder.stringsAsSet(record.Source.AllowedServices)
	encoder.string(record.Source.SourceInstance)
	encoder.string(record.Source.CredentialIdentity)

	// Stable normalized evidence selected by the derived:v1 contract.
	encoder.string(record.Service.Name)
	encoder.string(record.Service.Namespace)
	encoder.string(record.Service.InstanceID)
	encoder.string(record.Service.Environment)
	encoder.timestamp(record.EventTime)
	encoder.timestamp(record.ObservedTime)
	encoder.signed(int64(record.SeverityNumber))
	encoder.string(record.Correlation.TraceID)
	encoder.string(record.Correlation.SpanID)
	encoder.safeValue(record.Body)
	encoder.string(record.EventName)
	encoder.safeMap(record.Attributes)
	encoder.safeMap(record.ResourceAttributes)
	encoder.safeMap(record.ScopeAttributes)

	return hex.EncodeToString(encoder.digest.Sum(nil)), nil
}

func validSafeMap(values map[string]model.SafeValue) bool {
	for key, value := range values {
		if strings.TrimSpace(key) == "" || value.Validate() != nil {
			return false
		}
	}
	return true
}

type canonicalEncoder struct{ digest hash.Hash }

func (e canonicalEncoder) rawString(value string) { _, _ = e.digest.Write([]byte(value)) }

func (e canonicalEncoder) length(length int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(length))
	_, _ = e.digest.Write(encoded[:])
}

func (e canonicalEncoder) string(value string) {
	e.length(len(value))
	e.rawString(value)
}

func (e canonicalEncoder) signed(value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = e.digest.Write(encoded[:])
}

func (e canonicalEncoder) timestamp(value time.Time) { e.signed(value.UTC().UnixNano()) }

func (e canonicalEncoder) stringsAsSet(values []string) {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	unique := ordered[:0]
	for _, value := range ordered {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	e.length(len(unique))
	for _, value := range unique {
		e.string(value)
	}
}

func (e canonicalEncoder) safeMap(values map[string]model.SafeValue) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	e.length(len(keys))
	for _, key := range keys {
		e.string(key)
		e.safeValue(values[key])
	}
}

func (e canonicalEncoder) safeValue(value model.SafeValue) {
	e.string(string(value.Kind))
	switch value.Kind {
	case model.SafeKindEmpty:
	case model.SafeKindString:
		e.string(value.String)
	case model.SafeKindInt:
		e.signed(value.Int)
	case model.SafeKindDouble:
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], math.Float64bits(value.Double))
		_, _ = e.digest.Write(encoded[:])
	case model.SafeKindBool:
		if value.Bool {
			e.rawString("\x01")
		} else {
			e.rawString("\x00")
		}
	case model.SafeKindMap:
		e.safeMap(value.Map)
	case model.SafeKindSlice:
		e.length(len(value.Slice))
		for _, child := range value.Slice {
			e.safeValue(child)
		}
	case model.SafeKindWithheld:
		e.string(value.Withheld)
	}
}
