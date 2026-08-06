package model_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

func TestSafeValueRoundTripsThroughJSON(t *testing.T) {
	values := map[string]model.SafeValue{
		"string":   model.SafeString("charge declined"),
		"int":      model.SafeInt(-42),
		"double":   model.SafeDouble(1.5),
		"bool":     model.SafeBool(true),
		"withheld": model.Withheld("unparseable body"),
		"empty":    {},
		"map": model.SafeMap(map[string]model.SafeValue{
			"code":   model.SafeString("card_declined"),
			"nested": model.SafeMap(map[string]model.SafeValue{"retryable": model.SafeBool(false)}),
		}),
		"slice": model.SafeSlice(model.SafeString("a"), model.SafeInt(1), model.Withheld("token")),
	}

	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			var decoded model.SafeValue
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("decoding %s: %v", encoded, err)
			}
			if !reflect.DeepEqual(value, decoded) {
				t.Fatalf("round trip changed the value:\n want %+v\n got  %+v\n via  %s", value, decoded, encoded)
			}
		})
	}
}

func TestSafeValueEncodesOnlyThePayloadItsKindUses(t *testing.T) {
	encoded, err := json.Marshal(model.SafeString("x"))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	// A shape that carried every payload field would make every fixture eight
	// times longer and hide the field that actually changed.
	if got, want := string(encoded), `{"kind":"string","value":"x"}`; got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestWithheldContentCarriesNoPayload(t *testing.T) {
	encoded, err := json.Marshal(model.Withheld("jwt in body"))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	// Content removed for safety must not be reintroduced by a serializer.
	if strings.Contains(string(encoded), `"value"`) {
		t.Fatalf("want no payload alongside a withheld reason, got %s", encoded)
	}
	if !strings.Contains(string(encoded), `"reason":"jwt in body"`) {
		t.Fatalf("want the reason preserved, got %s", encoded)
	}
}

func TestDecodingRejectsAnUnknownValueKind(t *testing.T) {
	var decoded model.SafeValue

	err := json.Unmarshal([]byte(`{"kind":"pointer","value":"0xdeadbeef"}`), &decoded)

	if err == nil {
		t.Fatal("want an unknown value kind rejected, got no error")
	}
	if !strings.Contains(err.Error(), "pointer") {
		t.Fatalf("want the error to name the unknown kind, got %v", err)
	}
}

func TestDecodingRejectsAMalformedUnion(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		because string
	}{
		{
			name:    "string with no payload",
			encoded: `{"kind":"string"}`,
			because: "an empty string and a missing payload would be indistinguishable",
		},
		{
			name:    "int with no payload",
			encoded: `{"kind":"int"}`,
			because: "a zero and a missing payload would be indistinguishable",
		},
		{
			name:    "bool with no payload",
			encoded: `{"kind":"bool"}`,
			because: "false and a missing payload would be indistinguishable",
		},
		{
			name:    "map with no payload",
			encoded: `{"kind":"map"}`,
			because: "a nil map and a missing payload would be indistinguishable",
		},
		{
			name:    "slice with no payload",
			encoded: `{"kind":"slice"}`,
			because: "a nil slice and a missing payload would be indistinguishable",
		},
		{
			name:    "explicitly null payload",
			encoded: `{"kind":"string","value":null}`,
			because: "a null payload is a missing payload written out",
		},
		{
			name:    "no kind at all",
			encoded: `{"value":"orphan"}`,
			because: "a payload with no kind cannot be read by anything",
		},
		{
			name:    "withheld with no reason",
			encoded: `{"kind":"withheld"}`,
			because: "removed content must say why it was removed",
		},
		{
			name:    "withheld with a whitespace reason",
			encoded: `{"kind":"withheld","reason":"  "}`,
			because: "a blank reason is no reason",
		},
		{
			name:    "withheld carrying a value",
			encoded: `{"kind":"withheld","reason":"jwt","value":"eyJhbGciOi"}`,
			because: "content removed for safety must not be reintroduced by a decoder",
		},
		{
			name:    "reason on a value that was not withheld",
			encoded: `{"kind":"string","value":"x","reason":"jwt"}`,
			because: "a reason on a present value means the two fields disagree",
		},
		{
			name:    "payload of the wrong type for its kind",
			encoded: `{"kind":"int","value":"seven"}`,
			because: "the payload must match the kind that describes it",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var decoded model.SafeValue

			err := json.Unmarshal([]byte(test.encoded), &decoded)

			if err == nil {
				t.Fatalf("want %s rejected because %s, got %+v", test.encoded, test.because, decoded)
			}
		})
	}
}

func TestDecodingAcceptsAnExplicitlyEncodedZeroValue(t *testing.T) {
	// Rejecting a missing payload must not also reject a payload that is
	// genuinely a zero: an attribute really can be an empty string or a false.
	tests := map[string]model.SafeValue{
		`{"kind":"string","value":""}`:  model.SafeString(""),
		`{"kind":"int","value":0}`:      model.SafeInt(0),
		`{"kind":"double","value":0}`:   model.SafeDouble(0),
		`{"kind":"bool","value":false}`: model.SafeBool(false),
		`{"kind":"map","value":{}}`:     model.SafeMap(map[string]model.SafeValue{}),
		// Spelled out rather than written as SafeSlice(), because a variadic
		// call with no arguments produces a nil slice, and this case is about
		// a container that is present and empty.
		`{"kind":"slice","value":[]}`: {Kind: model.SafeKindSlice, Slice: []model.SafeValue{}},
	}

	for encoded, want := range tests {
		t.Run(encoded, func(t *testing.T) {
			var decoded model.SafeValue
			if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
				t.Fatalf("want %s accepted, got %v", encoded, err)
			}
			if !reflect.DeepEqual(want, decoded) {
				t.Fatalf("want %+v, got %+v", want, decoded)
			}
		})
	}
}

func TestAnAbsentContainerEncodesAsAnEmptyOne(t *testing.T) {
	// A nil map and an empty map mean the same thing to every consumer, so they
	// are written the same way, and a decoder never has to treat JSON null as a
	// container.
	encoded, err := json.Marshal(model.SafeValue{Kind: model.SafeKindMap})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if got, want := string(encoded), `{"kind":"map","value":{}}`; got != want {
		t.Fatalf("want %s, got %s", want, got)
	}

	var decoded model.SafeValue
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("want the encoded form to decode, got %v", err)
	}
	if decoded.Map == nil {
		t.Fatal("want an empty map rather than a nil one")
	}

	encoded, err = json.Marshal(model.SafeValue{Kind: model.SafeKindSlice})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if got, want := string(encoded), `{"kind":"slice","value":[]}`; got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestNormalizedLogRoundTripsThroughJSON(t *testing.T) {
	record := validRecord()
	record.Exception = &model.NormalizedException{
		Type:        "PaymentDeclined",
		SafeMessage: "charge declined for order <id>",
		StackFrames: []model.StackFrame{
			{Function: "charge", Module: "payment/handler", InApplication: true},
			{Function: "roundTrip", Module: "net/http"},
		},
	}
	record.Correlation = model.CorrelationIdentity{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	var decoded model.NormalizedLog
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if !decoded.EventTime.Equal(record.EventTime) || !decoded.ObservedTime.Equal(record.ObservedTime) {
		t.Fatalf("timestamps changed: %s/%s became %s/%s",
			record.EventTime, record.ObservedTime, decoded.EventTime, decoded.ObservedTime)
	}
	// Times compare equal above but carry different internal representations,
	// so they are normalized before the structural comparison.
	decoded.EventTime = record.EventTime
	decoded.ObservedTime = record.ObservedTime
	decoded.Source.ReceivedAt = record.Source.ReceivedAt
	if !reflect.DeepEqual(record, decoded) {
		t.Fatalf("round trip changed the record:\n want %+v\n got  %+v", record, decoded)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("a decoded record must still be valid: %v", err)
	}
}

func TestDecodingToleratesUnknownFieldsWithinAMajorVersion(t *testing.T) {
	// Readers accept unknown fields within the same major version, so a writer
	// that adds a field does not break an older reader mid-rollout.
	encoded := []byte(`{
	  "schema_version": "1.0",
	  "record_id": "abc",
	  "severity_class": "error",
	  "field_added_by_a_newer_writer": {"anything": true}
	}`)

	var decoded model.NormalizedLog
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("want the unknown field ignored, got %v", err)
	}
	if decoded.RecordID != "abc" {
		t.Fatalf("want the known fields decoded, got %+v", decoded)
	}
}
