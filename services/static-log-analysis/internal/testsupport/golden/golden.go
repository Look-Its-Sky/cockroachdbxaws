// Package golden compares values against committed fixtures.
//
// Fixtures live under the calling package's testdata directory, which go test
// makes the working directory. Committing them turns a change in normalization,
// redaction, fingerprinting, or serialization into a reviewable diff rather
// than a silently updated expectation.
//
// Fixtures are rewritten by running tests with UPDATE_GOLDEN=1. Continuous
// integration never sets it, and checks that the tree is clean afterwards, so a
// rewritten fixture cannot reach a merge unreviewed.
package golden

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

// Dir is the directory, relative to the calling package, that fixtures live in.
const Dir = "testdata"

// updateEnv is the variable that switches comparison into rewriting.
const updateEnv = "UPDATE_GOLDEN"

// Updating reports whether fixtures are being rewritten rather than compared.
func Updating() bool { return os.Getenv(updateEnv) == "1" }

// Path returns the path of a fixture relative to the calling package.
func Path(name string) string { return filepath.Join(Dir, filepath.FromSlash(name)) }

// Load returns the contents of a fixture used as test input.
//
// Unlike the assertions below it never rewrites: an input fixture that is
// missing is a broken test, not an expectation that needs updating.
func Load(t tb.TB, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(Path(name))
	if err != nil {
		t.Fatalf("golden: reading input fixture %s: %v", Path(name), err)
	}
	return content
}

// Bytes compares actual against the fixture named by name.
func Bytes(t tb.TB, name string, actual []byte) {
	t.Helper()
	path := Path(name)

	if Updating() {
		write(t, path, actual)
		t.Logf("golden: wrote %s (%s=1)", path, updateEnv)
		return
	}

	expected, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("golden: fixture %s does not exist; review the value and create it with %s=1 go test",
				path, updateEnv)
		}
		t.Fatalf("golden: reading fixture %s: %v", path, err)
	}

	if !bytes.Equal(expected, actual) {
		t.Fatalf("golden: %s does not match:\n%s\nIf the new value is correct, review it and rerun with %s=1.",
			path, diff(expected, actual), updateEnv)
	}
}

// String compares a string against a fixture.
func String(t tb.TB, name, actual string) {
	t.Helper()
	Bytes(t, name, []byte(ensureTrailingNewline(actual)))
}

// JSON compares the canonical JSON encoding of value against a fixture.
//
// Encoding is indented with sorted object keys, so a fixture stays readable in
// review and does not change when an unrelated map is iterated in a different
// order.
func JSON(t tb.TB, name string, value any) {
	t.Helper()
	Bytes(t, name, Encode(t, value))
}

// Encode returns the canonical JSON encoding used for fixtures. Tests that need
// to compare two values without a fixture use it directly.
func Encode(t tb.TB, value any) []byte {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	// Log-derived content is compared byte for byte, so escaping HTML would
	// change a fixture without any change in the value it describes.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatalf("golden: encoding %T: %v", value, err)
	}
	return buffer.Bytes()
}

func write(t tb.TB, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("golden: creating fixture directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("golden: writing fixture %s: %v", path, err)
	}
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// diff renders a line-oriented comparison. Fixtures are text, and the first
// differing line is almost always the one worth reading.
func diff(expected, actual []byte) string {
	if !isText(expected) || !isText(actual) {
		return fmt.Sprintf("  fixture is %d bytes, value is %d bytes (binary fixture, not shown)",
			len(expected), len(actual))
	}

	// Fixtures end with a newline, which would otherwise split into an empty
	// final line and shift every reported line number past the real change.
	expectedLines := strings.Split(strings.TrimSuffix(string(expected), "\n"), "\n")
	actualLines := strings.Split(strings.TrimSuffix(string(actual), "\n"), "\n")

	var out strings.Builder
	for i := 0; i < len(expectedLines) || i < len(actualLines); i++ {
		want, hasWant := at(expectedLines, i)
		got, hasGot := at(actualLines, i)
		switch {
		case hasWant && hasGot && want == got:
			continue
		case hasWant && hasGot:
			fmt.Fprintf(&out, "  line %d:\n    fixture: %s\n    value:   %s\n", i+1, want, got)
		case hasWant:
			fmt.Fprintf(&out, "  line %d only in fixture: %s\n", i+1, want)
		default:
			fmt.Fprintf(&out, "  line %d only in value:   %s\n", i+1, got)
		}
	}
	if out.Len() == 0 {
		return "  contents differ only in trailing bytes"
	}
	return strings.TrimRight(out.String(), "\n")
}

func at(lines []string, i int) (string, bool) {
	if i >= len(lines) {
		return "", false
	}
	return lines[i], true
}

func isText(content []byte) bool {
	return !bytes.ContainsRune(content, 0)
}
