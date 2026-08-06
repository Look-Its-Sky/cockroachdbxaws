package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Persisted structures carry a major and minor schema version. A reader accepts
// any minor version within the major it understands, because a writer may add
// fields during a rollout. A different major version means a field changed
// meaning or type, so the reader must refuse it rather than interpret it under
// the old rules.

// NormalizedLogSchemaMajor is the major version this build reads and writes.
const NormalizedLogSchemaMajor = 1

// NormalizedLogSchemaMinor is the minor version this build writes.
const NormalizedLogSchemaMinor = 0

// SchemaVersion is a parsed major.minor version.
type SchemaVersion struct {
	Major int
	Minor int
}

func (v SchemaVersion) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// CompatibleWith reports whether a reader at this version can read data written
// at other: the majors must match, and a newer minor is readable because
// unknown fields are ignored.
func (v SchemaVersion) CompatibleWith(other SchemaVersion) bool { return v.Major == other.Major }

// ParseSchemaVersion reads canonical major.minor text.
//
// The format is deliberately narrow. A version that arrives as "1", "v1.0", or
// "1.0.0" is a producer that is not following the contract, and guessing what
// it meant would defeat the point of carrying a version at all.
func ParseSchemaVersion(text string) (SchemaVersion, error) {
	major, minor, found := strings.Cut(text, ".")
	if !found {
		return SchemaVersion{}, fmt.Errorf("schema version %q is not major.minor", text)
	}
	majorValue, err := parseVersionPart(major)
	if err != nil {
		return SchemaVersion{}, fmt.Errorf("schema version %q has an invalid major: %w", text, err)
	}
	minorValue, err := parseVersionPart(minor)
	if err != nil {
		return SchemaVersion{}, fmt.Errorf("schema version %q has an invalid minor: %w", text, err)
	}
	return SchemaVersion{Major: majorValue, Minor: minorValue}, nil
}

func parseVersionPart(text string) (int, error) {
	if text == "" {
		return 0, fmt.Errorf("part is empty")
	}
	if len(text) > 1 && text[0] == '0' {
		return 0, fmt.Errorf("part %q has a leading zero", text)
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("part %q is not a decimal number", text)
		}
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("part %q is not a decimal number", text)
	}
	return value, nil
}

// Record identity versions are immutable. A later version never rewrites
// history, so each one names both the algorithm that produced an identifier and
// the shape that identifier has.
const (
	// RecordIDVersionOTLPV1 is SHA-256 over the trusted source instance and the
	// producer-assigned log record UID.
	RecordIDVersionOTLPV1 = "otlp:v1"
	// RecordIDVersionCloudWatchV1 is SHA-256 over account, region, log group,
	// log stream, and native event ID.
	RecordIDVersionCloudWatchV1 = "cw:v1"
	// RecordIDVersionDerivedV1 is SHA-256 over the length-delimited canonical
	// encoding used when no trusted UID is available. Its use is measured and
	// alerted, and it is never the normal OTLP path.
	RecordIDVersionDerivedV1 = "derived:v1"
)

// recordIDSpec describes what an identity version produces.
type recordIDSpec struct {
	// quality is the identity quality a record using this version must declare.
	quality IdentityQuality
	// hexDigits is the length of the hex-encoded digest. Every current version
	// is SHA-256, so every identifier is 64 lowercase hex characters.
	hexDigits int
}

var recordIDVersions = map[string]recordIDSpec{
	RecordIDVersionOTLPV1:       {quality: IdentityQualityNative, hexDigits: 64},
	RecordIDVersionCloudWatchV1: {quality: IdentityQualityNative, hexDigits: 64},
	RecordIDVersionDerivedV1:    {quality: IdentityQualityDerived, hexDigits: 64},
}

// KnownRecordIDVersions returns the identity versions this build understands,
// sorted, for diagnostics and error messages.
func KnownRecordIDVersions() []string {
	versions := make([]string, 0, len(recordIDVersions))
	for version := range recordIDVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	return versions
}
