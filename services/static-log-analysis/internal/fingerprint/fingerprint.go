// Package fingerprint groups already-safe normalized errors by the stable
// parts that describe what failed. It never accepts transport or raw log types:
// redaction and trusted-envelope admission must have completed first.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

// ErrorV1 is the immutable first error fingerprint algorithm.
const ErrorV1 = "error:v1"

// Part is one safe input that contributed to a fingerprint. Parts are ordered
// and retained so an operator can understand why records grouped together.
type Part struct {
	Field string
	Value string
}

// Result is a versioned SHA-256 error fingerprint and its safe explanation.
type Result struct {
	Version string
	Digest  string
	Parts   []Part
}

// Explanation renders the safe contributing fields in their canonical order.
func (r Result) Explanation() string {
	parts := make([]string, 0, len(r.Parts))
	for _, part := range r.Parts {
		parts = append(parts, part.Field+"="+part.Value)
	}
	return strings.Join(parts, "; ")
}

// Error calculates the first-version fingerprint for a normalized error.
func Error(record model.NormalizedLog) (Result, error) {
	if err := record.Validate(); err != nil {
		return Result{}, fmt.Errorf("fingerprint: invalid normalized log: %w", err)
	}
	if strings.TrimSpace(record.Service.Name) == "" {
		return Result{}, fmt.Errorf("fingerprint: service identity is required")
	}

	parts := []Part{{Field: "service.name", Value: record.Service.Name}}
	add(&parts, "service.environment", record.Service.Environment)
	if code, ok := scalar(record.Attributes["error.code"]); ok {
		add(&parts, "error.code", code)
	}
	if record.Exception != nil {
		add(&parts, "exception.type", record.Exception.Type)
		add(&parts, "exception.message_template", record.Exception.SafeMessage)
		for _, frame := range record.Exception.StackFrames {
			if !frame.InApplication {
				continue
			}
			add(&parts, "application_frame.function", frame.Function)
			add(&parts, "application_frame.module", frame.Module)
			add(&parts, "application_frame.file", withoutLineNumber(frame.File))
		}
	}
	if record.Body.Kind == model.SafeKindString {
		add(&parts, "body.message_template", record.Body.String)
	} else {
		add(&parts, "event.name", record.EventName)
	}
	for _, key := range []string{"http.route", "rpc.method", "db.operation.name", "code.function"} {
		if operation, ok := scalar(record.Attributes[key]); ok {
			add(&parts, "operation."+key, operation)
			break
		}
	}

	hash := sha256.New()
	writeCanonical(hash.Write, ErrorV1)
	for _, part := range parts {
		writeCanonical(hash.Write, part.Field)
		writeCanonical(hash.Write, part.Value)
	}
	return Result{Version: ErrorV1, Digest: hex.EncodeToString(hash.Sum(nil)), Parts: parts}, nil
}

func add(parts *[]Part, field, value string) {
	if strings.TrimSpace(value) != "" {
		*parts = append(*parts, Part{Field: field, Value: value})
	}
}

func scalar(value model.SafeValue) (string, bool) {
	switch value.Kind {
	case model.SafeKindString:
		return value.String, true
	case model.SafeKindInt:
		return strconv.FormatInt(value.Int, 10), true
	default:
		return "", false
	}
}

var trailingLineNumber = regexp.MustCompile(`(?::\d+)+$`)

func withoutLineNumber(file string) string {
	return trailingLineNumber.ReplaceAllString(file, "")
}

// writeCanonical uses a length prefix so field boundaries cannot collide even
// when safe messages contain punctuation used by the human explanation.
func writeCanonical(write func([]byte) (int, error), value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = write(length[:])
	_, _ = write([]byte(value))
}
