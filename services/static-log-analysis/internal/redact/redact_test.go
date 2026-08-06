package redact_test

import (
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

func TestDynamicIdentifiersBecomeTypedPlaceholders(t *testing.T) {
	policy := redact.MinimalPolicy()

	tests := []struct {
		name    string
		text    string
		want    string
		rule    string
		because string
	}{
		{
			name:    "order number",
			text:    "charge failed for order 4711: card declined",
			want:    "charge failed for order <number>: card declined",
			rule:    "dynamic.number",
			because: "two failures on different orders are the same failure",
		},
		{
			name:    "request token",
			text:    "declined for request 8f2c1d",
			want:    "declined for request <hex>",
			rule:    "dynamic.hex_token",
			because: "a request identifier differs on every occurrence",
		},
		{
			name:    "customer number",
			text:    "customer 90210 has no active card",
			want:    "customer <number> has no active card",
			rule:    "dynamic.number",
			because: "a customer identifier is not part of what went wrong",
		},
		{
			name:    "uuid",
			text:    "session 0194f0a0-0000-7000-8000-000000000001 expired",
			want:    "session <uuid> expired",
			rule:    "dynamic.uuid",
			because: "a session identifier differs on every occurrence",
		},
		{
			name:    "memory address",
			text:    "nil dereference at 0x00c000180a80",
			want:    "nil dereference at <address>",
			rule:    "dynamic.address",
			because: "an address differs between processes running the same code",
		},
		{
			name:    "several in one message",
			text:    "order 4711 for customer 90210 request 8f2c1d",
			want:    "order <number> for customer <number> request <hex>",
			rule:    "dynamic.number",
			because: "a message often carries more than one dynamic value",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := policy.Text(test.text)

			if result.Text != test.want {
				t.Fatalf("want %q because %s, got %q", test.want, test.because, result.Text)
			}
			if !containsString(result.RuleIDs, test.rule) {
				t.Errorf("want rule %s recorded, got %v", test.rule, result.RuleIDs)
			}
		})
	}
}

func TestTextThatCarriesNoDynamicValueIsUnchanged(t *testing.T) {
	policy := redact.MinimalPolicy()

	tests := []string{
		"charge failed: card declined",
		// Short numbers carry meaning: a status code, a retry count, a port.
		"upstream returned 503 after 2 retries",
		"at charge (payment/charge.go:118)",
	}

	for _, text := range tests {
		t.Run(text, func(t *testing.T) {
			result := policy.Text(text)

			if result.Text != text {
				t.Fatalf("want %q unchanged, got %q", text, result.Text)
			}
			if len(result.RuleIDs) != 0 {
				t.Errorf("want no rules recorded, got %v", result.RuleIDs)
			}
		})
	}
}

func TestWordsSpelledFromHexadecimalLettersAreNotIdentifiers(t *testing.T) {
	policy := redact.MinimalPolicy()

	// "decade", "facade", and "efface" are all spelled from a to f. A rule
	// matching on shape alone would replace them and make the message unreadable
	// while claiming to have removed an identifier.
	for _, word := range []string{"decade", "facade", "efface", "accede", "beefed"} {
		t.Run(word, func(t *testing.T) {
			text := "the " + word + " ended"
			if got := policy.Text(text).Text; got != text {
				t.Fatalf("want %q unchanged, got %q", text, got)
			}
		})
	}
}

func TestRedactionIsIdempotent(t *testing.T) {
	policy := redact.MinimalPolicy()
	text := "order 4711 for customer 90210 request 8f2c1d at 0x00c000180a80 " +
		"session 0194f0a0-0000-7000-8000-000000000001"

	once := policy.Text(text)
	twice := policy.Text(once.Text)

	// A record may be redacted again on a retry or a replay. If a second pass
	// changed anything, the same record would produce two different stored
	// values and two different fingerprints.
	if twice.Text != once.Text {
		t.Fatalf("want a second pass to change nothing:\n first  %q\n second %q", once.Text, twice.Text)
	}
	if len(twice.RuleIDs) != 0 {
		t.Errorf("want no rules to fire on already redacted text, got %v", twice.RuleIDs)
	}
}

func TestPlaceholdersContainNothingAnyRuleMatches(t *testing.T) {
	policy := redact.MinimalPolicy()

	// This is what makes idempotence structural rather than incidental.
	for _, placeholder := range []string{"<number>", "<hex>", "<uuid>", "<address>"} {
		if got := policy.Text(placeholder).Text; got != placeholder {
			t.Errorf("want %s left alone, got %s", placeholder, got)
		}
	}
}

func TestEmptyTextProducesNothing(t *testing.T) {
	result := redact.MinimalPolicy().Text("")

	if result.Text != "" || len(result.RuleIDs) != 0 {
		t.Fatalf("want an empty result, got %+v", result)
	}
}

func TestRuleIdentifiersAreReportedOnceAndInOrder(t *testing.T) {
	policy := redact.MinimalPolicy()

	result := policy.Text("order 4711 customer 90210 request 8f2c1d again 8a2b3c")

	// Stored evidence names the rules that produced it, so a policy change can
	// be re-evaluated. A list that varied between runs would make that useless.
	want := []string{"dynamic.hex_token", "dynamic.number"}
	if strings.Join(result.RuleIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("want %v, got %v", want, result.RuleIDs)
	}
}

func TestThePolicyReportsItsVersion(t *testing.T) {
	if got := redact.MinimalPolicy().Version(); got != redact.MinimalPolicyVersion {
		t.Fatalf("want %s, got %s", redact.MinimalPolicyVersion, got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
