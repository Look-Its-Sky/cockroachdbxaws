package agent_test

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestAssignmentGoldenStrictDecodeAndRoundTrip(t *testing.T) {
	valid, err := os.ReadFile("testdata/assignment.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agent.DecodeAssignment(valid)
	if err != nil {
		t.Fatalf("valid golden: %v", err)
	}
	encoded, err := agent.EncodeAssignment(decoded)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := agent.DecodeAssignment(encoded)
	if err != nil || !reflect.DeepEqual(decoded, roundTrip) {
		t.Fatalf("round trip: decoded=%+v roundTrip=%+v err=%v", decoded, roundTrip, err)
	}
	wantEncoded := `{"schema_version":"1.0","message_id":"0194f0a0-0000-7000-8000-000000000001","message_type":"agent.assignment.v1","created_at":"2026-08-06T18:04:51Z","region":"us-east-1","tenant_id":"tenant-a","classification":"SENSITIVE","producer":"static-log-analysis","correlation_id":"0194f0a0-0000-7000-8000-000000000002","incident_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","incident_generation":42,"investigation_id":"0194f0a0-0000-7000-8000-000000000003","service_id":"paymentservice","environment":"production","severity":"error","context_version":1}`
	if string(encoded) != wantEncoded {
		t.Fatalf("assignment encoding changed:\nwant %s\n got %s", wantEncoded, encoded)
	}
	invalid, err := os.ReadFile("testdata/assignment.invalid.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.DecodeAssignment(invalid); !errors.Is(err, agent.ErrInvalidAssignment) {
		t.Fatalf("invalid golden accepted: %v", err)
	}
}

func TestCheckedInAssignmentJSONSchemaCompilesAndMatchesGoldenFixtures(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile("../../api/schema/agent/v1/assignment.schema.json")
	if err != nil {
		t.Fatalf("compile checked-in assignment schema: %v", err)
	}
	tests := []struct {
		name  string
		path  string
		valid bool
	}{
		{name: "valid", path: "testdata/assignment.valid.json", valid: true},
		{name: "unknown", path: "testdata/assignment.invalid.json"},
		{name: "missing", path: "testdata/assignment.invalid-missing.json"},
		{name: "type", path: "testdata/assignment.invalid-type.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.Open(test.path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			instance, err := jsonschema.UnmarshalJSON(file)
			if err != nil {
				t.Fatal(err)
			}
			err = schema.Validate(instance)
			if test.valid && err != nil {
				t.Fatalf("valid fixture rejected by JSON Schema: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid fixture accepted by JSON Schema")
			}
		})
	}
	compatible, err := os.ReadFile("testdata/assignment.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	compatible = bytes.Replace(compatible, []byte(`"1.0"`), []byte(`"1.7"`), 1)
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(compatible))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("checked-in schema rejected compatible 1.x minor: %v", err)
	}
}

func TestAssignmentClosedSchemaRequiredTypesVersionsAndSize(t *testing.T) {
	valid, err := os.ReadFile("testdata/assignment.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		payload []byte
		valid   bool
	}{
		{name: "current", payload: valid, valid: true},
		{name: "compatible minor", payload: bytes.Replace(valid, []byte(`"1.0"`), []byte(`"1.7"`), 1), valid: true},
		{name: "other major", payload: bytes.Replace(valid, []byte(`"1.0"`), []byte(`"2.0"`), 1)},
		{name: "missing field", payload: bytes.Replace(valid, []byte("  \"tenant_id\": \"tenant-a\",\n"), nil, 1)},
		{name: "wrong type", payload: bytes.Replace(valid, []byte(`"context_version": 1`), []byte(`"context_version": "1"`), 1)},
		{name: "unknown field", payload: bytes.Replace(valid, []byte("\n}"), []byte(",\n  \"future\": true\n}"), 1)},
		{name: "non UTC", payload: bytes.Replace(valid, []byte(`2026-08-06T18:04:51Z`), []byte(`2026-08-06T13:04:51-05:00`), 1)},
		{name: "oversize", payload: append(append([]byte(nil), valid...), bytes.Repeat([]byte{' '}, agent.MaxAssignmentBytes)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := agent.DecodeAssignment(test.payload)
			if test.valid && err != nil {
				t.Fatalf("compatible payload rejected: %v", err)
			}
			if !test.valid && !errors.Is(err, agent.ErrInvalidAssignment) {
				t.Fatalf("invalid payload accepted: %v", err)
			}
		})
	}
}

func TestAssignmentBounds(t *testing.T) {
	valid, err := os.ReadFile("testdata/assignment.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := agent.DecodeAssignment(valid)
	if err != nil {
		t.Fatal(err)
	}
	value.ServiceID = strings.Repeat("s", agent.MaxAssignmentTextBytes)
	if _, err := agent.EncodeAssignment(value); err != nil {
		t.Fatalf("exact bound rejected: %v", err)
	}
	value.ServiceID += "s"
	if _, err := agent.EncodeAssignment(value); !errors.Is(err, agent.ErrInvalidAssignment) {
		t.Fatalf("over-bound service accepted: %v", err)
	}
}
