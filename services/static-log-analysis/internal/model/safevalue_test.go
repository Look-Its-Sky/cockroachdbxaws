package model_test

import (
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

func TestSafeValueConstructorsProduceValidValues(t *testing.T) {
	values := map[string]model.SafeValue{
		"string":   model.SafeString("declined"),
		"int":      model.SafeInt(42),
		"double":   model.SafeDouble(1.5),
		"bool":     model.SafeBool(true),
		"map":      model.SafeMap(map[string]model.SafeValue{"code": model.SafeString("card_declined")}),
		"slice":    model.SafeSlice(model.SafeString("a"), model.SafeInt(1)),
		"withheld": model.Withheld("unparseable body"),
		// The zero value stands for "no value", which is valid: a record may
		// carry an event name instead of a body.
		"empty": {},
	}
	for name, value := range values {
		if err := value.Validate(); err != nil {
			t.Errorf("%s value rejected: %v", name, err)
		}
	}
}

func TestSafeValueRejectsAPayloadThatDisagreesWithItsKind(t *testing.T) {
	// Building a SafeValue by literal is how a payload and a kind come to
	// disagree, and the consumer then silently reads a zero value.
	mismatched := model.SafeValue{Kind: model.SafeKindString, Int: 7}

	err := mismatched.Validate()
	if err == nil {
		t.Fatal("want a violation for a string value carrying an int payload, got none")
	}
}

func TestSafeValueRejectsWithheldWithoutAReason(t *testing.T) {
	if err := (model.SafeValue{Kind: model.SafeKindWithheld}).Validate(); err == nil {
		t.Fatal("want a violation for withheld content with no reason, got none")
	}
}

func TestSafeValueRejectsAPayloadWithNoKind(t *testing.T) {
	if err := (model.SafeValue{String: "orphan"}).Validate(); err == nil {
		t.Fatal("want a violation for a payload with no kind, got none")
	}
}

func TestSafeValueValidatesNestedValues(t *testing.T) {
	nested := model.SafeMap(map[string]model.SafeValue{
		"outer": model.SafeSlice(
			model.SafeMap(map[string]model.SafeValue{
				"inner": {Kind: model.SafeKindWithheld},
			}),
		),
	})

	err := nested.Validate()
	if err == nil {
		t.Fatal("want the nested violation to surface, got none")
	}
	var validation *model.ValidationError
	if !asValidation(err, &validation) {
		t.Fatalf("want *model.ValidationError, got %T", err)
	}
	if !validation.Has("map[outer].slice[0].map[inner].withheld") {
		t.Fatalf("want the violation to name its full path, got %v", validation.Fields())
	}
}

func TestSafeValueDepth(t *testing.T) {
	tests := []struct {
		name  string
		value model.SafeValue
		want  int
	}{
		{"scalar", model.SafeString("x"), 1},
		{"empty map", model.SafeMap(nil), 1},
		{"map of scalars", model.SafeMap(map[string]model.SafeValue{"a": model.SafeInt(1)}), 2},
		{
			name: "deepest branch wins",
			value: model.SafeMap(map[string]model.SafeValue{
				"shallow": model.SafeString("x"),
				"deep":    model.SafeSlice(model.SafeMap(map[string]model.SafeValue{"a": model.SafeInt(1)})),
			}),
			want: 4,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.value.Depth(); got != test.want {
				t.Fatalf("want depth %d, got %d", test.want, got)
			}
		})
	}
}

func asValidation(err error, target **model.ValidationError) bool {
	validation, ok := err.(*model.ValidationError)
	if ok {
		*target = validation
	}
	return ok
}
