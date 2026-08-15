package remediation

import (
	"fmt"
	"strconv"
	"strings"
)

// stage markers in the container's stdout, prefixed so a build log mentioning
// "diff" cannot be mistaken for the diff itself
const (
	sectionBegin = "__SRE_BEGIN__ "
	sectionEnd   = "__SRE_END__ "
)

// the stages a candidate run goes through, in order
const (
	stageClone   = "clone"
	stageHarness = "harness"
	stageSetup   = "setup"
	stageBuild   = "build"
	stageTest    = "test"
	stageDiff    = "diff"
)

// one stage's output and exit status
type Section struct {
	Stage    string
	Output   string
	ExitCode int
	// false when the stage was skipped, e.g. a service with no test
	// command. A skipped stage is not a passing one.
	Ran bool
}

func (s Section) Passed() bool { return s.Ran && s.ExitCode == 0 }

// the whole job: clone, let the harness change it, then install, build, test
// and print the diff. Never aborts early, so a failed build still reaches the
// diff stage and a candidate that does not compile is shown rather than lost.
func BuildScript(repo Repository, cloneURL, sha, harnessCommand string) string {
	var b strings.Builder

	// not `set -e`: a non-zero build must not skip the diff
	b.WriteString("set -u\n")
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")

	stage(&b, stageClone, cloneStageCommand(repo, cloneURL, sha))

	b.WriteString(fmt.Sprintf("cd %s || exit 97\n", shellQuote(repo.WorkingDir())))

	// identity is required before anything can be committed or diffed cleanly
	b.WriteString("git config --global user.email sre-agent@localhost\n")
	b.WriteString("git config --global user.name 'SRE Agent'\n")
	b.WriteString("git config --global --add safe.directory /workspace\n")

	stage(&b, stageHarness, harnessCommand)

	// taken before anything is installed, so the diff is what the model changed
	// and not what the build left lying around. From the repository root and
	// staged first, since `git diff` alone does not show a file it created.
	stage(&b, stageDiff, "git -C /workspace add -A && git -C /workspace --no-pager diff --cached")

	stageIf(&b, stageSetup, repo.SetupCommand)
	stageIf(&b, stageBuild, repo.BuildCommand)
	stageIf(&b, stageTest, repo.TestCommand)

	return b.String()
}

// how much history a sandbox gets, roughly a month at this repo's rate. A
// context budget rather than a correctness one: fetchCommitCommand reaches an
// implicated commit explicitly, so an older culprit is still readable.
const CloneDepth = 100

// clone, reach the implicated commit, then check there is a checkout at all.
// That last test is the point: the fallbacks end in `|| echo` and a stage exits
// with its last command, so node:22-alpine reported success with no git.
func cloneStageCommand(repo Repository, cloneURL, sha string) string {
	return cloneCommand(repo, cloneURL) + "\n" +
		fetchCommitCommand(sha) + "\n" +
		"[ -d /workspace/.git ]"
}

// a bounded clone of one branch. Not --filter=blob:none, which resolves blobs
// lazily and so needs the network for the container's whole life; a depth clone
// is self-contained, which is what lets the network be taken away.
func cloneCommand(repo Repository, cloneURL string) string {
	return fmt.Sprintf(
		"git clone --depth %d --single-branch --branch %s %s /workspace",
		CloneDepth, shellQuote(repo.DefaultBranch), shellQuote(cloneURL))
}

// how much further to reach for an abbreviated commit the clone missed; five
// times CloneDepth, months of this repo and still far short of the whole thing
const deepenBy = 5 * CloneDepth

// guarantee one commit is present regardless of clone depth, deepening when the
// SHA is abbreviated and cannot be fetched by name. Tolerant of failure: an
// unreachable commit is a finding for triage, not a dead run.
func fetchCommitCommand(sha string) string {
	quoted := shellQuote(sha)

	if isFullSHA(sha) {
		return fmt.Sprintf(
			"git -C /workspace fetch --depth 1 origin %s 2>&1 || echo 'could not fetch %s'",
			quoted, quoted)
	}

	return fmt.Sprintf(
		"git -C /workspace cat-file -e %s^{commit} 2>/dev/null || "+
			"git -C /workspace fetch --deepen %d 2>&1 || echo 'could not deepen to reach %s'",
		quoted, deepenBy, quoted)
}

// a full 40-character object name, which is the only thing a server will
// resolve in a fetch
func isFullSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// emit one delimited stage
func stage(b *strings.Builder, name, command string) {
	fmt.Fprintf(b, "echo '%s%s'\n", sectionBegin, name)
	fmt.Fprintf(b, "%s\n", command)
	fmt.Fprintf(b, "echo \"%s%s $?\"\n", sectionEnd, name)
}

// a stage that is skipped entirely when the service declares no command for it
func stageIf(b *strings.Builder, name, command string) {
	if strings.TrimSpace(command) == "" {
		return
	}
	stage(b, name, command)
}

// pulls the delimited stages back out of a container's output, ignoring
// anything outside a marker so a chatty shell cannot end up inside a diff
func ParseSections(output string) map[string]Section {
	sections := make(map[string]Section)

	var (
		current string
		body    []string
	)

	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimRight(line, "\r")

		if name, ok := strings.CutPrefix(trimmed, sectionBegin); ok {
			current = strings.TrimSpace(name)
			body = nil
			continue
		}

		if rest, ok := strings.CutPrefix(trimmed, sectionEnd); ok {
			name, code := splitEnd(rest)
			// an end marker for a stage that never began is corrupt output,
			// not a stage that ran
			if name == current && current != "" {
				sections[name] = Section{
					Stage:    name,
					Output:   strings.TrimRight(strings.Join(body, "\n"), "\n"),
					ExitCode: code,
					Ran:      true,
				}
			}
			current = ""
			body = nil
			continue
		}

		if current != "" {
			body = append(body, trimmed)
		}
	}

	return sections
}

// split "build 0" into its stage name and exit code
func splitEnd(rest string) (string, int) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0
	}
	if len(fields) == 1 {
		return fields[0], 0
	}

	code, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		// no parseable code means we cannot claim success
		return fields[0], 1
	}
	return fields[0], code
}

// single-quote a value for `sh -c`. The clone URL can carry a token, and the
// branch name comes from the database, so neither is pasted in raw.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
