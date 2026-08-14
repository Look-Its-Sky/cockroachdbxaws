package remediation

import (
	"fmt"
	"strconv"
	"strings"
)

// Sections are delimited in the container's stdout with these markers. A prefix
// that no compiler, test runner or coding harness would emit by accident, so a
// build log mentioning "diff" cannot be mistaken for the diff itself.
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

// Section is one stage's output and exit status.
type Section struct {
	Stage    string
	Output   string
	ExitCode int
	// Ran is false when the stage was skipped, e.g. a service with no test
	// command. A skipped stage is not a passing one.
	Ran bool
}

func (s Section) Passed() bool { return s.Ran && s.ExitCode == 0 }

// BuildScript composes the whole job: clone the repository, let the harness
// change it, then install, build, test and print the diff.
//
// Every stage is wrapped in markers and its exit code recorded, and the script
// never aborts early — a failed build still has to reach the diff stage, since
// a candidate that does not compile is worth showing as such rather than losing.
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

	// Taken immediately after the harness and before anything is installed or
	// built, so the diff is what the model changed and nothing else. Run last
	// it would sweep in whatever `npm ci` and the build left lying around.
	//
	// From the repository root, so a change outside the service directory is
	// still captured; staged first, because `git diff` alone does not show a
	// file the model created, and creating one is something ApplyCommand
	// explicitly allows. Paths the repository ignores stay ignored, which is
	// the right answer for node_modules and the wrong one nowhere that matters.
	stage(&b, stageDiff, "git -C /workspace add -A && git -C /workspace --no-pager diff --cached")

	stageIf(&b, stageSetup, repo.SetupCommand)
	stageIf(&b, stageBuild, repo.BuildCommand)
	stageIf(&b, stageTest, repo.TestCommand)

	return b.String()
}

// how much history a sandbox gets. Enough to read recent deploys and to blame
// a change in context, without paying for years of a monorepo on every
// candidate. At this repository's rate — dependabot lands most days — 100
// commits is roughly a month.
//
// This is a context budget, not a correctness dependency: the commit an
// incident implicates is fetched explicitly by fetchCommitCommand, so a culprit
// older than the depth is still readable.
const CloneDepth = 100

// the whole clone stage: clone, reach the implicated commit, then report
// whether there is actually a checkout to work with.
//
// That last check is the point of this existing at all. The fetch fallbacks
// deliberately end in `|| echo`, so a commit that cannot be reached is a
// finding rather than a dead run — but a stage's exit code is its last
// command's, so without a final test a *total* clone failure still reports
// success. It did exactly that: node:22-alpine ships no git, and the clone
// stage came back exit 0 with "git: not found" as its output, which
// Runner.candidate reads as a good checkout.
func cloneStageCommand(repo Repository, cloneURL, sha string) string {
	return cloneCommand(repo, cloneURL) + "\n" +
		fetchCommitCommand(sha) + "\n" +
		"[ -d /workspace/.git ]"
}

// a bounded clone of one branch.
//
// Not --filter=blob:none: a blobless clone is smaller up front but resolves
// file contents lazily, so it needs the network for the whole life of the
// container and every `git show` of an old commit is a round trip. A depth
// clone is self-contained once it finishes, which is what lets the network be
// taken away before model-written code runs.
func cloneCommand(repo Repository, cloneURL string) string {
	return fmt.Sprintf(
		"git clone --depth %d --single-branch --branch %s %s /workspace",
		CloneDepth, shellQuote(repo.DefaultBranch), shellQuote(cloneURL))
}

// how much further back to reach when the implicated commit is abbreviated and
// the depth clone did not already contain it. Five times CloneDepth: enough to
// cover months of this repository, and still far short of cloning it whole.
const deepenBy = 5 * CloneDepth

// guarantee one commit is present regardless of the clone depth.
//
// A **full** object name can be fetched directly, so a culprit 500 commits back
// costs one extra object rather than 500. An abbreviated one cannot:
// `git fetch origin 0c6f0ae` fails with "couldn't find remote ref", because the
// protocol has no way to resolve a short name on the server. Verified against
// the fork on 2026-08-12, and it matters because the deploys table stores
// seven characters — so this is the common case, not the exotic one.
//
// For a short SHA the fallback is to deepen, and only when the clone did not
// already reach it, which for the seeded culprits it does.
//
// Deliberately tolerant of failure throughout: a commit that cannot be reached
// is a finding for triage to report, not a reason to abandon the run before it
// has looked at anything.
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

// ParseSections pulls the delimited stages back out of a container's output.
//
// Anything printed outside a marked stage is ignored: shells are chatty, and a
// warning on stderr must not end up inside a diff.
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
