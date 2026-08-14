package routes

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"agent_space/incident"
	"agent_space/remediation"
	"agent_space/utils/queue"
	"agent_space/worker"
)

const remediationUnavailable = "Remediation is not running. It needs a container runtime, a database and a repository mapping"

const solutionsUnavailable = "Proposed fixes are not being stored. Check the database connection"

// how long a read for the UI may take. These are single queries; the long work
// happened somewhere else and is already written down.
const readTimeout = 15 * time.Second

// how long indexing a decision may take. Longer than a read, because it is an
// embedding round trip to a provider rather than a query.
const indexTimeout = 60 * time.Second

// Repositories lists the service-to-repository mapping, for the UI's header and
// for showing what "verified" means per service.
func Repositories(c *gin.Context) {
	if Repos == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": remediationUnavailable})
		return
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	mapped, err := Repos.List(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// the UI needs to know which services can be verified rather than merely
	// compiled, and that is not readable from the commands alone
	out := make([]gin.H, 0, len(mapped))
	for _, repo := range mapped {
		out = append(out, gin.H{
			"repository": repo,
			"has_tests":  repo.HasTests(),
			"url":        "https://" + repo.Redacted(),
		})
	}

	c.JSON(http.StatusOK, gin.H{"count": len(out), "repositories": out})
}

// Remediations lists recent remediation runs, newest first. Candidates are
// deliberately not included: a diff per candidate turns a list into megabytes.
func Remediations(c *gin.Context) {
	if Solutions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": solutionsUnavailable})
		return
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	limit, _ := strconv.Atoi(c.Query("limit"))
	outcomes, err := Solutions.Recent(ctx, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"count": len(outcomes), "remediations": outcomes})
}

// RemediationFor returns the triage finding and the ranked candidates for one
// investigation. This is what the picker in the UI is built on.
func RemediationFor(c *gin.Context) {
	if Solutions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": solutionsUnavailable})
		return
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	outcome, found, err := Solutions.Load(ctx, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		// also what an investigation that was never remediated looks like, and
		// what one still waiting for a free slot looks like
		c.JSON(http.StatusNotFound, gin.H{"error": "no remediation for that investigation"})
		return
	}

	c.JSON(http.StatusOK, outcome)
}

// StartRemediation proposes fixes for an investigation that already has a
// verdict. The queue path does this automatically; this is the demo's button,
// and the way to retry one that was dropped because the pool was full.
func StartRemediation(c *gin.Context) {
	if Remediation == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": remediationUnavailable})
		return
	}
	if Results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": workerUnavailable})
		return
	}

	investigationID := c.Param("id")
	record, ok := Results.Get(investigationID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no investigation with that id"})
		return
	}
	if record.Status == worker.StatusRunning {
		c.JSON(http.StatusConflict, gin.H{"error": "that investigation has not reached a verdict yet"})
		return
	}
	if record.Result.Verdict.CommitSHA == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "that investigation implicated no commit, so there is nothing for triage to read",
		})
		return
	}

	req := remediation.Request{
		InvestigationID: record.InvestigationID,
		IncidentID:      record.IncidentID,
		ServiceID:       record.ServiceID,
		VerdictAnswer:   record.Result.Answer,
		CommitSHA:       record.Result.Verdict.CommitSHA,
	}
	req.IncidentSummary = summaryFor(c, record)

	if !Remediation.Enqueue(req) {
		c.JSON(http.StatusConflict, gin.H{
			"error": "that investigation is already being remediated, or the pool is full",
		})
		return
	}

	// 202: the work outlives this request by design, and the result is read
	// back from GET /agent/:id/remediation
	c.JSON(http.StatusAccepted, gin.H{
		"investigation_id": record.InvestigationID,
		"status":           "queued",
		"poll":             "/agent/" + record.InvestigationID + "/remediation",
	})
}

// the prose the fix is written against.
//
// The stored context is preferred and the verdict is the fallback: an incident
// row that has since been deleted must not stop a candidate being proposed for
// an investigation that plainly succeeded.
func summaryFor(c *gin.Context, record worker.Record) string {
	if Incidents == nil || record.IncidentID == "" {
		return record.Result.Answer
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	inc, err := Incidents.Latest(ctx, record.IncidentID)
	if err != nil {
		return record.Result.Answer
	}

	return incident.BuildQuestion(queue.Assignment{
		IncidentID:      record.IncidentID,
		InvestigationID: record.InvestigationID,
		ServiceID:       record.ServiceID,
	}, inc)
}

// OpenPullRequest is the engineer picking a candidate. Picking is what opens
// the draft; nothing before this point touches the repository.
func OpenPullRequest(c *gin.Context) {
	if Solutions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": solutionsUnavailable})
		return
	}
	if Repos == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": remediationUnavailable})
		return
	}
	if !Publisher.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": remediation.ErrNoToken.Error()})
		return
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	candidate, err := Solutions.Candidate(ctx, c.Param("candidate"))
	if errors.Is(err, remediation.ErrNoCandidate) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// picking twice is a double-click, not an error, and opening a second
	// identical draft would be worse than saying so
	if candidate.PRURL != "" {
		c.JSON(http.StatusOK, gin.H{
			"candidate_id": candidate.ID,
			"pr_url":       candidate.PRURL,
			"already_open": true,
		})
		return
	}

	if !candidate.Verification.Applied {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "that candidate never applied cleanly, so there is nothing to open a pull request with",
		})
		return
	}

	repo, err := Repos.Get(ctx, candidate.ServiceID)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	url, err := Publisher.Open(ctx, repo, candidate)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	// the URL is what the engineer is about to be sent to, so a failure to
	// record it is reported rather than swallowed: an unrecorded PR is one
	// nobody can find again from the incident
	if err := Solutions.MarkOpened(ctx, candidate.ID, url); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"candidate_id": candidate.ID,
			"pr_url":       url,
			"warning":      "the draft is open but could not be recorded: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"candidate_id": candidate.ID,
		"pr_url":       url,
		"verified":     candidate.Verification.Summary(),
	})
}

// DecisionRequest is what the frontend posts when an engineer commits to a
// choice.
//
// Every field is optional on its own. What is deliberately absent is anything
// factual: the service, the strategy, and above all what was verified are read
// back from the stored run. This text ends up in a document that shapes future
// incidents, so a caller must not be able to assert that something passed tests
// it never ran.
type DecisionRequest struct {
	ChosenCandidateID string             `json:"chosen_candidate_id"`
	Rejections        []RejectionRequest `json:"rejections" binding:"omitempty,dive"`
	EngineerFix       string             `json:"engineer_fix" binding:"max=4000"`
	EngineerPRURL     string             `json:"engineer_pr_url" binding:"omitempty,url,max=500"`
	Notes             string             `json:"notes" binding:"max=2000"`
}

// One candidate the engineer passed over. The reason is optional: where they
// said nothing, it is filled in from what the sandbox established.
type RejectionRequest struct {
	CandidateID string `json:"candidate_id" binding:"required"`
	Reason      string `json:"reason" binding:"max=1000"`
}

// RecordDecision stores an engineer's verdict on the proposed fixes and puts it
// where the next similar incident will find it.
//
// This is the only point in the pipeline where human judgement is captured.
// Everything before it records what a model produced or what a container
// proved; without this the reasoning behind a pick is lost when the page closes.
func RecordDecision(c *gin.Context) {
	if Solutions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": solutionsUnavailable})
		return
	}

	var req DecisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	outcome, found, err := Solutions.Load(ctx, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "no remediation for that investigation"})
		return
	}

	reasons := make(map[string]string, len(req.Rejections))
	for _, r := range req.Rejections {
		reasons[r.CandidateID] = r.Reason
	}

	decision, err := remediation.NewDecision(outcome, remediation.DecisionInput{
		ChosenCandidateID: req.ChosenCandidateID,
		Reasons:           reasons,
		EngineerFix:       req.EngineerFix,
		EngineerPRURL:     req.EngineerPRURL,
		Notes:             req.Notes,
		IncidentSummary:   incidentProseFor(c, outcome),
	})
	switch {
	case errors.Is(err, remediation.ErrEmptyDecision):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	case errors.Is(err, remediation.ErrForeignCandidate):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	superseded, err := Solutions.SaveDecision(ctx, decision)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(superseded) > 0 {
		log.Printf("Remediation: decision for %s replaces %d earlier one(s)",
			decision.InvestigationID, len(superseded))
	}

	indexed, warning := indexDecision(decision, superseded)

	log.Printf("Remediation: decision for %s (%s): %s, rejected %d (%d explained, %d auto), %s",
		decision.InvestigationID, fallback(decision.ServiceID, "unknown service"),
		chosenNote(outcome, decision), len(decision.Rejections),
		decision.EngineerWroteReasons(),
		len(decision.Rejections)-decision.EngineerWroteReasons(),
		indexedNote(indexed))

	body := gin.H{
		"decision_id":           decision.ID,
		"investigation_id":      decision.InvestigationID,
		"chosen_candidate_id":   decision.ChosenCandidateID,
		"rejected":              len(decision.Rejections),
		"reasons_from_engineer": decision.EngineerWroteReasons(),
		"reasons_auto":          len(decision.Rejections) - decision.EngineerWroteReasons(),
		"indexed":               indexed,
		// echoed so the UI can show what was actually learned, which makes a bad
		// document obvious now rather than three incidents from now
		"document": decision.Document,
	}
	if warning != "" {
		body["warning"] = warning
	}

	c.JSON(http.StatusCreated, body)
}

// put the decision in the index, and drop any it replaced.
//
// Deliberately not on the request's context: an embedding call is an external
// round trip, and a client that navigates away must not leave a decision stored
// but unrecallable. A failure here is reported rather than swallowed, because
// an unindexed decision is one the system will never learn from.
func indexDecision(d remediation.Decision, superseded []string) (indexed bool, warning string) {
	if PrecedentIndex == nil {
		return false, "recorded, but there is no decision index configured, so it will not be recalled"
	}

	ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
	defer cancel()

	// order matters: drop the old documents first, so a failure to add the new
	// one cannot leave a reversed judgement as the only precedent on file
	if err := PrecedentIndex.Forget(ctx, superseded); err != nil {
		log.Printf("Remediation: could not remove %d superseded decision(s) from the index: %v",
			len(superseded), err)
	}

	if err := PrecedentIndex.Add(ctx, d); err != nil {
		log.Printf("Remediation: decision %s is recorded but not indexed, so it will not be recalled: %v",
			d.ID, err)
		return false, "recorded, but indexing failed, so it will not be recalled: " + err.Error()
	}

	if Solutions != nil {
		if err := Solutions.MarkIndexed(ctx, d.ID); err != nil {
			// it is in the index; only the bookkeeping failed
			log.Printf("Remediation: decision %s was indexed but not marked as such: %v", d.ID, err)
		}
	}
	return true, ""
}

func chosenNote(o remediation.Outcome, d remediation.Decision) string {
	if d.ChosenCandidateID == "" {
		return "chose none of them"
	}
	for _, c := range o.Candidates {
		if c.ID == d.ChosenCandidateID {
			return "chose " + strconv.Quote(c.Strategy)
		}
	}
	return "chose a candidate"
}

func indexedNote(indexed bool) string {
	if indexed {
		return "indexed"
	}
	return "NOT indexed"
}

// the prose the decision is written against, resolved server-side.
//
// Empty is acceptable: an incident row that has since been deleted must not
// stop a decision being recorded, and the triage evidence still carries the
// cause.
func incidentProseFor(c *gin.Context, o remediation.Outcome) string {
	if Incidents == nil || o.IncidentID == "" {
		return ""
	}

	ctx, cancel := contextFor(c)
	defer cancel()

	inc, err := Incidents.Latest(ctx, o.IncidentID)
	if err != nil {
		return ""
	}

	return incident.BuildQuestion(queue.Assignment{
		IncidentID:      o.IncidentID,
		InvestigationID: o.InvestigationID,
		ServiceID:       o.ServiceID,
	}, inc)
}

func fallback(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// a bounded context for a read, derived from the request so a client that gives
// up stops the query too
func contextFor(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), readTimeout)
}
