package remediation

import (
	"strings"
	"testing"
)

func checkoutRepo() Repository {
	return Repository{
		ServiceID: "checkout", Owner: "Look-Its-Sky",
		Repo: "opentelemetry-demo-auto-sre-test", DefaultBranch: "main",
		Subdirectory: "src/checkout", RuntimeImage: "golang:1.24",
		BuildCommand: "go build ./...", TestCommand: "go test ./...",
	}
}

func paymentRepo() Repository {
	return Repository{
		ServiceID: "payment", Owner: "Look-Its-Sky",
		Repo: "opentelemetry-demo-auto-sre-test", DefaultBranch: "main",
		Subdirectory: "src/payment", RuntimeImage: "node:22-alpine",
		SetupCommand: "npm ci --omit=dev", BuildCommand: "node --check index.js",
	}
}

func TestHasTests(t *testing.T) {
	// the distinction the whole verification story rests on: payment has no
	// tests, and a candidate for it must never be described as tested
	if !checkoutRepo().HasTests() {
		t.Error("checkout should report having tests")
	}
	if paymentRepo().HasTests() {
		t.Error("payment has no test command and must not claim tests")
	}
	if (Repository{TestCommand: "   "}).HasTests() {
		t.Error("whitespace is not a test command")
	}
}

func TestWorkingDir(t *testing.T) {
	tests := map[string]string{
		"src/checkout":  "/workspace/src/checkout",
		"/src/payment/": "/workspace/src/payment",
		"":              "/workspace",
	}

	for sub, want := range tests {
		got := Repository{Subdirectory: sub}.WorkingDir()
		if got != want {
			t.Errorf("WorkingDir(%q) = %q, want %q", sub, got, want)
		}
	}
}

func TestCloneURL(t *testing.T) {
	repo := checkoutRepo()

	// a public repository is cloned with no credential at all
	if got := repo.CloneURL(""); got != "https://github.com/Look-Its-Sky/opentelemetry-demo-auto-sre-test.git" {
		t.Errorf("unauthenticated clone URL = %q", got)
	}

	withToken := repo.CloneURL("github_pat_secret")
	if !strings.Contains(withToken, "x-access-token:github_pat_secret@github.com") {
		t.Errorf("authenticated clone URL = %q", withToken)
	}

	// and the form used for logging must never carry it
	if strings.Contains(repo.Redacted(), "secret") || strings.Contains(repo.Redacted(), "@") {
		t.Errorf("Redacted() leaks credentials: %q", repo.Redacted())
	}
}
