package routes

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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

// a bounded context for a read, derived from the request so a client that gives
// up stops the query too
func contextFor(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), readTimeout)
}
