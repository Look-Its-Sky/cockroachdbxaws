package remediation

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v75/github"
)

func apiError(status int, header http.Header) error {
	return &github.ErrorResponse{
		Response: &http.Response{StatusCode: status, Header: header},
		Message:  "Resource not accessible by personal access token",
	}
}

// the raw message names no permission and the repo's permissions block reports
// the account's access, so it sends people to our source, not their settings
func TestOpenTranslatesAForbiddenIntoSomethingActionable(t *testing.T) {
	header := http.Header{}
	header.Set("X-Accepted-GitHub-Permissions", "contents=write")

	err := permissionAwareError("create a commit", checkoutRepo(), apiError(http.StatusForbidden, header))

	if !errors.Is(err, ErrTokenCannotWrite) {
		t.Fatalf("a 403 is not reported as a token problem: %v", err)
	}

	msg := err.Error()
	for _, want := range []string{"contents=write", tokenSettingsURL, "create a commit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q: %s", want, msg)
		}
	}
}

// a fine-grained token that cannot see a repository gets a 404, not a 403, so
// the advice has to be about selection rather than about permissions
func TestOpenDistinguishesAnUnselectedRepository(t *testing.T) {
	err := permissionAwareError("read main", checkoutRepo(), apiError(http.StatusNotFound, http.Header{}))

	if !errors.Is(err, ErrTokenCannotWrite) {
		t.Fatalf("a 404 is not reported as a token problem: %v", err)
	}
	if !strings.Contains(err.Error(), "selected") {
		t.Errorf("the message does not mention repository selection: %s", err)
	}
}

// GitHub does not always send the header, and a message naming no permission at
// all is worse than one naming the two that are needed
func TestOpenFallsBackWhenGitHubNamesNoPermission(t *testing.T) {
	err := permissionAwareError("build a tree", checkoutRepo(), apiError(http.StatusForbidden, http.Header{}))

	if !strings.Contains(err.Error(), "contents=write") {
		t.Errorf("no permission was named: %s", err)
	}
}

// anything that is not a permission problem must not be dressed up as one
func TestOpenLeavesOtherFailuresAlone(t *testing.T) {
	err := permissionAwareError("open a pull request", checkoutRepo(), errors.New("connection reset"))

	if errors.Is(err, ErrTokenCannotWrite) {
		t.Errorf("a transport failure was reported as a token problem: %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the underlying cause was lost: %v", err)
	}
}

func TestOpenRefusesACandidateWithNoStoredContents(t *testing.T) {
	p := NewPublisher("ghp_notreal")

	// the diff is for a human to read; the PR is built from Edits, and a
	// candidate loaded without them cannot produce a commit
	_, err := p.Open(t.Context(), checkoutRepo(), Candidate{ID: "c1", Diff: "diff --git a/a b/a"})
	if err == nil {
		t.Fatal("a candidate with no file contents was accepted")
	}
	if !strings.Contains(err.Error(), "no file contents") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestPublisherWithoutATokenSaysSo(t *testing.T) {
	p := NewPublisher("")

	if p.Configured() {
		t.Error("an empty token reports as configured")
	}
	if _, err := p.Open(t.Context(), checkoutRepo(), Candidate{Edits: []FileEdit{{Path: "a", Contents: "b"}}}); !errors.Is(err, ErrNoToken) {
		t.Errorf("err = %v, want ErrNoToken", err)
	}
}

// drafts are the safety property that makes this wireable to a button
func TestNewPublisherOpensDrafts(t *testing.T) {
	if !NewPublisher("t").Draft {
		t.Error("the publisher would open a ready-for-review pull request")
	}
}

func TestPRBodyNeverUpgradesABuildIntoPassingTests(t *testing.T) {
	body := PRBody(Candidate{
		InvestigationID: "inv-1",
		Strategy:        "minimal",
		Verification:    Verification{Applied: true, BuildRan: true, Built: true},
	})

	if strings.Contains(body, "tests pass") {
		t.Errorf("a build-only candidate claims passing tests in its PR body:\n%s", body)
	}
	if !strings.Contains(body, "no tests") {
		t.Errorf("the PR body does not say the service has no tests:\n%s", body)
	}
}
