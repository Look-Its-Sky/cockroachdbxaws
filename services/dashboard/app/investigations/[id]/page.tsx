import { headers } from "next/headers";
import Link from "next/link";
import { notFound, redirect } from "next/navigation";
import { z } from "zod";
import { RefreshControl } from "@/components/refresh";
import { SignOut } from "@/components/sign-out";
import { StatusPill } from "@/components/status";
import { auth } from "@/lib/auth";
import { canViewDashboard } from "@/lib/authorization";
import { executionSummary, getAgentInvestigation, getAgentRemediation, recommendationLabel } from "@/lib/agent";
import { getAnalysisOverview } from "@/lib/overview";

export const dynamic = "force-dynamic";

type Props = { params: Promise<{ id: string }> };

function time(value: string | undefined): string {
  if (!value) return "Not finished";
  return new Intl.DateTimeFormat("en", {
    dateStyle: "medium",
    timeStyle: "medium",
    timeZone: "UTC",
  }).format(new Date(value));
}

export default async function InvestigationPage({ params }: Props) {
  const session = await auth.api.getSession({ headers: await headers() });
  if (!session) redirect("/sign-in");
  if (!canViewDashboard(session)) redirect("/unauthorized");

  const { id } = await params;
  if (!z.string().uuid().safeParse(id).success) notFound();

  const [analysisState, agentState, remediationState] = await Promise.all([
    getAnalysisOverview(),
    getAgentInvestigation(id),
    getAgentRemediation(id),
  ]);
  const detector = analysisState.available
    ? analysisState.value.recent_investigations.find((item) => item.investigation_id === id)
    : undefined;
  const run = agentState.available ? agentState.value : null;
  const remediation = remediationState.available ? remediationState.value : null;

  if (analysisState.available && !detector && agentState.available && !run) notFound();

  return (
    <main className="shell">
      <header className="topbar">
        <Link className="brand-mark" href="/" aria-label="Back to operations overview">SL</Link>
        <div className="brand-copy">
          <strong>Investigation detail</strong>
          <span>Detector signal → agent verdict</span>
        </div>
        <div className="top-actions">
          <Link className="nav-link" href="/remediations">Remediations</Link>
          <RefreshControl />
          <SignOut />
        </div>
      </header>

      <section className="detail-hero">
        <div>
          <Link className="back-link" href="/">← Operations overview</Link>
          <p className="eyebrow">INVESTIGATION</p>
          <h1>{detector?.service ?? run?.service_id ?? "Agent assignment"}</h1>
          <code>{id}</code>
        </div>
        <div className="health-cluster">
          <StatusPill healthy={analysisState.available} label="Detector projection" />
          <StatusPill healthy={Boolean(run)} label={run ? `Agent ${run.status}` : "Awaiting agent"} />
        </div>
      </section>

      <section className="detail-grid">
        <article className="panel detail-panel">
          <div className="panel-heading">
            <div><p className="eyebrow">WHY IT STARTED</p><h2>Detector signal</h2></div>
          </div>
          {detector ? (
            <dl className="fact-grid">
              <div><dt>Service</dt><dd>{detector.service}</dd></div>
              <div><dt>Environment</dt><dd>{detector.environment}</dd></div>
              <div><dt>Severity</dt><dd>{detector.severity}</dd></div>
              <div><dt>Rule trigger</dt><dd>{detector.trigger_reason.replaceAll("_", " ")}</dd></div>
              <div><dt>Detection state</dt><dd>{detector.state}</dd></div>
              <div><dt>Queued for agent</dt><dd>{time(detector.queued_at)}</dd></div>
            </dl>
          ) : (
            <p className="empty">The detector projection is unavailable or this run has aged out of its bounded recent list.</p>
          )}
          <p className="safety-note">Raw log bodies and context excerpts are intentionally excluded. This view shows only the safe metadata that caused the assignment.</p>
        </article>

        <article className="panel detail-panel">
          <div className="panel-heading">
            <div><p className="eyebrow">CURRENT WORK</p><h2>Agent run</h2></div>
          </div>
          {!agentState.available ? (
            <p className="empty">Agent data is {agentState.reason}. Detector health remains independent.</p>
          ) : !run ? (
            <p className="empty">The assignment has not reached this agent journal yet.</p>
          ) : (
            <dl className="fact-grid">
              <div><dt>Status</dt><dd><span className="state-tag">{run.status}</span></dd></div>
              <div><dt>Recommended action</dt><dd className="decision">{recommendationLabel(run)}</dd></div>
              <div><dt>Execution</dt><dd>{executionSummary(run, remediation)}</dd></div>
              <div><dt>Target commit</dt><dd>{run.result.verdict.commit_sha ?? "Not identified"}</dd></div>
              <div><dt>Confidence</dt><dd>{run.result.verdict.confidence === undefined ? "Not declared" : `${Math.round(run.result.verdict.confidence * 100)}%`}</dd></div>
              <div><dt>Verdict source</dt><dd>{run.result.verdict.source ?? "Not available"}</dd></div>
              <div><dt>Iterations</dt><dd>{run.result.iterations}</dd></div>
              <div><dt>Grounding sources</dt><dd>{run.result.sources}</dd></div>
              <div><dt>Started</dt><dd>{time(run.started_at)}</dd></div>
              <div><dt>Finished</dt><dd>{time(run.finished_at)}</dd></div>
            </dl>
          )}
          {remediation ? (
            <p className="safety-note"><Link className="investigation-link" href={`/remediations/${id}`}>Review remediation evidence</Link>. A recommendation and an executed change are separate records.</p>
          ) : (
            <p className="safety-note">A completed investigation is advisory. No rollback, code change, deployment, or pull request is implied without a separate remediation record.</p>
          )}
        </article>
      </section>

      <section className="panel progress-panel">
        <div className="panel-heading">
          <div><p className="eyebrow">LIVE SAFE ACTIVITY</p><h2>Agent progress</h2></div>
          <span>{run?.result.progress.length ?? 0} events</span>
        </div>
        {run?.result.progress.length ? (
          <ol className="progress-list">
            {run.result.progress.map((event, index) => (
              <li key={`${event.at}-${event.stage}-${index}`}>
                <span className={`progress-marker progress-${event.status}`} />
                <div>
                  <strong>{event.stage === "tool" ? event.tool?.replaceAll("_", " ") ?? "tool" : event.stage}</strong>
                  <p>{event.status}{event.iteration ? ` · iteration ${event.iteration}` : ""}{event.cause ? ` · ${event.cause.replaceAll("_", " ")}` : ""}</p>
                </div>
                <time dateTime={event.at}>{time(event.at)}</time>
              </li>
            ))}
          </ol>
        ) : (
          <p className="empty">No durable progress events exist for this run. Historical investigations completed before progress tracking was enabled show only their terminal trace.</p>
        )}
        <p className="safety-note">Progress is categorical only. Prompts, reasoning, model prose, tool arguments, tool output, and error text are never included.</p>
      </section>

      <section className="panel trace-panel">
        <div className="panel-heading">
          <div><p className="eyebrow">BOUNDED SAFE TRACE</p><h2>What the agent did</h2></div>
          <span>{run?.result.trace.length ?? 0} tool calls</span>
        </div>
        {run?.result.trace.length ? (
          <ol className="trace-list">
            {run.result.trace.map((step, index) => (
              <li key={`${step.iteration}-${step.tool}-${index}`}>
                <span className={`trace-icon ${step.failed ? "trace-failed" : "trace-good"}`}>{step.failed ? "!" : "✓"}</span>
                <div>
                  <strong>{step.tool.replaceAll("_", " ")}</strong>
                  <p>Iteration {step.iteration} · {step.duration_ms} ms · {step.failed ? step.cause?.replaceAll("_", " ") ?? "failed" : "completed"}</p>
                </div>
              </li>
            ))}
          </ol>
        ) : (
          <p className="empty">No safe tool-call summary is available for this run.</p>
        )}
        <p className="safety-note">Tool arguments, database output, model prose, and provider errors are stripped by the dashboard server before rendering.</p>
      </section>

      <footer>Read-only operator view · UTC · Refreshes every 10 seconds</footer>
    </main>
  );
}
