export interface ReleaseIdentity {
  commit: string;
  shortCommit: string;
}

const immutableCommit = /^[0-9a-f]{40}$/;

export function getReleaseIdentity(): ReleaseIdentity | null {
  const commit = process.env.SLA_RELEASE_SHA?.trim() ?? "";
  if (!immutableCommit.test(commit)) return null;
  return { commit, shortCommit: commit.slice(0, 12) };
}
