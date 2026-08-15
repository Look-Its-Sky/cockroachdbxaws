// Package agent defines the typed, versioned boundary shared with investigation agents.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

const (
	AssignmentSchemaVersion = "1.0"
	AssignmentMessageType   = "agent.assignment.v1"
	AssignmentProducer      = "static-log-analysis"
	MaxAssignmentBytes      = 16 << 10
	MaxAssignmentTextBytes  = 128
)

var ErrInvalidAssignment = errors.New("agent: invalid assignment")

// Assignment is the complete immutable queue pointer. Evidence is deliberately
// absent; an agent resolves ContextVersion through the scoped persistence API.
type Assignment struct {
	SchemaVersion      string    `json:"schema_version"`
	MessageID          string    `json:"message_id"`
	MessageType        string    `json:"message_type"`
	CreatedAt          time.Time `json:"created_at"`
	Region             string    `json:"region"`
	TenantID           string    `json:"tenant_id"`
	Classification     string    `json:"classification"`
	Producer           string    `json:"producer"`
	CorrelationID      string    `json:"correlation_id"`
	IncidentID         string    `json:"incident_id"`
	IncidentGeneration int64     `json:"incident_generation"`
	InvestigationID    string    `json:"investigation_id"`
	ServiceID          string    `json:"service_id"`
	Environment        string    `json:"environment"`
	Severity           string    `json:"severity"`
	ContextVersion     int64     `json:"context_version"`
}

func (a Assignment) Validate() error {
	version, err := model.ParseSchemaVersion(a.SchemaVersion)
	if err != nil || version.Major != 1 || a.MessageType != AssignmentMessageType || a.Producer != AssignmentProducer ||
		ids.Validate(a.MessageID) != nil || ids.Validate(a.CorrelationID) != nil || ids.Validate(a.InvestigationID) != nil ||
		!validUTC(a.CreatedAt) || !validText(a.Region) || !validText(a.TenantID) || !validText(a.ServiceID) ||
		!validText(a.Environment) || !validIncidentID(a.IncidentID) || a.IncidentGeneration < 1 || a.ContextVersion < 1 ||
		!validClassification(a.Classification) || !validSeverity(a.Severity) {
		return ErrInvalidAssignment
	}
	return nil
}

func EncodeAssignment(value Assignment) ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxAssignmentBytes {
		return nil, ErrInvalidAssignment
	}
	return encoded, nil
}

func DecodeAssignment(encoded []byte) (Assignment, error) {
	if len(encoded) == 0 || len(encoded) > MaxAssignmentBytes {
		return Assignment{}, ErrInvalidAssignment
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var result Assignment
	if err := decoder.Decode(&result); err != nil {
		return Assignment{}, ErrInvalidAssignment
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Assignment{}, ErrInvalidAssignment
	}
	if err := result.Validate(); err != nil {
		return Assignment{}, err
	}
	return result, nil
}

func validText(value string) bool {
	if value == "" || len(value) > MaxAssignmentTextBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:/", r)) {
			return false
		}
	}
	return true
}

func validIncidentID(value string) bool {
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

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Equal(time.Unix(0, value.UnixNano()).UTC())
}

func validClassification(value string) bool {
	switch value {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	default:
		return false
	}
}

func validSeverity(value string) bool {
	switch value {
	case "unspecified", "trace", "debug", "info", "warn", "error", "fatal":
		return true
	default:
		return false
	}
}

func (a Assignment) String() string {
	return fmt.Sprintf("%s %s", a.MessageType, a.MessageID)
}
