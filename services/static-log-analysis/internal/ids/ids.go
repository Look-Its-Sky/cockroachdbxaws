// Package ids produces the sortable identifiers used for messages, incidents,
// investigations, and outbox rows.
//
// Identifiers are UUIDv7: their leading bits are a millisecond timestamp, so
// they sort by creation order in an index without a separate ordering column.
// Record identity is not produced here; a record_id is derived from source
// identity and is defined by the identity contract.
package ids

import (
	"fmt"

	"github.com/google/uuid"
)

// Source produces identifiers. Production code takes a Source rather than
// calling a generator directly, because identifiers must be generated before a
// retryable database transaction begins and reused on every retry.
type Source interface {
	// New returns a canonical lowercase UUID string.
	New() (string, error)
}

// System returns the production source.
func System() Source { return systemSource{} }

type systemSource struct{}

func (systemSource) New() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generating uuidv7: %w", err)
	}
	return id.String(), nil
}

// Validate reports whether s is a canonical lowercase UUIDv7 in the RFC 4122
// variant. Identifiers that arrive from outside the service, such as a
// producer-assigned log record UID, are checked with this before use.
func Validate(s string) error {
	id, err := uuid.Parse(s)
	if err != nil {
		return fmt.Errorf("not a uuid: %w", err)
	}
	if got := id.String(); got != s {
		return fmt.Errorf("not canonical lowercase uuid text: want %q, got %q", got, s)
	}
	if v := id.Version(); v != 7 {
		return fmt.Errorf("want uuid version 7, got version %d", v)
	}
	if v := id.Variant(); v != uuid.RFC4122 {
		return fmt.Errorf("want RFC 4122 variant, got %v", v)
	}
	return nil
}
