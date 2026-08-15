const temporaryAdminEmail = "admin@static-log-analysis.local";

export function loginEmail(identity: string): string {
  const normalized = identity.trim().toLowerCase();
  return normalized === "admin" ? temporaryAdminEmail : normalized;
}

export { temporaryAdminEmail };
