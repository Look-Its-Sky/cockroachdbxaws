package redact_test

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
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
			name:    "ISO timestamp",
			text:    "request failed at 2026-08-06T12:03:04.123Z",
			want:    "request failed at <timestamp>",
			rule:    "dynamic.timestamp",
			because: "an occurrence timestamp is not part of the failure template",
		},
		{
			name:    "uppercase non-hex request identifier",
			text:    "request REQ-AZ91-KLM2 failed",
			want:    "request <identifier> failed",
			rule:    "dynamic.identifier",
			because: "common request IDs are not limited to lowercase hexadecimal",
		},
		{
			name:    "unseparated uppercase request identifier",
			text:    "request REQ9Z8Y7X6 failed",
			want:    "request <identifier> failed",
			rule:    "dynamic.identifier",
			because: "not every uppercase request ID uses a separator",
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
		"session 0194f0a0-0000-7000-8000-000000000001 at 2026-08-06T12:03:04Z request REQ-AZ91-KLM2"

	once := policy.Text(text)
	twice := policy.Text(once.Text)

	// A record may be redacted again on a retry or a replay. If a second pass
	// changed anything, the same record would produce two different stored
	// values and two different fingerprints.
	if twice.Text != once.Text {
		t.Fatalf("want a second pass to change nothing:\n first  %q\n second %q", once.Text, twice.Text)
	}
	if !containsString(twice.RuleIDs, "safety.preexisting_marker") {
		t.Errorf("want a repeated marker identified as preexisting, got %v", twice.RuleIDs)
	}
}

func TestPlaceholdersContainNothingAnyRuleMatches(t *testing.T) {
	policy := redact.MinimalPolicy()

	// This is what makes idempotence structural rather than incidental.
	for _, placeholder := range []string{"<number>", "<hex>", "<uuid>", "<address>", "<timestamp>", "<identifier>"} {
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

func TestKnownProhibitedValuesNeverAppearInSafeOutput(t *testing.T) {
	policy := redact.MinimalPolicy()
	tests := []string{
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature",
		"password=hunter2",
		"postgres://alice:swordfish@db.example/payments",
		"AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN PRIVATE KEY-----\nsecret material\n-----END PRIVATE KEY-----",
	}
	for _, unsafe := range tests {
		t.Run(unsafe[:min(24, len(unsafe))], func(t *testing.T) {
			result := policy.Text(unsafe)
			if result.Text == unsafe {
				t.Fatal("known prohibited content was unchanged")
			}
			if err := policy.ValidateText(result.Text); err != nil {
				t.Fatalf("redacted output must pass the final safety scan: %v", err)
			}
			for _, fragment := range []string{"hunter2", "swordfish", "AKIAIOSFODNN7EXAMPLE", "eyJhbGciOi", "secret material"} {
				if strings.Contains(result.Text, fragment) {
					t.Fatalf("safe output retained prohibited fragment %q", fragment)
				}
			}
		})
	}
}

func TestCompleteSecretBearingHeaderLinesAreRemoved(t *testing.T) {
	policy := redact.MinimalPolicy()
	corpus := []string{
		"Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260806/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=suffix-secret\nnext=safe",
		"Proxy-Authorization: Basic dXNlcjpwYXNzd29yZA== trailing-secret\nnext=safe",
		"Cookie: session=secret-one; customer=secret-two; token=secret-three\nnext=safe",
		"Set-Cookie: session=secret-one; HttpOnly; Secure; SameSite=Strict\nnext=safe",
		`password = "two word password" suffix-secret`,
		`"password" : "two word password" suffix-secret`,
	}
	for _, unsafe := range corpus {
		result := policy.Text(unsafe)
		for _, secret := range []string{"AKIA", "suffix-secret", "dXNlcj", "secret-one", "secret-two", "secret-three", "two word password"} {
			if strings.Contains(result.Text, secret) {
				t.Fatalf("redaction retained header/password suffix %q in %q", secret, result.Text)
			}
		}
		if err := policy.ValidateText(result.Text); err != nil {
			t.Fatalf("redacted line failed final scan: %v", err)
		}
		if twice := policy.Text(result.Text); twice.Text != result.Text || !containsString(twice.RuleIDs, "safety.preexisting_marker") {
			t.Fatalf("header redaction is not idempotent: first=%+v second=%+v", result, twice)
		}
	}
}

func TestSecurityRedactionIsIdempotentForEveryProhibitedShape(t *testing.T) {
	policy, err := redact.MinimalPolicy().WithForbiddenValues("configured customer payload")
	if err != nil {
		t.Fatalf("configure policy: %v", err)
	}
	for _, unsafe := range []string{
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature",
		`password="two word password"`,
		"postgres://alice:swordfish@db.example/payments",
		"AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN PRIVATE KEY-----\nsecret material\n-----END PRIVATE KEY-----",
		"email alice@example.com",
		"GET /charge?customer=alice&token=something",
		"configured customer payload",
	} {
		once := policy.Text(unsafe)
		twice := policy.Text(once.Text)
		if twice.Text != once.Text || !containsString(twice.RuleIDs, "safety.preexisting_marker") {
			t.Fatalf("second pass changed or re-matched safe output:\nfirst  %+v\nsecond %+v", once, twice)
		}
	}
}

func TestConfiguredForbiddenValuesCannotMutateGeneratedMarkers(t *testing.T) {
	for _, forbidden := range []string{"", "SECRET", "REDACTED", "FIELD_NAME", "[REDACTED_SECRET]", "prefix[REDACTED_SECRET]suffix"} {
		policy, err := redact.MinimalPolicy().WithForbiddenValues(forbidden)
		if policy != nil || !errors.Is(err, redact.ErrInvalidPolicy) {
			t.Fatalf("forbidden value %q must be rejected opaquely, policy=%v err=%v", forbidden, policy, err)
		}
		if strings.Contains(err.Error(), forbidden) && forbidden != "" {
			t.Fatalf("configuration error echoed forbidden value %q: %v", forbidden, err)
		}
	}

	policy, err := redact.MinimalPolicy().WithForbiddenValues("configured customer payload")
	if err != nil {
		t.Fatalf("configure valid policy: %v", err)
	}
	once := policy.Text("configured customer payload and password=hunter2")
	twice := policy.Text(once.Text)
	if strings.Contains(once.Text, "configured customer payload") || twice.Text != once.Text || !containsString(twice.RuleIDs, "safety.preexisting_marker") {
		t.Fatalf("valid forbidden policy is not safe and idempotent: first=%+v second=%+v", once, twice)
	}
}

func TestStructuredForbiddenScalarLexemesFailClosed(t *testing.T) {
	for _, forbidden := range []string{"1234", "true", "false", "12.75"} {
		policy, err := redact.MinimalPolicy().WithForbiddenValues(forbidden)
		if err != nil {
			t.Fatalf("configure %q: %v", forbidden, err)
		}
		for _, input := range []string{
			`{"value":` + forbidden + `}`,
			`{"nested":{"value":` + forbidden + `}}`,
			`[` + forbidden + `]`,
		} {
			got := policy.StructuredText(input)
			if got.Value.Kind == model.SafeKindMap || got.Value.Kind == model.SafeKindSlice {
				t.Fatalf("forbidden scalar %q survived structured conversion of %q: %+v", forbidden, input, got.Value)
			}
			if strings.Contains(got.Value.String, forbidden) {
				t.Fatalf("forbidden scalar %q survived safe output: %+v", forbidden, got.Value)
			}
		}
	}
}

func TestOpaqueSensitiveLabelsAreRedactedAsCompleteAssignments(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, label := range []string{
		"client_secret", "db_password", "x_api_key", "http_request_header_authorization",
		"client.secret", "db-password", "metadata.session_token", "pass_word", "pass.word", "pass-word",
	} {
		input := "prefix remains on another line\n" + label + "=top-secret-value\nnext=safe"
		got := policy.SafeText(input)
		if got.Value.Kind != model.SafeKindString || strings.Contains(got.Value.String, label) || strings.Contains(got.Value.String, "top-secret-value") {
			t.Fatalf("opaque label %q was not fully removed: %+v", label, got.Value)
		}
		if twice := policy.SafeText(got.Value.String); !reflect.DeepEqual(twice.Value, got.Value) || !containsString(twice.RuleIDs, "safety.preexisting_marker") {
			t.Fatalf("opaque-label result is not idempotent: first=%+v second=%+v", got, twice)
		}
	}
}

func TestDocumentedCustomerAssignmentsAndInternationalizedEmailsAreCovered(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, unsafe := range []string{
		"cvv=123", "cvc: 456", "pan=4111111111111111", "payment.card.number=4111111111111111",
		"request_body=customer payload", "request.body=customer payload", "query_params=customer=alice",
		"session_id=customer-session", "customer_id=alice", "contact alice@例え.テスト",
		"contact δοκιμή@παράδειγμα.δοκιμή",
	} {
		t.Run(unsafe, func(t *testing.T) {
			once := policy.Text(unsafe)
			if once.Text == unsafe || strings.Contains(once.Text, "customer") || strings.Contains(once.Text, "alice") || strings.Contains(once.Text, "411111") || strings.Contains(once.Text, "例え") || strings.Contains(once.Text, "δοκιμή") {
				t.Fatalf("documented customer content survived Text: %+v", once)
			}
			if err := policy.ValidateText(once.Text); err != nil {
				t.Fatalf("safe replacement failed final scan: %v", err)
			}
			if err := policy.ValidateText(unsafe); !errors.Is(err, redact.ErrProhibitedContent) {
				t.Fatalf("final scan accepted documented customer content: %v", err)
			}
			safe := policy.SafeText(unsafe)
			if safe.Value.Kind != model.SafeKindString || safe.Value.String != once.Text {
				t.Fatalf("SafeText disagrees with Text: text=%+v safe=%+v", once, safe)
			}
			if twice := policy.Text(once.Text); twice.Text != once.Text || !containsString(twice.RuleIDs, "safety.preexisting_marker") {
				t.Fatalf("customer-data redaction is not idempotent: first=%+v second=%+v", once, twice)
			}
		})
	}
}

func TestStructuredKeyCollisionsFailClosedDeterministically(t *testing.T) {
	policy := redact.MinimalPolicy()
	input := `{"a@example.com":"first-secret","b@example.com":"second-secret"}`
	for iteration := 0; iteration < 200; iteration++ {
		got := policy.StructuredText(input)
		if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("iteration %d: colliding safe keys must withhold enclosing content, got %+v", iteration, got.Value)
		}
		if encoded, err := got.Value.MarshalJSON(); err != nil || strings.Contains(string(encoded), "first-secret") || strings.Contains(string(encoded), "second-secret") {
			t.Fatalf("iteration %d: collision leaked a value: %s err=%v", iteration, encoded, err)
		}
	}
}

func TestStructuredRedactionUsesFieldClassificationRecursively(t *testing.T) {
	unsafe := `{"password":"hunter2","nested":{"contact":"alice@example.com"}}`
	got := redact.MinimalPolicy().StructuredText(unsafe)
	if got.Value.Kind != model.SafeKindMap {
		t.Fatalf("want structured safe output, got %+v", got.Value)
	}
	if err := redact.MinimalPolicy().ValidateValue(got.Value); err != nil {
		t.Fatalf("structured safe output failed final scan: %v", err)
	}
	if got.Value.Map["password"].String != "[REDACTED_FIELD]" {
		t.Fatalf("password field was not classified and removed: %+v", got.Value.Map)
	}
	if nested := got.Value.Map["nested"].Map["contact"].String; strings.Contains(nested, "alice") {
		t.Fatalf("nested email survived service-aware redaction: %q", nested)
	}
}

func TestSensitiveFieldClassificationHandlesCommonNestedNames(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, key := range []string{
		"password", "db.password", "password=misleading", "http.request.header.authorization",
		"payment.card.number", "customer_id", "session.id", "request-body", "pass_word", "pass.word", "pass-word",
	} {
		if !policy.SensitiveField(key) {
			t.Errorf("want %q classified sensitive", key)
		}
	}
	if policy.SensitiveField("passwordless.enabled") {
		t.Fatal("an unrelated field must not be classified by substring alone")
	}
}

func TestMixedScriptAndFullwidthOpaqueAssignmentKeysFailClosed(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, unsafe := range []string{
		"passwоrd=hunter2", "ｐａｓｓｗｏｒｄ=hunter2", `pass\u0077ord=hunter2`,
	} {
		got := policy.SafeText(unsafe)
		if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("disguised assignment key must fail closed for %q, got %+v", unsafe, got)
		}
	}
}

func TestDisguisedAssignmentsAfterEarlierSeparatorsFailClosedEveryBoundary(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, unsafe := range []string{
		"passwоrd=hunter2",
		"ｐａｓｓｗｏｒｄ=hunter2",
		"12:34 passwоrd=hunter2",
		"prefix: ｐａｓｓｗｏｒｄ=hunter2",
		"status=ok passwоrd=hunter2",
		"passwοrd: hunter2",
		"passwᴏrd: hunter2",
		"密碼: hunter2",
	} {
		t.Run(unsafe, func(t *testing.T) {
			text := policy.Text(unsafe)
			if text.Text != redact.WithheldText || !containsString(text.RuleIDs, "safety.unsafe_unicode") {
				t.Fatalf("Text must withhold disguised assignment opaquely, got %+v", text)
			}
			if strings.Contains(text.Text, "hunter2") || policy.Text(text.Text).Text != text.Text {
				t.Fatalf("Text result leaked or was unstable: first=%+v second=%+v", text, policy.Text(text.Text))
			}

			safe := policy.SafeText(unsafe)
			if safe.Value.Kind != model.SafeKindWithheld || safe.Value.Withheld != redact.ReasonRedactionFailure {
				t.Fatalf("SafeText must return typed withholding, got %+v", safe)
			}
			if err := policy.ValidateText(unsafe); !errors.Is(err, redact.ErrProhibitedContent) {
				t.Fatalf("final text scan accepted disguised assignment: %v", err)
			} else if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("final text scan echoed content: %v", err)
			}
			forged := model.NormalizedLog{Body: model.SafeString(unsafe)}
			if err := policy.ValidateRecord(forged); !errors.Is(err, redact.ErrProhibitedContent) {
				t.Fatalf("persistence-facing scan accepted disguised assignment: %v", err)
			} else if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("record scan echoed content: %v", err)
			}
		})
	}
}

func TestOpaqueAssignmentGuardAllowsASCIILabelsWithUnicodeValuesAndTimestamps(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, safe := range []string{
		"meeting starts at 12:34",
		"message=привет",
	} {
		if got := policy.SafeText(safe); got.Value.Kind != model.SafeKindString || got.Value.String != safe {
			t.Fatalf("ordinary safe text %q was withheld: %+v", safe, got)
		}
		if err := policy.ValidateText(safe); err != nil {
			t.Fatalf("ordinary safe text %q failed final scan: %v", safe, err)
		}
	}
}

func TestForbiddenValuesComposedByLaterRulesBecomeStableWithholding(t *testing.T) {
	for _, test := range []struct {
		forbidden string
		input     string
	}{
		{forbidden: "]/x", input: "alice@example.com/x"},
		{forbidden: "]/tail", input: "password=hunter2\n[REDACTED_SECRET]/tail"},
		{forbidden: ">/suffix", input: "order 1234/suffix"},
	} {
		policy, err := redact.MinimalPolicy().WithForbiddenValues(test.forbidden)
		if err != nil {
			t.Fatalf("configure %q: %v", test.forbidden, err)
		}
		once := policy.Text(test.input)
		if once.Text != redact.WithheldText || !containsString(once.RuleIDs, "safety.composed_forbidden") {
			t.Fatalf("newly composed forbidden value must withhold stably: %+v", once)
		}
		twice := policy.Text(once.Text)
		if twice.Text != once.Text || !containsString(twice.RuleIDs, "safety.preexisting_marker") {
			t.Fatalf("withheld result changed on retry: first=%+v second=%+v", once, twice)
		}
		safe := policy.SafeText(test.input)
		if safe.Value.Kind != model.SafeKindWithheld || safe.Value.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("SafeText must represent composed forbidden output as withheld: %+v", safe)
		}
	}
}

func TestBareCarriageReturnsCannotHideSecretAssignments(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, input := range []string{
		"safe\rAuthorization: Bearer hunter2\rnext=safe",
		"safe\rCookie: session=hunter2\rnext=safe",
		"safe\rpassword=hunter2\rnext=safe",
		"safe\r\npass_word=hunter2\r\nnext=safe",
	} {
		once := policy.Text(input)
		if strings.Contains(once.Text, "hunter2") || strings.Contains(once.Text, "\r") {
			t.Fatalf("CR-delimited assignment survived: %q", once.Text)
		}
		if err := policy.ValidateText(once.Text); err != nil {
			t.Fatalf("canonical result failed final scan: %v", err)
		}
		if twice := policy.Text(once.Text); twice.Text != once.Text {
			t.Fatalf("CR canonicalization is not idempotent: first=%+v second=%+v", once, twice)
		}
	}
	if err := policy.ValidateText("safe\rpassword=hunter2"); !errors.Is(err, redact.ErrProhibitedContent) {
		t.Fatalf("final scan missed CR-delimited secret: %v", err)
	}
}

func TestPreexistingMarkersCarryProvenanceAndWithheldMarkerGetsTyped(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, marker := range []string{"[REDACTED_EMAIL]", "prefix [REDACTED_SECRET] suffix"} {
		got := policy.SafeText(marker)
		if got.Value.Kind != model.SafeKindString || got.Value.String != marker || !containsString(got.RuleIDs, "safety.preexisting_marker") {
			t.Fatalf("raw marker lacks provenance: %+v", got)
		}
	}
	got := policy.SafeText(redact.WithheldText)
	if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure || !containsString(got.RuleIDs, "safety.preexisting_marker") {
		t.Fatalf("raw withheld marker must become typed withholding: %+v", got)
	}
}

func TestStructuredJSONLimitsAreExactAndFailClosed(t *testing.T) {
	policy := redact.MinimalPolicy()
	if nodes, members, ok := redact.StructuredJSONShape(`{"a":[0,1],"b":true}`); !ok || nodes != 5 || members != 2 {
		t.Fatalf("structured shape mismatch: nodes=%d members=%d ok=%t", nodes, members, ok)
	}
	if _, _, ok := redact.StructuredJSONShape(nestedJSON(redact.DefaultMaxStructuredDepth + 1)); ok {
		t.Fatal("over-depth structured shape must fail closed")
	}
	for _, malformed := range []string{"", `{"unterminated":[0,1]`, "0 1"} {
		if _, _, ok := redact.StructuredJSONShape(malformed); ok {
			t.Fatalf("malformed structured shape must fail closed: %q", malformed)
		}
	}
	if got := policy.StructuredText(nestedJSON(redact.DefaultMaxStructuredDepth)); got.Value.Kind != model.SafeKindMap {
		t.Fatalf("exact structured depth must pass, got %+v", got.Value)
	}
	for name, exact := range map[string]string{
		"flat array":  flatJSONArray(redact.DefaultMaxStructuredNodes - 2),
		"flat object": flatJSONObject(redact.DefaultMaxStructuredNodes - 1),
	} {
		if got := policy.StructuredText(exact); got.Value.Kind == model.SafeKindWithheld {
			t.Fatalf("%s at the exact node budget must pass, got %+v", name, got.Value)
		}
	}
	for _, unsafe := range []string{
		nestedJSON(redact.DefaultMaxStructuredDepth + 1),
		nestedJSON(20_000),
		flatJSONArray(redact.DefaultMaxStructuredNodes - 1),
		flatJSONObject(redact.DefaultMaxStructuredNodes),
	} {
		got := policy.StructuredText(unsafe)
		if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("over-limit JSON must be withheld, got %+v", got.Value)
		}
	}
}

func TestStructuredJSONDuplicateKeysAreWithheldBeforeMapMaterialization(t *testing.T) {
	policy := redact.MinimalPolicy()
	inputs := []string{
		`{"private_member_name":"first-secret","private_member_name":"second-secret"}`,
		`{"outer":{"private_member_name":"first-secret","private_member_name":"second-secret"}}`,
		`{"private_member_name":"first-secret","private_member_name":"second-secret","private_member_name":"third-secret"}`,
		`{"pass\u0077ord":"first-secret","password":"second-secret"}`,
		`{"foo":"first-secret","f\u006fo":"second-secret"}`,
		`{"private_member_name":"second-secret","safe":true,"private_member_name":"first-secret"}`,
		`{"safe":true,"private_member_name":"first-secret","private_member_name":"second-secret"}`,
	}
	var canonical redact.SafeResult
	for index, input := range inputs {
		got := policy.StructuredText(input)
		if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure ||
			!reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_duplicate_key"}) {
			t.Fatalf("duplicate input %d did not fail closed categorically: %+v", index, got)
		}
		if _, _, ok := redact.StructuredJSONShape(input); ok {
			t.Fatalf("duplicate input %d must not project as a materialized JSON map", index)
		}
		encoded := got.Value.Withheld + strings.Join(got.RuleIDs, "|")
		for _, secret := range []string{"first-secret", "second-secret", "third-secret", "private_member_name", "password", "foo"} {
			if strings.Contains(encoded, secret) {
				t.Fatalf("duplicate result leaked member content %q: %+v", secret, got)
			}
		}
		if repeated := policy.StructuredText(input); !reflect.DeepEqual(repeated, got) {
			t.Fatalf("duplicate result is nondeterministic: first=%+v repeated=%+v", got, repeated)
		}
		if index == 0 {
			canonical = got
		} else if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("duplicate permutations must have one stable result: want=%+v got=%+v", canonical, got)
		}
		if reapplied := policy.StructuredText(redact.WithheldText); !reflect.DeepEqual(reapplied.Value, got.Value) {
			t.Fatalf("withheld duplicate representation is not idempotent: first=%+v reapplied=%+v", got, reapplied)
		}
	}
}

func TestStructuredJSONDuplicateKeyScopeAndBoundaries(t *testing.T) {
	policy := redact.MinimalPolicy()
	siblings := `{"left":{"same":"one"},"right":{"same":"two"}}`
	if got := policy.StructuredText(siblings); got.Value.Kind != model.SafeKindMap {
		t.Fatalf("same key in sibling objects is not a duplicate: %+v", got)
	}

	exactDepthDuplicate := strings.Repeat(`{"outer":`, redact.DefaultMaxStructuredDepth-1) +
		`{"private_member_name":"first","private_member_name":"second"}` + strings.Repeat("}", redact.DefaultMaxStructuredDepth-1)
	if got := policy.StructuredText(exactDepthDuplicate); !reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_duplicate_key"}) {
		t.Fatalf("duplicate at exact depth must retain duplicate category: %+v", got)
	}
	overDepthDuplicate := `{"outer":` + exactDepthDuplicate + `}`
	if got := policy.StructuredText(overDepthDuplicate); !reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_limit"}) {
		t.Fatalf("over-depth duplicate must hit the structural limit first: %+v", got)
	}

	var exact strings.Builder
	exact.WriteByte('{')
	for index := 0; index < redact.DefaultMaxStructuredNodes-2; index++ {
		if index > 0 {
			exact.WriteByte(',')
		}
		exact.WriteString(`"k`)
		exact.WriteString(strconv.Itoa(index))
		exact.WriteString(`":0`)
	}
	exact.WriteString(`,"k0":1}`)
	if got := policy.StructuredText(exact.String()); !reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_duplicate_key"}) {
		t.Fatalf("duplicate at exact node/member budget must be categorized: %+v", got)
	}
}

func TestStructuredJSONDuplicateAndLimitPreflightPrecedeForbiddenFallback(t *testing.T) {
	policy, err := redact.MinimalPolicy().WithForbiddenValues("first-secret")
	if err != nil {
		t.Fatalf("configure forbidden value: %v", err)
	}
	duplicates := []string{
		`{"private_member_name":"first-secret","private_member_name":"second-secret"}`,
		`{"private_member_name":"second-secret","private_member_name":"first-secret"}`,
	}
	for _, duplicate := range duplicates {
		got := policy.StructuredText(duplicate)
		if got.Value.Kind != model.SafeKindWithheld ||
			!reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_duplicate_key"}) {
			t.Fatalf("forbidden fallback bypassed duplicate preflight: %+v", got)
		}
		if repeated := policy.StructuredText(duplicate); !reflect.DeepEqual(repeated, got) {
			t.Fatalf("duplicate+forbidden result is unstable: first=%+v repeated=%+v", got, repeated)
		}
		if reapplied := policy.StructuredText(redact.WithheldText); !reflect.DeepEqual(reapplied.Value, got.Value) {
			t.Fatalf("duplicate+forbidden withholding is not idempotent: first=%+v reapplied=%+v", got, reapplied)
		}
	}
	overDepth := strings.Repeat(`{"outer":`, redact.DefaultMaxStructuredDepth) + duplicates[0] +
		strings.Repeat("}", redact.DefaultMaxStructuredDepth)
	if got := policy.StructuredText(overDepth); got.Value.Kind != model.SafeKindWithheld ||
		!reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_limit"}) {
		t.Fatalf("forbidden fallback bypassed structural limit precedence: %+v", got)
	}
	overNodes := `["first-secret"` + strings.Repeat(",0", redact.DefaultMaxStructuredNodes-1) + `]`
	if got := policy.StructuredText(overNodes); got.Value.Kind != model.SafeKindWithheld ||
		!reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_limit"}) {
		t.Fatalf("forbidden fallback bypassed node limit precedence: %+v", got)
	}
	duplicateThenOverNodes := `{"private_member_name":"first-secret","private_member_name":"second-secret","tail":[0` +
		strings.Repeat(",0", redact.DefaultMaxStructuredNodes) + `]}`
	if got := policy.StructuredText(duplicateThenOverNodes); got.Value.Kind != model.SafeKindWithheld ||
		!reflect.DeepEqual(got.RuleIDs, []string{"safety.structured_limit"}) {
		t.Fatalf("structural limit must take precedence even after an earlier duplicate: %+v", got)
	}
	malformed := `{"safe":"first-secret"`
	if got := policy.StructuredText(malformed); got.Value.Kind != model.SafeKindString || strings.Contains(got.Value.String, "first-secret") {
		t.Fatalf("malformed forbidden JSON was not conservatively redacted: %+v", got)
	}
}

func nestedJSON(depth int) string {
	return strings.Repeat(`{"x":`, depth) + `"safe"` + strings.Repeat("}", depth)
}

func flatJSONArray(nodes int) string {
	return "[" + strings.Repeat("0,", nodes) + "0]"
}

func flatJSONObject(nodes int) string {
	var out strings.Builder
	out.WriteByte('{')
	for index := 0; index < nodes; index++ {
		if index > 0 {
			out.WriteByte(',')
		}
		out.WriteString(`"k`)
		out.WriteString(strconv.Itoa(index))
		out.WriteString(`":0`)
	}
	out.WriteByte('}')
	return out.String()
}

func TestUnsafeUnparseableContentBecomesWithheldMetadata(t *testing.T) {
	unsafe := string([]byte{'{', '"', 'p', 'a', 's', 's', 'w', 'o', 'r', 'd', '"', ':', '"', 0xff})
	got := redact.MinimalPolicy().StructuredText(unsafe)
	if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure {
		t.Fatalf("unsafe unparseable content must be withheld, got %+v", got.Value)
	}
	if strings.Contains(got.Value.Withheld, "password") || strings.Contains(got.Value.Withheld, unsafe) {
		t.Fatal("withheld metadata must not retain original content")
	}
}

func TestFinalValidationRejectsUnsafeSafeValueWithoutEchoingIt(t *testing.T) {
	secret := "password=do-not-echo"
	err := redact.MinimalPolicy().ValidateValue(model.SafeString(secret))
	if !errors.Is(err, redact.ErrProhibitedContent) {
		t.Fatalf("want prohibited content error, got %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("error retained prohibited content: %v", err)
	}
}

func TestFinalValidationRejectsUnknownAndEveryMismatchedSafeValuePayload(t *testing.T) {
	policy := redact.MinimalPolicy()
	secret := "password=hunter2"
	unknown := model.SafeValue{Kind: model.SafeValueKind("future"), String: secret}
	assertOpaqueProhibitedValue(t, policy, unknown, secret)
	if err := policy.ValidateRecord(model.NormalizedLog{Body: unknown}); !errors.Is(err, redact.ErrProhibitedContent) {
		t.Fatalf("ValidateRecord must inherit unknown-kind rejection, got %v", err)
	} else if strings.Contains(err.Error(), "future") || strings.Contains(err.Error(), secret) {
		t.Fatalf("ValidateRecord error echoed forged content: %v", err)
	}

	bases := []struct {
		name    string
		allowed string
		value   model.SafeValue
	}{
		{name: "empty", value: model.SafeValue{}},
		{name: "string", allowed: "string", value: model.SafeString("safe")},
		{name: "int", allowed: "int", value: model.SafeInt(1)},
		{name: "double", allowed: "double", value: model.SafeDouble(1.5)},
		{name: "bool", allowed: "bool", value: model.SafeBool(true)},
		{name: "map", allowed: "map", value: model.SafeMap(map[string]model.SafeValue{"safe": model.SafeBool(true)})},
		{name: "slice", allowed: "slice", value: model.SafeSlice(model.SafeBool(true))},
		{name: "withheld", allowed: "withheld", value: model.Withheld("safe reason")},
	}
	payloads := []string{"string", "int", "double", "bool", "map", "slice", "withheld"}
	for _, base := range bases {
		for _, payload := range payloads {
			if payload == base.allowed {
				continue
			}
			t.Run(base.name+"_hides_"+payload, func(t *testing.T) {
				forged := base.value
				setForgedPayload(&forged, payload, secret)
				assertOpaqueProhibitedValue(t, policy, forged, secret)
			})
		}
	}
}

func TestFinalValidationRejectsInvalidEmptyWithheldAndNestedShapes(t *testing.T) {
	policy := redact.MinimalPolicy()
	invalid := []model.SafeValue{
		{Kind: model.SafeKindWithheld},
		{Kind: model.SafeKindWithheld, Withheld: "   "},
		model.SafeMap(map[string]model.SafeValue{"child": {Kind: model.SafeKindInt, String: "password=hunter2"}}),
		model.SafeSlice(model.SafeMap(map[string]model.SafeValue{"child": {Kind: model.SafeValueKind("future")}})),
		model.SafeMap(map[string]model.SafeValue{" ": model.SafeBool(false)}),
	}
	for index, value := range invalid {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			assertOpaqueProhibitedValue(t, policy, value, "password=hunter2")
		})
	}
	for _, record := range []model.NormalizedLog{
		{Attributes: map[string]model.SafeValue{"nested": invalid[2]}},
		{ResourceAttributes: map[string]model.SafeValue{"nested": invalid[3]}},
		{ScopeAttributes: map[string]model.SafeValue{"nested": invalid[0]}},
	} {
		if err := policy.ValidateRecord(record); !errors.Is(err, redact.ErrProhibitedContent) {
			t.Fatalf("ValidateRecord accepted nested forged value: %v", err)
		}
	}
}

func TestFinalValidationAcceptsLegitimateZeroScalarShapes(t *testing.T) {
	policy := redact.MinimalPolicy()
	valid := []model.SafeValue{
		{}, model.SafeString(""), model.SafeInt(0), model.SafeDouble(0), model.SafeBool(false),
		model.SafeMap(nil), model.SafeMap(map[string]model.SafeValue{}),
		model.SafeSlice(), {Kind: model.SafeKindSlice, Slice: []model.SafeValue{}},
		model.Withheld("safe reason"),
	}
	for index, value := range valid {
		if err := policy.ValidateValue(value); err != nil {
			t.Errorf("valid zero shape %d rejected: %v", index, err)
		}
	}
}

func TestFinalValidationRejectsForgedUnsafeStructuralKeys(t *testing.T) {
	policy := redact.MinimalPolicy()
	invalidUTF8 := string([]byte{'p', 'a', 's', 's', 0xff, 'w', 'o', 'r', 'd'})
	unsafeKeys := []string{
		"passwοrd", "passwᴏrd", "ｐａｓｓｗｏｒｄ", `pass\u0077ord`, invalidUTF8,
	}
	for index, key := range unsafeKeys {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			nested := model.SafeSlice(model.SafeMap(map[string]model.SafeValue{key: model.SafeString("hunter2")}))
			if err := policy.ValidateValue(nested); !errors.Is(err, redact.ErrProhibitedContent) {
				t.Fatalf("nested forged key passed final value scan: %v", err)
			} else if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), key) {
				t.Fatalf("nested key error echoed hostile content: %v", err)
			}
			for _, record := range []model.NormalizedLog{
				{Attributes: map[string]model.SafeValue{key: model.SafeString("hunter2")}},
				{ResourceAttributes: map[string]model.SafeValue{"nested": nested}},
				{ScopeAttributes: map[string]model.SafeValue{key: model.SafeString("hunter2")}},
			} {
				if err := policy.ValidateRecord(record); !errors.Is(err, redact.ErrProhibitedContent) {
					t.Fatalf("forged record key passed persistence scan: %v", err)
				} else if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), key) {
					t.Fatalf("record key error echoed hostile content: %v", err)
				}
			}
		})
	}
}

func TestFinalValidationAllowsOnlyTypedGeneratedWithheldKeys(t *testing.T) {
	policy := redact.MinimalPolicy()
	valid := model.SafeMap(map[string]model.SafeValue{
		"[WITHHELD_FIELD_NAME]":   model.Withheld(redact.ReasonRedactionFailure),
		"[WITHHELD_FIELD_NAME]#2": model.Withheld(redact.ReasonRedactionFailure),
	})
	if err := policy.ValidateValue(valid); err != nil {
		t.Fatalf("generated withheld ordinals must pass final validation: %v", err)
	}
	for _, invalid := range []string{"[WITHHELD_FIELD_NAME]#1", "[WITHHELD_FIELD_NAME]#02", "[WITHHELD_FIELD_NAME]#x"} {
		value := model.SafeMap(map[string]model.SafeValue{invalid: model.Withheld(redact.ReasonRedactionFailure)})
		if err := policy.ValidateValue(value); !errors.Is(err, redact.ErrProhibitedContent) {
			t.Fatalf("forged withheld ordinal %q must fail closed: %v", invalid, err)
		}
	}
	forged := model.SafeMap(map[string]model.SafeValue{"[WITHHELD_FIELD_NAME]": model.SafeString("hunter2")})
	if err := policy.ValidateValue(forged); !errors.Is(err, redact.ErrProhibitedContent) {
		t.Fatalf("withheld key with retained value must fail closed: %v", err)
	}
}

func TestFinalValidationRequiresCanonicalValuesForSensitiveKeys(t *testing.T) {
	policy := redact.MinimalPolicy()
	for _, key := range []string{"password", "customer_id", "session_id", "cvv"} {
		t.Run(key, func(t *testing.T) {
			secret := "hostile-" + key
			nested := model.SafeMap(map[string]model.SafeValue{key: model.SafeString(secret)})
			if err := policy.ValidateValue(nested); !errors.Is(err, redact.ErrProhibitedContent) {
				t.Fatalf("nested sensitive key retained its value: %v", err)
			} else if strings.Contains(err.Error(), secret) {
				t.Fatalf("nested sensitive-key error echoed content: %v", err)
			}
			for _, record := range []model.NormalizedLog{
				{Attributes: map[string]model.SafeValue{key: model.SafeString(secret)}},
				{ResourceAttributes: map[string]model.SafeValue{key: model.SafeString(secret)}},
				{ScopeAttributes: map[string]model.SafeValue{key: model.SafeString(secret)}},
				{Attributes: map[string]model.SafeValue{"nested": nested}},
			} {
				if err := policy.ValidateRecord(record); !errors.Is(err, redact.ErrProhibitedContent) {
					t.Fatalf("forged sensitive record value passed final scan: %v", err)
				} else if strings.Contains(err.Error(), secret) {
					t.Fatalf("sensitive-key record error echoed content: %v", err)
				}
			}
		})
	}

	canonical := model.SafeString("[REDACTED_FIELD]")
	values := map[string]model.SafeValue{}
	for _, key := range []string{"password", "customer_id", "session_id", "cvv"} {
		values[key] = canonical
	}
	if err := policy.ValidateValue(model.SafeMap(values)); err != nil {
		t.Fatalf("canonical sensitive-field redactions must pass nested validation: %v", err)
	}
	record := model.NormalizedLog{Attributes: values, ResourceAttributes: values, ScopeAttributes: values}
	if err := policy.ValidateRecord(record); err != nil {
		t.Fatalf("canonical sensitive-field redactions must pass record scan: %v", err)
	}
}

func TestFinalValidationBoundsForgedDeepSafeValuesIteratively(t *testing.T) {
	value := model.SafeString("safe")
	for depth := 0; depth < 20_000; depth++ {
		value = model.SafeSlice(value)
	}
	if err := redact.MinimalPolicy().ValidateValue(value); !errors.Is(err, redact.ErrProhibitedContent) {
		t.Fatalf("deep forged value must fail closed without recursive overflow, got %v", err)
	}
}

func setForgedPayload(value *model.SafeValue, payload, secret string) {
	switch payload {
	case "string":
		value.String = secret
	case "int":
		value.Int = 7
	case "double":
		value.Double = 1.5
	case "bool":
		value.Bool = true
	case "map":
		value.Map = map[string]model.SafeValue{"hidden": model.SafeString(secret)}
	case "slice":
		value.Slice = []model.SafeValue{model.SafeString(secret)}
	case "withheld":
		value.Withheld = secret
	}
}

func assertOpaqueProhibitedValue(t *testing.T, policy *redact.Policy, value model.SafeValue, secrets ...string) {
	t.Helper()
	err := policy.ValidateValue(value)
	if !errors.Is(err, redact.ErrProhibitedContent) {
		t.Fatalf("forged SafeValue must fail closed, got %v for %+v", err, value)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("validation error echoed forged payload %q: %v", secret, err)
		}
	}
}

func TestFinalValidationRejectsAllPolicyCoveredCustomerData(t *testing.T) {
	policy := redact.MinimalPolicy()
	tests := map[string]model.NormalizedLog{
		"email value":     {Body: model.SafeString("alice@example.com")},
		"query value":     {Body: model.SafeString("GET /pay?customer=alice")},
		"email key":       {Attributes: map[string]model.SafeValue{"alice@example.com": model.SafeBool(true)}},
		"exception query": {Exception: &model.NormalizedException{SafeMessage: "GET /pay?token=secret"}},
		"raw reference":   {RawReference: &model.RegionalLogReference{Locator: "logs://group?token=secret"}},
	}
	for name, record := range tests {
		t.Run(name, func(t *testing.T) {
			if err := policy.ValidateRecord(record); !errors.Is(err, redact.ErrProhibitedContent) {
				t.Fatalf("forged safe record must fail final scan, got %v", err)
			}
		})
	}
	for _, safe := range []string{"[REDACTED_EMAIL]", "GET /pay[REDACTED_QUERY]"} {
		if err := policy.ValidateValue(model.SafeString(safe)); err != nil {
			t.Fatalf("policy placeholder %q must remain valid: %v", safe, err)
		}
	}
}

func TestUnicodeDisguisesFailClosed(t *testing.T) {
	for _, unsafe := range []string{
		"pass\u200bword=hunter2",
		"password\uff1ahunter2",
	} {
		got := redact.MinimalPolicy().SafeText(unsafe)
		if got.Value.Kind != model.SafeKindWithheld || got.Value.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("unicode-disguised secret must be withheld, got %+v", got.Value)
		}
	}
	if got := redact.MinimalPolicy().StructuredText(`{"pass\u200bword":"hunter2"}`); got.Value.Kind != model.SafeKindWithheld {
		t.Fatalf("unicode-disguised structured key must withhold content, got %+v", got.Value)
	}
	if got := redact.MinimalPolicy().StructuredText(`{"pass\u200bword":"hunter2"`); got.Value.Kind != model.SafeKindWithheld {
		t.Fatalf("escaped disguise in malformed JSON must withhold content, got %+v", got.Value)
	}
	for _, unsafe := range []string{
		`{"pass\u0077ord":"hunter2"`,
		`{"ｐａｓｓｗｏｒｄ":"hunter2"}`,
		`{"passwоrd":"hunter2"}`,
		`{"ｐａｓｓｗｏｒｄ":"hunter2"`,
		`{"passwоrd":"hunter2"`,
	} {
		if got := redact.MinimalPolicy().StructuredText(unsafe); got.Value.Kind != model.SafeKindWithheld {
			t.Fatalf("disguised structured key must withhold content for %q, got %+v", unsafe, got.Value)
		}
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
