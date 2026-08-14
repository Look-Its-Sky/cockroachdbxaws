package remediation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v75/github"
)

// ErrNoToken means no GitHub credential is configured, so a draft cannot open.
var ErrNoToken = errors.New("remediation: GITHUB_TOKEN is not set, so no pull request can be opened")

// Publisher opens draft pull requests for candidates an engineer has picked.
//
// It works through the Git data API rather than by pushing from the sandbox.
// The candidate's whole-file contents are already in hand, so a blob, a tree, a
// commit and a ref are four calls — and it means the token never has to enter a
// container that just ran code a model wrote.
type Publisher struct {
	Token string
	// Draft is what makes this safe to wire to a button. Always true in
	// practice; it is a field so a test can assert on it.
	Draft bool
}

func NewPublisher(token string) *Publisher {
	return &Publisher{Token: token, Draft: true}
}

func (p *Publisher) Configured() bool { return p != nil && strings.TrimSpace(p.Token) != "" }

// how long the whole sequence gets. Six sequential API calls against a public
// repository; anything slower is GitHub having a bad day.
const publishTimeout = 60 * time.Second

// Open creates a branch carrying the candidate's change and opens a draft PR
// against the repository's default branch. It returns the PR's URL.
func (p *Publisher) Open(ctx context.Context, repo Repository, c Candidate) (string, error) {
	if !p.Configured() {
		return "", ErrNoToken
	}
	if len(c.Edits) == 0 {
		return "", errors.New("remediation: this candidate has no file contents stored, so no commit can be built from it")
	}

	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	client := github.NewClient(nil).WithAuthToken(p.Token)
	owner, name := repo.Owner, repo.Repo
	base := fallback(repo.DefaultBranch, "main")

	baseRef, _, err := client.Git.GetRef(ctx, owner, name, "refs/heads/"+base)
	if err != nil {
		return "", permissionAwareError(fmt.Sprintf("read %s of %s", base, repo.Redacted()), repo, err)
	}
	baseSHA := baseRef.GetObject().GetSHA()

	baseCommit, _, err := client.Git.GetCommit(ctx, owner, name, baseSHA)
	if err != nil {
		return "", permissionAwareError("read the tip of "+base, repo, err)
	}

	entries := make([]*github.TreeEntry, 0, len(c.Edits))
	for _, e := range c.Edits {
		entries = append(entries, &github.TreeEntry{
			Path: github.Ptr(e.Path),
			// a regular non-executable file; the sandbox only ever writes these
			Mode:    github.Ptr("100644"),
			Type:    github.Ptr("blob"),
			Content: github.Ptr(e.Contents),
		})
	}

	tree, _, err := client.Git.CreateTree(ctx, owner, name, baseCommit.GetTree().GetSHA(), entries)
	if err != nil {
		return "", permissionAwareError("build a tree", repo, err)
	}

	commit, _, err := client.Git.CreateCommit(ctx, owner, name, github.Commit{
		Message: github.Ptr(commitMessage(c)),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: github.Ptr(baseSHA)}},
	}, nil)
	if err != nil {
		return "", permissionAwareError("create a commit", repo, err)
	}

	branch := branchName(c)
	if _, _, err := client.Git.CreateRef(ctx, owner, name, github.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: commit.GetSHA(),
	}); err != nil {
		return "", permissionAwareError("create branch "+branch, repo, err)
	}

	pr, _, err := client.PullRequests.Create(ctx, owner, name, &github.NewPullRequest{
		Title: github.Ptr(prTitle(c)),
		Head:  github.Ptr(branch),
		Base:  github.Ptr(base),
		Body:  github.Ptr(PRBody(c)),
		Draft: github.Ptr(p.Draft),
	})
	if err != nil {
		return "", permissionAwareError("open a pull request", repo, err)
	}

	return pr.GetHTMLURL(), nil
}

// ErrTokenCannotWrite means the credential is valid but read-only.
var ErrTokenCannotWrite = errors.New("remediation: GITHUB_TOKEN cannot write to this repository")

// where the permission actually lives, since this is not guessable from the error
const tokenSettingsURL = "https://github.com/settings/personal-access-tokens"

// turn GitHub's 403 into something the person reading it can act on.
//
// A fine-grained token that is missing a permission says only "Resource not
// accessible by personal access token", and the repository's own `permissions`
// block unhelpfully reports push: true — that is the *account's* access, not
// the token's grant. Surfacing the raw message sends whoever clicked the button
// to read our source instead of their token settings.
func permissionAwareError(what string, repo Repository, err error) error {
	var apiErr *github.ErrorResponse
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		switch apiErr.Response.StatusCode {
		case 403:
			// GitHub names the missing permission in a header, so quote it
			// rather than guessing which one it was
			needed := apiErr.Response.Header.Get("X-Accepted-GitHub-Permissions")
			if needed == "" {
				needed = "contents=write, pull_requests=write"
			}
			return fmt.Errorf("%w: could not %s. The token needs %q on %s. "+
				"Grant it at %s — note that the repository's own permissions block reports the "+
				"account's access, not the token's",
				ErrTokenCannotWrite, what, needed, repo.Redacted(), tokenSettingsURL)

		case 404:
			// a fine-grained token that cannot see a repository at all gets a
			// 404 rather than a 403, so this is selection rather than permission
			return fmt.Errorf("%w: could not %s. %s is not in the token's selected "+
				"repositories, or does not exist. Check both at %s",
				ErrTokenCannotWrite, what, repo.Redacted(), tokenSettingsURL)
		}
	}

	return fmt.Errorf("remediation: could not %s: %w", what, err)
}

// the candidate id is in the branch name so two candidates from the same
// investigation cannot collide, and so a stray branch is traceable
func branchName(c Candidate) string {
	return fmt.Sprintf("sre-agent/%s-%s", slug(fallback(c.ServiceID, "fix")), shortID(c.ID))
}

func prTitle(c Candidate) string {
	if s := strings.TrimSpace(c.Summary); s != "" {
		return "fix(" + fallback(c.ServiceID, "sre") + "): " + s
	}
	return "fix(" + fallback(c.ServiceID, "sre") + "): proposed remediation"
}

func commitMessage(c Candidate) string {
	var b strings.Builder

	b.WriteString(prTitle(c))
	if s := strings.TrimSpace(c.Rationale); s != "" {
		b.WriteString("\n\n")
		b.WriteString(s)
	}
	fmt.Fprintf(&b, "\n\nProposed by the SRE agent for investigation %s (strategy: %s).\n",
		c.InvestigationID, c.Strategy)
	return b.String()
}

// PRBody is what a reviewer reads first.
//
// It states what was actually verified in the words Verification.Summary uses,
// because the one thing this must never do is let "it compiles" reach a
// reviewer looking like "the tests pass".
func PRBody(c Candidate) string {
	var b strings.Builder

	b.WriteString("> Drafted by the SRE agent. Nothing here has been reviewed by a person yet.\n\n")

	if s := strings.TrimSpace(c.Rationale); s != "" {
		b.WriteString("## Why\n\n")
		b.WriteString(s)
		b.WriteString("\n\n")
	}

	b.WriteString("## What was verified\n\n")
	fmt.Fprintf(&b, "This change %s.\n\n", c.Verification.Summary())

	if len(c.Files) > 0 {
		b.WriteString("## Files changed\n\n")
		for _, f := range c.Files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Provenance\n\n")
	fmt.Fprintf(&b, "- Incident: `%s`\n", fallback(c.IncidentID, "unknown"))
	fmt.Fprintf(&b, "- Investigation: `%s`\n", c.InvestigationID)
	fmt.Fprintf(&b, "- Service: `%s`\n", fallback(c.ServiceID, "unknown"))
	fmt.Fprintf(&b, "- Strategy: `%s`\n", c.Strategy)

	return b.String()
}

// lowercase, hyphen-separated, safe in a git ref
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
