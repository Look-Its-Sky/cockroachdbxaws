package remediation

import (
	"strings"
	"testing"
)

func TestBuildScriptStagesInOrder(t *testing.T) {
	script := BuildScript(paymentRepo(), "https://github.com/o/r.git", "0c6f0ae", "opencode run 'fix it'")

	want := []string{stageClone, stageHarness, stageDiff, stageSetup, stageBuild}
	last := -1
	for _, stage := range want {
		at := strings.Index(script, sectionBegin+stage)
		if at < 0 {
			t.Fatalf("stage %q is missing from the script", stage)
		}
		if at < last {
			t.Errorf("stage %q runs out of order", stage)
		}
		last = at
	}

	// payment declares no test command, so no test stage may be emitted
	if strings.Contains(script, sectionBegin+stageTest) {
		t.Error("a test stage was emitted for a service with no test command")
	}
}

// `npm ci` and a build leave files behind. Diffing after them would present
// dependencies and build output to a reviewer as though the model wrote them.
func TestBuildScriptDiffsBeforeAnythingIsInstalledOrBuilt(t *testing.T) {
	script := BuildScript(paymentRepo(), "https://github.com/o/r.git", "0c6f0ae", "harness")

	diffAt := strings.Index(script, sectionBegin+stageDiff)
	setupAt := strings.Index(script, sectionBegin+stageSetup)
	buildAt := strings.Index(script, sectionBegin+stageBuild)

	if diffAt > setupAt || diffAt > buildAt {
		t.Error("the diff is taken after setup or build, so it can capture their leavings")
	}
}

// `git diff` alone shows nothing for a file the model created, and creating
// one is something ApplyCommand explicitly allows
func TestBuildScriptDiffIncludesNewFiles(t *testing.T) {
	script := BuildScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae", "harness")

	if !strings.Contains(script, "git -C /workspace add -A") {
		t.Error("the diff stage does not stage changes, so a new file would be invisible")
	}
	if !strings.Contains(script, "diff --cached") {
		t.Error("the diff is not taken from the index it just staged")
	}
}

func TestBuildScriptDoesNotAbortOnFailure(t *testing.T) {
	script := BuildScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae", "harness")

	// `set -e` would skip the diff whenever the build failed, losing the very
	// candidate we most want to show as broken
	for _, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) == "set -e" || strings.HasPrefix(strings.TrimSpace(line), "set -e ") {
			t.Error("the script aborts on the first failure; a failed build would lose the diff")
		}
	}
	if !strings.Contains(script, sectionBegin+stageTest) {
		t.Error("checkout declares tests but no test stage was emitted")
	}
}

func TestBuildScriptQuotesTheCloneURL(t *testing.T) {
	// the URL can carry a token; it must never be pasted in unquoted
	script := BuildScript(checkoutRepo(), "https://x-access-token:se'cret@github.com/o/r.git", "0c6f0ae", "harness")

	if strings.Contains(script, "se'cret@") {
		t.Error("the clone URL was not escaped")
	}
	if !strings.Contains(script, `'\''`) {
		t.Error("expected the quote in the URL to be shell-escaped")
	}
}

func TestBuildScriptRunsInTheServiceDirectory(t *testing.T) {
	script := BuildScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae", "harness")

	if !strings.Contains(script, "cd '/workspace/src/checkout'") {
		t.Errorf("script does not enter the service directory:\n%s", script)
	}
	// but the diff is taken from the repository root, so changes outside the
	// service directory are still captured
	if !strings.Contains(script, "git -C /workspace --no-pager diff") {
		t.Error("the diff is not taken from the repository root")
	}
}

// a server cannot resolve an abbreviated object name in a fetch, and the
// deploys table stores seven characters
func TestFetchCommitCommandHandlesShortAndFullSHAs(t *testing.T) {
	const full = "0c6f0ae70920e87405ab44d1e3a160bce1d4e82c"

	if got := fetchCommitCommand(full); !strings.Contains(got, "fetch --depth 1 origin") {
		t.Errorf("a full SHA is not fetched directly: %s", got)
	}

	short := fetchCommitCommand("0c6f0ae")
	if strings.Contains(short, "fetch --depth 1 origin") {
		t.Errorf("an abbreviated SHA is fetched by name, which always fails: %s", short)
	}
	if !strings.Contains(short, "--deepen") {
		t.Errorf("an abbreviated SHA has no fallback for a commit past the clone depth: %s", short)
	}
	// and the deepen only fires when the clone did not already reach it
	if !strings.Contains(short, "cat-file -e") {
		t.Errorf("the deepen is unconditional: %s", short)
	}
}

// A stage's exit code is its last command's, and the fetch fallbacks all end in
// `|| echo`. Without a final test the clone stage reports success even when git
// is absent entirely — which is what node:22-alpine did, and what
// Runner.candidate reads as a good checkout before proceeding on nothing.
func TestCloneStageReportsWhetherThereIsACheckout(t *testing.T) {
	for _, sha := range []string{"0c6f0ae", "0c6f0ae70920e87405ab44d1e3a160bce1d4e82c"} {
		got := cloneStageCommand(checkoutRepo(), "https://github.com/o/r.git", sha)

		if !strings.HasSuffix(strings.TrimSpace(got), "[ -d /workspace/.git ]") {
			t.Errorf("the clone stage for %q does not end by checking the checkout exists:\n%s", sha, got)
		}
	}

	// and every script that clones goes through it
	for name, script := range map[string]string{
		"BuildScript":        BuildScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae", "harness"),
		"BuildTriageScript":  BuildTriageScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae"),
		"BuildInspectScript": BuildInspectScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae", []string{"a.go"}),
	} {
		if !strings.Contains(script, "[ -d /workspace/.git ]") {
			t.Errorf("%s can report a successful clone with no checkout", name)
		}
	}
}

func TestIsFullSHA(t *testing.T) {
	tests := []struct {
		sha  string
		want bool
	}{
		{"0c6f0ae70920e87405ab44d1e3a160bce1d4e82c", true},
		{"0c6f0ae", false},
		{"", false},
		// 40 characters, but not all hex
		{"0c6f0ae70920e87405ab44d1e3a160bce1d4e82z", false},
		{"0C6F0AE70920E87405AB44D1E3A160BCE1D4E82C", false},
	}

	for _, tc := range tests {
		if got := isFullSHA(tc.sha); got != tc.want {
			t.Errorf("isFullSHA(%q) = %v, want %v", tc.sha, got, tc.want)
		}
	}
}

func TestParseSections(t *testing.T) {
	output := strings.Join([]string{
		"noise before anything starts",
		sectionBegin + "clone",
		"Cloning into '/workspace'...",
		sectionEnd + "clone 0",
		sectionBegin + "build",
		"main.go:12: undefined: foo",
		sectionEnd + "build 2",
		sectionBegin + "diff",
		"diff --git a/src/checkout/main.go b/src/checkout/main.go",
		"-	old",
		"+	new",
		sectionEnd + "diff 0",
		"trailing noise",
	}, "\n")

	got := ParseSections(output)

	if len(got) != 3 {
		t.Fatalf("parsed %d sections, want 3: %#v", len(got), got)
	}
	if !got[stageClone].Passed() {
		t.Error("clone should have passed")
	}
	if got[stageBuild].ExitCode != 2 || got[stageBuild].Passed() {
		t.Errorf("build section = %+v, want a failure with code 2", got[stageBuild])
	}
	if !strings.Contains(got[stageBuild].Output, "undefined: foo") {
		t.Errorf("build output = %q", got[stageBuild].Output)
	}
	// noise outside any stage must not land in a section, least of all the diff
	if strings.Contains(got[stageDiff].Output, "noise") {
		t.Errorf("unmarked output leaked into the diff: %q", got[stageDiff].Output)
	}
	if !strings.HasPrefix(got[stageDiff].Output, "diff --git") {
		t.Errorf("diff = %q", got[stageDiff].Output)
	}
}

func TestParseSectionsMissingStageIsNotPassing(t *testing.T) {
	// a container killed mid-run leaves a begin with no end
	output := sectionBegin + "test\nrunning tests..."

	got := ParseSections(output)
	if section, ok := got[stageTest]; ok {
		t.Errorf("an unterminated stage was recorded as %+v", section)
	}
	// and a stage that never ran must not read as success
	if got[stageTest].Passed() {
		t.Error("a missing stage reports as passed")
	}
	if got[stageTest].Ran {
		t.Error("a missing stage reports as having run")
	}
}

func TestParseSectionsIgnoresUnmatchedEndMarker(t *testing.T) {
	// a harness that echoes our own markers must not be able to fabricate a
	// passing stage it never ran
	output := sectionEnd + "test 0"

	if got := ParseSections(output); len(got) != 0 {
		t.Errorf("an unmatched end marker produced sections: %#v", got)
	}
}

func TestSplitEnd(t *testing.T) {
	tests := []struct {
		rest string
		name string
		code int
	}{
		{"build 0", "build", 0},
		{"build 2", "build", 2},
		{"build", "build", 0},
		// an unparseable code cannot be read as success
		{"build oops", "build", 1},
		{"", "", 0},
	}

	for _, tc := range tests {
		name, code := splitEnd(tc.rest)
		if name != tc.name || code != tc.code {
			t.Errorf("splitEnd(%q) = %q/%d, want %q/%d", tc.rest, name, code, tc.name, tc.code)
		}
	}
}
