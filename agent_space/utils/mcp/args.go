package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseArgs normalises whatever the model produced into MCP tool arguments: plain JSON, fenced JSON, double-encoded JSON, or bare text for a single-required-field tool. Anything else errors with the expected schema quoted so the model can retry.
func ParseArgs(input string, schema any) (map[string]any, error) {
	trimmed := stripFence(input)

	if trimmed == "" {
		// a tool requiring nothing is legitimately called with no arguments, signalled as "" or "{}"
		if len(requiredFields(schema)) == 0 {
			return map[string]any{}, nil
		}
		return nil, argErr("empty input", input, schema)
	}

	if args, ok := decodeObject(trimmed); ok {
		return args, nil
	}

	// a JSON string may hold the real object, which is what langchaingo's __arg1 round trip produces
	var unquoted string
	if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
		if args, ok := decodeObject(stripFence(unquoted)); ok {
			return args, nil
		}
		// the string held prose, not JSON; fall through with the unwrapped text
		trimmed = strings.TrimSpace(unquoted)
	}

	// unstructured text is only unambiguous when the tool wants exactly one thing
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

// the "required" list out of a JSON Schema, map-shaped because it is the server's raw JSON
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

// compact one-line form of a JSON Schema, for prompts and error messages
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
