// Command sandboxcheck runs the container half of remediation end to end with no model involved; exits non-zero if a stage that should have run did not.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/remediation"
	"agent_space/utils"
)

// a real file rather than a no-op, so the harness stage, the diff stage and the
// working directory are all exercised and an empty diff means something is wrong
const markerPath = "SRE_SANDBOX_CHECK.md"

const markerContents = `# sandbox check

Written by cmd/sandboxcheck to prove the harness stage can modify the checkout.
This file never leaves the container.
`

func main() {
	var (
		service = flag.String("service", "checkout", "service id to look up in service_repositories")
		sha     = flag.String("sha", "0c6f0ae", "commit to fetch explicitly, regardless of clone depth")
		mode    = flag.String("mode", "build", "build, triage or inspect")
		binary  = flag.String("binary", "docker", "container CLI: docker, podman or nerdctl")
		// so a runtime image can be proven before it is written into the
		// mapping, which is the order those two things should happen in
		image   = flag.String("image", "", "override the service's runtime_image")
		files   = flag.String("files", "", "comma-separated paths for inspect mode")
		timeout = flag.Duration("timeout", 15*time.Minute, "ceiling for the container")
		verbose = flag.Bool("v", false, "print every stage's output, not just failing ones")
	)
	flag.Parse()

	utils.LoadConfig()

	ctx := context.Background()
	sandbox := &remediation.ContainerSandbox{Binary: *binary, NamePrefix: "sandboxcheck"}

	// checked first: a missing runtime is the likeliest reason this command is
	// being run at all, and it should say so before spending a database round trip
	if err := sandbox.Available(ctx); err != nil {
		fail("%v\n\nsandboxcheck needs a working container runtime. Try -binary podman.", err)
	}

	repo := lookup(ctx, *service)
	if *image != "" {
		fmt.Printf("image:   %s (overriding %s from the mapping)\n", *image, repo.RuntimeImage)
		repo.RuntimeImage = *image
	}

	fmt.Printf("%s -> %s/%s (%s)\n", repo.ServiceID, repo.Redacted(), repo.Subdirectory, repo.RuntimeImage)
	fmt.Printf("build: %s\ntest:  %s\n\n", or(repo.BuildCommand, "(none)"), or(repo.TestCommand, "(none declared)"))

	script, expected := buildScript(*mode, repo, *sha, *files)

	fmt.Printf("running %s in %s (ceiling %s)...\n", *mode, repo.RuntimeImage, *timeout)
	start := time.Now()

	run, err := sandbox.Run(ctx, remediation.Spec{
		Image:   repo.RuntimeImage,
		Script:  script,
		Timeout: *timeout,
	})
	if err != nil {
		fail("container: %v", err)
	}

	fmt.Printf("exit %d after %s%s\n\n", run.ExitCode, run.Duration.Round(time.Second), timedOutNote(run.TimedOut))

	sections := remediation.ParseSections(run.Output)
	if len(sections) == 0 {
		fmt.Println("no delimited stages came back. Raw output follows:")
		fmt.Println(clip(run.Output, 4000))
		os.Exit(1)
	}

	report(sections, expected, *verbose)

	if failed := missing(sections, expected); len(failed) > 0 {
		fmt.Printf("\n%d stage(s) did not run or did not pass: %s\n", len(failed), strings.Join(failed, ", "))
		fmt.Println("The container plumbing is not ready for remediation.")
		os.Exit(1)
	}

	fmt.Printf("\n%s ok in %s. Clone, SHA fetch, stage markers and parsing all work.\n",
		*mode, time.Since(start).Round(time.Second))
}

// the script for a mode, and the stages that must have passed for it to count
func buildScript(mode string, repo remediation.Repository, sha, files string) (string, []string) {
	switch mode {
	case "build":
		apply, err := remediation.ApplyCommand([]remediation.FileEdit{
			{Path: markerPath, Contents: markerContents},
		})
		if err != nil {
			fail("compose the apply command: %v", err)
		}

		// setup, build and test are only expected when the service declares
		// them: a Node service with no tests must not read as a failure here
		expected := []string{"clone", "harness", "diff"}
		if repo.SetupCommand != "" {
			// the one command in the mapping that has to reach a registry, so
			// the one most likely to fail in a way that must not pass silently
			expected = append(expected, "setup")
		}
		if repo.BuildCommand != "" {
			expected = append(expected, "build")
		}
		if repo.HasTests() {
			expected = append(expected, "test")
		}
		return remediation.BuildScript(repo, repo.CloneURL(os.Getenv("GITHUB_TOKEN")), sha, apply), expected

	case "triage":
		return remediation.BuildTriageScript(repo, repo.CloneURL(os.Getenv("GITHUB_TOKEN")), sha),
			[]string{"clone", "commit", "commit_log", "files"}

	case "inspect":
		wanted := split(files)
		if len(wanted) == 0 {
			fail("inspect mode needs -files, e.g. -files %s/main.go", repo.Subdirectory)
		}
		return remediation.BuildInspectScript(repo, repo.CloneURL(os.Getenv("GITHUB_TOKEN")), sha, wanted),
			[]string{"clone", "tree", "sources"}

	default:
		fail("unknown mode %q; use build, triage or inspect", mode)
		return "", nil
	}
}

func report(sections map[string]remediation.Section, expected []string, verbose bool) {
	fmt.Println("stage        exit  bytes")
	fmt.Println("-------------------------")

	for _, name := range expected {
		s, ok := sections[name]
		if !ok {
			fmt.Printf("%-12s   --  did not run\n", name)
			continue
		}
		fmt.Printf("%-12s %4d  %d\n", name, s.ExitCode, len(s.Output))
	}

	for name, s := range sections {
		if verbose || !s.Passed() {
			fmt.Printf("\n--- %s (exit %d) ---\n%s\n", name, s.ExitCode, clip(s.Output, 3000))
		}
	}
}

// the expected stages that did not run or did not pass
func missing(sections map[string]remediation.Section, expected []string) []string {
	var out []string
	for _, name := range expected {
		if !sections[name].Passed() {
			out = append(out, name)
		}
	}
	return out
}

// the repository mapping, read from the same table the worker reads
func lookup(ctx context.Context, service string) remediation.Repository {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		fail("DATABASE_URL is not set; sandboxcheck reads the mapping from service_repositories.")
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fail("connect to %s: %v", utils.RedactURL(connStr), err)
	}
	defer pool.Close()

	repos, err := remediation.NewRepositories(ctx, pool)
	if err != nil {
		fail("%v", err)
	}

	repo, err := repos.Get(ctx, service)
	if err != nil {
		fail("%v\n\nApply the mapping with `go run ./cmd/seed -file scripts/repos.sql`.", err)
	}
	return repo
}

func split(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func timedOutNote(timedOut bool) string {
	if timedOut {
		return " (timed out)"
	}
	return ""
}

func or(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sandboxcheck: "+format+"\n", args...)
	os.Exit(2)
}
