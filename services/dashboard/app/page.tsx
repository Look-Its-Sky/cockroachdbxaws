import { headers } from "next/headers";
import Link from "next/link";
import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewDashboard } from "@/lib/authorization";
import { getDashboardOverview } from "@/lib/overview";
import { MetricCard, StatusPill } from "@/components/status";
import { RefreshControl } from "@/components/refresh";
import { SignOut } from "@/components/sign-out";
import { recommendationLabel, type AgentInvestigationState } from "@/lib/agent";
import { getReleaseIdentity } from "@/lib/release";

export const dynamic = "force-dynamic";

function total(values: Record<string, number>): number {
  return Object.values(values).reduce((sum, value) => sum + value, 0);
}

function time(value: string): string {
  return new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "short", timeZone: "UTC" }).format(new Date(value));
}

function agentStatus(state: AgentInvestigationState | undefined): string {
  if (!state) return "not checked";
  if (!state.available) return state.reason;
  return state.value?.status ?? "no agent record";
}

function verdict(state: AgentInvestigationState | undefined): string {
  if (!state?.available || !state.value) return "—";
  return recommendationLabel(state.value);
}

export default async function DashboardPage() {
  const session = await auth.api.getSession({ headers: await headers() });
  if (!session) redirect("/sign-in");
  if (!canViewDashboard(session)) redirect("/unauthorized");

  const overview = await getDashboardOverview();
  const release = getReleaseIdentity();
  const analysis = overview.analysis.available ? overview.analysis.value : null;
  const queues = overview.queues.available ? overview.queues.value : null;
  const metrics = overview.cloudwatch.available ? overview.cloudwatch.value.metrics : {};
  const rejected = Object.entries(metrics)
    .filter(([name]) => name.includes("rejected_") || name.endsWith("rejections_total"))
    .reduce((sum, [, value]) => sum + value, 0);

  return (
    <main className="shell">
      <header className="topbar">
        <div className="brand-mark">SL</div>
        <div className="brand-copy">
          <strong>Static Log Analysis</strong>
          <span>Operations overview</span>
        </div>
        <div className="top-actions">
          <Link className="nav-link" href="/remediations">Remediations</Link>
          <RefreshControl />
          <SignOut />
        </div>
      </header>

      <section className="hero">
        <div>
          <p className="eyebrow">SYSTEM PULSE</p>
          <h1>From signal to investigation.</h1>
          <p>Safe, read-only visibility across ingestion, incident grouping, and agent delivery.</p>
        </div>
        <div className="health-cluster">
          <StatusPill healthy={overview.cloudwatch.available && overview.cloudwatch.value.ready} label="CloudWatch worker" />
          <StatusPill healthy={overview.outbox.available && overview.outbox.value.ready} label="Outbox worker" />
          <StatusPill healthy={overview.analysis.available} label="CockroachDB" />
          <StatusPill healthy={overview.queues.available && (queues?.deadLetter ?? 1) === 0} label="Agent queue" />
          <StatusPill healthy={overview.agent.available && overview.agent.value.ready} label="Agent runtime" />
        </div>
      </section>

      <section className="metrics-grid" aria-label="Pipeline totals">
        <MetricCard label="Records observed" value={analysis?.records_seen ?? "—"} note="Deduplicated at admission" />
        <MetricCard label="Incident families" value={analysis?.incident_families ?? "—"} note="Related errors grouped" />
        <MetricCard label="Investigations" value={analysis ? total(analysis.investigations) : "—"} note="Across every state" />
        <MetricCard label="Ready for agents" value={queues?.available ?? "—"} note={`${queues?.inFlight ?? 0} currently in flight`} />
      </section>

      <section className="content-grid">
        <article className="panel investigations">
          <div className="panel-heading">
            <div><p className="eyebrow">LATEST WORK</p><h2>Recent investigations</h2></div>
            <span>{analysis?.recent_investigations.length ?? 0} shown</span>
          </div>
          {analysis?.recent_investigations.length ? (
            <div className="table-wrap">
              <table>
                <thead><tr><th>Service</th><th>Signal</th><th>Detection</th><th>Agent</th><th>Recommendation</th><th>Queued</th></tr></thead>
                <tbody>
                  {analysis.recent_investigations.map((item) => {
                    const run = overview.agentInvestigations[item.investigation_id];
                    return (
                    <tr key={item.investigation_id}>
                      <td><Link className="investigation-link" href={`/investigations/${item.investigation_id}`}>{item.service}</Link><span>{item.environment}</span></td>
                      <td><strong className="severity-label">{item.severity}</strong><span>{item.trigger_reason.replaceAll("_", " ")}</span></td>
                      <td><span className="state-tag">{item.state}</span></td>
                      <td><span className={`state-tag state-${agentStatus(run).replaceAll(" ", "-")}`}>{agentStatus(run)}</span></td>
                      <td><strong>{verdict(run)}</strong></td>
                      <td>{time(item.queued_at)}</td>
                    </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          ) : <p className="empty">No investigations are available yet.</p>}
        </article>

        <aside className="panel pipeline-panel">
          <div className="panel-heading"><div><p className="eyebrow">DELIVERY</p><h2>Pipeline health</h2></div></div>
          <div className="pipeline-step"><span>01</span><div><strong>Durable journal</strong><p>{metrics.static_log_analysis_journal_pending_records ?? "—"} pending · {metrics.static_log_analysis_journal_bytes ?? "—"} bytes</p></div></div>
          <div className="pipeline-line" />
          <div className="pipeline-step"><span>02</span><div><strong>Static detection</strong><p>{rejected} safely rejected · {metrics.static_log_analysis_quarantined_total ?? 0} quarantined</p></div></div>
          <div className="pipeline-line" />
          <div className="pipeline-step"><span>03</span><div><strong>Transactional outbox</strong><p>{analysis?.outbox.pending ?? 0} pending · {analysis?.outbox.published ?? 0} published</p></div></div>
          <div className="pipeline-line" />
          <div className="pipeline-step"><span>04</span><div><strong>Amazon SQS</strong><p>{queues?.available ?? "—"} available · {queues?.deadLetter ?? "—"} dead-lettered</p></div></div>
        </aside>
      </section>

      <footer title={release?.commit}>Release {release?.shortCommit ?? "unidentified"} · Last checked {time(overview.collectedAt)} · UTC · Content-safe operational metadata only</footer>
    </main>
  );
}
