// Package identity derives the record identifiers that make processing
// effectively once.
//
// An identity version is immutable. Changing how an identifier is computed
// means adding a version, never editing one, because the same record must hash
// to the same value across restarts, replays, and releases.
package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

var ErrInvalidRecordUID = errors.New("identity: record uid is invalid")

// OTLPV1 derives the preferred OTLP record identity:
//
//	SHA-256("otlp:v1" || trusted_source_instance || log.record.uid)
//
// The record UID is a uniqueness claim made by the producer, not an
// authorization. Hashing it together with the instance the Collector
// authenticated as means one source cannot collide with, or impersonate,
// another by choosing a UID.
//
// The concatenation is unambiguous because a canonical UUID is always 36
// characters, so no pair of inputs can be split two ways.
func OTLPV1(sourceInstance, recordUID string) (string, error) {
	if strings.TrimSpace(sourceInstance) == "" {
		return "", fmt.Errorf("identity: source instance is required")
	}
	if err := ids.Validate(recordUID); err != nil {
		// A producer that assigns a UID after a retry boundary, or assigns
		// something that is not a UUIDv7, cannot be deduplicated by this
		// version, and guessing at its intent would create a false identity.
		return "", ErrInvalidRecordUID
	}

	digest := sha256.Sum256([]byte(model.RecordIDVersionOTLPV1 + sourceInstance + recordUID))
	return hex.EncodeToString(digest[:]), nil
}

// ErrIncompleteLocator means a CloudWatch event did not name every component
// its identity version is defined over.
var ErrIncompleteLocator = errors.New("identity: cloudwatch locator is incomplete")

// CloudWatchLocator is the native identity of one CloudWatch log event.
//
// Account and Region come from the adapter's own authenticated configuration,
// never from a value the source API reported about itself. An identifier built
// from an unauthenticated locator would be forgeable by whatever produced it.
type CloudWatchLocator struct {
	Account   string
	Region    string
	LogGroup  string
	LogStream string
	// EventID is the native CloudWatch event identifier. Content hashing is
	// prohibited, because two legitimate identical messages do occur.
	EventID string
}

// CloudWatchV1 derives the CloudWatch record identity:
//
//	SHA-256("cw:v1" || D(account) || D(region) || D(log_group) || D(log_stream) || D(event_id))
//
// where D length-delimits a component with a big-endian uint32 byte count.
//
// Unlike the fixed-width UUID of OTLPV1, a log group and a log stream are
// free-form operator-chosen names, so plain concatenation would let one
// identifier stand for two different streams. Two streams sharing a record_id
// would merge their occurrences into one incident contribution and lose the
// other's evidence, so the boundaries are encoded rather than assumed.
func CloudWatchV1(locator CloudWatchLocator) (string, error) {
	components := []struct {
		name  string
		value string
	}{
		{"account", locator.Account},
		{"region", locator.Region},
		{"log group", locator.LogGroup},
		{"log stream", locator.LogStream},
		{"event id", locator.EventID},
	}
	digest := sha256.New()
	digest.Write([]byte(model.RecordIDVersionCloudWatchV1))
	for _, component := range components {
		if strings.TrimSpace(component.value) == "" {
			// The message names only which component was absent. A locator
			// travels into logs that are not redacted, so it is never echoed.
			return "", fmt.Errorf("%w: %s is required", ErrIncompleteLocator, component.name)
		}
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(component.value)))
		digest.Write(length[:])
		digest.Write([]byte(component.value))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
