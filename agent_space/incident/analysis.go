package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/utils/queue"
)

const maxAnalysisContextBytes = 256 << 10

var (
	ErrInvalidContext = errors.New("incident: analysis context is invalid")
	ErrConfiguration  = errors.New("incident: analysis context boundary mismatch")
)

// AnalysisResolver reads the immutable context owned by static-log-analysis.
// Its pool uses a distinct, read-only DSN; agent-owned tables stay in the pool
// configured by DATABASE_URL.
type AnalysisResolver struct {
	Pool     *pgxpool.Pool
	Boundary queue.Boundary
}

func NewAnalysisResolver(pool *pgxpool.Pool, boundary queue.Boundary) *AnalysisResolver {
	return &AnalysisResolver{Pool: pool, Boundary: boundary}
}

func (r *AnalysisResolver) Resolve(ctx context.Context, a queue.Assignment) (Context, error) {
	if r == nil || r.Pool == nil {
		return Context{}, errors.New("incident: no analysis database pool configured")
	}
	if a.Region != r.Boundary.Region || a.TenantID != r.Boundary.TenantID ||
		a.Classification != r.Boundary.Classification {
		return Context{}, ErrConfiguration
	}

	const query = `
		SELECT f.service_id, f.environment, f.severity,
		       c.snapshot, c.classification, c.created_at
		FROM investigations AS i
		JOIN incident_families AS f
		  ON f.region=i.region AND f.tenant_id=i.tenant_id AND f.incident_id=i.incident_id
		JOIN investigation_contexts AS c
		  ON c.region=i.region AND c.tenant_id=i.tenant_id
		 AND c.investigation_id=i.investigation_id
		WHERE i.region=$1 AND i.tenant_id=$2 AND i.investigation_id=$3
		  AND i.incident_id=$4 AND c.version=$5
		  AND octet_length(c.snapshot::STRING) <= $6`

	var (
		service, environment, severity, classification string
		snapshot                                       []byte
		createdAt                                      time.Time
	)
	err := r.Pool.QueryRow(ctx, query, a.Region, a.TenantID, a.InvestigationID,
		a.IncidentID, a.ContextVersion, maxAnalysisContextBytes).Scan(
		&service, &environment, &severity, &snapshot, &classification, &createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, fmt.Errorf("%w: investigation %s version %d", ErrNotFound, a.InvestigationID, a.ContextVersion)
	}
	if err != nil {
		return Context{}, fmt.Errorf("incident: read analysis context: %w", err)
	}
	if service != a.ServiceID || environment != a.Environment || severity != a.Severity || classification != a.Classification {
		return Context{}, ErrInvalidContext
	}

	result, err := contextFromAnalysisSnapshot(a, snapshot, createdAt.UTC())
	if err != nil {
		return Context{}, err
	}
	result.ServiceID = service
	result.Environment = environment
	result.Severity = severity
	return result, nil
}

type encodedSafeValue struct {
	Kind   string          `json:"kind"`
	Value  json.RawMessage `json:"value,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

type safeValue struct {
	kind   string
	string string
	int    int64
	double float64
	bool   bool
	object map[string]safeValue
	array  []safeValue
	reason string
}

func contextFromAnalysisSnapshot(a queue.Assignment, encoded []byte, createdAt time.Time) (Context, error) {
	if len(encoded) == 0 || len(encoded) > maxAnalysisContextBytes || createdAt.IsZero() {
		return Context{}, ErrInvalidContext
	}
	root, err := decodeSafeValue(encoded, 1)
	if err != nil || root.kind != "map" {
		return Context{}, ErrInvalidContext
	}

	schema, schemaOK := root.object["schema_version"]
	incidentID, incidentOK := root.object["incident_id"]
	generation, generationOK := root.object["generation"]
	rule, ruleOK := root.object["rule_id"]
	threshold, thresholdOK := root.object["threshold"]
	window, windowOK := root.object["window_seconds"]
	representative, representativeOK := root.object["representative_error"]
	if !schemaOK || schema.kind != "string" || schema.string != "1.0" ||
		!incidentOK || incidentID.kind != "string" || incidentID.string != a.IncidentID ||
		!generationOK || generation.kind != "int" || generation.int != a.IncidentGen ||
		!ruleOK || rule.kind != "string" || strings.TrimSpace(rule.string) == "" ||
		!thresholdOK || threshold.kind != "int" || threshold.int < 1 ||
		!windowOK || window.kind != "int" || window.int < 1 || !representativeOK {
		return Context{}, ErrInvalidContext
	}

	excerpt, err := renderSafeValue(representative)
	if err != nil {
		return Context{}, ErrInvalidContext
	}
	return Context{
		IncidentID:     a.IncidentID,
		ContextVersion: a.ContextVersion,
		ServiceID:      a.ServiceID,
		Environment:    a.Environment,
		Severity:       a.Severity,
		Summary: fmt.Sprintf("Rule %s reached %d matching errors within %d seconds.",
			rule.string, threshold.int, window.int),
		LogExcerpt: excerpt,
		DetectedAt: createdAt.UTC(),
	}, nil
}

func decodeSafeValue(encoded []byte, depth int) (safeValue, error) {
	if depth > 16 {
		return safeValue{}, ErrInvalidContext
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire encodedSafeValue
	if err := decoder.Decode(&wire); err != nil {
		return safeValue{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return safeValue{}, ErrInvalidContext
	}
	result := safeValue{kind: wire.Kind, reason: wire.Reason}
	switch wire.Kind {
	case "string":
		if wire.Reason != "" || json.Unmarshal(wire.Value, &result.string) != nil {
			return safeValue{}, ErrInvalidContext
		}
	case "int":
		if wire.Reason != "" || json.Unmarshal(wire.Value, &result.int) != nil {
			return safeValue{}, ErrInvalidContext
		}
	case "double":
		if wire.Reason != "" || json.Unmarshal(wire.Value, &result.double) != nil {
			return safeValue{}, ErrInvalidContext
		}
	case "bool":
		if wire.Reason != "" || json.Unmarshal(wire.Value, &result.bool) != nil {
			return safeValue{}, ErrInvalidContext
		}
	case "withheld":
		if len(wire.Value) != 0 || strings.TrimSpace(wire.Reason) == "" {
			return safeValue{}, ErrInvalidContext
		}
	case "map":
		if wire.Reason != "" {
			return safeValue{}, ErrInvalidContext
		}
		var children map[string]json.RawMessage
		if json.Unmarshal(wire.Value, &children) != nil || children == nil {
			return safeValue{}, ErrInvalidContext
		}
		result.object = make(map[string]safeValue, len(children))
		for key, child := range children {
			decoded, err := decodeSafeValue(child, depth+1)
			if err != nil {
				return safeValue{}, err
			}
			result.object[key] = decoded
		}
	case "slice":
		if wire.Reason != "" {
			return safeValue{}, ErrInvalidContext
		}
		var children []json.RawMessage
		if json.Unmarshal(wire.Value, &children) != nil || children == nil {
			return safeValue{}, ErrInvalidContext
		}
		result.array = make([]safeValue, 0, len(children))
		for _, child := range children {
			decoded, err := decodeSafeValue(child, depth+1)
			if err != nil {
				return safeValue{}, err
			}
			result.array = append(result.array, decoded)
		}
	default:
		return safeValue{}, ErrInvalidContext
	}
	return result, nil
}

func renderSafeValue(value safeValue) (string, error) {
	plain, err := plainSafeValue(value)
	if err != nil {
		return "", err
	}
	if text, ok := plain.(string); ok {
		return text, nil
	}
	encoded, err := json.Marshal(plain)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func plainSafeValue(value safeValue) (any, error) {
	switch value.kind {
	case "string":
		return value.string, nil
	case "int":
		return value.int, nil
	case "double":
		return value.double, nil
	case "bool":
		return value.bool, nil
	case "withheld":
		return "[withheld: " + value.reason + "]", nil
	case "map":
		result := make(map[string]any, len(value.object))
		for key, child := range value.object {
			plain, err := plainSafeValue(child)
			if err != nil {
				return nil, err
			}
			result[key] = plain
		}
		return result, nil
	case "slice":
		result := make([]any, 0, len(value.array))
		for _, child := range value.array {
			plain, err := plainSafeValue(child)
			if err != nil {
				return nil, err
			}
			result = append(result, plain)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown safe value kind %q", strconv.Quote(value.kind))
	}
}
