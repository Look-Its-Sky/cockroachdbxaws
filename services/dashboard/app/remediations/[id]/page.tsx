import { headers } from "next/headers";
import Link from "next/link";
import { notFound, redirect } from "next/navigation";
import { z } from "zod";
import { RefreshControl } from "@/components/refresh";
import { SignOut } from "@/components/sign-out";
import { StatusPill } from "@/components/status";
import { auth } from "@/lib/auth";
import { canViewDashboard } from "@/lib/authorization";
import { getAgentRemediation, verificationSummary } from "@/lib/agent";

export const dynamic = "force-dynamic";

type Props = { params: Promise<{ id: string }> };

function time(value: string | undefined): string {
  if (!value) return "Not finished";
  return new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "medium", timeZone: "UTC" }).format(new Date(value));
}

export default async function RemediationPage({ params }: Props) {
  const session = await auth.api.getSession({ headers: await headers() });
  if (!session) redirect("/sign-in");
  if (!canViewDashboard(session)) redirect("/unauthorized");

  const { id } = await params;
  if (!z.string().uuid().safeParse(id).success) notFound();
  const state = await getAgentRemediation(id);
  if (state.available && !state.value) notFound();
  const remediation = state.available ? state.value : null;

  return (
    <main className="shell">
      <header className="topbar">
        <Link className="brand-mark" href="/" aria-label="Operations overview">SL</Link>
        <div className="brand-copy"><strong>Remediation detail</strong><span>Sandbox evidence, not authorization</span></div>
        <div className="top-actions"><Link className="nav-link" href="/remediations">All remediations</Link><RefreshControl /><SignOut /></div>
      </header>

      <section className="detail-hero">
        <div><Link className="back-link" href={`/investigations/${id}`}>← Investigation</Link><p className="eyebrow">REMEDIATION EXECUTION</p><h1>{remediation?.service_id ?? "Unavailable"}</h1><code>{id}</code></div>
        <div className="health-cluster"><StatusPill healthy={state.available} label="Agent API" /><StatusPill healthy={Boolean(remediation && remediation.status === "done")} label={remediation ? `Execution ${remediation.status}` : "No execution record"} /></div>
      </section>

      {!state.available ? <section className="panel"><p className="empty">Remediation data is {state.reason}. The detector and investigation records remain independently available.</p></section> : remediation ? <>
        <section className="detail-grid">
          <article className="panel detail-panel">
            <div className="panel-heading"><div><p className="eyebrow">WHAT RAN</p><h2>Execution record</h2></div></div>
            <dl className="fact-grid">
              <div><dt>Status</dt><dd><span className={`state-tag state-${remediation.status}`}>{remediation.status}</span></dd></div>
              <div><dt>Started</dt><dd>{time(remediation.started_at)}</dd></div>
              <div><dt>Finished</dt><dd>{time(remediation.finished_at)}</dd></div>
              <div><dt>Candidate fixes</dt><dd>{remediation.candidates.length}</dd></div>
              <div><dt>Engineer decision</dt><dd>{remediation.decided ? "Recorded" : "Not recorded"}</dd></div>
              <div><dt>Repository</dt><dd>{remediation.repository ? `${remediation.repository.owner}/${remediation.repository.repo}` : "Not available"}</dd></div>
            </dl>
          </article>

          <article className="panel detail-panel">
            <div className="panel-heading"><div><p className="eyebrow">CAUSE GATE</p><h2>Triage</h2></div></div>
            {remediation.triage ? <>
              <dl className="fact-grid">
                <div><dt>Finding</dt><dd>{remediation.triage.status.replaceAll("_", " ")}</dd></div>
                <div><dt>Commit</dt><dd>{remediation.triage.commit ?? "Not established"}</dd></div>
                <div><dt>Commit exists</dt><dd>{remediation.triage.commit_exists ? "Verified" : "No"}</dd></div>
                <div><dt>Confidence</dt><dd>{remediation.triage.confidence === undefined ? "Not declared" : `${Math.round(remediation.triage.confidence * 100)}%`}</dd></div>
              </dl>
              {remediation.triage.evidence && <p className="bounded-prose">{remediation.triage.evidence}</p>}
            </> : <p className="empty">Triage has not produced a result.</p>}
          </article>
        </section>

        <section className="panel candidates-panel">
          <div className="panel-heading"><div><p className="eyebrow">RANKED OUTPUT</p><h2>Candidate fixes</h2></div><span>{remediation.candidates.length}</span></div>
          {remediation.candidates.length ? <div className="candidate-list">{remediation.candidates.map((candidate, index) => <article className="candidate-card" key={candidate.id}>
            <div className="candidate-heading">
              <div><span className="candidate-rank">{String(index + 1).padStart(2, "0")}</span><p className="eyebrow">{candidate.strategy.replaceAll("-", " ")}</p><h3>{candidate.summary ?? "No summary supplied"}</h3></div>
              <span className={`state-tag state-${candidate.status}`}>{candidate.status.replaceAll("_", " ")}</span>
            </div>
            {candidate.rationale && <p className="candidate-rationale">{candidate.rationale}</p>}
            <div className="verification-callout"><strong>What was verified</strong><p>{verificationSummary(candidate.verification)}</p></div>
            <dl className="candidate-facts">
              <div><dt>Files</dt><dd>{candidate.files.length ? candidate.files.join(", ") : "None recorded"}</dd></div>
              <div><dt>Repair attempts</dt><dd>{candidate.repairs}</dd></div>
              <div><dt>Sandbox duration</dt><dd>{candidate.verification.duration_ms} ms</dd></div>
              <div><dt>Created</dt><dd>{time(candidate.created_at)}</dd></div>
            </dl>
            {candidate.diff ? <details className="diff-disclosure"><summary>Review bounded diff</summary><pre>{candidate.diff}</pre></details> : <p className="candidate-missing">No usable diff was stored for this candidate.</p>}
            {candidate.pr_url && <p className="candidate-link"><a href={candidate.pr_url} target="_blank" rel="noreferrer">Open recorded draft PR ↗</a></p>}
          </article>)}</div> : <p className="empty">No candidate was produced. A stopped triage is a valid outcome; it means the agent declined to spend sandbox work on an unsupported commit.</p>}
        </section>

        <section className="panel decision-panel">
          <div className="panel-heading"><div><p className="eyebrow">HUMAN AUTHORITY</p><h2>Engineer decision</h2></div></div>
          {remediation.decision ? <dl className="fact-grid">
            <div><dt>Chosen candidate</dt><dd>{remediation.decision.chosen_candidate_id ?? "All candidates rejected"}</dd></div>
            <div><dt>Recorded</dt><dd>{time(remediation.decision.decided_at)}</dd></div>
            <div><dt>Available to future runs</dt><dd>{remediation.decision.indexed_at ? "Indexed" : "Not indexed"}</dd></div>
          </dl> : <p className="empty">No engineer has authorized or recorded a choice. Model output is not approval.</p>}
          <p className="safety-note">Raw build logs, provider errors, executable repository commands, embedded decision documents, and full source files are excluded at the dashboard server boundary.</p>
        </section>
      </> : null}

      <footer>Read-only operator view · UTC · Evidence does not equal deployment</footer>
    </main>
  );
}
