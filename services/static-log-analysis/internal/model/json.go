package model

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrInvalidSafeValueJSON = errors.New("model: invalid SafeValue JSON")

// JSON here is a readable, round-trippable representation used for fixtures,
// diagnostics, and agent-facing payloads. It is not the durable internal
// format, which is Protobuf. Field names match the documented contract, so a
// fixture reads the same way the requirement does.

// safeValueJSON is the wire shape of a SafeValue: one kind and one payload,
// rather than a struct with every payload field present and empty.
type safeValueJSON struct {
	Kind   SafeValueKind   `json:"kind"`
	Value  json.RawMessage `json:"value,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

// MarshalJSON writes the value's kind and only the payload that kind uses.
//
// A nil map or slice is written as an empty container rather than as JSON null,
// so that every non-empty kind always carries a payload and a decoder can treat
// a missing one as malformed.
func (v SafeValue) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, ErrInvalidSafeValueJSON
	}
	if v.Kind == SafeKindEmpty {
		return []byte("null"), nil
	}

	out := safeValueJSON{Kind: v.Kind}
	var payload any
	switch v.Kind {
	case SafeKindString:
		payload = v.String
	case SafeKindInt:
		payload = v.Int
	case SafeKindDouble:
		payload = v.Double
	case SafeKindBool:
		payload = v.Bool
	case SafeKindMap:
		if v.Map == nil {
			payload = map[string]SafeValue{}
		} else {
			payload = v.Map
		}
	case SafeKindSlice:
		if v.Slice == nil {
			payload = []SafeValue{}
		} else {
			payload = v.Slice
		}
	case SafeKindWithheld:
		// Withheld content carries a reason and never a payload, so that a
		// value removed for safety cannot be reintroduced by a serializer.
		out.Reason = v.Withheld
		return json.Marshal(out)
	default:
		return nil, ErrInvalidSafeValueJSON
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidSafeValueJSON
	}
	out.Value = encoded
	return json.Marshal(out)
}

// UnmarshalJSON reads a value written by MarshalJSON.
//
// The payload is required, not optional. A value carrying a kind and nothing
// else would otherwise decode to an empty string, a zero, or a nil map, and
// there would be no way to tell an intentionally encoded zero from a truncated
// or hand-written payload. This encoding is used for agent-facing content, so
// the ambiguity has to be refused rather than resolved by default.
func (v *SafeValue) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*v = SafeValue{}
		return nil
	}

	var in safeValueJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return ErrInvalidSafeValueJSON
	}

	hasPayload := len(in.Value) > 0 && string(in.Value) != "null"

	if in.Kind == SafeKindWithheld {
		if hasPayload {
			return ErrInvalidSafeValueJSON
		}
		if strings.TrimSpace(in.Reason) == "" {
			return ErrInvalidSafeValueJSON
		}
		*v = SafeValue{Kind: SafeKindWithheld, Withheld: in.Reason}
		return nil
	}
	if in.Reason != "" {
		return ErrInvalidSafeValueJSON
	}

	decoded := SafeValue{Kind: in.Kind}
	var target any
	switch in.Kind {
	case SafeKindEmpty:
		return ErrInvalidSafeValueJSON
	case SafeKindString:
		target = &decoded.String
	case SafeKindInt:
		target = &decoded.Int
	case SafeKindDouble:
		target = &decoded.Double
	case SafeKindBool:
		target = &decoded.Bool
	case SafeKindMap:
		target = &decoded.Map
	case SafeKindSlice:
		target = &decoded.Slice
	default:
		return ErrInvalidSafeValueJSON
	}

	if !hasPayload {
		return ErrInvalidSafeValueJSON
	}
	if err := json.Unmarshal(in.Value, target); err != nil {
		return ErrInvalidSafeValueJSON
	}
	*v = decoded
	return nil
}
