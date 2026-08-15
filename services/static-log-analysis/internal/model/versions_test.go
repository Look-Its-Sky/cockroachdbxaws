package model_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

func TestParseSchemaVersion(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		want  model.SchemaVersion
		valid bool
	}{
		{name: "previous minor", text: "1.0", want: model.SchemaVersion{Major: 1, Minor: 0}, valid: true},
		{name: "current", text: "1.1", want: model.SchemaVersion{Major: 1, Minor: 1}, valid: true},
		{name: "later minor", text: "1.7", want: model.SchemaVersion{Major: 1, Minor: 7}, valid: true},
		{name: "two digit minor", text: "1.10", want: model.SchemaVersion{Major: 1, Minor: 10}, valid: true},
		{name: "later major", text: "2.0", want: model.SchemaVersion{Major: 2, Minor: 0}, valid: true},

		{name: "major only", text: "1"},
		{name: "patch version", text: "1.0.0"},
		{name: "v prefix", text: "v1.0"},
		{name: "leading zero", text: "01.0"},
		{name: "non numeric minor", text: "1.x"},
		{name: "empty minor", text: "1."},
		{name: "empty major", text: ".0"},
		{name: "surrounding space", text: " 1.0"},
		{name: "negative", text: "-1.0"},
		{name: "empty", text: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := model.ParseSchemaVersion(test.text)
			if !test.valid {
				if err == nil {
					// Guessing what a non-conforming version meant would defeat
					// the point of carrying a version at all.
					t.Fatalf("want %q rejected, got %v", test.text, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("want %q accepted, got %v", test.text, err)
			}
			if got != test.want {
				t.Fatalf("want %+v, got %+v", test.want, got)
			}
		})
	}
}

func TestCompatibilityIsDecidedByMajorVersion(t *testing.T) {
	reader := model.SchemaVersion{Major: 1, Minor: 0}

	tests := []struct {
		writer model.SchemaVersion
		want   bool
	}{
		{model.SchemaVersion{Major: 1, Minor: 0}, true},
		// A newer writer may add fields, which a reader ignores.
		{model.SchemaVersion{Major: 1, Minor: 7}, true},
		// A different major means a field changed meaning or type.
		{model.SchemaVersion{Major: 2, Minor: 0}, false},
		{model.SchemaVersion{Major: 0, Minor: 9}, false},
	}

	for _, test := range tests {
		if got := reader.CompatibleWith(test.writer); got != test.want {
			t.Errorf("reader %s reading %s: want %v, got %v", reader, test.writer, test.want, got)
		}
	}
}

func TestTheDeclaredSchemaVersionMatchesItsParts(t *testing.T) {
	parsed, err := model.ParseSchemaVersion(model.NormalizedLogSchemaVersion)
	if err != nil {
		t.Fatalf("the build's own schema version does not parse: %v", err)
	}
	if parsed.Major != model.NormalizedLogSchemaMajor || parsed.Minor != model.NormalizedLogSchemaMinor {
		t.Fatalf("want %d.%d, got %s", model.NormalizedLogSchemaMajor, model.NormalizedLogSchemaMinor, parsed)
	}
}

func TestRecordAcceptsCompatibleSchemaVersionsAndRejectsOthers(t *testing.T) {
	tests := []struct {
		version  string
		accepted bool
		because  string
	}{
		{version: "1.0", accepted: true, because: "an older minor remains readable"},
		{version: "1.1", accepted: true, because: "it is the version this build writes"},
		{version: "1.7", accepted: true,
			because: "a newer minor only adds fields, which a reader ignores"},
		{version: "2.0", accepted: false,
			because: "a new major may change the meaning or type of an existing field"},
		{version: "0.9", accepted: false, because: "an older major is a different contract"},
		{version: "1", accepted: false, because: "the version format is not major.minor"},
		{version: "v1.0", accepted: false, because: "the version format is not canonical"},
		{version: "1.0.0", accepted: false, because: "the version format carries an extra part"},
	}

	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			record := validRecord()
			record.SchemaVersion = test.version

			err := record.Validate()

			if test.accepted {
				if err != nil {
					t.Fatalf("want %s accepted because %s, got %v", test.version, test.because, err)
				}
				return
			}
			var validation *model.ValidationError
			if !errors.As(err, &validation) || !validation.Has("schema_version") {
				t.Fatalf("want %s rejected because %s, got %v", test.version, test.because, err)
			}
		})
	}
}

func TestRecordAcceptsEveryKnownIdentityVersion(t *testing.T) {
	known := model.KnownRecordIDVersions()
	if len(known) == 0 {
		t.Fatal("want at least one known identity version")
	}

	for _, version := range known {
		t.Run(version, func(t *testing.T) {
			record := validRecord()
			record.RecordIDVersion = version
			if version == model.RecordIDVersionDerivedV1 {
				record.IdentityQuality = model.IdentityQualityDerived
			}

			if err := record.Validate(); err != nil {
				t.Fatalf("want the known version %s accepted, got %v", version, err)
			}
		})
	}
}

func TestAnUnknownIdentityVersionNamesTheOnesThatAreKnown(t *testing.T) {
	record := validRecord()
	record.RecordIDVersion = "sha1:v1"

	err := record.Validate()

	var validation *model.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("want a validation error, got %v", err)
	}
	// Naming the alternatives is what turns a rejected record into a fixable
	// producer configuration.
	if !strings.Contains(err.Error(), model.RecordIDVersionOTLPV1) {
		t.Fatalf("want the known versions listed, got %v", err)
	}
}
