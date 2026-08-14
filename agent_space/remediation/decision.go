package remediation

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Why a candidate was not taken. The distinction is the whole quality signal
// for this feature: a reason from the sandbox is something the system already
// knew, and a reason from an engineer is the only part it could not compute
// for itself.
type RejectionSource string

const (
	RejectionFromEngineer     RejectionSource = "engineer"
	RejectionFromVerification RejectionSource = "verification"
)

// Rejection is one candidate the engineer did not take, and why.
type Rejection struct {
	CandidateID string          `json:"candidate_id"`
	Strategy    string          `json:"strategy,omitempty"`
	Reason      string          `json:"reason"`
	Source      RejectionSource `json:"source"`
}

// Decision is an engineer's verdict on a set of proposed fixes.
//
// This is the only place in the pipeline where human judgement is written down.
// Everything else records what a model produced or what a container proved.
type Decision struct {
	ID              string `json:"id"`
	InvestigationID string `json:"investigation_id"`
	IncidentID      string `json:"incident_id,omitempty"`
	ServiceID       string `json:"service_id,omitempty"`

	// ChosenCandidateID is empty when every candidate was rejected, which is
	// not a failure to record: what the engineer wrote instead is the most
	// useful thing in this table.
	ChosenCandidateID string      `json:"chosen_candidate_id,omitempty"`
	Rejections        []Rejection `json:"rejections,omitempty"`

	EngineerFix   string `json:"engineer_fix,omitempty"`
	EngineerPRURL string `json:"engineer_pr_url,omitempty"`
	Notes         string `json:"notes,omitempty"`

	// Document is the prose that gets embedded. Stored rather than rebuilt on
	// demand, so changing the embedding model is a re-index rather than a loss,
	// and so what was recalled later is exactly what was written now.
	Document string `json:"document,omitempty"`

	DecidedAt time.Time `json:"decided_at"`
	// IndexedAt is nil until the embedding lands. Nil is also the work queue
	// for a retry: a decision that is not indexed will never be recalled.
	IndexedAt *time.Time `json:"indexed_at,omitempty"`
}

// DecisionInput is what an engineer supplied, and nothing else.
//
// Every factual field — what was verified, which strategy won, what triage
// found — is read from the stored Outcome instead. The document this produces
// becomes context for future incidents, so a caller must not be able to assert
// that something passed its tests when it did not.
type DecisionInput struct {
	ChosenCandidateID string
	// Reasons the engineer typed, by candidate id. A candidate absent from this
	// map is filled in from what the sandbox established about it.
	Reasons map[string]string

	EngineerFix   string
	EngineerPRURL string
	Notes         string

	// IncidentSummary is the prose the investigation ran against, resolved
	// server-side. It leads the document, because the thing a future incident
	// is matched against is a description of a fault.
	IncidentSummary string
}

var (
	// ErrEmptyDecision means nothing was actually decided. Such a record would
	// still be embedded and recalled, while teaching nothing.
	ErrEmptyDecision = errors.New("remediation: a decision needs a chosen candidate, an engineer fix, or a note")
	// ErrForeignCandidate means an id that is not part of this investigation.
	ErrForeignCandidate = errors.New("remediation: that candidate does not belong to this investigation")
)

// how much of the incident prose to carry into the document. Enough to match on,
// short enough that a handful of precedents do not crowd out the live incident.
const maxSummaryChars = 1200

// NewDecision assembles a decision from what the engineer said and what the run
// already established.
//
// Candidates the engineer did not mention are still recorded as rejected, with
// the reason the sandbox produced. Silence is the common case — an engineer
// picks one and types nothing — and a record that only listed the winner would
// lose the fact that the others were seen and passed over.
func NewDecision(o Outcome, in DecisionInput) (Decision, error) {
	byID := make(map[string]Candidate, len(o.Candidates))
	for _, c := range o.Candidates {
		byID[c.ID] = c
	}

	if id := strings.TrimSpace(in.ChosenCandidateID); id != "" {
		if _, ok := byID[id]; !ok {
			return Decision{}, fmt.Errorf("%w: %s", ErrForeignCandidate, id)
		}
	}
	for id := range in.Reasons {
		if _, ok := byID[id]; !ok {
			return Decision{}, fmt.Errorf("%w: %s", ErrForeignCandidate, id)
		}
	}

	chosen := strings.TrimSpace(in.ChosenCandidateID)
	fix := strings.TrimSpace(in.EngineerFix)
	notes := strings.TrimSpace(in.Notes)

	if chosen == "" && fix == "" && notes == "" {
		return Decision{}, ErrEmptyDecision
	}

	d := Decision{
		ID:                uuid.NewString(),
		InvestigationID:   o.InvestigationID,
		IncidentID:        o.IncidentID,
		ServiceID:         o.ServiceID,
		ChosenCandidateID: chosen,
		EngineerFix:       fix,
		EngineerPRURL:     strings.TrimSpace(in.EngineerPRURL),
		Notes:             notes,
		DecidedAt:         time.Now().UTC(),
	}

	// ranked order, so the document reads in the order the engineer was shown
	for _, c := range o.Candidates {
		if c.ID == chosen {
			continue
		}

		reason, source := strings.TrimSpace(in.Reasons[c.ID]), RejectionFromEngineer
		if reason == "" {
			reason, source = verificationReason(c), RejectionFromVerification
		}

		d.Rejections = append(d.Rejections, Rejection{
			CandidateID: c.ID,
			Strategy:    c.Strategy,
			Reason:      reason,
			Source:      source,
		})
	}

	d.Document = buildDecisionDocument(o, d, in.IncidentSummary)
	return d, nil
}

// what the sandbox established about a candidate nobody explained.
//
// A candidate that never produced a diff failed before it could be judged, and
// saying "it was not chosen" would imply a judgement that was never made.
func verificationReason(c Candidate) string {
	if c.Error != "" {
		return "never produced a usable change: " + c.Error
	}
	return c.Verification.Summary()
}

// EngineerWroteReasons counts the rejections a human explained.
//
// Surfaced because it is the measure of whether this feature is earning its
// keep: rejections that are only ever auto-filled teach the system nothing it
// could not already work out.
func (d Decision) EngineerWroteReasons() int {
	n := 0
	for _, r := range d.Rejections {
		if r.Source == RejectionFromEngineer {
			n++
		}
	}
	return n
}

// RejectedEverything reports the case worth reading closely: the agent proposed
// fixes and a human took none of them.
func (d Decision) RejectedEverything() bool {
	return d.ChosenCandidateID == "" && len(d.Rejections) > 0
}

// the prose that gets embedded.
//
// Shaped like the incidents already in the index — a paragraph naming the
// symptom, the commit and the resolution — so it retrieves alongside them and
// reads as one more piece of history rather than as a different kind of record.
func buildDecisionDocument(o Outcome, d Decision, summary string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Remediation decision (%s)", d.DecidedAt.Format("2006-01-02"))
	if d.IncidentID != "" {
		fmt.Fprintf(&b, " for incident %s", d.IncidentID)
	}
	if d.ServiceID != "" {
		fmt.Fprintf(&b, ", service %s", d.ServiceID)
	}
	b.WriteString(".\n\n")

	if s := strings.TrimSpace(summary); s != "" {
		fmt.Fprintf(&b, "Incident: %s\n\n", clip(s, maxSummaryChars))
	}

	if t := o.Triage; t != nil {
		b.WriteString("Cause: ")
		if t.Commit != "" {
			fmt.Fprintf(&b, "commit %s", t.Commit)
			if t.CommitSubject != "" {
				fmt.Fprintf(&b, " (%q)", t.CommitSubject)
			}
		} else {
			b.WriteString("no commit was implicated")
		}
		if e := strings.TrimSpace(t.Evidence); e != "" {
			fmt.Fprintf(&b, " — %s", clip(e, maxSummaryChars))
		}
		b.WriteString("\n\n")
	}

	writeChosen(&b, o, d)
	writeRejections(&b, d)

	if d.Notes != "" {
		fmt.Fprintf(&b, "\nNote from the engineer: %s\n", d.Notes)
	}
	return b.String()
}

func writeChosen(b *strings.Builder, o Outcome, d Decision) {
	if d.ChosenCandidateID == "" {
		fmt.Fprintf(b, "Chosen fix: none. All %d proposed fixes were rejected.\n", len(d.Rejections))
		if d.EngineerFix != "" {
			fmt.Fprintf(b, "The engineer fixed it instead by: %s\n", d.EngineerFix)
		}
		if d.EngineerPRURL != "" {
			fmt.Fprintf(b, "Their change: %s\n", d.EngineerPRURL)
		}
		return
	}

	for _, c := range o.Candidates {
		if c.ID != d.ChosenCandidateID {
			continue
		}

		fmt.Fprintf(b, "Chosen fix (%s strategy): %s\n", c.Strategy, fallback(c.Summary, "no summary was given"))
		// said plainly, and never overstated: a service with no tests says so
		fmt.Fprintf(b, "What was verified: %s.\n", c.Verification.Summary())
		if len(c.Files) > 0 {
			fmt.Fprintf(b, "Files changed: %s\n", strings.Join(c.Files, ", "))
		}
		if c.Repairs > 0 {
			fmt.Fprintf(b, "It took %d repair round(s) to get there.\n", c.Repairs)
		}
		return
	}
}

func writeRejections(b *strings.Builder, d Decision) {
	if len(d.Rejections) == 0 {
		return
	}

	b.WriteString("\nRejected:\n")
	for _, r := range d.Rejections {
		source := "from the sandbox"
		if r.Source == RejectionFromEngineer {
			source = "from the engineer"
		}
		fmt.Fprintf(b, "- %s: %s [%s]\n", fallback(r.Strategy, "unnamed strategy"), r.Reason, source)
	}
}
