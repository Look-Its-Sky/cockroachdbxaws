package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
		return nil, fmt.Errorf("model: cannot encode unknown value kind %q", v.Kind)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("model: encoding %s value: %w", v.Kind, err)
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
		return fmt.Errorf("model: decoding value: %w", err)
	}

	hasPayload := len(in.Value) > 0 && string(in.Value) != "null"

	if in.Kind == SafeKindWithheld {
		if hasPayload {
			return fmt.Errorf("model: withheld content must not carry a value")
		}
		if strings.TrimSpace(in.Reason) == "" {
			return fmt.Errorf("model: withheld content must carry a reason")
		}
		*v = SafeValue{Kind: SafeKindWithheld, Withheld: in.Reason}
		return nil
	}
	if in.Reason != "" {
		return fmt.Errorf("model: a %s value must not carry a withheld reason", in.Kind)
	}

	decoded := SafeValue{Kind: in.Kind}
	var target any
	switch in.Kind {
	case SafeKindEmpty:
		return fmt.Errorf("model: value has no kind")
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
		return fmt.Errorf("model: unknown value kind %q", in.Kind)
	}

	if !hasPayload {
		return fmt.Errorf("model: a %s value requires a payload", in.Kind)
	}
	if err := json.Unmarshal(in.Value, target); err != nil {
		return fmt.Errorf("model: decoding %s value: %w", in.Kind, err)
	}
	*v = decoded
	return nil
}
