package remediation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tmc/langchaingo/llms"
)

// answers one question before any fix is attempted: is the fault actually
// present in the code as it stands? Asking costs one cheap model call; fanning
// out N containers to fix something already fixed costs real money.
type TriageStatus string

const (
	// the fault is present, and the evidence says where
	TriageConfirmed TriageStatus = "confirmed"
	// the code does not contain the described fault; nothing to fix
	TriageNotPresent TriageStatus = "not_present"
	// not enough evidence either way
	TriageInconclusive TriageStatus = "inconclusive"
	// the implicated commit is not in the repository at all
	TriageCommitMissing TriageStatus = "commit_missing"
)

// the gate's finding
type Triage struct {
	Status TriageStatus `json:"status"`
	// the commit the verdict implicated, as resolved in the repository
	Commit       string `json:"commit,omitempty"`
	CommitExists bool   `json:"commit_exists"`
	// Files the fault is believed to live in, from the model's reading
	Files []string `json:"files,omitempty"`
	// the reasoning, shown to the engineer next to the candidates
	Evidence   string  `json:"evidence,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// what the sandbox actually gathered, kept for the trace
	CommitSubject string `json:"commit_subject,omitempty"`
}

// whether it is worth spending containers; inconclusive proceeds, since the
// gate stops obvious waste rather than overruling an undecided small model
func (t Triage) ShouldRemediate() bool {
	return t.Status == TriageConfirmed || t.Status == TriageInconclusive
}

// the extra stages triage adds on top of a clone
const (
	stageCommit    = "commit"
	stageCommitLog = "commit_log"
	stageFiles     = "files"
	stageSource    = "source"
)

// caps on what is fed to the model. A dependency bump can touch a lock file
// with a hundred thousand lines, and none of it bears on the question.
const (
	maxPatchLines  = 400
	maxSourceLines = 600
)

// gathers evidence about an implicated commit, and only reads
func BuildTriageScript(repo Repository, cloneURL, sha string) string {
	var b strings.Builder

	b.WriteString("set -u\n")
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")

	// the implicated commit is very often older than the clone depth — the
	// seeded checkout culprit is 67 commits back — so it is fetched by name
	stage(&b, stageClone, cloneStageCommand(repo, cloneURL, sha))
	b.WriteString("cd /workspace || exit 97\n")
	b.WriteString("git config --global --add safe.directory /workspace\n")

	quoted := shellQuote(sha)

	// does the commit exist at all? A fabricated SHA has to be distinguishable
	// from a real one whose fault has since been fixed.
	stage(&b, stageCommit, fmt.Sprintf("git cat-file -e %s^{commit} && git rev-parse %s", quoted, quoted))
	stage(&b, stageCommitLog, fmt.Sprintf(
		"git --no-pager show --stat --format='%%H%%n%%an%%n%%aI%%n%%s' %s | head -n 40", quoted))

	// the change itself, capped
	stage(&b, stageFiles, fmt.Sprintf(
		"git --no-pager show --unified=3 --format='' %s | head -n %d", quoted, maxPatchLines))

	// and the code as it stands now, which is what the fault must be present in
	dir := shellQuote(repo.WorkingDir())
	stage(&b, stageSource, fmt.Sprintf(
		"git --no-pager show --name-only --format='' %s | grep -v '^$' | head -n 20 | "+
			"while read -r f; do echo \"----- $f\"; head -n %d %s 2>/dev/null || echo '(file no longer exists)'; done",
		quoted, maxSourceLines, "/workspace/\"$f\""))

	// list the service directory too, so a model can see what it is working with
	stage(&b, "listing", fmt.Sprintf("ls -1 %s", dir))

	return b.String()
}

const triagePrompt = `You are triaging a production incident before any fix is attempted.

Below is what an incident reported, the commit that was implicated, that commit's
diff, and the current contents of the files it touched.

Answer one question: is the fault the incident describes actually present in the
code as it stands now?

Be strict about this. The commit may already be a fix rather than a cause. The
fault may have been corrected since. Reporting a fault that is not there sends a
coding agent to invent a change to working code, which is worse than saying so.

Reply with a short paragraph of evidence citing specific files and lines, then a
fenced JSON block and nothing after it:

` + "```json" + `
{"status": "confirmed", "files": ["src/checkout/main.go"], "confidence": 0.7}
` + "```" + `

status is one of:
  confirmed    - the described fault is present in the current code
  not_present  - the current code does not contain it (already fixed, or the
                 commit was a fix rather than a cause)
  inconclusive - the evidence does not settle it`

// Judge asks a model whether the gathered evidence shows the fault is real.
func Judge(ctx context.Context, model llms.Model, incidentSummary, verdictAnswer string, sections map[string]Section) (Triage, error) {
	commit := sections[stageCommit]

	// a commit that is not in the repository settles it without a model call
	if !commit.Passed() {
		return Triage{
			Status:       TriageCommitMissing,
			CommitExists: false,
			Evidence: "The implicated commit is not present in the repository, so nothing can be " +
				"read from it. Either the deploy record names a commit from another repository, " +
				"or it was never pushed.",
		}, nil
	}

	resolved, subject := commitIdentity(sections)

	prompt := buildTriagePrompt(incidentSummary, verdictAnswer, sections)
	resp, err := model.GenerateContent(ctx,
		[]llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, triagePrompt),
			llms.TextParts(llms.ChatMessageTypeHuman, prompt),
		},
		llms.WithMaxTokens(1500),
	)
	if err != nil {
		return Triage{}, fmt.Errorf("remediation: triage: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Triage{}, fmt.Errorf("remediation: triage: model returned no choices")
	}

	triage := parseTriage(strings.TrimSpace(resp.Choices[0].Content))
	triage.Commit = resolved
	triage.CommitSubject = subject
	triage.CommitExists = true
	return triage, nil
}

// resolved SHA and subject line out of the gathered sections
func commitIdentity(sections map[string]Section) (sha, subject string) {
	if lines := nonEmptyLines(sections[stageCommit].Output); len(lines) > 0 {
		sha = lines[len(lines)-1]
	}

	// `show --format='%H%n%an%n%aI%n%s'` puts the subject on the fourth line
	if lines := nonEmptyLines(sections[stageCommitLog].Output); len(lines) >= 4 {
		subject = lines[3]
	}
	return sha, subject
}

func buildTriagePrompt(incidentSummary, verdictAnswer string, sections map[string]Section) string {
	var b strings.Builder

	b.WriteString("## What the incident reported\n\n")
	b.WriteString(incidentSummary)
	b.WriteString("\n\n## What the investigation concluded\n\n")
	b.WriteString(verdictAnswer)

	writeSection(&b, "## The implicated commit\n\n", sections[stageCommitLog])
	writeSection(&b, "## Its diff\n\n", sections[stageFiles])
	writeSection(&b, "## Those files as they stand now\n\n", sections[stageSource])

	return b.String()
}

func writeSection(b *strings.Builder, heading string, s Section) {
	if strings.TrimSpace(s.Output) == "" {
		return
	}
	b.WriteString("\n\n")
	b.WriteString(heading)
	b.WriteString("```\n")
	b.WriteString(s.Output)
	b.WriteString("\n```")
}

var triageBlockRE = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// the model's finding, falling back to prose because a small model states a
// clear conclusion and then forgets the block it was asked for
func parseTriage(answer string) Triage {
	if matches := triageBlockRE.FindAllStringSubmatch(answer, -1); len(matches) > 0 {
		var parsed struct {
			Status     string   `json:"status"`
			Files      []string `json:"files"`
			Confidence float64  `json:"confidence"`
		}
		raw := matches[len(matches)-1][1]
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			if status := normaliseTriage(parsed.Status); status != "" {
				return Triage{
					Status:     status,
					Files:      parsed.Files,
					Confidence: clampConfidence(parsed.Confidence),
					Evidence:   strings.TrimSpace(triageBlockRE.ReplaceAllString(answer, "")),
				}
			}
		}
	}

	return Triage{
		Status:   triageFromProse(answer),
		Evidence: answer,
	}
}

func triageFromProse(answer string) TriageStatus {
	lower := strings.ToLower(answer)

	// look for the negative first: "not present" contains "present"
	for _, phrase := range []string{"not_present", "not present", "already fixed", "no longer present", "is a fix"} {
		if strings.Contains(lower, phrase) {
			return TriageNotPresent
		}
	}
	if strings.Contains(lower, "confirmed") || strings.Contains(lower, "is present") {
		return TriageConfirmed
	}
	return TriageInconclusive
}

func normaliseTriage(raw string) TriageStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "confirmed", "present", "reproduced":
		return TriageConfirmed
	case "not_present", "not present", "absent", "fixed":
		return TriageNotPresent
	case "inconclusive", "unknown", "unclear":
		return TriageInconclusive
	default:
		return ""
	}
}

func clampConfidence(c float64) float64 {
	switch {
	case c < 0:
		return 0
	case c > 1:
		return 1
	default:
		return c
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
