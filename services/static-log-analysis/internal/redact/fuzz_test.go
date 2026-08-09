package redact_test

import (
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// testing.md requires Go fuzzing for bodies, attributes, and regex
// configuration. These targets exist because the properties they assert are
// universally quantified: they must hold for every input, and a table of
// examples can only ever show they hold for the inputs someone thought of.
//
// Each one asserts an invariant the service's security rests on, not merely
// that nothing panicked.

// forbiddenSeeds are the service-specific values a deployment configures. They
// stand in for the real secrets an operator would list.
var forbiddenSeeds = []string{"sup3r-s3cret-password", "AKIAIOSFODNN7EXAMPLE"}

func fuzzPolicy(f *testing.F) *redact.Policy {
	f.Helper()
	policy, err := redact.MinimalPolicy().WithForbiddenValues(forbiddenSeeds...)
	if err != nil {
		f.Fatal(err)
	}
	return policy
}

// seedCorpus is the shared starting corpus: the shapes security.md names, plus
// the marker and delimiter cases that have historically composed new secrets
// out of a redaction's own output.
func seedCorpus(f *testing.F) {
	for _, seed := range []string{
		"",
		"card declined",
		"password=sup3r-s3cret-password",
		"AKIAIOSFODNN7EXAMPLE",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig",
		`{"password":"sup3r-s3cret-password","user":"a"}`,
		"postgresql://root:sup3r-s3cret-password@10.0.0.1:26257/defaultdb",
		"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----",
		"user@example.com",
		"[REDACTED_FORBIDDEN_VALUE]",
		"[WITHHELD]",
		"a\r\nb\rc",
		"\ufeffhidden",
		"pass\u200bword=sup3r-s3cret-password",
		strings.Repeat("nested ", 64),
		`{"a":{"b":{"c":[1,2,{"d":"sup3r-s3cret-password"}]}}}`,
	} {
		f.Add(seed)
	}
}

// FuzzRedactedTextNeverContainsAConfiguredForbiddenValue is the load-bearing
// one. operations.md's SLO is that 100% of persisted records pass prohibited
// content validation and the corpus has zero known secret escapes; this is that
// statement quantified over every input rather than over a fixture list.
func FuzzRedactedTextNeverContainsAConfiguredForbiddenValue(f *testing.F) {
	seedCorpus(f)
	policy := fuzzPolicy(f)
	f.Fuzz(func(t *testing.T, input string) {
		result := policy.Text(input)
		for _, forbidden := range forbiddenSeeds {
			if strings.Contains(result.Text, forbidden) {
				t.Fatalf("redacted output still contains a configured forbidden value\ninput:  %q\noutput: %q", input, result.Text)
			}
		}
	})
}

// FuzzSafeTextIsAlwaysAcceptedByTheValidatorThatGuardsPersistence pins the
// agreement between the two halves of the policy. Persistence re-validates
// everything, so a SafeText result the validator would reject is a record that
// was proven safe and then refused at the boundary that matters.
func FuzzSafeTextIsAlwaysAcceptedByTheValidatorThatGuardsPersistence(f *testing.F) {
	seedCorpus(f)
	policy := fuzzPolicy(f)
	f.Fuzz(func(t *testing.T, input string) {
		result := policy.SafeText(input)
		if result.Value.Kind != model.SafeKindString {
			// Withheld is always a valid outcome: failing closed is the point.
			return
		}
		if err := policy.ValidateText(result.Value.String); err != nil {
			t.Fatalf("SafeText produced a value the persistence validator rejects\ninput: %q\nvalue: %q\nerror: %v",
				input, result.Value.String, err)
		}
	})
}

// FuzzRedactionIsIdempotent pins Milestone 2's first requirement over every
// input. A second pass that changes the text means a replayed or re-processed
// record does not converge, and two copies of one record would differ.
func FuzzRedactionIsIdempotent(f *testing.F) {
	seedCorpus(f)
	policy := fuzzPolicy(f)
	f.Fuzz(func(t *testing.T, input string) {
		once := policy.Text(input).Text
		twice := policy.Text(once).Text
		if once != twice {
			t.Fatalf("redaction is not idempotent\ninput: %q\nonce:  %q\ntwice: %q", input, once, twice)
		}
	})
}

// FuzzStructuredTextNeverLeaksThroughAStructuredBody covers the nested
// structured path separately, because it walks keys and values through
// different rules than opaque prose does.
func FuzzStructuredTextNeverLeaksThroughAStructuredBody(f *testing.F) {
	seedCorpus(f)
	policy := fuzzPolicy(f)
	f.Fuzz(func(t *testing.T, input string) {
		result := policy.StructuredText(input)
		if result.Value.Kind != model.SafeKindString {
			return
		}
		for _, forbidden := range forbiddenSeeds {
			if strings.Contains(result.Value.String, forbidden) {
				t.Fatalf("structured redaction leaked a forbidden value\ninput:  %q\noutput: %q", input, result.Value.String)
			}
		}
		if err := policy.ValidateText(result.Value.String); err != nil {
			t.Fatalf("StructuredText produced a value the persistence validator rejects: %v", err)
		}
	})
}

// FuzzFieldNameClassificationAgreesWithItsOwnValidator pins that the strict
// key boundary and the check that guards persistence cannot disagree. A key
// SafeFieldName accepts but ValidateFieldName rejects would make a record
// unpersistable after it had already been acknowledged.
func FuzzFieldNameClassificationAgreesWithItsOwnValidator(f *testing.F) {
	for _, seed := range []string{"", "user_id", "Authorization", "pass word", "ключ", "a\u200bb", "[WITHHELD_FIELD_NAME]", "..", "a.b"} {
		f.Add(seed)
	}
	policy := fuzzPolicy(f)
	f.Fuzz(func(t *testing.T, input string) {
		result := policy.SafeFieldName(input)
		err := policy.ValidateFieldName(input)
		accepted := result.Value.Kind == model.SafeKindString && result.Value.String == input
		if accepted && err != nil {
			t.Fatalf("SafeFieldName accepted %q unchanged but ValidateFieldName rejected it: %v", input, err)
		}
	})
}

// FuzzStructuredJSONShapeIsTotal pins that the bounded preflight terminates and
// returns rather than panicking, for any bytes at all. It runs before decoding,
// so it is the first thing untrusted input touches.
func FuzzStructuredJSONShapeIsTotal(f *testing.F) {
	seedCorpus(f)
	f.Add(strings.Repeat("[", 4096))
	f.Add(strings.Repeat(`{"a":`, 2048))
	f.Fuzz(func(t *testing.T, input string) {
		nodes, members, ok := redact.StructuredJSONShape(input)
		if !ok {
			return
		}
		if nodes < 0 || members < 0 {
			t.Fatalf("shape reported negative counts for %q: nodes=%d members=%d", input, nodes, members)
		}
	})
}
