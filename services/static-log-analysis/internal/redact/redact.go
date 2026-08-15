// Package redact removes content that must not be persisted from untrusted log
// fields. Redaction precedes normalization, and every persistence-facing value
// is subject to a final prohibited-content scan.
package redact

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

const (
	ReasonRedactionFailure = "content_withheld_redaction_failure"
	WithheldText           = "[CONTENT_WITHHELD_REDACTION_FAILURE]"
	MinimalPolicyVersion   = "2.2"
	// Structured JSON is preflighted with these fixed ceilings before it is
	// recursively materialized into Go or model values.
	DefaultMaxStructuredDepth = 16
	DefaultMaxStructuredNodes = 10_000
)

// Policy errors are intentionally opaque: errors must never echo the secret or
// customer payload that caused the failure.
var (
	ErrProhibitedContent = errors.New("redact: prohibited content remains")
	ErrInvalidPolicy     = errors.New("redact: invalid policy")
)

// Result is redacted text together with the rules that changed it.
type Result struct {
	Text    string
	RuleIDs []string
}

type rule struct {
	id          string
	pattern     *regexp.Regexp
	placeholder string
	accept      func(match string) bool
}

// Policy is immutable after construction and safe for concurrent use.
type Policy struct {
	version         string
	rules           []rule
	safetyRules     []rule
	forbiddenValues []string
}

func (p *Policy) Version() string { return p.version }

func containsDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// MinimalPolicy is the baseline universal and service-aware safety policy.
// Placeholders contain no values matched by the policy, making it idempotent.
func MinimalPolicy() *Policy {
	securityRules := []rule{
		{id: "universal.private_key", pattern: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), placeholder: "[REDACTED_PRIVATE_KEY]"},
		{id: "universal.authorization", pattern: regexp.MustCompile(`(?im)^[^\r\n]*(?:authorization|proxy[-_.]?authorization)["']?\s*[:=]\s*[^\r\n]*`), placeholder: "[REDACTED_AUTHORIZATION]"},
		{id: "universal.cookie", pattern: regexp.MustCompile(`(?im)^[^\r\n]*(?:cookie|set[-_.]?cookie)["']?\s*[:=]\s*[^\r\n]*`), placeholder: "[REDACTED_COOKIE]"},
		{id: "universal.connection_password", pattern: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^:/\s]+:[^@\s/]+@[^\s]+`), placeholder: "[REDACTED_CONNECTION_STRING]"},
		{id: "universal.jwt", pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`), placeholder: "[REDACTED_TOKEN]"},
		{id: "universal.aws_access_key", pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), placeholder: "[REDACTED_CLOUD_CREDENTIAL]"},
		{id: "universal.named_secret", pattern: regexp.MustCompile(`(?im)^[^\r\n]*(?:[[:alnum:]]+[._-])*(?:pass[._-]?word|passwd|pwd|api[._-]?key|secret|access[._-]?token|session[._-]?token|request[._-]?header[._-]?authorization)["']?\s*[:=]\s*[^\r\n]*`), placeholder: "[REDACTED_SECRET]"},
		{id: "service.customer_assignment", pattern: regexp.MustCompile(`(?im)^[^\r\n]*(?:[[:alnum:]]+[._-])*(?:cvv|cvc|pan|request[._-]?body|query[._-]?params|session[._-]?id|customer[._-]?id|card[._-]?number|payment[._-]?card(?:[._-]?number)?)["']?\s*[:=]\s*[^\r\n]*`), placeholder: "[REDACTED_FIELD]"},
		{id: "service.email", pattern: regexp.MustCompile(`[\p{L}\p{N}.!#$%&'*+/=?^_{|}~-]+@[\p{L}\p{N}](?:[\p{L}\p{N}-]{0,61}[\p{L}\p{N}])?(?:\.[\p{L}\p{N}](?:[\p{L}\p{N}-]{0,61}[\p{L}\p{N}])?)+`), placeholder: "[REDACTED_EMAIL]"},
		{id: "service.query", pattern: regexp.MustCompile(`\?[^\s#]+`), placeholder: "[REDACTED_QUERY]"},
	}
	dynamicRules := []rule{
		{id: "dynamic.timestamp", pattern: regexp.MustCompile(`\b[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt ][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:[Zz]|[+-][0-9]{2}:[0-9]{2})?\b`), placeholder: "<timestamp>"},
		{id: "dynamic.uuid", pattern: regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), placeholder: "<uuid>"},
		{id: "dynamic.address", pattern: regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`), placeholder: "<address>"},
		{id: "dynamic.hex_token", pattern: regexp.MustCompile(`\b[0-9a-f]{6,}\b`), placeholder: "<hex>", accept: containsDigit},
		{id: "dynamic.identifier", pattern: regexp.MustCompile(`\b[A-Z][A-Z0-9_-]{7,}\b`), placeholder: "<identifier>", accept: containsDigit},
		{id: "dynamic.number", pattern: regexp.MustCompile(`\b\d{4,}\b`), placeholder: "<number>"},
	}
	allRules := append(securityRules, dynamicRules...)
	return &Policy{version: MinimalPolicyVersion, rules: allRules, safetyRules: securityRules}
}

// WithForbiddenValues returns a copy which also removes configured forbidden
// customer values. Values that overlap stable output markers are refused: an
// accepted policy can therefore replace every occurrence directly while its
// generated output remains idempotent.
func (p *Policy) WithForbiddenValues(values ...string) (*Policy, error) {
	copyPolicy := *p
	copyPolicy.rules = append([]rule(nil), p.rules...)
	copyPolicy.safetyRules = append([]rule(nil), p.safetyRules...)
	copyPolicy.forbiddenValues = append([]string(nil), p.forbiddenValues...)
	for _, value := range values {
		if value == "" || !utf8.ValidString(value) || unsafeUnicode(value) {
			return nil, ErrInvalidPolicy
		}
		for _, marker := range stableMarkers {
			if strings.Contains(marker, value) || strings.Contains(value, marker) {
				return nil, ErrInvalidPolicy
			}
		}
		copyPolicy.forbiddenValues = append(copyPolicy.forbiddenValues, value)
	}
	return &copyPolicy, nil
}

// Text applies universal, service-aware, and dynamic-value redaction.
func (p *Policy) Text(text string) Result {
	if text == "" {
		return Result{}
	}
	if !utf8.ValidString(text) || unsafeUnicode(text) {
		return Result{Text: WithheldText, RuleIDs: []string{"safety.invalid_utf8"}}
	}
	matched := map[string]bool{}
	if containsStableMarker(text) {
		matched["safety.preexisting_marker"] = true
	}
	redacted := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if p.unsafeOpaqueAssignmentKey(redacted) {
		return Result{Text: WithheldText, RuleIDs: sortedKeys(mergeRule(matched, "safety.unsafe_unicode"))}
	}
	if matched["safety.preexisting_marker"] && p.containsForbidden(redacted) {
		matched["safety.composed_forbidden"] = true
		return Result{Text: WithheldText, RuleIDs: sortedKeys(matched)}
	}
	for _, forbidden := range p.forbiddenValues {
		if strings.Contains(redacted, forbidden) {
			redacted = strings.ReplaceAll(redacted, forbidden, "[REDACTED_FORBIDDEN_VALUE]")
			matched["service.forbidden_value"] = true
		}
	}
	for _, candidate := range p.rules {
		redacted = candidate.pattern.ReplaceAllStringFunc(redacted, func(match string) string {
			if candidate.accept != nil && !candidate.accept(match) {
				return match
			}
			matched[candidate.id] = true
			return candidate.placeholder
		})
	}
	// A replacement can join its closing delimiter to adjacent input and form
	// a configured forbidden value that was not present before the rules ran.
	// The post-rule check therefore fails closed to the one stable marker.
	if p.containsForbidden(redacted) {
		matched["safety.composed_forbidden"] = true
		redacted = WithheldText
	}
	return Result{Text: redacted, RuleIDs: sortedKeys(matched)}
}

type SafeResult struct {
	Value   model.SafeValue
	RuleIDs []string
}

// SafeText redacts opaque text and withholds content the final scan cannot
// prove safe.
func (p *Policy) SafeText(text string) SafeResult {
	if !utf8.ValidString(text) || unsafeUnicode(text) {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.unsafe_unicode"}}
	}
	result := p.Text(text)
	if result.Text == WithheldText {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: result.RuleIDs}
	}
	if p.ValidateText(result.Text) != nil {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: append(result.RuleIDs, "safety.final_scan")}
	}
	return SafeResult{Value: model.SafeString(result.Text), RuleIDs: result.RuleIDs}
}

// SafeFieldName applies the stricter boundary required for structural keys.
// Unlike prose, a non-ASCII or escaped key cannot be proven distinct from a
// sensitive field name and is withheld before its value is inspected.
func (p *Policy) SafeFieldName(text string) SafeResult {
	if unsafeFieldName(text) || strings.HasPrefix(text, "[WITHHELD_FIELD_NAME]") {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.unsafe_field_name"}}
	}
	return p.SafeText(text)
}

// ValidateFieldName applies SafeFieldName's strict classification without
// rewriting the key. Normalizer-generated withheld-key ordinals are the sole
// exception to the raw marker-prefix rejection.
func (p *Policy) ValidateFieldName(text string) error {
	if generatedWithheldFieldName(text) {
		return nil
	}
	result := p.SafeFieldName(text)
	if result.Value.Kind != model.SafeKindString || result.Value.String != text {
		return ErrProhibitedContent
	}
	return nil
}

func unsafeFieldName(text string) bool {
	return strings.TrimSpace(text) == "" || !utf8.ValidString(text) || unsafeUnicode(text) || !asciiOnly(text) || strings.Contains(strings.ToLower(text), `\u`)
}

func generatedWithheldFieldName(text string) bool {
	const marker = "[WITHHELD_FIELD_NAME]"
	if text == marker {
		return true
	}
	suffix, ok := strings.CutPrefix(text, marker+"#")
	if !ok || suffix == "" || suffix[0] == '0' {
		return false
	}
	ordinal, err := strconv.Atoi(suffix)
	return err == nil && ordinal >= 2 && strconv.Itoa(ordinal) == suffix
}

// StructuredText redacts JSON recursively. Parse failures fall back to opaque
// text redaction; unsafe byte sequences are withheld with metadata only.
func (p *Policy) StructuredText(text string) SafeResult {
	if !utf8.ValidString(text) {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.unsafe_unicode"}}
	}
	preflightErr := preflightStructuredJSON(text, DefaultMaxStructuredDepth, DefaultMaxStructuredNodes)
	if errors.Is(preflightErr, errStructuredLimit) {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.structured_limit"}}
	}
	if errors.Is(preflightErr, errStructuredDuplicateKey) {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.structured_duplicate_key"}}
	}
	if p.containsForbidden(text) {
		return p.SafeText(text)
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		if unsafeMalformedStructuredKey(text) {
			return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.unsafe_unicode"}}
		}
		return p.SafeText(text)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return p.SafeText(text)
	}
	rules := map[string]bool{}
	value, ok := p.structuredValue(raw, "", rules)
	if !ok || p.ValidateValue(value) != nil {
		return SafeResult{Value: model.Withheld(ReasonRedactionFailure), RuleIDs: []string{"safety.final_scan"}}
	}
	return SafeResult{Value: value, RuleIDs: sortedKeys(rules)}
}

func (p *Policy) structuredValue(raw any, key string, rules map[string]bool) (model.SafeValue, bool) {
	if p.SensitiveField(key) {
		rules["service.sensitive_field"] = true
		return model.SafeString("[REDACTED_FIELD]"), true
	}
	switch value := raw.(type) {
	case nil:
		return model.Withheld("json null"), true
	case string:
		result := p.SafeText(value)
		for _, id := range result.RuleIDs {
			rules[id] = true
		}
		return result.Value, true
	case bool:
		return model.SafeBool(value), true
	case json.Number:
		if integer, err := strconv.ParseInt(string(value), 10, 64); err == nil {
			return model.SafeInt(integer), true
		}
		float, err := strconv.ParseFloat(string(value), 64)
		return model.SafeDouble(float), err == nil
	case []any:
		items := make([]model.SafeValue, 0, len(value))
		for _, child := range value {
			converted, ok := p.structuredValue(child, "", rules)
			if !ok {
				return model.SafeValue{}, false
			}
			items = append(items, converted)
		}
		return model.SafeSlice(items...), true
	case map[string]any:
		items := make(map[string]model.SafeValue, len(value))
		keys := make([]string, 0, len(value))
		for childKey := range value {
			keys = append(keys, childKey)
		}
		sort.Strings(keys)
		for _, childKey := range keys {
			child := value[childKey]
			keyResult := p.SafeFieldName(childKey)
			for _, id := range keyResult.RuleIDs {
				rules[id] = true
			}
			if keyResult.Value.Kind == model.SafeKindWithheld {
				return model.SafeValue{}, false
			}
			if _, collision := items[keyResult.Value.String]; collision {
				return model.SafeValue{}, false
			}
			converted, ok := p.structuredValue(child, childKey, rules)
			if !ok {
				return model.SafeValue{}, false
			}
			items[keyResult.Value.String] = converted
		}
		return model.SafeMap(items), true
	default:
		return model.SafeValue{}, false
	}
}

// SensitiveField reports whether policy classifies an attribute value by name.
func (p *Policy) SensitiveField(key string) bool {
	parts := strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, normalized := range parts {
		switch normalized {
		case "authorization", "cookie", "password", "passwd", "pwd", "apikey", "secret", "accesstoken", "sessiontoken", "requestbody", "cardnumber", "cvv", "cvc", "pan":
			return true
		}
	}
	for index := 0; index+1 < len(parts); index++ {
		if parts[index] == "pass" && parts[index+1] == "word" {
			return true
		}
	}
	joined := strings.Join(parts, "")
	for _, suffix := range []string{
		"apikey", "accesstoken", "sessiontoken", "sessionid", "customerid",
		"requestbody", "queryparams", "cardnumber", "paymentcard", "paymentcardnumber", "cvv", "cvc", "pan",
	} {
		if strings.HasSuffix(joined, suffix) {
			return true
		}
	}
	return false
}

// ValidateText is the final prohibited-content scan.
func (p *Policy) ValidateText(text string) error {
	if !utf8.ValidString(text) || unsafeUnicode(text) {
		return ErrProhibitedContent
	}
	canonical := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if p.unsafeOpaqueAssignmentKey(canonical) {
		return ErrProhibitedContent
	}
	for _, forbidden := range p.forbiddenValues {
		if strings.Contains(canonical, forbidden) {
			return ErrProhibitedContent
		}
	}
	for _, candidate := range p.safetyRules {
		matches := candidate.pattern.FindAllString(canonical, -1)
		for _, match := range matches {
			if candidate.accept == nil || candidate.accept(match) {
				return ErrProhibitedContent
			}
		}
	}
	return nil
}

func (p *Policy) containsForbidden(text string) bool {
	for _, forbidden := range p.forbiddenValues {
		if strings.Contains(text, forbidden) {
			return true
		}
	}
	return false
}

func asciiOnly(text string) bool {
	for _, r := range text {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

var malformedStructuredKeyPattern = regexp.MustCompile(`"(?:[^"\\]|\\.)*(?:\\u[0-9A-Fa-f]{4}|[^\x00-\x7F])(?:[^"\\]|\\.)*"\s*[:=]`)

func unsafeMalformedStructuredKey(text string) bool {
	return malformedStructuredKeyPattern.MatchString(text)
}

func unsafeUnicode(text string) bool {
	lower := strings.ToLower(text)
	for _, escaped := range []string{`\u200b`, `\u200c`, `\u200d`, `\u2060`, `\ufeff`, `\uff1a`, `\uff1d`} {
		if strings.Contains(lower, escaped) {
			return true
		}
	}
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) {
			return true
		}
		switch r {
		case '\uff1a', '\uff1d', '\ufe55', '\ufe66':
			return true
		}
	}
	return false
}

func (p *Policy) unsafeOpaqueAssignmentKey(text string) bool {
	for start := 0; start <= len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		line := text[start:end]
		for separator, r := range line {
			if r != ':' && r != '=' {
				continue
			}
			label := opaqueLabelBefore(line, separator)
			if label == "" || (asciiOnly(label) && !strings.Contains(strings.ToLower(label), `\u`)) {
				continue
			}
			// A finite confusable table cannot prove any non-ASCII or escaped
			// adjacent label safe. Both ':' and '=' therefore fail closed,
			// including colon-space forms that resemble ordinary prose.
			return true
		}
		if end == len(text) {
			break
		}
		start = end + 1
	}
	return false
}

func opaqueLabelBefore(line string, separator int) string {
	end := separator
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:end])
		if unicode.IsSpace(r) || r == '"' || r == '\'' || r == '`' {
			end -= size
			continue
		}
		break
	}
	start := end
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:start])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' && r != '-' && r != '\\' {
			break
		}
		start -= size
	}
	return line[start:end]
}

func containsStableMarker(text string) bool {
	for _, marker := range stableMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func mergeRule(rules map[string]bool, id string) map[string]bool {
	rules[id] = true
	return rules
}

var (
	errStructuredLimit        = errors.New("redact: structured input exceeds limit")
	errStructuredDuplicateKey = errors.New("redact: structured input contains a duplicate object key")
)

// preflightStructuredJSON walks decoder tokens iteratively. Object member names
// are not values, but their decoded forms are tracked within each object to
// prevent duplicate-key collapse. Member tracking and container/scalar value
// nodes are each bounded by the same maximum. Syntax errors are left to
// StructuredText's opaque fallback.
func preflightStructuredJSON(text string, maximumDepth, maximumNodes int) error {
	_, _, err := structuredJSONShape(text, maximumDepth, maximumNodes)
	return err
}

// StructuredJSONShape returns the bounded value-node and object-member counts
// used by StructuredText. ok is false for malformed or over-limit input, which
// StructuredText treats as opaque or withheld rather than materializing.
func StructuredJSONShape(text string) (nodes, objectMembers int, ok bool) {
	nodes, objectMembers, err := structuredJSONShape(text, DefaultMaxStructuredDepth, DefaultMaxStructuredNodes)
	return nodes, objectMembers, err == nil
}

func structuredJSONShape(text string, maximumDepth, maximumNodes int) (int, int, error) {
	type frame struct {
		object       bool
		expectingKey bool
		keys         map[string]struct{}
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	frames := make([]frame, 0, maximumDepth)
	nodes := 0
	objectMembers := 0
	rootValues := 0
	duplicateFound := false
	addNode := func() error {
		nodes++
		if nodes > maximumNodes {
			return errStructuredLimit
		}
		return nil
	}
	completeValue := func() {
		if len(frames) > 0 && frames[len(frames)-1].object {
			frames[len(frames)-1].expectingKey = true
		} else if len(frames) == 0 {
			rootValues++
		}
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if duplicateFound {
				return 0, 0, errStructuredDuplicateKey
			}
			if len(frames) != 0 || rootValues != 1 {
				return 0, 0, io.ErrUnexpectedEOF
			}
			return nodes, objectMembers, nil
		}
		if err != nil {
			if duplicateFound {
				return 0, 0, errStructuredDuplicateKey
			}
			return 0, 0, err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if err := addNode(); err != nil {
					return 0, 0, err
				}
				frames = append(frames, frame{object: delimiter == '{', expectingKey: delimiter == '{'})
				if len(frames) > maximumDepth {
					return 0, 0, errStructuredLimit
				}
			case '}', ']':
				if len(frames) > 0 {
					frames = frames[:len(frames)-1]
					completeValue()
				}
			}
			continue
		}
		if len(frames) > 0 && frames[len(frames)-1].object && frames[len(frames)-1].expectingKey {
			key, ok := token.(string)
			if !ok {
				return 0, 0, io.ErrUnexpectedEOF
			}
			objectMembers++
			if objectMembers > maximumNodes {
				return 0, 0, errStructuredLimit
			}
			current := &frames[len(frames)-1]
			if current.keys == nil {
				current.keys = make(map[string]struct{})
			}
			if _, duplicate := current.keys[key]; duplicate {
				duplicateFound = true
			} else {
				current.keys[key] = struct{}{}
			}
			current.expectingKey = false
			continue
		}
		if err := addNode(); err != nil {
			return 0, 0, err
		}
		completeValue()
	}
}

var stableMarkers = []string{
	"[CONTENT_WITHHELD_REDACTION_FAILURE]", "[REDACTED_PRIVATE_KEY]", "[REDACTED_AUTHORIZATION]",
	"[REDACTED_COOKIE]", "[REDACTED_CONNECTION_STRING]", "[REDACTED_TOKEN]",
	"[REDACTED_CLOUD_CREDENTIAL]", "[REDACTED_SECRET]", "[REDACTED_EMAIL]",
	"[REDACTED_QUERY]", "[REDACTED_FORBIDDEN_VALUE]", "[REDACTED_FIELD]", "[WITHHELD_FIELD_NAME]",
	"<timestamp>", "<uuid>", "<address>", "<hex>", "<identifier>", "<number>",
}

// ValidateValue scans the complete value, including structured keys.
func (p *Policy) ValidateValue(value model.SafeValue) error {
	type pendingValue struct {
		value          model.SafeValue
		containerDepth int
	}
	stack := []pendingValue{{value: value}}
	scheduled := 1
	for len(stack) > 0 {
		last := len(stack) - 1
		item := stack[last]
		stack = stack[:last]
		current := item.value
		nonStringPayload := current.Int != 0 || current.Double != 0 || current.Bool || current.Map != nil || current.Slice != nil || current.Withheld != ""
		nonIntPayload := current.String != "" || current.Double != 0 || current.Bool || current.Map != nil || current.Slice != nil || current.Withheld != ""
		nonDoublePayload := current.String != "" || current.Int != 0 || current.Bool || current.Map != nil || current.Slice != nil || current.Withheld != ""
		nonBoolPayload := current.String != "" || current.Int != 0 || current.Double != 0 || current.Map != nil || current.Slice != nil || current.Withheld != ""
		nonMapPayload := current.String != "" || current.Int != 0 || current.Double != 0 || current.Bool || current.Slice != nil || current.Withheld != ""
		nonSlicePayload := current.String != "" || current.Int != 0 || current.Double != 0 || current.Bool || current.Map != nil || current.Withheld != ""
		nonWithheldPayload := current.String != "" || current.Int != 0 || current.Double != 0 || current.Bool || current.Map != nil || current.Slice != nil
		switch current.Kind {
		case model.SafeKindEmpty:
			if current.String != "" || current.Int != 0 || current.Double != 0 || current.Bool || current.Map != nil || current.Slice != nil || current.Withheld != "" {
				return ErrProhibitedContent
			}
		case model.SafeKindString:
			if nonStringPayload || p.ValidateText(current.String) != nil {
				return ErrProhibitedContent
			}
		case model.SafeKindMap:
			if nonMapPayload {
				return ErrProhibitedContent
			}
			if len(current.Map) > DefaultMaxStructuredNodes-scheduled {
				return ErrProhibitedContent
			}
			containerDepth := item.containerDepth + 1
			if containerDepth > DefaultMaxStructuredDepth {
				return ErrProhibitedContent
			}
			for key, child := range current.Map {
				if strings.TrimSpace(key) == "" || p.ValidateFieldName(key) != nil ||
					generatedWithheldFieldName(key) && child.Kind != model.SafeKindWithheld ||
					p.SensitiveField(key) && !canonicalRedactedFieldValue(child) {
					return ErrProhibitedContent
				}
				stack = append(stack, pendingValue{value: child, containerDepth: containerDepth})
			}
			scheduled += len(current.Map)
		case model.SafeKindSlice:
			if nonSlicePayload || len(current.Slice) > DefaultMaxStructuredNodes-scheduled {
				return ErrProhibitedContent
			}
			containerDepth := item.containerDepth + 1
			if containerDepth > DefaultMaxStructuredDepth {
				return ErrProhibitedContent
			}
			for _, child := range current.Slice {
				stack = append(stack, pendingValue{value: child, containerDepth: containerDepth})
			}
			scheduled += len(current.Slice)
		case model.SafeKindInt:
			if nonIntPayload || p.containsForbidden(strconv.FormatInt(current.Int, 10)) {
				return ErrProhibitedContent
			}
		case model.SafeKindDouble:
			if nonDoublePayload || p.containsForbidden(strconv.FormatFloat(current.Double, 'g', -1, 64)) {
				return ErrProhibitedContent
			}
		case model.SafeKindBool:
			if nonBoolPayload || p.containsForbidden(strconv.FormatBool(current.Bool)) {
				return ErrProhibitedContent
			}
		case model.SafeKindWithheld:
			if nonWithheldPayload || strings.TrimSpace(current.Withheld) == "" || p.ValidateText(current.Withheld) != nil {
				return ErrProhibitedContent
			}
		default:
			return ErrProhibitedContent
		}
	}
	return nil
}

// ValidateRecord scans all log-derived strings immediately before a caller may
// hand the record to persistence.
func (p *Policy) ValidateRecord(record model.NormalizedLog) error {
	texts := []string{
		record.SchemaVersion, record.RecordID, record.RecordIDVersion, string(record.IdentityQuality), record.BatchID,
		string(record.Source.SourceType), record.Source.SourceAccount, record.Source.Region,
		record.Source.SourceInstance, record.Source.CredentialIdentity, record.Region,
		record.TimestampInferenceReason, record.ObservedTimeInferenceReason, record.SeverityText, record.EventName,
		record.Service.Name, record.Service.Namespace, record.Service.InstanceID, record.Service.Environment,
		record.Deployment.ID, record.Deployment.Version, record.Correlation.TraceID, record.Correlation.SpanID,
		record.Redaction.PolicyVersion,
	}
	texts = append(texts, record.Source.AllowedEnvironments...)
	texts = append(texts, record.Source.AllowedServices...)
	texts = append(texts, record.Redaction.RuleIDs...)
	texts = append(texts, record.Redaction.WithheldFields...)
	if ref := record.RawReference; ref != nil {
		texts = append(texts, string(ref.SourceType), ref.Region, ref.Locator, ref.Classification)
	}
	if record.Exception != nil {
		texts = append(texts, record.Exception.Type, record.Exception.SafeMessage)
		for _, frame := range record.Exception.StackFrames {
			texts = append(texts, frame.Function, frame.Module, frame.File)
		}
	}
	for _, text := range texts {
		if p.ValidateText(text) != nil {
			return ErrProhibitedContent
		}
	}
	if p.ValidateValue(record.Body) != nil {
		return ErrProhibitedContent
	}
	for _, attributes := range []map[string]model.SafeValue{record.Attributes, record.ResourceAttributes, record.ScopeAttributes} {
		for key, value := range attributes {
			if p.ValidateFieldName(key) != nil ||
				generatedWithheldFieldName(key) && value.Kind != model.SafeKindWithheld ||
				p.SensitiveField(key) && !canonicalRedactedFieldValue(value) ||
				p.ValidateValue(value) != nil {
				return ErrProhibitedContent
			}
		}
	}
	return nil
}

func canonicalRedactedFieldValue(value model.SafeValue) bool {
	return value.Kind == model.SafeKindString && value.String == "[REDACTED_FIELD]" &&
		value.Int == 0 && value.Double == 0 && !value.Bool && value.Map == nil && value.Slice == nil && value.Withheld == ""
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
