import { headers } from "next/headers";
import Link from "next/link";
import { redirect } from "next/navigation";
import { RefreshControl } from "@/components/refresh";
import { SignOut } from "@/components/sign-out";
import { StatusPill } from "@/components/status";
import { auth } from "@/lib/auth";
import { canViewDashboard } from "@/lib/authorization";
import { getAgentRemediations, getAgentRepositories } from "@/lib/agent";
import { getReleaseIdentity } from "@/lib/release";

export const dynamic = "force-dynamic";

function time(value: string | undefined): string {
  if (!value) return "Not finished";
  return new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "short", timeZone: "UTC" }).format(new Date(value));
}

export default async function RemediationsPage() {
  const session = await auth.api.getSession({ headers: await headers() });
  if (!session) redirect("/sign-in");
  if (!canViewDashboard(session)) redirect("/unauthorized");

  const [runsState, repositoriesState] = await Promise.all([getAgentRemediations(), getAgentRepositories()]);
  const release = getReleaseIdentity();
  const runs = runsState.available ? runsState.value : [];
  const repositories = repositoriesState.available ? repositoriesState.value : [];

  return (
    <main className="shell">
      <header className="topbar">
        <Link className="brand-mark" href="/" aria-label="Operations overview">SL</Link>
        <div className="brand-copy"><strong>Remediation evidence</strong><span>Read-only agent output</span></div>
        <div className="top-actions"><Link className="nav-link" href="/">Overview</Link><RefreshControl /><SignOut /></div>
      </header>

      <section className="detail-hero">
        <div><p className="eyebrow">REVIEW QUEUE</p><h1>Evidence before action.</h1><p className="hero-copy">Candidate fixes and sandbox verification are proposals. Nothing on this page authorizes or claims a deployment.</p></div>
        <div className="health-cluster"><StatusPill healthy={runsState.available} label="Remediation journal" /><StatusPill healthy={repositoriesState.available} label="Repository mappings" /></div>
      </section>

      <section className="content-grid remediation-grid">
        <article className="panel investigations">
          <div className="panel-heading"><div><p className="eyebrow">RECENT RUNS</p><h2>Remediations</h2></div><span>{runs.length} shown</span></div>
          {!runsState.available ? <p className="empty">Remediation data is {runsState.reason}. Analysis health remains independent.</p> : runs.length ? (
            <div className="table-wrap"><table>
              <thead><tr><th>Service</th><th>Execution</th><th>Triage</th><th>Candidates</th><th>Decision</th><th>Started</th></tr></thead>
              <tbody>{runs.map((run) => <tr key={run.investigation_id}>
                <td><Link className="investigation-link" href={`/remediations/${run.investigation_id}`}>{run.service_id ?? "Unknown service"}</Link><span>{run.investigation_id}</span></td>
                <td><span className={`state-tag state-${run.status}`}>{run.status}</span></td>
                <td><strong>{run.triage?.status.replaceAll("_", " ") ?? "Pending"}</strong><span>{run.triage?.commit ?? "No commit established"}</span></td>
                <td><strong>{run.candidate_count}</strong><span>stored proposals</span></td>
                <td><strong>{run.decided ? "Recorded" : "Awaiting review"}</strong></td>
                <td>{time(run.started_at)}</td>
              </tr>)}</tbody>
            </table></div>
          ) : <p className="empty">No remediation has run. Rollback recommendations do not create remediation records.</p>}
        </article>

        <aside className="panel mapping-panel">
          <div className="panel-heading"><div><p className="eyebrow">SOURCE BOUNDARY</p><h2>Mapped services</h2></div><span>{repositories.length}</span></div>
          {!repositoriesState.available ? <p className="empty">Repository mappings are {repositoriesState.reason}.</p> : repositories.length ? (
            <ul className="mapping-list">{repositories.map((repository) => <li key={repository.service_id}>
              <div><strong>{repository.service_id}</strong><span>{repository.owner}/{repository.repo} · {repository.default_branch}</span></div>
              <span className={`capability ${repository.has_tests ? "capability-strong" : "capability-limited"}`}>{repository.has_tests ? "Build + tests" : "No declared tests"}</span>
            </li>)}</ul>
          ) : <p className="empty">No service is mapped to a repository, so remediation cannot start.</p>}
          <p className="safety-note">Executable setup, build, and test commands remain server-side. This view exposes capability only.</p>
        </aside>
      </section>
      <footer title={release?.commit}>Release {release?.shortCommit ?? "unidentified"} · Read-only operator view · UTC · Recommendations are not execution</footer>
    </main>
  );
}
