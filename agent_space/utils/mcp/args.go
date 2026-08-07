package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseArgs turns whatever the model produced into MCP tool arguments.
//
// MCP tools take structured JSON described by a JSON Schema, but langchaingo's
// tools.Tool hands over one opaque string, and models improvise even when given
// a real schema. This normalises the shapes seen in practice:
//
//   - a plain JSON object                      -> used directly
//   - an object wrapped in a ``` fence         -> fence stripped
//   - a JSON object encoded inside a JSON      -> unwrapped, then re-parsed
//     string (langchaingo's __arg1 round trip)
//   - bare prose for a single-required-field   -> wrapped as {field: input}
//     tool (e.g. a raw SQL statement)
//
// Anything else yields an error that quotes the expected schema, because an
// agent that is told what went wrong can retry, whereas an opaque failure ends
// the run.
func ParseArgs(input string, schema any) (map[string]any, error) {
	trimmed := stripFence(input)

	if trimmed == "" {
		// A tool whose schema requires nothing is legitimately called with no
		// arguments, and models signal that with "" or "{}".
		if len(requiredFields(schema)) == 0 {
			return map[string]any{}, nil
		}
		return nil, argErr("empty input", input, schema)
	}

	if args, ok := decodeObject(trimmed); ok {
		return args, nil
	}

	// A JSON string may itself hold the real object. This is exactly what
	// happens when a model puts a JSON blob into langchaingo's __arg1 slot.
	var unquoted string
	if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
		if args, ok := decodeObject(stripFence(unquoted)); ok {
			return args, nil
		}
		// The string held prose, not JSON. Fall through with the unwrapped
		// text so the single-field shortcut below gets a clean value.
		trimmed = strings.TrimSpace(unquoted)
	}

	// Unstructured text is only unambiguous when the tool wants exactly one
	// thing. select_query(statement) and list_tables(database) both qualify,
	// and both are tools this agent leans on.
	if required := requiredFields(schema); len(required) == 1 {
		return map[string]any{required[0]: trimmed}, nil
	}

	return nil, argErr("input is not a JSON object", input, schema)
}

// stripFence removes markdown code fences and surrounding whitespace.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}

	s = strings.TrimPrefix(s, "```")
	// Drop an optional language tag on the opening fence.
	if i := strings.IndexByte(s, '\n'); i >= 0 && !strings.ContainsAny(s[:i], "{[\"") {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// decodeObject parses s as a JSON object, rejecting other JSON shapes.
func decodeObject(s string) (map[string]any, bool) {
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, false
	}
	return out, true
}

// requiredFields reads the "required" list out of a JSON Schema. The schema
// arrives as the server's raw JSON, so it is map-shaped rather than typed.
func requiredFields(schema any) []string {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["required"].([]any)
	if !ok {
		return nil
	}

	fields := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			fields = append(fields, s)
		}
	}
	return fields
}

// argErr builds the self-describing error the model sees on a bad call.
func argErr(reason, input string, schema any) error {
	rendered := renderSchema(schema)
	return fmt.Errorf(
		"could not parse tool arguments (%s). Received: %s. Respond with a JSON object matching this schema: %s",
		reason, truncate(input, 200), rendered,
	)
}

// renderSchema produces a compact one-line form of a JSON Schema for prompts
// and error messages.
func renderSchema(schema any) string {
	if schema == nil {
		return "{}"
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return "{}"
	}
	return truncate(string(encoded), 1200)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
