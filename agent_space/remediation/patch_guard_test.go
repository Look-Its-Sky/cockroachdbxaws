package remediation

// the extensive battery for checkWholeFiles. patch_test.go holds the focused
// unit tests; this file is the adversarial table, the proof that a refused
// candidate spends no container, the repair-round checks, and the fuzzer.
//
// Drive it from scripts/test-truncation-guard.sh, or by hand:
//
//	go test ./remediation/ -run 'TestGuardRedTeam|TestTruncatedProposal|TestRepair' -v
//	go test ./remediation/ -run '^$' -fuzz FuzzCheckWholeFiles -fuzztime 30s

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// a realistic original: plausible code, no ellipses in any comment, and none
// of the stand-in phrases, so a faithful copy can never trip the marker
const guardOriginal = `package money

import (
	"fmt"
)

// Discount applies a percentage discount to the given amount.
func Discount(amount, percent int) int {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return amount - amount*percent/100
}

// Clamp keeps the amount inside the range the ledger accepts.
func Clamp(amount int) int {
	if amount < 0 {
		return 0
	}
	if amount > 1_000_000 {
		return 1_000_000
	}
	return amount
}

func String(amount int) string {
	return fmt.Sprintf("%d", amount)
}
`

const guardCartOriginal = `package checkout

// Cart holds the items in a checkout.
type Cart struct {
	Items []string
}

// Add puts an item in the cart.
func (c *Cart) Add(item string) {
	c.Items = append(c.Items, item)
}
`

func guardAnswer(blocks ...string) string {
	var b strings.Builder
	b.WriteString("SUMMARY: fix the fault\nRATIONALE: it was unbounded.\n\n")
	for _, blk := range blocks {
		b.WriteString(blk)
	}
	return b.String()
}

func guardBlock(path, contents string) string {
	return fileBegin + path + "\n" + contents + fileEnd + "\n"
}

// the first percent% of the bytes: a truncation of the file as it stands
func guardPrefix(s string, percent int) string {
	return s[:len(s)*percent/100]
}

// the first n whole lines: a truncation that still ends on a newline, so the
// block closes and the guard itself is what is under test
func guardLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if n >= len(lines) {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n"
}

// the whole attack surface, one row at a time. Every row goes through the real
// Propose path, so what is tested is the thing a model's answer actually hits.
// The accepted rows additionally run the real apply script in a temp dir and
// compare the written bytes: "accepted" must mean the shell reproduces the
// file exactly.
func TestGuardRedTeam(t *testing.T) {
	const moneyPath = "src/checkout/money.go"
	const cartPath = "src/checkout/cart.go"

	standard := func() []Sourced {
		return []Sourced{{Path: moneyPath, Contents: guardOriginal}}
	}
	standardTree := moneyPath + "\n" + cartPath + "\n"

	type attack struct {
		name    string
		sources []Sourced
		tree    string
		blocks  []string
		wantErr string // "" = must be accepted; otherwise a substring of the refusal
	}

	attacks := []attack{
		// ── the size ratio ──────────────────────────────────────────────
		{
			name:    "a truncation to 49 percent is refused",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardLines(guardOriginal, 17))}, // 251 of 508 bytes
			wantErr: "shrank",
		},
		{
			name:    "a truncation to 41 percent is refused",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardLines(guardOriginal, 13))}, // 209 of 508 bytes
			wantErr: "shrank",
		},
		{
			name:    "shrink to nothing",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, "")},
			wantErr: "shrank",
		},
		{
			// a real fix can delete more than half a file, and this guard will
			// refuse it anyway: a refused candidate costs a model call, a
			// silently halved file costs the incident
			name:    "a large legitimate deletion is refused too",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardLines(guardOriginal, 9))}, // 156 of 508 bytes
			wantErr: "shrank",
		},
		{
			name:    "a truncation to 67 percent is allowed",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardLines(guardOriginal, 19))}, // 343 of 508 bytes
			wantErr: "",
		},
		{
			// the boundary, as the block format sees it: the trailing newline is
			// lost when a file is read, on both the inspect side and the proposal
			// side, so a faithful half of a 520-byte file is 259 bytes — under
			// half, and refused
			name:    "a faithful half is refused by half a byte",
			sources: []Sourced{{Path: "a.go", Contents: strings.Repeat("func f() int { return 1 }\n", 20)}},
			tree:    "a.go\n",
			blocks:  []string{guardBlock("a.go", strings.Repeat("func f() int { return 1 }\n", 10))},
			wantErr: "shrank",
		},
		{
			name:    "a little over half is allowed",
			sources: []Sourced{{Path: "a.go", Contents: strings.Repeat("func f() int { return 1 }\n", 20)}},
			tree:    "a.go\n",
			blocks:  []string{guardBlock("a.go", strings.Repeat("func f() int { return 1 }\n", 11))},
			wantErr: "",
		},
		{
			// a model that runs out mid-line never reaches the guard at all: the
			// parser refuses the unclosed block first. Both layers refuse, and
			// neither writes the file
			name:    "a model that stops mid-line is refused by the parser",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardPrefix(guardOriginal, 55))},
			wantErr: "never closed",
		},

		// ── stand-in comments ───────────────────────────────────────────
		{
			name:    "stand-in at the end",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\n// ... rest unchanged\n")},
			wantErr: "stand-in",
		},
		{
			name:    "stand-in in the middle",
			sources: standard(), tree: standardTree,
			blocks: []string{guardBlock(moneyPath,
				guardPrefix(guardOriginal, 60)+"// ... rest of the file\n"+guardOriginal[len(guardOriginal)*60/100:])},
			wantErr: "stand-in",
		},
		{
			name:    "stand-in with a sharp ellipsis",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\n# the rest of the code …\n")},
			wantErr: "stand-in",
		},
		{
			name:    "hash comment stand-in",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\n# ... rest unchanged\n")},
			wantErr: "stand-in",
		},
		{
			name:    "sql dash stand-in",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"-- ... unchanged below --\n")},
			wantErr: "stand-in",
		},
		{
			name:    "html comment stand-in",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"<!-- ... rest of the file ... -->\n")},
			wantErr: "stand-in",
		},
		{
			name:    "star comment stand-in",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"* remaining code elided for brevity ...\n")},
			wantErr: "stand-in",
		},
		{
			name:    "uppercase stand-in",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"// REST UNCHANGED ...\n")},
			wantErr: "stand-in",
		},
		{
			name:    "crlf stand-in is caught",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\r\n// ... rest unchanged\r\n")},
			wantErr: "stand-in",
		},

		// ── the marker must not fire on plausible code ──────────────────
		{
			name:    "a phrase inside a string literal passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\nlog.Printf(\"the rest of the file...\")\n")},
			wantErr: "",
		},
		{
			name:    "a comment with the phrase but no ellipsis passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\n// cache the rest of the file\n")},
			wantErr: "",
		},
		{
			name:    "a comment with an ellipsis but no phrase passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\n// TODO: and so on...\n")},
			wantErr: "",
		},

		// ── the faithful copy ───────────────────────────────────────────
		{
			name:    "a faithful copy passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal)},
			wantErr: "",
		},
		{
			name:    "a faithful copy with additions passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\nfunc New() int { return 1 }\n")},
			wantErr: "",
		},
		{
			// a source file that already contains a stand-in line is
			// pathological, and a faithful copy of it is still refused: the
			// guard cannot tell the copy from the truncation, so it errs on
			// the side of the incident
			name:    "a faithful copy of a file that already carries a stand-in is refused",
			sources: []Sourced{{Path: moneyPath, Contents: guardOriginal + "\n// ... rest unchanged\n"}},
			tree:    standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal+"\n// ... rest unchanged\n")},
			wantErr: "stand-in",
		},

		// ── files the model was not shown ───────────────────────────────
		{
			name:    "a too large rewrite is refused",
			sources: append(standard(), Sourced{Path: "big.json", TooLarge: true}),
			tree:    "big.json\n" + standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal), guardBlock("big.json", "{}\n")},
			wantErr: "too large",
		},
		{
			name:    "an unseen existing file is refused",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock(cartPath, "package cart\n")},
			wantErr: "not shown",
		},
		{
			name:    "a genuinely new file passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock("src/checkout/new.go", "package checkout\n")},
			wantErr: "",
		},
		{
			name:    "a tree line with surrounding spaces",
			sources: standard(), tree: "  " + cartPath + "  \n",
			blocks:  []string{guardBlock(cartPath, "package cart\n")},
			wantErr: "not shown",
		},
		{
			// beyond the tree's 300-line cap a path is simply not in the set,
			// so the check degrades to the old behaviour rather than misfiring
			name:    "a file beyond the tree cap passes",
			sources: standard(), tree: standardTree,
			blocks:  []string{guardBlock("src/deep/file.go", "x\n")},
			wantErr: "",
		},

		// ── missing files ───────────────────────────────────────────────
		{
			name:    "recreating a missing file passes",
			sources: []Sourced{{Path: moneyPath, Missing: true}},
			tree:    standardTree,
			blocks:  []string{guardBlock(moneyPath, "package money\n")},
			wantErr: "",
		},
		{
			// a missing file has no original to lose, so a stand-in in the
			// recreation is garbage the build may catch, not deleted code
			name:    "a missing file with a stand-in passes",
			sources: []Sourced{{Path: moneyPath, Missing: true}},
			tree:    standardTree,
			blocks:  []string{guardBlock(moneyPath, "package money\n// ... rest unchanged\n")},
			wantErr: "",
		},

		// ── several edits at once ───────────────────────────────────────
		{
			name:    "the second of two edits is the bad one",
			sources: []Sourced{{Path: moneyPath, Contents: guardOriginal}, {Path: cartPath, Contents: guardCartOriginal}},
			tree:    standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal), guardBlock(cartPath, guardLines(guardCartOriginal, 7))}, // 95 of 197
			wantErr: "shrank",
		},
		{
			name:    "two faithful edits pass",
			sources: []Sourced{{Path: moneyPath, Contents: guardOriginal}, {Path: cartPath, Contents: guardCartOriginal}},
			tree:    standardTree,
			blocks:  []string{guardBlock(moneyPath, guardOriginal), guardBlock(cartPath, guardCartOriginal)},
			wantErr: "",
		},

		// ── tiny files ──────────────────────────────────────────────────
		{
			name:    "a one byte original unchanged passes",
			sources: []Sourced{{Path: "x", Contents: "x"}},
			tree:    "x\n",
			blocks:  []string{guardBlock("x", "x\n")}, // read back as the one byte
			wantErr: "",
		},
		{
			name:    "a one byte original erased is refused",
			sources: []Sourced{{Path: "x", Contents: "x"}},
			tree:    "x\n",
			blocks:  []string{guardBlock("x", "")},
			wantErr: "shrank",
		},
	}

	for _, a := range attacks {
		t.Run(a.name, func(t *testing.T) {
			p, err := Propose(context.Background(),
				&sequencedModel{answers: []string{guardAnswer(a.blocks...)}},
				ProposalInput{Sources: a.sources, Tree: a.tree},
				DefaultStrategies[0])

			if a.wantErr != "" {
				if err == nil {
					t.Fatalf("the attack landed: %+v", p)
				}
				if !strings.Contains(err.Error(), a.wantErr) {
					t.Fatalf("refused for the wrong reason:\ngot:  %v\nwant a reason containing %q", err, a.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("a complete file was refused: %v", err)
			}

			// the accepted side must survive the shell: run the real apply
			// script against a temp dir standing in for /workspace
			cmd, err := ApplyCommand(p.Edits)
			if err != nil {
				t.Fatalf("ApplyCommand: %v", err)
			}
			dir := t.TempDir()
			// a root-level file's parent is /workspace itself, which the first
			// replacement (the one with the trailing slash) does not reach
			script := strings.ReplaceAll(strings.ReplaceAll(cmd, "/workspace/", dir+"/"),
				"'/workspace'", "'"+dir+"'")
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the apply stage failed: %v\n%s", err, out)
			}
			for _, e := range p.Edits {
				want := e.Contents
				if !strings.HasSuffix(want, "\n") {
					want += "\n" // the apply stage ends every file on a newline
				}
				got, err := os.ReadFile(filepath.Join(dir, e.Path))
				if err != nil {
					t.Fatalf("read back %s: %v", e.Path, err)
				}
				if string(got) != want {
					t.Errorf("bytes did not survive the apply stage:\ngot:\n%q\nwant:\n%q", got, want)
				}
			}
		})
	}
}

// the point of the guard is to fail a candidate before it costs anything. A
// container is the unit of spending, so the assertion is about containers:
// a truncated proposal must leave triage and inspect as the only runs.
func TestTruncatedProposalSpendsNoContainer(t *testing.T) {
	const truncated = "SUMMARY: fix it\nRATIONALE: because\n" +
		fileBegin + "src/checkout/money.go\n" +
		"// ... rest unchanged\n" +
		fileEnd

	sandbox := &fakeSandbox{
		triage:    triageOutput(),
		inspect:   inspectOutput,
		candidate: passingCandidate, // must never be reached
	}

	runner := &Runner{
		Sandbox: sandbox,
		Model: &sequencedModel{answers: []string{
			// the first call is triage's judgement, the rest are proposals
			"The fault is present in money.go.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"],\"confidence\":0.8}\n```",
			truncated,
		}},
		Strategies: DefaultStrategies,
	}

	outcome := runWithRepo(t, runner, testRepo())

	if len(outcome.Candidates) != len(DefaultStrategies) {
		t.Fatalf("got %d candidates, want %d", len(outcome.Candidates), len(DefaultStrategies))
	}
	for _, c := range outcome.Candidates {
		if c.Status != CandidateFailed {
			t.Errorf("candidate %s: status = %s, want failed", c.Strategy, c.Status)
		}
		if !strings.Contains(c.Error, "stand-in") {
			t.Errorf("candidate %s: error = %q, want the stand-in reason", c.Strategy, c.Error)
		}
	}

	if got := len(sandbox.specs); got != 2 {
		t.Errorf("spent %d containers, want 2 (triage + inspect only)", got)
	}
}

// a repair round is checked against the files the model left last round, not
// the originals: it is correcting its own work
func TestRepairRejectsATruncatedRewrite(t *testing.T) {
	previous := Proposal{
		Summary:   "fix it",
		Rationale: "because",
		Edits: []FileEdit{
			{Path: "a.go", Contents: strings.Repeat("func f() int { return 1 }\n", 20)},
			{Path: "b.go", Contents: guardCartOriginal},
		},
	}

	answer := guardAnswer(guardBlock("a.go", strings.Repeat("func f() int { return 1 }\n", 9)))

	_, err := Repair(context.Background(),
		&sequencedModel{answers: []string{answer}},
		ProposalInput{Tree: "a.go\nb.go\n"},
		previous,
		Failure{Stage: "build", Log: "undefined: Discount"},
		DefaultStrategies[0])

	if err == nil {
		t.Fatal("a repair that truncated a file was accepted")
	}
	if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("wrong reason: %v", err)
	}
}

func TestRepairAcceptsACompleteRewrite(t *testing.T) {
	previous := Proposal{
		Summary:   "fix it",
		Rationale: "because",
		Edits:     []FileEdit{{Path: "a.go", Contents: guardOriginal}},
	}

	answer := guardAnswer(guardBlock("a.go", guardOriginal+"\nfunc New() int { return 1 }\n"))

	p, err := Repair(context.Background(),
		&sequencedModel{answers: []string{answer}},
		ProposalInput{Tree: "a.go\n"},
		previous,
		Failure{Stage: "build", Log: "undefined: Discount"},
		DefaultStrategies[0])

	if err != nil {
		t.Fatalf("a complete repair was rejected: %v", err)
	}
	if len(p.Edits) != 1 || p.Edits[0].Path != "a.go" {
		t.Fatalf("edits = %+v", p.Edits)
	}
}

// the invariants checkWholeFiles must hold for any input, not just the table.
// The table pins the behaviour; the fuzzer looks for the input outside it.
func FuzzCheckWholeFiles(f *testing.F) {
	f.Add("a.go", "package a\n", "a.go", "package a\n", false, false, "a.go")
	f.Add("a.go", "// ... rest unchanged\n", "a.go", strings.Repeat("x\n", 100), false, false, "a.go")
	f.Add("big.json", "{}\n", "big.json", "", true, false, "big.json")
	f.Add("b.go", "package b\n", "a.go", "package a\n", false, false, "a.go\nb.go")

	f.Fuzz(func(t *testing.T, path, contents, srcPath, src string, tooLarge, missing bool, tree string) {
		source := Sourced{Path: srcPath, Contents: src, TooLarge: tooLarge, Missing: missing}
		err := checkWholeFiles([]FileEdit{{Path: path, Contents: contents}}, []Sourced{source}, tree)

		shown := path == srcPath
		inTree := false
		for line := range strings.SplitSeq(tree, "\n") {
			// checkWholeFiles never registers a blank trimmed line as a path, so
			// the oracle must not either, or an empty edit path spuriously
			// matches an empty tree line
			if p := strings.TrimSpace(line); p != "" && p == path {
				inTree = true
				break
			}
		}
		_, hasStandIn := standInLine(contents)
		_, originalStandIn := standInLine(src)

		switch {
		case shown && tooLarge:
			// the first case in the guard, so it wins even over missing
			if err == nil {
				t.Fatalf("a too large file was rewritten: %q", srcPath)
			}
		case shown && missing && !tooLarge:
			// a recreation is a new file
			if err != nil {
				t.Fatalf("recreating a missing file was refused: %v", err)
			}
		case shown && !tooLarge && !missing:
			orig, new := len(src), len(contents)
			switch {
			case float64(new) < 0.5*float64(orig):
				if err == nil {
					t.Fatalf("shrank from %d to %d bytes and was accepted", orig, new)
				}
			case hasStandIn && !originalStandIn:
				if err == nil {
					t.Fatalf("a stand-in was accepted")
				}
			case contents == src:
				if err != nil {
					t.Fatalf("a faithful copy was refused: %v", err)
				}
			}
		case !shown && inTree:
			if err == nil {
				t.Fatalf("an unseen existing file was rewritten: %q", path)
			}
		case !shown && !inTree:
			if err != nil {
				t.Fatalf("a genuinely new file was refused: %v", err)
			}
		}
	})
}
