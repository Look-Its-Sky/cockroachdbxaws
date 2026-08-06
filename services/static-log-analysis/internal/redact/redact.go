// Package redact removes content that must not be stored from text taken out of
// a log record.
//
// Redaction runs before normalization, not after it. A NormalizedLog is defined
// as already safe, so raw content is never placed in one and cleaned up
// afterwards: there is no window in which an unredacted value exists in a
// structure the rest of the service is allowed to read.
//
// This is the minimal policy the first ingestion slice needs: dynamic
// identifiers become typed placeholders, so that two occurrences differing only
// in a request, order, or customer identifier read the same. The prohibited
// value corpus, per-service policies, and structured-body handling belong to
// the security milestone.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Result is redacted text together with the rules that changed it.
//
// The rule identifiers are kept so that stored evidence can be re-evaluated
// when a policy changes, rather than being reprocessed blindly.
type Result struct {
	Text    string
	RuleIDs []string
}

// rule replaces one class of dynamic value with a typed placeholder.
type rule struct {
	id      string
	pattern *regexp.Regexp
	// placeholder names the type of the value that was removed, not the value.
	placeholder string
	// accept lets a rule refuse a match its pattern over-approximates. RE2 has
	// no lookaround, so a condition like "contains a digit" is expressed here.
	accept func(match string) bool
}

// Policy is a versioned set of redaction rules.
type Policy struct {
	version string
	rules   []rule
}

// Version returns the policy version recorded on every record it redacts.
func (p *Policy) Version() string { return p.version }

// MinimalPolicyVersion is the version of the policy below. It changes whenever
// the rules change, because stored evidence names the version that produced it.
const MinimalPolicyVersion = "1.0"

// containsDigit distinguishes an identifier from an ordinary word. Without it,
// a hexadecimal token rule also matches English words spelled from a to f.
func containsDigit(s string) bool {
	return strings.ContainsAny(s, "0123456789")
}

// MinimalPolicy returns the policy the first ingestion slice uses.
//
// Rules are applied in order, from the most specific shape to the least, so
// that a UUID is recognised as a UUID rather than being partly consumed by the
// hexadecimal rule.
func MinimalPolicy() *Policy {
	return &Policy{
		version: MinimalPolicyVersion,
		rules: []rule{
			{
				id:          "dynamic.uuid",
				pattern:     regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`),
				placeholder: "<uuid>",
			},
			{
				id:          "dynamic.address",
				pattern:     regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`),
				placeholder: "<address>",
			},
			{
				// Request, correlation, and session identifiers are normally
				// rendered as hexadecimal tokens.
				id:          "dynamic.hex_token",
				pattern:     regexp.MustCompile(`\b[0-9a-f]{6,}\b`),
				placeholder: "<hex>",
				accept:      containsDigit,
			},
			{
				// Order, customer, and account numbers. Short runs are left
				// alone: a status code or a small count carries meaning.
				id:          "dynamic.number",
				pattern:     regexp.MustCompile(`\b\d{4,}\b`),
				placeholder: "<number>",
			},
		},
	}
}

// Text redacts a single string.
//
// Applying Text to its own output changes nothing further: placeholders contain
// no run of characters any rule matches, so a record that is redacted twice,
// through a retry or a replay, is identical to one redacted once.
func (p *Policy) Text(text string) Result {
	if text == "" {
		return Result{}
	}

	matched := map[string]bool{}
	redacted := text
	for _, r := range p.rules {
		redacted = r.pattern.ReplaceAllStringFunc(redacted, func(match string) string {
			if r.accept != nil && !r.accept(match) {
				return match
			}
			matched[r.id] = true
			return r.placeholder
		})
	}

	return Result{Text: redacted, RuleIDs: sortedKeys(matched)}
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
