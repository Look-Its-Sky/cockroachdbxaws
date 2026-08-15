type DashboardSession = {
  user: {
    role?: string | null;
    banned?: boolean | null;
  };
};

// canViewDashboard is deliberately stricter than "has a session". Accounts
// live in the EC2-local auth database, public signup is disabled, and only an
// explicitly provisioned admin role may read operational data.
export function canViewDashboard(session: DashboardSession | null | undefined): boolean {
  if (!session || session.user.banned) return false;
  return (session.user.role ?? "")
    .split(",")
    .map((role) => role.trim())
    .includes("admin");
}
