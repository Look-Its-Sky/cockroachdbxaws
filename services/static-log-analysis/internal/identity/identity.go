// Package identity derives the record identifiers that make processing
// effectively once.
//
// An identity version is immutable. Changing how an identifier is computed
// means adding a version, never editing one, because the same record must hash
// to the same value across restarts, replays, and releases.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

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
		return "", fmt.Errorf("identity: %s is not a usable record uid: %w", recordUID, err)
	}

	digest := sha256.Sum256([]byte(model.RecordIDVersionOTLPV1 + sourceInstance + recordUID))
	return hex.EncodeToString(digest[:]), nil
}
