package remediation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

func TestParseProposalReadsSummaryRationaleAndFiles(t *testing.T) {
	answer := `SUMMARY: clamp the discount so it cannot exceed the total
RATIONALE: The discount was applied without a bound, so a coupon larger than
the order total produced a negative charge.

` + fileBegin + `src/checkout/money/money.go
package money

func Apply(total, discount int64) int64 {
	if discount > total {
		discount = total
	}
	return total - discount
}
` + fileEnd + `
`

	p, err := ParseProposal(answer)
	if err != nil {
		t.Fatalf("ParseProposal: %v", err)
	}

	if p.Summary != "clamp the discount so it cannot exceed the total" {
		t.Errorf("summary = %q", p.Summary)
	}
	// the rationale wraps across two lines and both belong to it
	if !strings.Contains(p.Rationale, "without a bound") || !strings.Contains(p.Rationale, "negative charge") {
		t.Errorf("rationale did not absorb its continuation line: %q", p.Rationale)
	}

	if len(p.Edits) != 1 {
		t.Fatalf("got %d edits, want 1", len(p.Edits))
	}
	if p.Edits[0].Path != "src/checkout/money/money.go" {
		t.Errorf("path = %q", p.Edits[0].Path)
	}
	if !strings.HasPrefix(p.Edits[0].Contents, "package money") {
		t.Errorf("contents lost their first line: %q", p.Edits[0].Contents)
	}
	if strings.Contains(p.Edits[0].Contents, fileEnd) {
		t.Error("the end marker leaked into the file contents")
	}
}

// a model that runs out of tokens mid-file must not have half a file written
// over a whole one
func TestParseProposalRejectsAnUnclosedBlock(t *testing.T) {
	answer := "SUMMARY: fix it\n" + fileBegin + "src/checkout/main.go\npackage main\n\nfunc main() {\n"

	if _, err := ParseProposal(answer); err == nil {
		t.Fatal("an unterminated file block was accepted")
	}
}

func TestParseProposalAcceptsNoChange(t *testing.T) {
	p, err := ParseProposal("SUMMARY: no change\nRATIONALE: the current code already handles this.")
	if err != nil {
		t.Fatalf("ParseProposal: %v", err)
	}
	if len(p.Edits) != 0 {
		t.Fatalf("got %d edits, want none", len(p.Edits))
	}
}

func TestParseProposalRejectsEscapingPaths(t *testing.T) {
	for _, path := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		".git/config",
		"",
	} {
		answer := fileBegin + path + "\nowned\n" + fileEnd + "\n"
		if _, err := ParseProposal(answer); err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
}

func TestParseProposalRejectsTheSameFileTwice(t *testing.T) {
	answer := fileBegin + "a.go\nfirst\n" + fileEnd + "\n" +
		fileBegin + "./a.go\nsecond\n" + fileEnd + "\n"

	if _, err := ParseProposal(answer); err == nil {
		t.Fatal("a duplicate path was accepted; the later block would silently win")
	}
}

func TestApplyCommandWritesAbsolutePaths(t *testing.T) {
	cmd, err := ApplyCommand([]FileEdit{{Path: "src/checkout/main.go", Contents: "package main\n"}})
	if err != nil {
		t.Fatalf("ApplyCommand: %v", err)
	}

	// BuildScript has already cd'd into the service directory by this point, so
	// a relative path would write to the wrong place and a cd would leak into
	// the build and test stages
	if !strings.Contains(cmd, "'/workspace/src/checkout/main.go'") {
		t.Errorf("target is not absolute:\n%s", cmd)
	}
	if strings.Contains(cmd, "\ncd ") {
		t.Errorf("the apply stage changes directory, which would leak into later stages:\n%s", cmd)
	}
	// a quoted heredoc delimiter, so $VAR and backticks in the file survive
	if !strings.Contains(cmd, "<<'"+patchDelimiter+"'") {
		t.Errorf("heredoc delimiter is not quoted:\n%s", cmd)
	}
}

// the stage's exit code is its last command's, so a write that failed must not
// be followed by something that succeeds
func TestApplyCommandFailsWhenAFileWasNotWritten(t *testing.T) {
	cmd, err := ApplyCommand([]FileEdit{{Path: "a.go", Contents: "package a\n"}})
	if err != nil {
		t.Fatalf("ApplyCommand: %v", err)
	}

	if !strings.Contains(cmd, "exit 91") {
		t.Errorf("nothing checks that the file arrived:\n%s", cmd)
	}
	// the last thing the stage runs must be the check, not an unconditional echo
	if strings.HasSuffix(strings.TrimSpace(cmd), patchDelimiter) {
		t.Errorf("the stage ends on the heredoc, so its exit code says nothing:\n%s", cmd)
	}
}

// the apply stage is shell, so the only honest test of it is a shell.
//
// Run against a temporary directory standing in for /workspace, which is the
// one substitution needed to exercise the real generated script.
func TestApplyCommandWritesTheFilesItSaysItDoes(t *testing.T) {
	// deliberately nasty: a raw string, an unexpanded variable, a backtick and
	// a single quote all have to survive being written by a shell
	const contents = "package money\n\n// $HOME `whoami` it's fine\nconst Q = `raw\nstring`\n"

	cmd, err := ApplyCommand([]FileEdit{
		{Path: "src/checkout/money.go", Contents: contents},
		{Path: "new/dir/added.go", Contents: "package added\n"},
	})
	if err != nil {
		t.Fatalf("ApplyCommand: %v", err)
	}

	dir := t.TempDir()
	script := strings.ReplaceAll(cmd, "/workspace/", dir+"/")

	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the apply stage failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}

	written, err := os.ReadFile(filepath.Join(dir, "src/checkout/money.go"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(written) != contents {
		t.Errorf("contents were mangled by the shell:\ngot:\n%q\nwant:\n%q", written, contents)
	}

	// a file in a directory that did not exist yet
	if _, err := os.Stat(filepath.Join(dir, "new/dir/added.go")); err != nil {
		t.Errorf("a new directory was not created: %v", err)
	}
}

func TestApplyCommandRejectsContentsCarryingTheDelimiter(t *testing.T) {
	_, err := ApplyCommand([]FileEdit{{
		Path:     "a.go",
		Contents: "package main\n" + patchDelimiter + "\nrm -rf /\n",
	}})
	if err == nil {
		t.Fatal("contents containing the heredoc delimiter were accepted")
	}
}

func TestApplyCommandRejectsAnEmptyProposal(t *testing.T) {
	if _, err := ApplyCommand(nil); err == nil {
		t.Fatal("a proposal that changes nothing was accepted")
	}
}

func TestParseSourcesSeparatesFilesFromTheirState(t *testing.T) {
	output := strings.Join([]string{
		sectionBegin + stageSources,
		fileBegin + "a.go",
		"package a",
		"",
		"const X = 1",
		fileEnd,
		fileBegin + "big.json",
		fileTooLarge,
		fileEnd,
		fileBegin + "gone.go",
		fileMissing,
		fileEnd,
		sectionEnd + stageSources + " 0",
	}, "\n")

	sources := ParseSources(ParseSections(output))
	if len(sources) != 3 {
		t.Fatalf("got %d sources, want 3", len(sources))
	}

	if sources[0].Contents != "package a\n\nconst X = 1" {
		t.Errorf("contents = %q", sources[0].Contents)
	}
	if !sources[1].TooLarge || sources[1].Contents != "" {
		t.Errorf("an oversized file came back as editable: %+v", sources[1])
	}
	if !sources[2].Missing {
		t.Errorf("a missing file came back as present: %+v", sources[2])
	}

	// only the first is safe to hand a model to rewrite whole
	if got := readable(sources); len(got) != 1 || got[0].Path != "a.go" {
		t.Errorf("readable = %+v", got)
	}
}

func TestBuildInspectScriptDropsPathsThatEscape(t *testing.T) {
	repo := Repository{Owner: "o", Repo: "r", DefaultBranch: "main", Subdirectory: "src/checkout"}
	script := BuildInspectScript(repo, "https://example.invalid/r.git", "abc1234",
		[]string{"src/checkout/main.go", "../../etc/passwd"})

	if !strings.Contains(script, "/workspace/src/checkout/main.go") {
		t.Error("the legitimate path was dropped")
	}
	if strings.Contains(script, "etc/passwd") {
		t.Errorf("a path escaping the checkout reached the script:\n%s", script)
	}
}

func TestProposalPromptCarriesPrecedentAsAConstraint(t *testing.T) {
	in := ProposalInput{
		IncidentSummary: "Checkout returned 500s on orders over $10.",
		Repository:      Repository{ServiceID: "checkout", RuntimeImage: "golang:1.25", BuildCommand: "go build ./..."},
		Precedents: []string{
			"Remediation decision (2026-08-13). Chosen fix (minimal strategy): widen the accumulator. Rejected: root-cause: rewrote more than the incident justified [from the engineer].",
		},
	}

	prompt := buildProposalPrompt(in)

	for _, want := range []string{
		"How this team has judged fixes before",
		"widen the accumulator",
		// the guard against every strategy collapsing onto the same answer:
		// fan-out is only worth its containers while the three stay different
		"Do NOT copy the fix that was chosen",
		"the strategy you were given below still decides what you produce",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestProposalPromptOmitsPrecedentSectionWhenThereIsNone(t *testing.T) {
	prompt := buildProposalPrompt(ProposalInput{
		IncidentSummary: "Checkout returned 500s.",
		Repository:      Repository{ServiceID: "checkout", RuntimeImage: "golang:1.25"},
	})

	if strings.Contains(prompt, "How this team has judged") {
		t.Errorf("the precedent section appears with no precedents:\n%s", prompt)
	}
}

// The failure that presented as "the model has nothing to say" and was really
// "the budget was too small to say it". Reasoning tokens are billed against the
// completion budget, so a model that thinks at length returns an empty message
// with a length stop — no error, nothing to parse.
func TestEmptyModelMessageNamesTheReasoningBudget(t *testing.T) {
	_, err := choiceContent(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{Content: "  \n ", StopReason: "length"}},
	}, "repair (minimal)")

	if err == nil {
		t.Fatal("an empty message was accepted as a usable response")
	}
	for _, want := range []string{"repair (minimal)", "length", "budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestNoChoicesReadsDifferentlyFromAnEmptyMessage(t *testing.T) {
	_, err := choiceContent(&llms.ContentResponse{}, "propose (minimal)")
	if err == nil {
		t.Fatal("a response with no choices was accepted")
	}
	// a provider that returned nothing at all is not a budget problem, and
	// saying so would send someone tuning the wrong dial
	if strings.Contains(err.Error(), "budget") {
		t.Errorf("no-choices error blames the budget: %v", err)
	}
}

func TestContentIsReturnedUnchangedWhenPresent(t *testing.T) {
	got, err := choiceContent(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{Content: "SUMMARY: fixed it", StopReason: "stop"}},
	}, "propose (minimal)")
	if err != nil || got != "SUMMARY: fixed it" {
		t.Errorf("got %q, err %v", got, err)
	}
}
