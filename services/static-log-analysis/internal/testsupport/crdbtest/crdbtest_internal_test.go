package crdbtest

import (
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

func TestReplaceDatabasePointsAtAnotherDatabaseOnTheSameCluster(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "with query parameters",
			dsn:  "postgres://root@127.0.0.1:32768/defaultdb?sslmode=disable",
			want: "postgres://root@127.0.0.1:32768/t_example_1?sslmode=disable",
		},
		{
			name: "without query parameters",
			dsn:  "postgres://root@127.0.0.1:32768/defaultdb",
			want: "postgres://root@127.0.0.1:32768/t_example_1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := replaceDatabase(test.dsn, "t_example_1"); got != test.want {
				t.Fatalf("want %s, got %s", test.want, got)
			}
		})
	}
}

func TestDatabaseNamesAreUniqueAndValid(t *testing.T) {
	first := databaseName(tb.NewRecorder("TestSomething/with a sub-test"))
	second := databaseName(tb.NewRecorder("TestSomething/with a sub-test"))

	// Two tests with the same name, or one test calling Pool twice, must not
	// collide: a shared database would make one test's rows another's.
	if first == second {
		t.Fatalf("want distinct database names, both were %s", first)
	}
	for _, name := range []string{first, second} {
		if !strings.HasPrefix(name, "t_testsomething_with_a_sub_test") {
			t.Errorf("want the test name preserved in %s", name)
		}
		for _, r := range name {
			isLower := r >= 'a' && r <= 'z'
			isDigit := r >= '0' && r <= '9'
			if !isLower && !isDigit && r != '_' {
				t.Errorf("want an identifier-safe name, got %q in %s", r, name)
			}
		}
	}
}

func TestDatabaseNamesAreBounded(t *testing.T) {
	name := databaseName(tb.NewRecorder(strings.Repeat("VeryLongTestName", 20)))

	if len(name) > 64 {
		t.Fatalf("want a bounded identifier, got %d characters: %s", len(name), name)
	}
}

func TestQuoteIdentifierEscapesQuotes(t *testing.T) {
	if got, want := quoteIdentifier(`odd"name`), `"odd""name"`; got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestDisabledTestsAreSkipped(t *testing.T) {
	t.Setenv(disableEnv, "off")
	t.Setenv(requireEnv, "")
	recorder := tb.NewRecorder(t.Name())

	stopped := recorder.ExpectFatal(func() { Pool(recorder) })

	if !stopped {
		t.Fatal("want the test stopped when the harness is disabled, it continued")
	}
	if len(recorder.Skips()) != 1 {
		t.Fatalf("want a skip, got skips=%v fatals=%v", recorder.Skips(), recorder.Fatals())
	}
	if failure := recorder.FailureText(); !strings.Contains(failure, requireEnv) {
		t.Errorf("want the skip to say how to make absence a failure, got:\n%s", failure)
	}
}

func TestRequireDockerTurnsAnUnavailableClusterIntoAFailure(t *testing.T) {
	t.Setenv(disableEnv, "off")
	t.Setenv(requireEnv, "1")
	recorder := tb.NewRecorder(t.Name())

	stopped := recorder.ExpectFatal(func() { Pool(recorder) })

	if !stopped {
		t.Fatal("want the test stopped, it continued")
	}
	// A skipped storage test in continuous integration is the same as no
	// storage test, so the gate has to be able to demand a real database.
	if len(recorder.Fatals()) != 1 {
		t.Fatalf("want a failure rather than a skip, got skips=%v fatals=%v",
			recorder.Skips(), recorder.Fatals())
	}
}
