package admission_test

import (
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

func TestUncertainClassificationIsProtected(t *testing.T) {
	classifier, err := admission.Compile([]admission.Rule{{
		Services:   []string{"paymentservice"},
		Severities: []model.SeverityClass{model.SeverityClassError},
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	got := classifier.Classify(admission.Record{Severity: model.SeverityClassError})
	if !got.Protected || got.Certain {
		t.Fatalf("missing service is uncertain and must default protected, got %+v", got)
	}
}

func TestClassifierOnlyAnswersWhetherARuleCouldConsumeTheRecord(t *testing.T) {
	classifier, err := admission.Compile([]admission.Rule{{
		Services:           []string{"paymentservice"},
		Environments:       []string{"production"},
		Severities:         []model.SeverityClass{model.SeverityClassError},
		RequiredAttributes: []string{"exception.type"},
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	protected := classifier.Classify(admission.Record{
		Service: "paymentservice", Environment: "production", Severity: model.SeverityClassError,
		Attributes: map[string]struct{}{"exception.type": {}},
	})
	if !protected.Protected || !protected.Certain {
		t.Fatalf("matching record must be certainly protected, got %+v", protected)
	}

	unprotected := classifier.Classify(admission.Record{
		Service: "paymentservice", Environment: "production", Severity: model.SeverityClassInfo,
		Attributes: map[string]struct{}{"exception.type": {}},
	})
	if unprotected.Protected || !unprotected.Certain {
		t.Fatalf("known mismatch must be explicitly unprotected, got %+v", unprotected)
	}
}

func TestFailedRuleCompilationCannotCreateAnOpenClassifier(t *testing.T) {
	if _, err := admission.Compile([]admission.Rule{{Severities: []model.SeverityClass{"critical"}}}); err == nil {
		t.Fatal("unknown severity must fail compilation")
	}

	got := (*admission.Classifier)(nil).Classify(admission.Record{})
	if !got.Protected || got.Certain {
		t.Fatalf("unavailable classifier must default protected and uncertain, got %+v", got)
	}
}

func TestUnknownRecordEnumsAreUncertainAndProtected(t *testing.T) {
	classifier, err := admission.Compile(nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, record := range []admission.Record{
		{Source: "forged", Severity: model.SeverityClassInfo},
		{Source: model.SourceTypeOTLP, Severity: "critical"},
	} {
		got := classifier.Classify(record)
		if !got.Protected || got.Certain {
			t.Fatalf("unknown record enum must fail closed, record=%+v classification=%+v", record, got)
		}
	}
}
