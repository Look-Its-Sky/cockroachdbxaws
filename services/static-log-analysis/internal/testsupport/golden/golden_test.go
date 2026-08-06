package golden_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/golden"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

// fixture is the shape stored in testdata/committed.json. Field order in the
// struct is the field order in the fixture.
type fixture struct {
	Region   string `json:"region"`
	Service  string `json:"service"`
	Severity string `json:"severity"`
}

func committed() fixture {
	return fixture{Region: "us-east-1", Service: "paymentservice", Severity: "ERROR"}
}

func TestJSONMatchesACommittedFixture(t *testing.T) {
	golden.JSON(t, "committed.json", committed())
}

func TestJSONFailsWhenTheValueChanged(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())
	changed := committed()
	changed.Severity = "FATAL"

	fataled := recorder.ExpectFatal(func() {
		golden.JSON(recorder, "committed.json", changed)
	})

	if !fataled {
		t.Fatal("want a changed value to fail against its fixture, got no failure")
	}
	failure := recorder.FailureText()
	// The failure has to say which line changed and how to accept it, or the
	// next person just reruns with the update flag without reading anything.
	if !strings.Contains(failure, `"severity": "FATAL"`) {
		t.Errorf("want the failure to show the new value, got:\n%s", failure)
	}
	if !strings.Contains(failure, `"severity": "ERROR"`) {
		t.Errorf("want the failure to show the fixture value, got:\n%s", failure)
	}
	if !strings.Contains(failure, "UPDATE_GOLDEN=1") {
		t.Errorf("want the failure to say how to accept the change, got:\n%s", failure)
	}
}

func TestBytesFailsWhenTheFixtureDoesNotExist(t *testing.T) {
	t.Chdir(t.TempDir())
	recorder := tb.NewRecorder(t.Name())

	fataled := recorder.ExpectFatal(func() {
		golden.Bytes(recorder, "absent.txt", []byte("value"))
	})

	if !fataled {
		t.Fatal("want a missing fixture to fail, got no failure")
	}
	// Creating the fixture silently on first run would let a wrong expectation
	// be committed without anyone ever looking at it.
	if failure := recorder.FailureText(); !strings.Contains(failure, "does not exist") {
		t.Errorf("want the failure to say the fixture is missing, got:\n%s", failure)
	}
	if _, err := os.Stat(golden.Path("absent.txt")); !os.IsNotExist(err) {
		t.Error("want no fixture written on a failed comparison")
	}
}

func TestUpdatingWritesTheFixtureIncludingItsDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("UPDATE_GOLDEN", "1")
	recorder := tb.NewRecorder(t.Name())

	golden.Bytes(recorder, "nested/deeper/value.txt", []byte("written\n"))

	if recorder.Failed() {
		t.Fatalf("want writing to succeed, got:\n%s", recorder.FailureText())
	}
	written, err := os.ReadFile(filepath.Join("testdata", "nested", "deeper", "value.txt"))
	if err != nil {
		t.Fatalf("reading the written fixture: %v", err)
	}
	if string(written) != "written\n" {
		t.Fatalf("want the fixture to hold the new value, got %q", written)
	}
}

func TestUpdatingRewritesAnExistingFixture(t *testing.T) {
	t.Chdir(t.TempDir())
	recorder := tb.NewRecorder(t.Name())

	t.Setenv("UPDATE_GOLDEN", "1")
	golden.Bytes(recorder, "value.txt", []byte("first\n"))
	golden.Bytes(recorder, "value.txt", []byte("second\n"))

	t.Setenv("UPDATE_GOLDEN", "")
	golden.Bytes(recorder, "value.txt", []byte("second\n"))

	if recorder.Failed() {
		t.Fatalf("want the rewritten fixture to match, got:\n%s", recorder.FailureText())
	}
}

func TestUpdatingIsOffByDefault(t *testing.T) {
	if golden.Updating() {
		t.Fatal("want fixture rewriting off unless UPDATE_GOLDEN=1 is set")
	}
	t.Setenv("UPDATE_GOLDEN", "true")
	if golden.Updating() {
		t.Fatal("want only the exact value 1 to enable rewriting")
	}
}

func TestEncodingDoesNotDependOnMapIterationOrder(t *testing.T) {
	value := map[string]any{
		"zeta":  1,
		"alpha": 2,
		"mid":   map[string]any{"b": true, "a": false},
	}

	first := string(golden.Encode(t, value))
	for i := 0; i < 50; i++ {
		if got := string(golden.Encode(t, value)); got != first {
			t.Fatalf("encoding varies between runs:\n%s\nand\n%s", first, got)
		}
	}
	if !strings.Contains(first, "\"alpha\"") {
		t.Fatalf("want object keys present, got %s", first)
	}
	if strings.Index(first, "\"alpha\"") > strings.Index(first, "\"zeta\"") {
		t.Fatalf("want object keys sorted, got %s", first)
	}
}

func TestEncodingLeavesLogDerivedTextIntact(t *testing.T) {
	// Log bodies routinely contain markup and query strings. Escaping them
	// would make a fixture change without the value changing.
	value := map[string]string{"body": `charge failed for <order> & "id"=1`}

	encoded := string(golden.Encode(t, value))

	if !strings.Contains(encoded, `<order> & \"id\"=1`) {
		t.Fatalf("want the text preserved without HTML escaping, got %s", encoded)
	}
}

func TestStringComparesWithATrailingNewline(t *testing.T) {
	t.Chdir(t.TempDir())
	recorder := tb.NewRecorder(t.Name())

	t.Setenv("UPDATE_GOLDEN", "1")
	golden.String(recorder, "line.txt", "no trailing newline")
	t.Setenv("UPDATE_GOLDEN", "")
	golden.String(recorder, "line.txt", "no trailing newline")

	if recorder.Failed() {
		t.Fatalf("want a value without a trailing newline to round trip, got:\n%s", recorder.FailureText())
	}
	written, err := os.ReadFile(filepath.Join("testdata", "line.txt"))
	if err != nil {
		t.Fatalf("reading the written fixture: %v", err)
	}
	if !strings.HasSuffix(string(written), "\n") {
		t.Errorf("want fixtures to end with a newline, got %q", written)
	}
}

func TestLoadReadsAnInputFixture(t *testing.T) {
	content := golden.Load(t, "committed.json")
	if !strings.Contains(string(content), "paymentservice") {
		t.Fatalf("want the fixture contents, got %q", content)
	}
}

func TestLoadFailsOnAMissingInputFixture(t *testing.T) {
	t.Chdir(t.TempDir())
	recorder := tb.NewRecorder(t.Name())

	fataled := recorder.ExpectFatal(func() { golden.Load(recorder, "absent.json") })

	if !fataled {
		t.Fatal("want a missing input fixture to fail, got no failure")
	}
	// An input fixture is never created by rewriting, so the failure must not
	// suggest it.
	if failure := recorder.FailureText(); strings.Contains(failure, "UPDATE_GOLDEN") {
		t.Errorf("want no rewrite suggestion for an input fixture, got:\n%s", failure)
	}
}

func TestDiffNamesTheDifferingLines(t *testing.T) {
	t.Chdir(t.TempDir())
	recorder := tb.NewRecorder(t.Name())

	t.Setenv("UPDATE_GOLDEN", "1")
	golden.String(recorder, "multi.txt", "same\nfixture only\nalso same")
	t.Setenv("UPDATE_GOLDEN", "")

	recorder.ExpectFatal(func() {
		golden.String(recorder, "multi.txt", "same\nvalue instead\nalso same\nextra")
	})

	failure := recorder.FailureText()
	if !strings.Contains(failure, "line 2") {
		t.Errorf("want the changed line identified, got:\n%s", failure)
	}
	if !strings.Contains(failure, "line 4 only in value") {
		t.Errorf("want the added line identified, got:\n%s", failure)
	}
	if strings.Contains(failure, "same") && strings.Contains(failure, "line 1") {
		t.Errorf("want unchanged lines omitted, got:\n%s", failure)
	}
}

func TestPathIsUnderTestdata(t *testing.T) {
	if got, want := golden.Path("nested/value.json"), filepath.Join("testdata", "nested", "value.json"); got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}
