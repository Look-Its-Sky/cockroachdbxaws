package remediation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tmc/langchaingo/llms"
)

// Request is one investigation handed over for remediation.
type Request struct {
	InvestigationID string
	IncidentID      string
	ServiceID       string
	// IncidentSummary is the prose the investigation was run against.
	IncidentSummary string
	// VerdictAnswer is what the agent concluded, in its own words.
	VerdictAnswer string
	// CommitSHA is the commit the verdict implicated. Empty means triage has
	// nothing to read, which it reports rather than guessing around.
	CommitSHA string
}

// how long each kind of container may run.
//
// The read-only stages are a clone and some git plumbing, so they get minutes
// rather than the quarter of an hour a build and a test suite need.
const (
	inspectTimeout   = 6 * time.Minute
	candidateTimeout = DefaultSandboxTimeout
)

// how long a whole remediation may take, however many candidates it fans out
// to. Well clear of N candidates running concurrently at their own ceiling; it
// exists so a job cannot occupy a slot forever.
const jobTimeout = 45 * time.Minute

// defaults for the pool. Two jobs at a time, three containers each, is six
// containers on a demo laptop — which is about what one will take.
const (
	defaultJobConcurrency       = 2
	defaultCandidateConcurrency = 3
	defaultQueueDepth           = 32
	// One repair round. The failures worth repairing are mechanical — a missing
	// type conversion, an unused import — and a model that cannot fix its own
	// compiler error when shown it once will not fix it on the fifth go, while
	// every round costs a container and a model call per candidate.
	defaultMaxRepairs = 1
)

// Runner turns a verdict into ranked, verified candidate fixes.
//
// It has its own bounded pool and is driven asynchronously on purpose. The SQS
// worker handles one message at a time under a ten-minute run budget; a
// remediation is N containers at fifteen minutes each, and doing that inline
// would stop the queue being consumed while a detector keeps producing.
type Runner struct {
	Repos   *Repositories
	Sandbox Sandbox
	Model   llms.Model
	// Store persists outcomes. Nil keeps everything in memory, the way the
	// worker's journal is optional: durability is not worth refusing to run for.
	Store *Solutions
	// GitHubToken is only used to clone; a public repository needs none, and it
	// never enters the sandbox for one.
	GitHubToken string

	Strategies           []Strategy
	JobConcurrency       int
	CandidateConcurrency int
	// MaxRepairs bounds how many times a candidate is shown its own failure and
	// asked again. 0 uses defaultMaxRepairs; negative turns repair off.
	MaxRepairs int

	jobs     chan Request
	started  sync.Once
	inFlight sync.Map // investigation id -> struct{}, so a redelivery is not fanned out twice

	// the repository lookup, substituted by tests so the pipeline can be driven
	// without a database. Nil means Repos.Get, which is the only production path.
	lookup func(ctx context.Context, serviceID string) (Repository, error)
}

func (r *Runner) repository(ctx context.Context, serviceID string) (Repository, error) {
	if r.lookup != nil {
		return r.lookup(ctx, serviceID)
	}
	if r.Repos == nil {
		return Repository{}, errors.New("remediation: no repository mapping is configured")
	}
	return r.Repos.Get(ctx, serviceID)
}

// Start brings up the pool. Safe to call more than once; only the first counts.
func (r *Runner) Start(ctx context.Context) {
	r.started.Do(func() {
		r.jobs = make(chan Request, defaultQueueDepth)

		workers := r.JobConcurrency
		if workers <= 0 {
			workers = defaultJobConcurrency
		}

		for range workers {
			go func() {
				for req := range r.jobs {
					if ctx.Err() != nil {
						return
					}
					r.runJob(ctx, req)
				}
			}()
		}

		log.Printf("Remediation: %d concurrent jobs, %d candidates each",
			workers, len(r.strategies()))
	})
}

// Enqueue hands an investigation over and returns immediately.
//
// It reports false when the queue is full or the investigation is already being
// remediated. Both are ordinary: the caller acknowledges its message either
// way, because a dropped remediation must never cost a verdict that was already
// paid for and persisted.
func (r *Runner) Enqueue(req Request) bool {
	if r == nil || r.jobs == nil {
		return false
	}
	if _, busy := r.inFlight.LoadOrStore(req.InvestigationID, struct{}{}); busy {
		return false
	}

	select {
	case r.jobs <- req:
		return true
	default:
		r.inFlight.Delete(req.InvestigationID)
		log.Printf("Remediation: queue is full, not remediating %s", req.InvestigationID)
		return false
	}
}

func (r *Runner) runJob(ctx context.Context, req Request) {
	defer r.inFlight.Delete(req.InvestigationID)

	// derived from the process, never from the message: a remediation outlives
	// the SQS visibility timeout by design, since the message is long acked
	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	outcome := r.Run(jobCtx, req)
	log.Printf("Remediation: %s: %s (%d candidates)",
		req.InvestigationID, outcome.Status, len(outcome.Candidates))
}

// Run remediates one investigation synchronously.
//
// It returns an Outcome rather than an error: every failure here is something
// an engineer needs to read next to the incident, so it is recorded on the
// outcome and persisted rather than thrown.
func (r *Runner) Run(ctx context.Context, req Request) Outcome {
	outcome := Outcome{
		InvestigationID: req.InvestigationID,
		IncidentID:      req.IncidentID,
		ServiceID:       req.ServiceID,
		Status:          RemediationRunning,
		StartedAt:       time.Now().UTC(),
	}
	r.save(ctx, outcome)

	repo, err := r.repository(ctx, req.ServiceID)
	if err != nil {
		return r.finish(ctx, outcome, RemediationFailed, err)
	}
	outcome.Repository = &repo

	cloneURL := repo.CloneURL(r.GitHubToken)

	// the gate. Its whole purpose is to be cheap relative to what follows, so
	// it runs before any candidate container is started.
	triage, sections, err := r.triage(ctx, repo, cloneURL, req)
	if err != nil {
		return r.finish(ctx, outcome, RemediationFailed, err)
	}
	outcome.Triage = &triage
	r.save(ctx, outcome)

	if !triage.ShouldRemediate() {
		log.Printf("Remediation: %s: triage says %s, not proposing fixes",
			req.InvestigationID, triage.Status)
		return r.finish(ctx, outcome, RemediationStopped, nil)
	}

	sources, tree, err := r.inspect(ctx, repo, cloneURL, req.CommitSHA, filesToRead(triage, sections))
	if err != nil {
		return r.finish(ctx, outcome, RemediationFailed, err)
	}
	if len(readable(sources)) == 0 {
		return r.finish(ctx, outcome, RemediationFailed,
			errors.New("remediation: none of the implicated files could be read from the checkout"))
	}

	in := ProposalInput{
		IncidentSummary: req.IncidentSummary,
		VerdictAnswer:   req.VerdictAnswer,
		TriageEvidence:  triage.Evidence,
		Repository:      repo,
		Sources:         sources,
		Tree:            tree,
	}

	// drop a previous run's candidates before this one's land, or the UI offers
	// two generations of fix side by side with no way to tell them apart
	if r.Store != nil {
		if err := r.Store.Supersede(ctx, req.InvestigationID); err != nil {
			log.Printf("Remediation: %s: could not clear the previous candidates: %v",
				req.InvestigationID, err)
		}
	}

	outcome.Candidates = r.fanOut(ctx, repo, cloneURL, req, in)
	Rank(outcome.Candidates)

	return r.finish(ctx, outcome, RemediationDone, nil)
}

// triage reads the implicated commit and asks whether the fault is really there.
func (r *Runner) triage(ctx context.Context, repo Repository, cloneURL string, req Request) (Triage, map[string]Section, error) {
	if strings.TrimSpace(req.CommitSHA) == "" {
		// no commit to read means the gate cannot do its job, which is a
		// finding rather than a reason to fan out blind
		return Triage{
			Status: TriageCommitMissing,
			Evidence: "The investigation did not implicate a commit, so there is nothing to read. " +
				"A fix cannot be proposed without knowing what changed.",
		}, nil, nil
	}

	run, err := r.Sandbox.Run(ctx, Spec{
		Image:   repo.RuntimeImage,
		Script:  BuildTriageScript(repo, cloneURL, req.CommitSHA),
		Timeout: inspectTimeout,
	})
	if err != nil {
		return Triage{}, nil, fmt.Errorf("remediation: triage sandbox: %w", err)
	}

	sections := ParseSections(run.Output)
	if !sections[stageClone].Passed() {
		return Triage{}, nil, fmt.Errorf("remediation: could not clone %s: %s",
			repo.Redacted(), clip(sections[stageClone].Output, 500))
	}

	triage, err := Judge(ctx, r.Model, req.IncidentSummary, req.VerdictAnswer, sections)
	if err != nil {
		return Triage{}, sections, err
	}
	return triage, sections, nil
}

// inspect reads the implicated files out of a fresh checkout, whole.
//
// A second clone, after triage has already made one. That is deliberate: the
// gate exists to stop the expensive path, so nothing beyond the gate should be
// paid for before it has passed.
func (r *Runner) inspect(ctx context.Context, repo Repository, cloneURL, sha string, files []string) ([]Sourced, string, error) {
	if len(files) == 0 {
		return nil, "", errors.New("remediation: triage named no files to read")
	}

	run, err := r.Sandbox.Run(ctx, Spec{
		Image:   repo.RuntimeImage,
		Script:  BuildInspectScript(repo, cloneURL, sha, files),
		Timeout: inspectTimeout,
	})
	if err != nil {
		return nil, "", fmt.Errorf("remediation: inspect sandbox: %w", err)
	}

	sections := ParseSections(run.Output)
	if !sections[stageClone].Passed() {
		return nil, "", fmt.Errorf("remediation: could not clone %s to read it: %s",
			repo.Redacted(), clip(sections[stageClone].Output, 500))
	}

	return ParseSources(sections), sections[stageTree].Output, nil
}

// fanOut proposes and verifies one candidate per strategy, concurrently.
func (r *Runner) fanOut(ctx context.Context, repo Repository, cloneURL string, req Request, in ProposalInput) []Candidate {
	strategies := r.strategies()
	candidates := make([]Candidate, len(strategies))

	limit := r.CandidateConcurrency
	if limit <= 0 {
		limit = defaultCandidateConcurrency
	}
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for i, s := range strategies {
		wg.Add(1)
		go func(i int, s Strategy) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			candidates[i] = r.candidate(ctx, repo, cloneURL, req, in, s)
		}(i, s)
	}
	wg.Wait()

	return candidates
}

// one strategy, start to finish: ask the model, then find out whether it worked.
func (r *Runner) candidate(ctx context.Context, repo Repository, cloneURL string, req Request, in ProposalInput, s Strategy) Candidate {
	c := Candidate{
		ID:              uuid.NewString(),
		InvestigationID: req.InvestigationID,
		IncidentID:      req.IncidentID,
		ServiceID:       req.ServiceID,
		Strategy:        s.Name,
		Status:          CandidateProposed,
		CreatedAt:       time.Now().UTC(),
	}

	proposal, err := Propose(ctx, r.Model, in, s)
	if err != nil {
		return c.failed(err.Error())
	}

	c.Summary = proposal.Summary
	c.Rationale = proposal.Rationale

	if len(proposal.Edits) == 0 {
		// a model that declines to change working code is doing the right thing
		return c.failed("the model proposed no change: " + fallback(proposal.Summary, "it gave no reason"))
	}

	// Propose, verify, and — when the toolchain rejects it for a reason the
	// model can act on — show it the output and go again. Bounded, because a
	// model that cannot fix its own compiler error in one look will not fix it
	// in five, and each round is a container.
	for attempt := 0; ; attempt++ {
		apply, err := ApplyCommand(proposal.Edits)
		if err != nil {
			return c.failed(err.Error())
		}

		c.Files = c.Files[:0]
		for _, e := range proposal.Edits {
			c.Files = append(c.Files, e.Path)
		}
		c.Edits = proposal.Edits
		c.Summary = fallback(proposal.Summary, c.Summary)
		c.Rationale = fallback(proposal.Rationale, c.Rationale)

		run, err := r.Sandbox.Run(ctx, Spec{
			Image:   repo.RuntimeImage,
			Script:  BuildScript(repo, cloneURL, req.CommitSHA, apply),
			Timeout: candidateTimeout,
		})
		if err != nil {
			return c.failed(err.Error())
		}

		sections := ParseSections(run.Output)
		if !sections[stageClone].Passed() {
			return c.failed("could not clone the repository to verify the change")
		}

		c.Verification = verificationFrom(sections, run)
		c.Diff = sections[stageDiff].Output

		// a change that applied cleanly but altered nothing is not a fix, and
		// showing an engineer an empty diff wastes the one thing they are short of
		if c.Verification.Applied && strings.TrimSpace(c.Diff) == "" {
			return c.failed("the change was written but left the repository identical")
		}

		if c.Verification.Satisfied(repo) || attempt >= r.repairBudget() || !c.Verification.NeedsRepair() {
			return c
		}

		failure := failureFrom(sections)
		log.Printf("Remediation: %s: %s failed at %s, repairing (round %d)",
			req.InvestigationID, s.Name, failure.Stage, attempt+1)

		repaired, err := Repair(ctx, r.Model, in, proposal, failure, s)
		if err != nil {
			// the unrepaired candidate is still worth showing as what it is
			log.Printf("Remediation: %s: repair (%s) failed, keeping the unrepaired candidate: %v",
				req.InvestigationID, s.Name, err)
			return c
		}
		if len(repaired.Edits) == 0 {
			// silent here would be indistinguishable from repair never having
			// been attempted, which is exactly the confusion this cost once
			log.Printf("Remediation: %s: repair (%s) came back with no files to write (%q); "+
				"keeping the unrepaired candidate",
				req.InvestigationID, s.Name, clip(repaired.Summary, 120))
			return c
		}

		proposal = repaired
		c.Repairs = attempt + 1
	}
}

func (r *Runner) repairBudget() int {
	if r.MaxRepairs > 0 {
		return r.MaxRepairs
	}
	if r.MaxRepairs < 0 {
		return 0
	}
	return defaultMaxRepairs
}

// which stage to quote back, preferring the build: a compilation failure makes
// the test stage fail too, and its output is the compiler's either way
func failureFrom(sections map[string]Section) Failure {
	if build := sections[stageBuild]; build.Ran && !build.Passed() {
		return Failure{Stage: stageBuild, Log: build.Output}
	}
	return Failure{Stage: stageTest, Log: sections[stageTest].Output}
}

func (c Candidate) failed(reason string) Candidate {
	c.Status = CandidateFailed
	c.Error = reason
	return c
}

// read the stages back as a claim about what was proven
func verificationFrom(sections map[string]Section, run Run) Verification {
	harness := sections[stageHarness]
	build := sections[stageBuild]
	test := sections[stageTest]

	v := Verification{
		Applied:    harness.Passed(),
		BuildRan:   build.Ran,
		Built:      build.Passed(),
		TestRan:    test.Ran,
		Tested:     test.Passed(),
		TimedOut:   run.TimedOut,
		DurationMS: run.Duration.Milliseconds(),
	}

	// the log is what an engineer reads when this failed, so it is the failing
	// stage's output rather than the whole container's
	switch {
	case !v.Applied:
		v.Log = clip(harness.Output, maxLogChars)
	case v.TestRan && !v.Tested:
		v.Log = clip(test.Output, maxLogChars)
	case v.BuildRan && !v.Built:
		v.Log = clip(build.Output, maxLogChars)
	}
	return v
}

// how much of a failing stage to keep. A Go test failure is a few lines; a
// compiler having a bad day is not, and none of it belongs in a database row.
const maxLogChars = 8000

// which files to read whole before proposing a fix: what triage named, falling
// back to what the commit touched when it named nothing.
func filesToRead(t Triage, sections map[string]Section) []string {
	if len(t.Files) > 0 {
		return t.Files
	}

	// BuildTriageScript prints "----- <path>" ahead of each file it dumps
	var files []string
	for line := range strings.SplitSeq(sections[stageSource].Output, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "----- "); ok {
			if p = strings.TrimPrefix(strings.TrimSpace(p), "/workspace/"); p != "" {
				files = append(files, p)
			}
		}
	}
	return files
}

// the files that actually came back with contents
func readable(sources []Sourced) []Sourced {
	var out []Sourced
	for _, s := range sources {
		if !s.Missing && !s.TooLarge && strings.TrimSpace(s.Contents) != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *Runner) strategies() []Strategy {
	if len(r.Strategies) > 0 {
		return r.Strategies
	}
	return DefaultStrategies
}

// stamp the outcome and persist it one last time
func (r *Runner) finish(ctx context.Context, outcome Outcome, status RemediationStatus, err error) Outcome {
	now := time.Now().UTC()
	outcome.FinishedAt = &now
	outcome.Status = status
	if err != nil {
		outcome.Error = err.Error()
	}

	r.save(ctx, outcome)
	return outcome
}

// best effort, like the worker's journal: losing durability must not lose a
// candidate an engineer is about to be shown
func (r *Runner) save(ctx context.Context, outcome Outcome) {
	if r.Store == nil {
		return
	}

	// its own context: an outcome that finished as the job timed out is still
	// worth writing down
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()

	if err := r.Store.Save(saveCtx, outcome); err != nil {
		log.Printf("Remediation: could not persist %s: %v", outcome.InvestigationID, err)
	}
}

func fallback(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
