package normalize_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/normalize"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
)

func TestNormalizationProducesOnlyFinallyValidatedSafeContent(t *testing.T) {
	s := newScenario(t)
	source := s.producer.Record(
		otlpgen.WithBody("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature"),
		otlpgen.WithAttribute("password", otlpgen.StringValue("hunter2")),
		otlpgen.WithAttribute("database.url", otlpgen.StringValue("postgres://alice:swordfish@db.example/payments")),
	)

	record := s.admitOne(t, source)
	if err := redact.MinimalPolicy().ValidateRecord(record); err != nil {
		t.Fatalf("normalized content failed the mandatory final scan: %v", err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal safe representation: %v", err)
	}
	for _, prohibited := range []string{"eyJhbGciOi", "hunter2", "swordfish"} {
		if strings.Contains(string(encoded), prohibited) {
			t.Fatalf("safe representation retained prohibited value %q", prohibited)
		}
	}
}

func TestStructuredAndMalformedJSONAreRedactedInBodyAndAttributes(t *testing.T) {
	s := newScenario(t)
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithBody(`{"outer":{"password":"body-secret","contact":"body@example.com"}}`),
		otlpgen.WithAttribute("structured", otlpgen.StringValue(`{"nested":{"password":"attribute-secret"}}`)),
		otlpgen.WithAttribute("malformed", otlpgen.StringValue(`{"password" : "malformed-secret" suffix-secret`)),
	))
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal normalized record: %v", err)
	}
	for _, secret := range []string{"body-secret", "body@example.com", "attribute-secret", "malformed-secret", "suffix-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("normalized record retained structured secret %q: %s", secret, encoded)
		}
	}
	if record.Body.Kind != model.SafeKindMap || record.Attributes["structured"].Kind != model.SafeKindMap {
		t.Fatalf("valid JSON was not structurally redacted: body=%+v attribute=%+v", record.Body, record.Attributes["structured"])
	}
}

func TestDuplicateStructuredKeysWithholdBodiesAndAttributes(t *testing.T) {
	s := newScenario(t)
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithBody(`{"body_private_member":"body-first-secret","body_private_member":"body-second-secret"}`),
		otlpgen.WithAttribute("structured", otlpgen.StringValue(`{"outer":{"pass\u0077ord":"attribute-first-secret","password":"attribute-second-secret"}}`)),
	))
	if record.Body.Kind != model.SafeKindWithheld || record.Attributes["structured"].Kind != model.SafeKindWithheld {
		t.Fatalf("duplicate structured content must be withheld: body=%+v attribute=%+v", record.Body, record.Attributes["structured"])
	}
	if !containsRule(record.Redaction.RuleIDs, "safety.structured_duplicate_key") {
		t.Fatalf("duplicate-key rule provenance missing: %+v", record.Redaction)
	}
	wantPaths := map[string]bool{"body": true, "attributes.structured": true}
	for _, path := range record.Redaction.WithheldFields {
		delete(wantPaths, path)
	}
	if len(wantPaths) != 0 {
		t.Fatalf("duplicate-key withheld paths missing: %+v", record.Redaction.WithheldFields)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal duplicate-key record: %v", err)
	}
	for _, secret := range []string{"body-first-secret", "body-second-secret", "attribute-first-secret", "attribute-second-secret", "body_private_member", "password"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("duplicate-key normalization leaked %q: %s", secret, encoded)
		}
	}
}

func TestStructuredPreflightPrecedesForbiddenFallbackDuringNormalization(t *testing.T) {
	policy, err := redact.MinimalPolicy().WithForbiddenValues("first-secret")
	if err != nil {
		t.Fatalf("configure forbidden value: %v", err)
	}
	s := newScenario(t)
	s.normalizer = normalize.New(normalize.WithRedactionPolicy(policy))
	overDepth := strings.Repeat(`{"outer":`, redact.DefaultMaxStructuredDepth+1) + `"first-secret"` +
		strings.Repeat("}", redact.DefaultMaxStructuredDepth+1)
	overNodes := `["first-secret"` + strings.Repeat(",0", redact.DefaultMaxStructuredNodes-1) + `]`
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithBody(`{"private_member_name":"first-secret","private_member_name":"body-second-secret"}`),
		otlpgen.WithAttribute("duplicate_reverse", otlpgen.StringValue(`{"private_member_name":"attribute-second-secret","private_member_name":"first-secret"}`)),
		otlpgen.WithAttribute("over_depth", otlpgen.StringValue(overDepth)),
		otlpgen.WithAttribute("over_nodes", otlpgen.StringValue(overNodes)),
		otlpgen.WithAttribute("malformed", otlpgen.StringValue(`{"safe":"first-secret"`)),
	))
	for _, value := range []model.SafeValue{
		record.Body, record.Attributes["duplicate_reverse"], record.Attributes["over_depth"], record.Attributes["over_nodes"],
	} {
		if value.Kind != model.SafeKindWithheld {
			t.Fatalf("duplicate/limit content bypassed preflight: %+v", value)
		}
	}
	if record.Attributes["malformed"].Kind != model.SafeKindString {
		t.Fatalf("malformed forbidden content must remain safely redacted: %+v", record.Attributes["malformed"])
	}
	if !containsRule(record.Redaction.RuleIDs, "safety.structured_duplicate_key") ||
		!containsRule(record.Redaction.RuleIDs, "safety.structured_limit") ||
		!containsRule(record.Redaction.RuleIDs, "service.forbidden_value") {
		t.Fatalf("preflight/fallback provenance missing: %+v", record.Redaction.RuleIDs)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal normalized record: %v", err)
	}
	for _, secret := range []string{"first-secret", "body-second-secret", "attribute-second-secret", "private_member_name"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("ordered preflight/fallback leaked %q: %s", secret, encoded)
		}
	}
	if err := policy.ValidateRecord(record); err != nil {
		t.Fatalf("ordered preflight/fallback output failed final scan: %v", err)
	}
}

func TestWithheldFieldsContainStableSafePaths(t *testing.T) {
	s := newScenario(t)
	invalidUTF8 := string([]byte{'x', 0xff})
	record := s.producer.Record(
		otlpgen.WithBodyValue(&common.AnyValue{Value: &common.AnyValue_BytesValue{BytesValue: []byte("body-secret")}}),
		otlpgen.WithAttribute("unsafe", otlpgen.StringValue(invalidUTF8)),
		otlpgen.WithAttribute("nested", &common.AnyValue{Value: &common.AnyValue_ArrayValue{ArrayValue: &common.ArrayValue{Values: []*common.AnyValue{
			otlpgen.StringValue(invalidUTF8),
		}}}}),
		otlpgen.WithAttribute("json_nested", otlpgen.StringValue(`{"safe":{"value":"pass\u200bword"}}`)),
		otlpgen.WithAttribute("pass\u200bword", otlpgen.StringValue("unicode-key-secret")),
	)
	request := s.producer.Request(record)
	request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes,
		&common.KeyValue{Key: "resource_unsafe", Value: otlpgen.StringValue(invalidUTF8)})
	request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = append(request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes,
		&common.KeyValue{Key: "scope_unsafe", Value: otlpgen.StringValue(invalidUTF8)})
	result, err := s.normalizer.Request(s.envelope, request)
	if err != nil || len(result.Rejected) != 0 || len(result.Records) != 1 {
		t.Fatalf("want safe withheld record, result=%+v err=%v", result, err)
	}
	want := []string{"attributes.[WITHHELD_FIELD_NAME]", "attributes.json_nested.safe.value", "attributes.nested[0]", "attributes.unsafe", "body", "resource_attributes.resource_unsafe", "scope_attributes.scope_unsafe"}
	got := result.Records[0].Redaction.WithheldFields
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want stable withheld paths %v, got %v", want, got)
	}
	encoded, marshalErr := json.Marshal(result.Records[0])
	if marshalErr != nil || strings.Contains(string(encoded), "unicode-key-secret") {
		t.Fatalf("unsafe key/value leaked through safe metadata: encoded=%s err=%v", encoded, marshalErr)
	}
}

func TestUnsafeExceptionAndScalarStringsAddWithheldPaths(t *testing.T) {
	s := newScenario(t)
	invalid := string([]byte{'x', 0xff})
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithSeverity(logs.SeverityNumber_SEVERITY_NUMBER_ERROR, invalid),
		otlpgen.WithEventName(invalid),
		otlpgen.WithException(invalid, invalid, invalid),
	))
	want := []string{
		"attributes.exception.message", "attributes.exception.stacktrace", "attributes.exception.type",
		"event_name", "exception.safe_message", "exception.stack_frames", "exception.type", "severity_text",
	}
	if got := record.Redaction.WithheldFields; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want stable scalar/exception paths %v, got %v", want, got)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "\\ufffd") {
		t.Fatalf("invalid exception bytes were lossily retained: %s", encoded)
	}
}

func TestHostileClaimsAndUIDsNeverAppearInRejectionErrors(t *testing.T) {
	s := newScenario(t)
	secret := "password=hunter2"
	producer := otlpgen.New(otlpgen.WithService(secret))
	result, err := s.normalizer.Request(s.envelope, producer.Request(producer.Record()))
	if err != nil || len(result.Rejected) != 1 || !errors.Is(result.Rejected[0].Err, normalize.ErrClaimNotPermitted) {
		t.Fatalf("want categorized claim rejection, result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Rejected[0].Err.Error(), secret) || strings.Contains(result.Rejected[0].Err.Error(), "hunter2") {
		t.Fatalf("claim error echoed hostile value: %v", result.Rejected[0].Err)
	}

	result, err = s.normalizer.Request(s.envelope, s.producer.Request(s.producer.Record(otlpgen.WithRecordUID(secret))))
	if err != nil || len(result.Rejected) != 1 || !errors.Is(result.Rejected[0].Err, normalize.ErrUnusableIdentity) {
		t.Fatalf("want categorized identity rejection, result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Rejected[0].Err.Error(), secret) || strings.Contains(result.Rejected[0].Err.Error(), "hunter2") {
		t.Fatalf("identity rejection echoed hostile uid: %v", result.Rejected[0].Err)
	}
}

func TestMalformedAndDuplicateClaimErrorsNeverEchoPayloads(t *testing.T) {
	s := newScenario(t)
	secret := "password=hunter2"
	tests := []struct {
		name  string
		value *common.AnyValue
	}{
		{"duplicate string", otlpgen.StringValue(secret)},
		{"non-string", &common.AnyValue{Value: &common.AnyValue_BytesValue{BytesValue: []byte(secret)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := s.producer.Request(s.producer.Record())
			request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes,
				&common.KeyValue{Key: "service.name", Value: test.value})
			result, err := s.normalizer.Request(s.envelope, request)
			if err != nil || len(result.Rejected) != 1 || !errors.Is(result.Rejected[0].Err, normalize.ErrClaimNotPermitted) {
				t.Fatalf("want categorized per-record claim rejection, result=%+v err=%v", result, err)
			}
			if strings.Contains(result.Rejected[0].Err.Error(), secret) || strings.Contains(result.Rejected[0].Err.Error(), "hunter2") {
				t.Fatalf("claim error echoed hostile content: %v", result.Rejected[0].Err)
			}
		})
	}
}

func TestUnsafeOpaqueBodyIsWithheldWithoutRetainingTheOriginal(t *testing.T) {
	s := newScenario(t)
	unsafe := string([]byte{'p', 'a', 's', 's', 'w', 'o', 'r', 'd', '=', 0xff})
	record := s.admitOne(t, s.producer.Record(otlpgen.WithBody(unsafe)))

	if record.Body.Kind != model.SafeKindWithheld || record.Body.Withheld != redact.ReasonRedactionFailure {
		t.Fatalf("want withheld redaction metadata, got %+v", record.Body)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal safe representation: %v", err)
	}
	if strings.Contains(string(encoded), "password") {
		t.Fatalf("withheld representation retained original content: %s", encoded)
	}
}

func TestConfiguredForbiddenJSONScalarsDoNotSurviveNormalization(t *testing.T) {
	for _, forbidden := range []string{"1234", "true", "false", "12.75"} {
		t.Run(forbidden, func(t *testing.T) {
			policy, err := redact.MinimalPolicy().WithForbiddenValues(forbidden)
			if err != nil {
				t.Fatalf("configure policy: %v", err)
			}
			s := newScenario(t)
			s.normalizer = normalize.New(normalize.WithRedactionPolicy(policy))
			for _, body := range []string{
				`{"value":` + forbidden + `}`,
				`{"nested":{"value":` + forbidden + `}}`,
			} {
				record := s.admitOne(t, s.producer.Record(otlpgen.WithBody(body)))
				encoded, marshalErr := json.Marshal(record.Body)
				if marshalErr != nil {
					t.Fatalf("marshal: %v", marshalErr)
				}
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("forbidden scalar %q survived normalization: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestDisguisedStructuredKeysAreWithheldByNormalizer(t *testing.T) {
	s := newScenario(t)
	for _, body := range []string{
		`{"pass\u0077ord":"hunter2"`,
		`{"ｐａｓｓｗｏｒｄ":"hunter2"}`,
		`{"passwоrd":"hunter2"}`,
		`{"ｐａｓｓｗｏｒｄ":"hunter2"`,
		`{"passwоrd":"hunter2"`,
	} {
		record := s.admitOne(t, s.producer.Record(otlpgen.WithBody(body)))
		if record.Body.Kind != model.SafeKindWithheld || record.Body.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("disguised key in %q must withhold body, got %+v", body, record.Body)
		}
		encoded, err := json.Marshal(record)
		if err != nil || strings.Contains(string(encoded), "hunter2") {
			t.Fatalf("disguised key leaked original: %s err=%v", encoded, err)
		}
	}
}

func TestOpaqueSensitiveAssignmentsAreRemovedByNormalizer(t *testing.T) {
	s := newScenario(t)
	for _, label := range []string{
		"client_secret", "db_password", "x_api_key", "http_request_header_authorization",
	} {
		body := "safe line\n" + label + "=top-secret-value\nnext=safe"
		record := s.admitOne(t, s.producer.Record(otlpgen.WithBody(body)))
		if record.Body.Kind != model.SafeKindString || strings.Contains(record.Body.String, label) || strings.Contains(record.Body.String, "top-secret-value") {
			t.Fatalf("opaque assignment %q survived normalization: %+v", label, record.Body)
		}
	}
}

func TestDocumentedCustomerAssignmentsAreRemovedAcrossNormalizedTextBoundaries(t *testing.T) {
	s := newScenario(t)
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithBody("request_body=customer payload"),
		otlpgen.WithAttribute("opaque", otlpgen.StringValue("query_params=customer=alice")),
		otlpgen.WithException("PaymentDeclined", "cvv=123", "at charge (payment/charge.go:118)"),
		otlpgen.WithEventName("contact alice@例え.テスト"),
	))
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, fragment := range []string{"customer payload", "customer=alice", "alice@", "cvv=123"} {
		if strings.Contains(string(encoded), fragment) {
			t.Fatalf("normalized text retained documented customer fragment %q: %s", fragment, encoded)
		}
	}
	if err := redact.MinimalPolicy().ValidateRecord(record); err != nil {
		t.Fatalf("normalized customer-data replacements failed final scan: %v", err)
	}
}

func TestCollidingStructuredKeysWithholdBodyDeterministically(t *testing.T) {
	s := newScenario(t)
	body := `{"a@example.com":"first-secret","b@example.com":"second-secret"}`
	for iteration := 0; iteration < 100; iteration++ {
		record := s.admitOne(t, s.producer.Record(otlpgen.WithBody(body)))
		if record.Body.Kind != model.SafeKindWithheld || record.Body.Withheld != redact.ReasonRedactionFailure {
			t.Fatalf("iteration %d: collision must withhold body, got %+v", iteration, record.Body)
		}
		encoded, err := json.Marshal(record)
		if err != nil || strings.Contains(string(encoded), "first-secret") || strings.Contains(string(encoded), "second-secret") {
			t.Fatalf("iteration %d: collision leaked value: %s err=%v", iteration, encoded, err)
		}
	}
}

func TestStructuredStringLimitsWithholdAndReportCompletePaths(t *testing.T) {
	s := newScenario(t)
	deep := strings.Repeat(`{"x":`, redact.DefaultMaxStructuredDepth+1) + `"safe"` + strings.Repeat("}", redact.DefaultMaxStructuredDepth+1)
	flat := "[" + strings.Repeat("0,", redact.DefaultMaxStructuredNodes) + "0]"
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithBody(deep),
		otlpgen.WithAttribute("flat", otlpgen.StringValue(flat)),
	))
	if record.Body.Kind != model.SafeKindWithheld || record.Attributes["flat"].Kind != model.SafeKindWithheld {
		t.Fatalf("over-budget JSON strings must be withheld before materialization: body=%+v flat=%+v", record.Body, record.Attributes["flat"])
	}
	want := []string{"attributes.flat", "body"}
	if got := record.Redaction.WithheldFields; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want complete structured-limit paths %v, got %v", want, got)
	}
}

func TestUnsafeOTLPMapKeysUseStableOrdinalsWithoutLosingFields(t *testing.T) {
	s := newScenario(t)
	unsafeKeyOne := "pass\u200bword"
	unsafeKeyTwo := "password\uff1asecret"
	nested := &common.AnyValue{Value: &common.AnyValue_KvlistValue{KvlistValue: &common.KeyValueList{Values: []*common.KeyValue{
		{Key: unsafeKeyOne, Value: otlpgen.StringValue("nested-first-secret")},
		{Key: unsafeKeyTwo, Value: otlpgen.StringValue("nested-second-secret")},
	}}}}
	request := s.producer.Request(s.producer.Record(
		otlpgen.WithAttribute(unsafeKeyOne, otlpgen.StringValue("record-first-secret")),
		otlpgen.WithAttribute(unsafeKeyTwo, otlpgen.StringValue("record-second-secret")),
		otlpgen.WithAttribute("nested", nested),
	))
	request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes,
		&common.KeyValue{Key: unsafeKeyOne, Value: otlpgen.StringValue("resource-first-secret")},
		&common.KeyValue{Key: unsafeKeyTwo, Value: otlpgen.StringValue("resource-second-secret")})
	request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = append(request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes,
		&common.KeyValue{Key: unsafeKeyOne, Value: otlpgen.StringValue("scope-first-secret")},
		&common.KeyValue{Key: unsafeKeyTwo, Value: otlpgen.StringValue("scope-second-secret")})

	result, err := s.normalizer.Request(s.envelope, request)
	if err != nil || len(result.Rejected) != 0 || len(result.Records) != 1 {
		t.Fatalf("want unsafe keys safely admitted, result=%+v err=%v", result, err)
	}
	record := result.Records[0]
	want := []string{
		"attributes.[WITHHELD_FIELD_NAME]", "attributes.[WITHHELD_FIELD_NAME]#2",
		"attributes.nested.[WITHHELD_FIELD_NAME]", "attributes.nested.[WITHHELD_FIELD_NAME]#2",
		"resource_attributes.[WITHHELD_FIELD_NAME]", "resource_attributes.[WITHHELD_FIELD_NAME]#2",
		"scope_attributes.[WITHHELD_FIELD_NAME]", "scope_attributes.[WITHHELD_FIELD_NAME]#2",
	}
	if got := record.Redaction.WithheldFields; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want stable complete paths %v, got %v", want, got)
	}
	for _, values := range []map[string]model.SafeValue{record.Attributes, record.ResourceAttributes, record.ScopeAttributes} {
		if _, ok := values["[WITHHELD_FIELD_NAME]"]; !ok {
			t.Fatalf("first unsafe field was lost: %+v", values)
		}
		if _, ok := values["[WITHHELD_FIELD_NAME]#2"]; !ok {
			t.Fatalf("second unsafe field was lost: %+v", values)
		}
	}
	encoded, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		t.Fatalf("marshal safe record: %v", marshalErr)
	}
	for _, secret := range []string{"first-secret", "second-secret", unsafeKeyOne, unsafeKeyTwo} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("unsafe map content leaked through normalization: %s", encoded)
		}
	}
}

func TestEveryNonASCIIOrEscapedOTLPFieldNameIsWithheldAtEveryLevel(t *testing.T) {
	s := newScenario(t)
	unsafeKeys := []string{
		"passwοrd", "passwᴏrd", "ｐａｓｓｗｏｒｄ", "密碼", `pass\u0077ord`, string([]byte{'x', 0xff}),
	}
	makeValues := func(prefix string) []*common.KeyValue {
		values := make([]*common.KeyValue, 0, len(unsafeKeys))
		for index, key := range unsafeKeys {
			values = append(values, &common.KeyValue{Key: key, Value: otlpgen.StringValue(prefix + "-hunter2-" + strconv.Itoa(index))})
		}
		return values
	}
	nested := &common.AnyValue{Value: &common.AnyValue_KvlistValue{KvlistValue: &common.KeyValueList{Values: makeValues("nested")}}}
	record := s.producer.Record(otlpgen.WithAttribute("safe", otlpgen.StringValue("retained")), otlpgen.WithAttribute("nested", nested))
	record.Attributes = append(record.Attributes, makeValues("record")...)
	request := s.producer.Request(record)
	request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes, makeValues("resource")...)
	request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = append(request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes, makeValues("scope")...)
	result, err := s.normalizer.Request(s.envelope, request)
	if err != nil || len(result.Rejected) != 0 || len(result.Records) != 1 {
		t.Fatalf("normalize unsafe field names: result=%+v err=%v", result, err)
	}
	normalized := result.Records[0]
	valueSets := []map[string]model.SafeValue{
		normalized.Attributes, normalized.ResourceAttributes, normalized.ScopeAttributes, normalized.Attributes["nested"].Map,
	}
	for _, values := range valueSets {
		for index := range unsafeKeys {
			key := "[WITHHELD_FIELD_NAME]"
			if index > 0 {
				key += "#" + strconv.Itoa(index+1)
			}
			if values[key].Kind != model.SafeKindWithheld {
				t.Fatalf("unsafe field ordinal %q missing or not withheld: %+v", key, values)
			}
		}
	}
	var wantPaths []string
	for _, root := range []string{"attributes", "attributes.nested", "resource_attributes", "scope_attributes"} {
		for index := range unsafeKeys {
			path := root + ".[WITHHELD_FIELD_NAME]"
			if index > 0 {
				path += "#" + strconv.Itoa(index+1)
			}
			wantPaths = append(wantPaths, path)
		}
	}
	sort.Strings(wantPaths)
	if !reflect.DeepEqual(normalized.Redaction.WithheldFields, wantPaths) {
		t.Fatalf("want complete unsafe-field paths %v, got %v", wantPaths, normalized.Redaction.WithheldFields)
	}
	encoded, marshalErr := json.Marshal(normalized)
	if marshalErr != nil {
		t.Fatalf("marshal: %v", marshalErr)
	}
	for _, unsafe := range []string{"hunter2", "passw", "密碼", `\u0077`} {
		if strings.Contains(string(encoded), unsafe) {
			t.Fatalf("unsafe field name/value leaked: %s", encoded)
		}
	}
}

func TestOTLPKeyCollisionsWithholdEveryAmbiguousEntryAtEveryLevel(t *testing.T) {
	s := newScenario(t)
	build := func(reverse bool) *otlpgen.ExportRequest {
		pair := []*common.KeyValue{
			{Key: "a@example.com", Value: otlpgen.StringValue("first-secret")},
			{Key: "b@example.com", Value: otlpgen.StringValue("second-secret")},
		}
		if reverse {
			pair[0], pair[1] = pair[1], pair[0]
		}
		nested := &common.AnyValue{Value: &common.AnyValue_KvlistValue{KvlistValue: &common.KeyValueList{Values: append([]*common.KeyValue{
			{Key: "safe", Value: otlpgen.StringValue("retained")},
		}, pair...)}}}
		record := s.producer.Record(
			otlpgen.WithAttribute("safe", otlpgen.StringValue("retained")),
			otlpgen.WithAttribute("nested", nested),
		)
		record.Attributes = append(record.Attributes, pair...)
		request := s.producer.Request(record)
		request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes,
			&common.KeyValue{Key: "safe", Value: otlpgen.StringValue("retained")})
		request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes, pair...)
		request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = append(request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes,
			&common.KeyValue{Key: "safe", Value: otlpgen.StringValue("retained")})
		request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = append(request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes, pair...)
		return request
	}

	var first model.NormalizedLog
	for iteration, reverse := range []bool{false, true} {
		result, err := s.normalizer.Request(s.envelope, build(reverse))
		if err != nil || len(result.Rejected) != 0 || len(result.Records) != 1 {
			t.Fatalf("iteration %d: normalize collision fixture: result=%+v err=%v", iteration, result, err)
		}
		record := result.Records[0]
		wantPaths := []string{
			"attributes.[WITHHELD_FIELD_NAME]", "attributes.[WITHHELD_FIELD_NAME]#2",
			"attributes.nested.[WITHHELD_FIELD_NAME]", "attributes.nested.[WITHHELD_FIELD_NAME]#2",
			"resource_attributes.[WITHHELD_FIELD_NAME]", "resource_attributes.[WITHHELD_FIELD_NAME]#2",
			"scope_attributes.[WITHHELD_FIELD_NAME]", "scope_attributes.[WITHHELD_FIELD_NAME]#2",
		}
		if !reflect.DeepEqual(record.Redaction.WithheldFields, wantPaths) {
			t.Fatalf("iteration %d: want complete collision paths %v, got %v", iteration, wantPaths, record.Redaction.WithheldFields)
		}
		for _, values := range []map[string]model.SafeValue{record.Attributes, record.ResourceAttributes, record.ScopeAttributes, record.Attributes["nested"].Map} {
			if values["[WITHHELD_FIELD_NAME]"].Kind != model.SafeKindWithheld || values["[WITHHELD_FIELD_NAME]#2"].Kind != model.SafeKindWithheld {
				t.Fatalf("iteration %d: both colliding entries must be withheld: %+v", iteration, values)
			}
			if _, ambiguous := values["[REDACTED_EMAIL]"]; ambiguous {
				t.Fatalf("iteration %d: ambiguous normalized key retained: %+v", iteration, values)
			}
			if got := values["safe"]; got.Kind != model.SafeKindString || got.String != "retained" {
				t.Fatalf("iteration %d: noncolliding sibling was not retained: %+v", iteration, values)
			}
		}
		encoded, marshalErr := json.Marshal(record)
		if marshalErr != nil || strings.Contains(string(encoded), "first-secret") || strings.Contains(string(encoded), "second-secret") || strings.Contains(string(encoded), "@example.com") {
			t.Fatalf("iteration %d: collision leaked content: %s err=%v", iteration, encoded, marshalErr)
		}
		if iteration == 0 {
			first = record
			continue
		}
		if !reflect.DeepEqual(first.Attributes, record.Attributes) || !reflect.DeepEqual(first.ResourceAttributes, record.ResourceAttributes) ||
			!reflect.DeepEqual(first.ScopeAttributes, record.ScopeAttributes) || !reflect.DeepEqual(first.Redaction.WithheldFields, record.Redaction.WithheldFields) {
			t.Fatalf("permuting colliding keys changed safe output:\nfirst=%+v\nsecond=%+v", first, record)
		}
	}
}

func TestDuplicateAndLiteralMarkerKeyCollisionsWithholdAllMembers(t *testing.T) {
	s := newScenario(t)
	for _, test := range []struct {
		name string
		keys []string
	}{
		{name: "duplicate raw key", keys: []string{"duplicate", "duplicate"}},
		{name: "literal redaction marker", keys: []string{"a@example.com", "[REDACTED_EMAIL]"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := s.producer.Record()
			for index, key := range test.keys {
				record.Attributes = append(record.Attributes, &common.KeyValue{Key: key, Value: otlpgen.StringValue("secret-" + strconv.Itoa(index))})
			}
			normalized := s.admitOne(t, record)
			if normalized.Attributes["[WITHHELD_FIELD_NAME]"].Kind != model.SafeKindWithheld ||
				normalized.Attributes["[WITHHELD_FIELD_NAME]#2"].Kind != model.SafeKindWithheld {
				t.Fatalf("all colliding members must be withheld: %+v", normalized.Attributes)
			}
			for _, key := range []string{"duplicate", "[REDACTED_EMAIL]"} {
				if _, retained := normalized.Attributes[key]; retained {
					t.Fatalf("ambiguous key %q retained: %+v", key, normalized.Attributes)
				}
			}
		})
	}
}

func TestDuplicateRecordUIDAttributesAreDroppedBeforeCollisionHandling(t *testing.T) {
	s := newScenario(t)
	record := s.producer.Record()
	record.Attributes = append(record.Attributes,
		&common.KeyValue{Key: otlpgen.RecordUIDAttribute, Value: otlpgen.StringValue("0194f0a0-0000-7000-8000-000000000099")})
	normalized := s.admitOne(t, record)
	if _, retained := normalized.Attributes[otlpgen.RecordUIDAttribute]; retained {
		t.Fatalf("record UID retained after identity derivation: %+v", normalized.Attributes)
	}
	if _, placeholder := normalized.Attributes["[WITHHELD_FIELD_NAME]"]; placeholder {
		t.Fatalf("duplicate identity-only attributes became evidence placeholders: %+v", normalized.Attributes)
	}
}

func TestExactWithheldMarkerBecomesTypedAtNormalizationBoundary(t *testing.T) {
	s := newScenario(t)
	record := s.admitOne(t, s.producer.Record(
		otlpgen.WithBody(redact.WithheldText),
		otlpgen.WithAttribute("already_withheld", otlpgen.StringValue(redact.WithheldText)),
	))
	if record.Body.Kind != model.SafeKindWithheld || record.Attributes["already_withheld"].Kind != model.SafeKindWithheld {
		t.Fatalf("exact withheld markers must retain typed provenance: body=%+v attribute=%+v", record.Body, record.Attributes["already_withheld"])
	}
	want := []string{"attributes.already_withheld", "body"}
	if got := record.Redaction.WithheldFields; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want complete withheld paths %v, got %v", want, got)
	}
	if !containsRule(record.Redaction.RuleIDs, "safety.preexisting_marker") {
		t.Fatalf("preexisting marker provenance is missing: %+v", record.Redaction)
	}
}

func containsRule(rules []string, want string) bool {
	for _, rule := range rules {
		if rule == want {
			return true
		}
	}
	return false
}
