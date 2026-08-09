package model_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

func TestPublicModelValidationErrorsNeverEchoRejectedContent(t *testing.T) {
	fragments := []string{"password=hunter2", "alice@example.com", "bad/timezone", "wrong-region", "future-kind"}

	envelope := validRecord().Source
	envelope.SourceType = model.SourceType("password=hunter2")
	envelope.ReceivedAt = envelope.ReceivedAt.In(time.FixedZone("bad/timezone", 3600))
	assertOpaqueModelError(t, envelope.Validate(), fragments...)

	record := validRecord()
	record.SchemaVersion = "password=hunter2"
	record.RecordID = "alice@example.com"
	record.RecordIDVersion = "future-kind"
	record.IdentityQuality = model.IdentityQuality("password=hunter2")
	record.Region = "wrong-region"
	record.Source.Region = "password=hunter2"
	record.SeverityClass = model.SeverityClass("future-kind")
	record.Service.Status = model.EnrichmentStatus("password=hunter2")
	record.Deployment.Status = model.EnrichmentStatus("future-kind")
	record.Attributes = map[string]model.SafeValue{
		"alice@example.com": {Kind: model.SafeValueKind("future-kind"), String: "password=hunter2"},
	}
	err := record.Validate()
	assertOpaqueModelError(t, err, fragments...)
	var validation *model.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("want ValidationError, got %T", err)
	}
	for _, field := range validation.Fields() {
		assertNoModelFragments(t, field, fragments...)
	}

	value := model.SafeMap(map[string]model.SafeValue{
		"alice@example.com": {Kind: model.SafeValueKind("future-kind"), String: "password=hunter2"},
	})
	assertOpaqueModelError(t, value.Validate(), fragments...)
}

func TestSafeValueJSONErrorsAreCategoricalAtEveryNestingLevel(t *testing.T) {
	fragments := []string{"password=hunter2", "alice@example.com", "future-kind"}
	for _, encoded := range []string{
		`{"kind":"future-kind","value":"password=hunter2"}`,
		`{"kind":"map","value":{"alice@example.com":{"kind":"future-kind","value":"password=hunter2"}}}`,
		`{"kind":"int","value":"password=hunter2"}`,
		`{"kind":"string","value":"safe","reason":"password=hunter2"}`,
		`{"kind":"withheld","reason":"safe","value":"password=hunter2"}`,
	} {
		var decoded model.SafeValue
		err := json.Unmarshal([]byte(encoded), &decoded)
		if !errors.Is(err, model.ErrInvalidSafeValueJSON) {
			t.Fatalf("want categorical SafeValue JSON error for %q, got %v", encoded, err)
		}
		assertOpaqueModelError(t, err, fragments...)
	}

	_, err := json.Marshal(model.SafeValue{Kind: model.SafeKindInt, Int: 1, String: "password=hunter2"})
	if !errors.Is(err, model.ErrInvalidSafeValueJSON) {
		t.Fatalf("mismatched SafeValue marshal must fail categorically, got %v", err)
	}
	assertOpaqueModelError(t, err, fragments...)
}

func TestParseSchemaVersionErrorsAreOpaque(t *testing.T) {
	for _, unsafe := range []string{"password=hunter2", "alice@example.com.1", "01.password=hunter2"} {
		_, err := model.ParseSchemaVersion(unsafe)
		if !errors.Is(err, model.ErrInvalidSchemaVersion) {
			t.Fatalf("want categorical schema-version error, got %v", err)
		}
		assertOpaqueModelError(t, err, unsafe, "password=hunter2", "alice@example.com")
	}
}

func assertOpaqueModelError(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want rejected content to produce an error")
	}
	assertNoModelFragments(t, err.Error(), fragments...)
}

func assertNoModelFragments(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if fragment != "" && strings.Contains(text, fragment) {
			t.Fatalf("error or field path echoed rejected fragment %q: %s", fragment, text)
		}
	}
}
