package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool           *pgxpool.Pool
	validator      RecordValidator
	scope          Scope
	classification string
	hooks          transactionHooks
}

type transactionHooks struct {
	onAttempt        func(int, []ProcessInput)
	beforeFamilyLock func(context.Context, pgx.Tx, ProcessInput) error
	beforeOutbox     func(context.Context, pgx.Tx) error
	afterOutbox      func(context.Context, pgx.Tx) error
}

func New(pool *pgxpool.Pool, config Config) (*Store, error) {
	if pool == nil || nilInterface(config.Validator) || !validText(config.Validator.Version(), MaxScopeBytes) ||
		!validScope(config.Scope) || !validClassification(config.Classification) || config.Topology != TopologySingleRegion {
		return nil, ErrInvalidInput
	}
	return &Store{pool: pool, validator: config.Validator, scope: config.Scope, classification: config.Classification}, nil
}

func (s *Store) permits(scope Scope) bool {
	return s != nil && scope == s.scope
}

// Boundary returns the immutable scope, classification, and redaction-policy
// version this Store was constructed for. A coordinator proves once at startup
// that they agree with its own configuration and its journal manifest, because
// the per-record replay comparison cannot distinguish a misconfigured boundary
// from a forged record and must fail closed on both.
func (s *Store) Boundary() Boundary {
	if s == nil || nilInterface(s.validator) {
		return Boundary{}
	}
	return Boundary{Scope: s.scope, Classification: s.classification, PolicyVersion: s.validator.Version()}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func validScope(scope Scope) bool {
	return validText(scope.Region, MaxScopeBytes) && validText(scope.TenantID, MaxScopeBytes)
}

func validText(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:/", r)) {
			return false
		}
	}
	return true
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC &&
		value.Equal(time.Unix(0, value.UnixNano()).UTC())
}

func safeDeadline(now time.Time, ttl time.Duration) (time.Time, error) {
	if !validUTC(now) || ttl <= 0 || now.After(time.Unix(0, math.MaxInt64).UTC().Add(-ttl)) {
		return time.Time{}, ErrInvalidInput
	}
	return now.Add(ttl), nil
}

func validUUIDv7(value string) bool { return ids.Validate(value) == nil }

func validFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validClassification(value string) bool {
	switch value {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	default:
		return false
	}
}

func validDetectionStatus(value string) bool {
	switch value {
	case "active", "quiet", "ended":
		return true
	default:
		return false
	}
}

func validTriggerReason(value string) bool {
	switch value {
	case "fatal", "security", "five_in_five", "explicit_rule", "health_failure", "error_rate":
		return true
	default:
		return false
	}
}

func databaseError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrConfiguration) || errors.Is(err, ErrIncompatibleSchema) ||
		errors.Is(err, ErrStaleClaim) || errors.Is(err, ErrImmutable) || errors.Is(err, ErrIdentityConflict) {
		return err
	}
	return ErrUnavailable
}

func encodeSafeValue(value model.SafeValue) ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, ErrInvalidInput
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidInput
	}
	return encoded, nil
}
