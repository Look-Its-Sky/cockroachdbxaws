package agent

import (
	"encoding/json"
	"regexp"
	"strings"
)

// what the agent decided to do about an incident. Prose is what a human reads;
// this is what the remediation pipeline branches on.
type Decision string

const (
	DecisionRollback Decision = "ROLLBACK"
	DecisionHotfix   Decision = "HOTFIX"
	// the model did not commit to either, which is a real answer and must not
	// be silently rounded to one of the above
	DecisionUnknown Decision = "UNKNOWN"
)

// the machine-readable half of an answer.
type Verdict struct {
	Decision  Decision `json:"decision"`
	CommitSHA string   `json:"commit_sha,omitempty"`
	Service   string   `json:"service,omitempty"`
	// 0 when the model gave none; never invented
	Confidence float64 `json:"confidence,omitempty"`
	// how the fields were arrived at, so a weak parse is visible rather than
	// indistinguishable from a confident one
	Source VerdictSource `json:"source,omitempty"`
}

// VerdictSource records how a verdict was extracted.
type VerdictSource string

const (
	// the model emitted the JSON block it was asked for
	SourceDeclared VerdictSource = "declared"
	// no block, so the decision was read out of the prose
	SourceInferred VerdictSource = "inferred"
	// neither a block nor a recognisable decision in the prose
	SourceAbsent VerdictSource = "absent"
)

// Decided reports whether the verdict names an action to take.
func (v Verdict) Decided() bool {
	return v.Decision == DecisionRollback || v.Decision == DecisionHotfix
}

// what the model is asked to append, and what parseVerdict looks for first
const verdictInstruction = `

Finally, after your prose, append a fenced JSON block exactly like this, with no
other text after it:

` + "```json" + `
{"decision": "ROLLBACK", "commit_sha": "a91f3c2", "service": "checkout", "confidence": 0.8}
` + "```" + `

decision is ROLLBACK or HOTFIX. commit_sha is the commit you implicated, copied
exactly. confidence is between 0 and 1. The prose above it is what a human
reads; this block is what the automation acts on, so it must agree with it.`

var (
	// a fenced json block, the last one wins if the model emits several
	jsonBlockRE = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
	// git short SHAs through full ones. Deliberately not anchored to a length,
	// since the deploys table uses 7 and real tooling emits 40
	shaRE = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
)

// pull the structured verdict out of an answer, returning it alongside the
// prose with the JSON block removed.
//
// The block is the happy path. The fallback exists because the models this runs
// against are small and free, and one that writes a perfect paragraph but
// forgets the block should still drive the pipeline — silently returning
// UNKNOWN there would strand every incident.
func parseVerdict(answer string) (Verdict, string) {
	if v, prose, ok := verdictFromBlock(answer); ok {
		return v, prose
	}
	return verdictFromProse(answer), strings.TrimSpace(answer)
}

func verdictFromBlock(answer string) (Verdict, string, bool) {
	matches := jsonBlockRE.FindAllStringSubmatchIndex(answer, -1)
	if len(matches) == 0 {
		return Verdict{}, "", false
	}

	last := matches[len(matches)-1]
	raw := answer[last[2]:last[3]]

	var parsed struct {
		Decision   string  `json:"decision"`
		CommitSHA  string  `json:"commit_sha"`
		Service    string  `json:"service"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Verdict{}, "", false
	}

	decision := normaliseDecision(parsed.Decision)
	if decision == DecisionUnknown {
		// a block that names no decision is worth no more than no block
		return Verdict{}, "", false
	}

	// prose is everything outside the block, so a human never reads the JSON
	prose := strings.TrimSpace(answer[:last[0]] + answer[last[1]:])

	return Verdict{
		Decision:   decision,
		CommitSHA:  strings.TrimSpace(parsed.CommitSHA),
		Service:    strings.TrimSpace(parsed.Service),
		Confidence: clampConfidence(parsed.Confidence),
		Source:     SourceDeclared,
	}, prose, true
}

// read the decision out of the prose. The system prompt asks for the decision
// in the first sentence, so the earliest mention wins rather than the loudest.
func verdictFromProse(answer string) Verdict {
	upper := strings.ToUpper(answer)

	rollback := strings.Index(upper, "ROLLBACK")
	hotfix := strings.Index(upper, "HOTFIX")

	var decision Decision
	switch {
	case rollback < 0 && hotfix < 0:
		return Verdict{Decision: DecisionUnknown, Source: SourceAbsent}
	case hotfix < 0, rollback >= 0 && rollback < hotfix:
		decision = DecisionRollback
	default:
		decision = DecisionHotfix
	}

	return Verdict{
		Decision:  decision,
		CommitSHA: firstSHA(answer),
		Source:    SourceInferred,
	}
}

// the first thing shaped like a commit SHA. Hex words that are really English
// ("added", "decade") are possible in principle but need to be 7+ characters of
// pure hex, which prose does not produce.
func firstSHA(answer string) string {
	for _, candidate := range shaRE.FindAllString(answer, -1) {
		if strings.ContainsAny(candidate, "0123456789") {
			return candidate
		}
	}
	return ""
}

func normaliseDecision(raw string) Decision {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "ROLLBACK", "ROLL_BACK", "ROLL BACK", "REVERT":
		return DecisionRollback
	case "HOTFIX", "HOT_FIX", "HOT FIX", "FIX", "PATCH":
		return DecisionHotfix
	default:
		return DecisionUnknown
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
