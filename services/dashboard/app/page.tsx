import { headers } from "next/headers";
import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewDashboard } from "@/lib/authorization";
import { getDashboardOverview } from "@/lib/overview";
import { MetricCard, StatusPill } from "@/components/status";
import { RefreshControl } from "@/components/refresh";
import { SignOut } from "@/components/sign-out";

export const dynamic = "force-dynamic";

function total(values: Record<string, number>): number {
  return Object.values(values).reduce((sum, value) => sum + value, 0);
}

function time(value: string): string {
  return new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "short", timeZone: "UTC" }).format(new Date(value));
}

export default async function DashboardPage() {
  const session = await auth.api.getSession({ headers: await headers() });
  if (!session) redirect("/sign-in");
  if (!canViewDashboard(session)) redirect("/unauthorized");

  const overview = await getDashboardOverview();
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
                <thead><tr><th>Service</th><th>Trigger</th><th>Status</th><th>Queued</th></tr></thead>
                <tbody>
                  {analysis.recent_investigations.map((item) => (
                    <tr key={item.investigation_id}>
                      <td><strong>{item.service}</strong><span>{item.environment} · {item.severity}</span></td>
                      <td>{item.trigger_reason.replaceAll("_", " ")}</td>
                      <td><span className="state-tag">{item.state}</span></td>
                      <td>{time(item.queued_at)}</td>
                    </tr>
                  ))}
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

      <footer>Last checked {time(overview.collectedAt)} · UTC · Content-safe operational metadata only</footer>
    </main>
  );
}
