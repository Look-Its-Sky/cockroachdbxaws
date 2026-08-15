export function StatusPill({ healthy, label }: { healthy: boolean; label: string }) {
  return <span className={`status ${healthy ? "status-good" : "status-bad"}`}>{label}</span>;
}

export function MetricCard({ label, value, note }: { label: string; value: string | number; note: string }) {
  return (
    <article className="metric-card">
      <p>{label}</p>
      <strong>{value}</strong>
      <span>{note}</span>
    </article>
  );
}
