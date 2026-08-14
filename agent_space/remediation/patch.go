package remediation

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/tmc/langchaingo/llms"
)

// The model rewrites whole files rather than emitting a unified diff.
//
// A diff has to be correct about line numbers and context before it can be
// applied at all, and a small model gets that wrong far more often than it gets
// the actual fix wrong — which turns a good change into an apply failure. A
// whole file is written with `cat`, which cannot fail for the wrong reasons, and
// `git diff` inside the sandbox produces the real unified diff afterwards. The
// diff an engineer reviews is therefore always one git itself computed.
const (
	// matched without the trailing space, so a block header carrying no path
	// still opens a block and is then rejected by validation, rather than being
	// mistaken for prose and silently swallowing the file under it
	fileMarker = "__SRE_FILE__"
	fileBegin  = fileMarker + " "
	fileEnd    = "__SRE_FILE_END__"
	// stands in for a file too big to quote in a prompt and too big to have
	// rewritten wholesale
	fileTooLarge = "__SRE_FILE_TOO_LARGE__"
	fileMissing  = "__SRE_FILE_MISSING__"
)

// the heredoc delimiter the apply stage writes files with. Contents containing
// this line are rejected in Go rather than producing a script that ends early.
const patchDelimiter = "__SRE_PATCH_EOF__"

// caps on what crosses into a prompt. A file bigger than this is not a
// candidate for whole-file rewriting anyway: the model would spend its whole
// budget copying it out, and any slip silently deletes code.
const (
	maxFileBytes = 60000
	maxFileCount = 12
)

// the stages BuildInspectScript adds
const (
	stageTree    = "tree"
	stageSources = "sources"
)

// Sourced is one file as it stands in the repository.
type Sourced struct {
	Path     string
	Contents string
	// TooLarge or Missing mean Contents is empty and the file must not be
	// offered to the model as something it can rewrite.
	TooLarge bool
	Missing  bool
}

// BuildInspectScript reads whole files out of a fresh checkout. It only reads:
// this runs before anything has been decided, and a failed inspection must not
// leave a checkout a later stage would inherit — it does not, because the
// container is destroyed either way.
func BuildInspectScript(repo Repository, cloneURL, sha string, files []string) string {
	var b strings.Builder

	b.WriteString("set -u\n")
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")

	stage(&b, stageClone, cloneStageCommand(repo, cloneURL, sha))
	b.WriteString("cd /workspace || exit 97\n")
	b.WriteString("git config --global --add safe.directory /workspace\n")

	// the shape of the service, so a fix may add a file in the right place
	// rather than inventing a layout
	stage(&b, stageTree, fmt.Sprintf(
		"find %s -type f -not -path '*/.git/*' | sed 's|^/workspace/||' | sort | head -n 300",
		shellQuote(repo.WorkingDir())))

	var body strings.Builder
	for _, f := range clampFiles(files) {
		quoted := shellQuote("/workspace/" + f)
		fmt.Fprintf(&body, "echo '%s%s'\n", fileBegin, f)
		// the size test is here rather than in Go because only the container
		// knows what is actually in the checkout
		fmt.Fprintf(&body,
			"if [ ! -f %s ]; then echo '%s'; "+
				"elif [ \"$(wc -c < %s)\" -gt %d ]; then echo '%s'; "+
				"else cat %s; fi\n",
			quoted, fileMissing, quoted, maxFileBytes, fileTooLarge, quoted)
		fmt.Fprintf(&body, "echo '%s'\n", fileEnd)
	}

	if body.Len() > 0 {
		stage(&b, stageSources, strings.TrimRight(body.String(), "\n"))
	}
	return b.String()
}

// drop paths that could escape the checkout, and cap the count
func clampFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		clean, err := repoPath(f)
		if err != nil {
			continue
		}
		out = append(out, clean)
		if len(out) == maxFileCount {
			break
		}
	}
	return out
}

// ParseSources pulls whole files back out of an inspection run.
func ParseSources(sections map[string]Section) []Sourced {
	body := sections[stageSources].Output
	if strings.TrimSpace(body) == "" {
		return nil
	}

	var (
		out     []Sourced
		current *Sourced
		lines   []string
	)

	for line := range strings.SplitSeq(body, "\n") {
		if name, ok := strings.CutPrefix(line, fileBegin); ok {
			current = &Sourced{Path: strings.TrimSpace(name)}
			lines = nil
			continue
		}

		if line == fileEnd {
			if current != nil {
				current.Contents = strings.Join(lines, "\n")
				out = append(out, *current)
			}
			current = nil
			lines = nil
			continue
		}

		if current == nil {
			continue
		}

		switch line {
		case fileTooLarge:
			current.TooLarge = true
		case fileMissing:
			current.Missing = true
		default:
			lines = append(lines, line)
		}
	}

	return out
}

// FileEdit is one whole file as the model would have it.
type FileEdit struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

// Proposal is one model's answer: what it would change and why.
type Proposal struct {
	Summary   string
	Rationale string
	Edits     []FileEdit
}

// Strategy is one way of asking for a fix. Several exist so the engineer is
// offered genuinely different changes rather than the same one three times at
// different temperatures.
type Strategy struct {
	Name        string
	Instruction string
	Temperature float64
}

// Deliberately different in kind, not degree. The narrow one is usually right
// during an incident; the thorough one is what a reviewer would have asked for
// anyway; the defensive one is what stops the same page firing next week.
var DefaultStrategies = []Strategy{
	{
		Name:        "minimal",
		Temperature: 0.1,
		Instruction: "Make the smallest change that removes the fault. Touch one file if you can. " +
			"Do not refactor, rename, reformat, or improve anything you were not asked about.",
	},
	{
		Name:        "defensive",
		Temperature: 0.3,
		Instruction: "Fix the fault and guard against the class of bug it belongs to — validate the " +
			"input, handle the boundary case, fail loudly instead of silently. Stay within the files " +
			"the fault lives in.",
	},
	{
		Name:        "root-cause",
		Temperature: 0.5,
		Instruction: "Address the underlying cause rather than the symptom, even if the change is " +
			"larger. If the fault is possible because of how the code is structured, change that " +
			"structure. Say plainly in your rationale what the larger change buys.",
	},
}

// ProposalInput is everything the model is given to write a fix from.
type ProposalInput struct {
	IncidentSummary string
	VerdictAnswer   string
	TriageEvidence  string
	Repository      Repository
	Sources         []Sourced
	Tree            string
	// Precedents are decisions engineers recorded on similar incidents: which
	// fix they took and what they turned down. Empty is the ordinary case
	// early on, and changes nothing.
	Precedents []string
}

const proposalPrompt = `You are fixing a production fault in a service that is currently in incident.

You are given the incident, what an investigation concluded, a triage note saying
the fault is present, the layout of the service, and the current contents of the
files the fault is believed to live in.

Reply with a one-line summary, a short rationale, and then the complete new
contents of every file you are changing, in exactly this format:

SUMMARY: one line, what you changed
RATIONALE: two or three sentences, why this fixes the fault

` + fileBegin + `path/relative/to/the/repository/root
<the complete new contents of that file>
` + fileEnd + `

Rules that matter:
- Write the WHOLE file, from its first line to its last. What you write replaces
  the file entirely, so anything you leave out is deleted.
- Do NOT wrap the contents in markdown fences and do NOT add commentary inside a
  file block. The block is written to disk verbatim.
- Paths are relative to the repository root, exactly as they were shown to you.
- Change as little as the strategy allows. You are being reviewed by a person
  under time pressure.
- If the evidence does not support any change, reply with SUMMARY: no change and
  no file blocks. Inventing a change to working code is worse than saying so.`

// Propose asks the model for one candidate fix.
func Propose(ctx context.Context, model llms.Model, in ProposalInput, s Strategy) (Proposal, error) {
	if model == nil {
		return Proposal{}, errors.New("remediation: no model configured")
	}

	resp, err := model.GenerateContent(ctx,
		[]llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, proposalPrompt+"\n\nYour strategy for this attempt:\n"+s.Instruction),
			llms.TextParts(llms.ChatMessageTypeHuman, buildProposalPrompt(in)),
		},
		llms.WithMaxTokens(proposalBudget),
		llms.WithTemperature(s.Temperature),
	)
	if err != nil {
		return Proposal{}, fmt.Errorf("remediation: propose (%s): %w", s.Name, err)
	}
	content, err := choiceContent(resp, "propose ("+s.Name+")")
	if err != nil {
		return Proposal{}, err
	}
	return ParseProposal(content)
}

// the model's text, or an error saying why there is none.
//
// An empty message with a "length" stop is the reasoning-budget failure, and it
// is worth naming in those words: it presents as the model having nothing to
// say, which reads as a bad model rather than as a budget that was too small.
// That misreading cost two full pipeline runs and a model swap to unpick.
func choiceContent(resp *llms.ContentResponse, what string) (string, error) {
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("remediation: %s: model returned no choices", what)
	}

	choice := resp.Choices[0]
	if strings.TrimSpace(choice.Content) != "" {
		return choice.Content, nil
	}

	return "", fmt.Errorf("remediation: %s: the model returned an empty message (stop reason: %s). "+
		"Reasoning counts against the %d-token completion budget, so a model that thinks at length "+
		"can exhaust it before writing anything; raise proposalBudget or use a model that reasons less",
		what, fallback(choice.StopReason, "unknown"), proposalBudget)
}

// Whole files come back out, so this has to be far larger than the verdict's.
//
// It also has to absorb reasoning. Providers count a model's internal thinking
// against this same budget, so a reasoning model given too little spends all of
// it deliberating and returns an empty message — no error, no content, nothing
// to parse. Measured on qwen3.7-flash: ~3,000 reasoning tokens for a trivial
// one-function fix, before a single character of the answer. A real repair
// carries whole files and a compiler log, and reasons proportionally.
const proposalBudget = 32000

func buildProposalPrompt(in ProposalInput) string {
	var b strings.Builder

	b.WriteString("## The incident\n\n")
	b.WriteString(in.IncidentSummary)

	if s := strings.TrimSpace(in.VerdictAnswer); s != "" {
		b.WriteString("\n\n## What the investigation concluded\n\n")
		b.WriteString(s)
	}
	if s := strings.TrimSpace(in.TriageEvidence); s != "" {
		b.WriteString("\n\n## What triage found in the code\n\n")
		b.WriteString(s)
	}

	writePrecedents(&b, in.Precedents)

	fmt.Fprintf(&b, "\n\n## The service\n\n%s, in %s, built with `%s`.\n",
		in.Repository.ServiceID, in.Repository.Redacted(), in.Repository.RuntimeImage)
	if in.Repository.HasTests() {
		fmt.Fprintf(&b, "Its tests are run with `%s`, and they will be run against your change.\n",
			in.Repository.TestCommand)
	} else {
		b.WriteString("This service declares no tests, so your change will only be compiled. " +
			"Be correspondingly careful.\n")
	}

	if s := strings.TrimSpace(in.Tree); s != "" {
		b.WriteString("\n## Files in the service\n\n```\n")
		b.WriteString(s)
		b.WriteString("\n```\n")
	}

	b.WriteString("\n## The files as they stand now\n")
	for _, src := range in.Sources {
		switch {
		case src.Missing:
			fmt.Fprintf(&b, "\n%s no longer exists in the repository.\n", src.Path)
		case src.TooLarge:
			fmt.Fprintf(&b, "\n%s is too large to show or to rewrite. Do not edit it.\n", src.Path)
		default:
			fmt.Fprintf(&b, "\n%s%s\n%s\n%s\n", fileBegin, src.Path, src.Contents, fileEnd)
		}
	}

	return b.String()
}

// Failure is how the previous attempt was rejected, in the toolchain's words.
type Failure struct {
	// Stage is "build" or "test": which one said no.
	Stage string
	Log   string
}

const repairPrompt = `Your previous attempt at this fix did not work. You are being shown exactly
what the toolchain said, and the current contents of the files as you left them.

Correct the problem and reply in the same format as before: a summary, a
rationale, and the complete new contents of every file you are changing.

Rules that matter:
- The output below is ground truth. It is not a suggestion and it is not a
  matter of opinion — the code did not compile, or the tests did not pass.
- Fix the reported problem without abandoning the fix. Reverting to the original
  code makes the build pass and leaves the fault in production, which is worse
  than failing.
- Change as little as the error requires. A type conversion is a type
  conversion; do not rewrite the function around it.
- Write the WHOLE file, from its first line to its last, exactly as before.
  Anything you leave out is deleted.`

// Repair shows a candidate its own failure and asks again.
//
// It exists because the failures that come back are overwhelmingly mechanical —
// a missing type conversion, an unused import, an off-by-one in a test
// expectation — while the reasoning about *what* to change was right. Throwing
// that away and resampling from scratch costs the same and usually reproduces
// the same class of slip; handing back the compiler output fixes it in one.
// what this team has accepted and turned down before.
//
// Framed as a constraint rather than as an example, deliberately. The three
// strategies are meant to differ, and the fan-out only earns its containers
// while they do: a model shown "here is a fix that was accepted" will write
// that fix three times over, and ranking is left with nothing to choose
// between. What is useful here is the taste — how large a change this team
// tolerates, what it calls out of scope — not the patch.
func writePrecedents(b *strings.Builder, precedents []string) {
	if len(precedents) == 0 {
		return
	}

	b.WriteString("\n\n## How this team has judged fixes before\n\n")
	b.WriteString("Decisions made by the engineers who will review you, on earlier " +
		"incidents. Read them for what this team accepts and rejects: how large a " +
		"change it tolerates, what it treats as out of scope, which trade-offs it " +
		"has already argued out. Respect that as a constraint on what you write.\n\n" +
		"Do NOT copy the fix that was chosen. It was written for a different fault, " +
		"and the strategy you were given below still decides what you produce here.\n")

	for i, p := range precedents {
		fmt.Fprintf(b, "\n--- past decision %d ---\n%s\n", i+1, p)
	}
}

func Repair(ctx context.Context, model llms.Model, in ProposalInput, previous Proposal, f Failure, s Strategy) (Proposal, error) {
	if model == nil {
		return Proposal{}, errors.New("remediation: no model configured")
	}

	// the files as the model left them, not as they were originally: it has to
	// correct its own work, and it cannot do that against the old contents
	repairInput := in
	repairInput.Sources = sourcedFrom(previous.Edits)

	var b strings.Builder
	b.WriteString(buildProposalPrompt(repairInput))

	fmt.Fprintf(&b, "\n\n## What you changed last time\n\n%s\n",
		fallback(previous.Summary, "(no summary given)"))

	fmt.Fprintf(&b, "\n## How the %s stage rejected it\n\n```\n%s\n```\n",
		f.Stage, clip(strings.TrimSpace(f.Log), maxFailureChars))

	resp, err := model.GenerateContent(ctx,
		[]llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, repairPrompt+"\n\nThe strategy you are working to:\n"+s.Instruction),
			llms.TextParts(llms.ChatMessageTypeHuman, b.String()),
		},
		llms.WithMaxTokens(proposalBudget),
		// lower than the original attempt whatever the strategy asked for: this
		// is a correction against a known-good error message, not exploration
		llms.WithTemperature(0.0),
	)
	if err != nil {
		return Proposal{}, fmt.Errorf("remediation: repair (%s): %w", s.Name, err)
	}
	content, err := choiceContent(resp, "repair ("+s.Name+")")
	if err != nil {
		return Proposal{}, err
	}
	return ParseProposal(content)
}

// how much of a failure to quote back. A Go compiler error is three lines; a
// test suite having a bad day is not, and the useful part is at the top.
const maxFailureChars = 6000

// present the model's own edits as the current state of the files
func sourcedFrom(edits []FileEdit) []Sourced {
	out := make([]Sourced, 0, len(edits))
	for _, e := range edits {
		out = append(out, Sourced{Path: e.Path, Contents: e.Contents})
	}
	return out
}

// ParseProposal reads a model's answer back into a proposal.
//
// A missing block is not an error: "no change" is a legitimate answer, and the
// caller distinguishes an empty proposal from a failed one.
func ParseProposal(answer string) (Proposal, error) {
	var (
		p       Proposal
		current *FileEdit
		lines   []string
	)

	for line := range strings.SplitSeq(answer, "\n") {
		trimmed := strings.TrimRight(line, "\r")

		if strings.TrimSpace(trimmed) == fileEnd {
			if current != nil {
				current.Contents = strings.Join(lines, "\n")
				p.Edits = append(p.Edits, *current)
			}
			current = nil
			lines = nil
			continue
		}

		if name, ok := strings.CutPrefix(strings.TrimSpace(trimmed), fileMarker); ok {
			// a block left unterminated by a model that ran out of budget is
			// half a file, and writing half a file deletes the rest of it
			current = &FileEdit{Path: strings.TrimSpace(name)}
			lines = nil
			continue
		}

		if current != nil {
			lines = append(lines, trimmed)
			continue
		}

		if rest, ok := cutPrefixFold(trimmed, "SUMMARY:"); ok {
			p.Summary = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := cutPrefixFold(trimmed, "RATIONALE:"); ok {
			p.Rationale = strings.TrimSpace(rest)
			continue
		}
		// prose after RATIONALE: keeps flowing into it, since models wrap
		if p.Rationale != "" && strings.TrimSpace(trimmed) != "" && len(p.Edits) == 0 {
			p.Rationale += " " + strings.TrimSpace(trimmed)
		}
	}

	if current != nil {
		return p, fmt.Errorf("remediation: file block for %q was never closed; "+
			"the model most likely ran out of tokens mid-file", current.Path)
	}

	if err := validateEdits(p.Edits); err != nil {
		return p, err
	}

	p.Rationale = strings.TrimSpace(p.Rationale)
	return p, nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < len(prefix) || !strings.EqualFold(trimmed[:len(prefix)], prefix) {
		return "", false
	}
	return trimmed[len(prefix):], true
}

// every reason an edit cannot be written, checked before a container is spent
func validateEdits(edits []FileEdit) error {
	seen := make(map[string]bool, len(edits))

	for i, e := range edits {
		clean, err := repoPath(e.Path)
		if err != nil {
			return fmt.Errorf("remediation: edit %d: %w", i+1, err)
		}
		if seen[clean] {
			return fmt.Errorf("remediation: the model wrote %s twice; "+
				"the later block would silently win", clean)
		}
		seen[clean] = true

		// the contents are written with a heredoc, so a line equal to the
		// delimiter would end the file early and leave the rest as shell
		for line := range strings.SplitSeq(e.Contents, "\n") {
			if strings.TrimSpace(line) == patchDelimiter {
				return fmt.Errorf("remediation: %s contains the heredoc delimiter", clean)
			}
		}

		edits[i].Path = clean
	}
	return nil
}

// a repository-relative path that cannot escape the checkout
func repoPath(p string) (string, error) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", errors.New("path contains a null byte")
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("path %q is absolute; paths are relative to the repository root", p)
	}

	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the repository", p)
	}
	if strings.HasPrefix(clean, ".git/") {
		return "", fmt.Errorf("path %q is inside .git", p)
	}
	return clean, nil
}

// ApplyCommand is the harness stage: it writes the proposed files into the
// checkout. Absolute paths throughout, because BuildScript has already changed
// into the service directory by this point and a `cd` here would leak into the
// build and test stages that follow.
func ApplyCommand(edits []FileEdit) (string, error) {
	if len(edits) == 0 {
		return "", errors.New("remediation: proposal changes no files")
	}
	if err := validateEdits(edits); err != nil {
		return "", err
	}

	var b strings.Builder
	targets := make([]string, 0, len(edits))

	for _, e := range edits {
		target := "/workspace/" + e.Path
		targets = append(targets, shellQuote(target))

		fmt.Fprintf(&b, "mkdir -p %s || exit 90\n", shellQuote(path.Dir(target)))
		// quoted delimiter: no expansion, so a file containing $VAR or a
		// backtick is written exactly as the model wrote it
		fmt.Fprintf(&b, "cat > %s <<'%s'\n", shellQuote(target), patchDelimiter)
		b.WriteString(e.Contents)
		if !strings.HasSuffix(e.Contents, "\n") {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s\n", patchDelimiter)
	}

	// The stage's exit code is the last command's, so this loop is what makes
	// "applied" mean anything: without it a failed write would still be
	// followed by a successful echo, and the candidate would be verified as
	// though the change were there.
	fmt.Fprintf(&b, "for f in %s; do\n", strings.Join(targets, " "))
	b.WriteString("  [ -e \"$f\" ] || { echo \"could not write $f\"; exit 91; }\n")
	b.WriteString("  echo \"wrote $f\"\n")
	b.WriteString("done")

	return b.String(), nil
}
