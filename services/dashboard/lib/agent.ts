import { z } from "zod";

export type AgentComponentState<T> =
  | { available: true; value: T }
  | { available: false; reason: "unavailable" | "misconfigured" };

const requestTimeoutMS = 4_000;
const maxResponseBytes = 256 * 1024;
const maxOverviewInvestigations = 10;
const maxRemediations = 50;

const agentHealthSchema = z.object({ message: z.literal("pong") });

const traceStepSchema = z.object({
  iteration: z.number().int().nonnegative().max(100),
  tool: z.string().min(1).max(64),
  duration_ms: z.number().int().nonnegative().max(3_600_000),
  failed: z.boolean(),
  cause: z.enum([
    "malformed_call",
    "unknown_tool",
    "invalid_arguments",
    "tool_error",
    "rejected",
    "transport",
  ]).optional(),
});

const progressSchema = z.object({
  stage: z.enum(["context", "model", "tool", "verdict"]),
  status: z.enum(["started", "completed", "failed"]),
  iteration: z.number().int().positive().max(100).optional(),
  tool: z.string().min(1).max(64).optional(),
  cause: z.enum([
    "malformed_call",
    "unknown_tool",
    "invalid_arguments",
    "tool_error",
    "rejected",
    "transport",
  ]).optional(),
  at: z.string().datetime(),
});

const verdictSchema = z.object({
  decision: z.enum(["", "ROLLBACK", "HOTFIX", "UNKNOWN"]).transform((value) => value || "UNKNOWN"),
  commit_sha: z.string().max(40).optional(),
  service: z.string().max(128).optional(),
  confidence: z.number().min(0).max(1).optional(),
  source: z.enum(["declared", "inferred", "absent"]).optional(),
});

const resultSchema = z.object({
  verdict: verdictSchema,
  sources: z.number().int().nonnegative().max(10_000),
  iterations: z.number().int().nonnegative().max(100),
  trace: z.array(traceStepSchema).max(50).nullable().transform((value) => value ?? []),
  progress: z.array(progressSchema).max(50).nullish().transform((value) => value ?? []),
  truncated: z.boolean(),
});

const repositorySchema = z.object({
  service_id: z.string().min(1).max(128),
  owner: z.string().min(1).max(128),
  repo: z.string().min(1).max(128),
  default_branch: z.string().min(1).max(128),
  subdirectory: z.string().max(240).optional(),
  runtime_image: z.string().min(1).max(240),
});

const safeHTTPSURL = z.string().url().max(500).refine((value) => new URL(value).protocol === "https:");

const repositoryCapabilitySchema = z.object({
  repository: repositorySchema,
  has_tests: z.boolean(),
  url: safeHTTPSURL,
}).transform(({ repository, ...capability }) => ({ ...repository, ...capability }));

const triageListSchema = z.object({
  status: z.enum(["confirmed", "inconclusive", "not_present", "commit_missing"]),
  commit: z.string().max(64).optional(),
});

const triageDetailSchema = triageListSchema.extend({
  commit_exists: z.boolean(),
  commit_subject: z.string().max(500).optional(),
  files: z.array(z.string().max(240)).max(40).optional().transform((value) => value ?? []),
  evidence: z.string().max(8_000).optional(),
  confidence: z.number().min(0).max(1).optional(),
});

const remediationStatusSchema = z.enum(["running", "done", "stopped", "failed"]);

const verificationSchema = z.object({
  applied: z.boolean(),
  build_ran: z.boolean(),
  built: z.boolean(),
  test_ran: z.boolean(),
  tested: z.boolean(),
  timed_out: z.boolean(),
  duration_ms: z.number().int().nonnegative().max(3_600_000),
});

const candidateSchema = z.object({
  id: z.string().uuid(),
  investigation_id: z.string().uuid(),
  incident_id: z.string().max(128).optional(),
  service_id: z.string().max(128).optional(),
  strategy: z.enum(["minimal", "defensive", "root-cause"]),
  summary: z.string().max(2_000).optional(),
  rationale: z.string().max(6_000).optional(),
  files: z.array(z.string().max(240)).max(40).optional().transform((value) => value ?? []),
  diff: z.string().max(96 * 1024).optional(),
  verification: verificationSchema,
  repairs: z.number().int().nonnegative().max(10).optional().transform((value) => value ?? 0),
  status: z.enum(["proposed", "selected", "rejected", "pr_opened", "failed"]),
  pr_url: safeHTTPSURL.optional(),
  rejection_reason: z.string().max(1_000).optional(),
  created_at: z.string().datetime(),
});

const decisionSchema = z.object({
  id: z.string().uuid(),
  chosen_candidate_id: z.string().uuid().optional(),
  decided_at: z.string().datetime(),
  indexed_at: z.string().datetime().optional(),
});

const remediationListItemSchema = z.object({
  investigation_id: z.string().uuid(),
  incident_id: z.string().max(128).optional(),
  service_id: z.string().max(128).optional(),
  status: remediationStatusSchema,
  triage: triageListSchema.optional(),
  candidate_count: z.number().int().nonnegative().max(100).optional().transform((value) => value ?? 0),
  decided: z.boolean(),
  started_at: z.string().datetime(),
  finished_at: z.string().datetime().optional(),
});

const remediationDetailSchema = z.object({
  investigation_id: z.string().uuid(),
  incident_id: z.string().max(128).optional(),
  service_id: z.string().max(128).optional(),
  status: remediationStatusSchema,
  repository: repositorySchema.optional(),
  triage: triageDetailSchema.optional(),
  candidates: z.array(candidateSchema).max(6).optional().transform((value) => value ?? []),
  decided: z.boolean(),
  decision: decisionSchema.optional(),
  started_at: z.string().datetime(),
  finished_at: z.string().datetime().optional(),
});

const repositoriesResponseSchema = z.object({
  repositories: z.array(repositoryCapabilitySchema).max(100),
}).transform((value) => value.repositories);

const remediationsResponseSchema = z.object({
  remediations: z.array(remediationListItemSchema).max(maxRemediations).nullable(),
}).transform((value) => value.remediations ?? []);

export const agentInvestigationSchema = z.object({
  investigation_id: z.string().uuid(),
  incident_id: z.string().min(1).max(128),
  correlation_id: z.string().uuid().optional(),
  service_id: z.string().max(128).optional(),
  status: z.enum(["running", "done", "failed", "retrying"]),
  result: resultSchema,
  started_at: z.string().datetime(),
  finished_at: z.string().datetime().optional(),
});

export type AgentInvestigation = z.infer<typeof agentInvestigationSchema>;
export type AgentInvestigationState = AgentComponentState<AgentInvestigation | null>;
export type AgentRepository = z.infer<typeof repositoryCapabilitySchema>;
export type AgentRemediationListItem = z.infer<typeof remediationListItemSchema>;
export type AgentRemediation = z.infer<typeof remediationDetailSchema>;
export type AgentVerification = z.infer<typeof verificationSchema>;

function baseURL(): string | null {
  const raw = process.env.AGENT_API_URL?.trim();
  if (!raw) return null;
  try {
    const parsed = new URL(raw);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return null;
    return parsed.toString().replace(/\/$/, "");
  } catch {
    return null;
  }
}

async function boundedJSON(response: Response): Promise<unknown> {
  const declared = Number(response.headers.get("content-length") ?? 0);
  if (declared > maxResponseBytes) throw new Error("unavailable");
  const text = await response.text();
  if (new TextEncoder().encode(text).byteLength > maxResponseBytes) throw new Error("unavailable");
  return JSON.parse(text);
}

function unavailable<T>(): AgentComponentState<T> {
  return { available: false, reason: "unavailable" };
}

async function authenticatedGet<T>(path: string, schema: z.ZodType<T>): Promise<AgentComponentState<T>> {
  const base = baseURL();
  const token = process.env.AGENT_API_TOKEN?.trim();
  if (!base || !token) return { available: false, reason: "misconfigured" };

  try {
    const response = await fetch(`${base}${path}`, {
      cache: "no-store",
      headers: { "X-Agent-Token": token },
      signal: AbortSignal.timeout(requestTimeoutMS),
    });
    if (!response.ok) return unavailable();
    return { available: true, value: schema.parse(await boundedJSON(response)) };
  } catch {
    return unavailable();
  }
}

export async function getAgentHealth(): Promise<AgentComponentState<{ ready: boolean }>> {
  const base = baseURL();
  if (!base) return { available: false, reason: "misconfigured" };
  try {
    const response = await fetch(`${base}/ping`, {
      cache: "no-store",
      signal: AbortSignal.timeout(requestTimeoutMS),
    });
    if (!response.ok) return unavailable();
    agentHealthSchema.parse(await boundedJSON(response));
    return { available: true, value: { ready: true } };
  } catch {
    return unavailable();
  }
}

export async function getAgentInvestigation(investigationID: string): Promise<AgentInvestigationState> {
  const base = baseURL();
  const token = process.env.AGENT_API_TOKEN?.trim();
  if (!base || !token) return { available: false, reason: "misconfigured" };
  if (!z.string().uuid().safeParse(investigationID).success) return unavailable();

  try {
    const response = await fetch(`${base}/agent/${investigationID}`, {
      cache: "no-store",
      headers: { "X-Agent-Token": token },
      signal: AbortSignal.timeout(requestTimeoutMS),
    });
    if (response.status === 404) return { available: true, value: null };
    if (!response.ok) return unavailable();
    return { available: true, value: agentInvestigationSchema.parse(await boundedJSON(response)) };
  } catch {
    return unavailable();
  }
}

export async function getAgentRepositories(): Promise<AgentComponentState<AgentRepository[]>> {
  return authenticatedGet("/repositories", repositoriesResponseSchema);
}

export async function getAgentRemediations(limit = maxRemediations): Promise<AgentComponentState<AgentRemediationListItem[]>> {
  const boundedLimit = Number.isInteger(limit) && limit > 0 && limit <= maxRemediations ? limit : maxRemediations;
  return authenticatedGet(`/remediations?limit=${boundedLimit}`, remediationsResponseSchema);
}

export async function getAgentRemediation(investigationID: string): Promise<AgentComponentState<AgentRemediation | null>> {
  const base = baseURL();
  const token = process.env.AGENT_API_TOKEN?.trim();
  if (!base || !token) return { available: false, reason: "misconfigured" };
  if (!z.string().uuid().safeParse(investigationID).success) return unavailable();

  try {
    const response = await fetch(`${base}/agent/${investigationID}/remediation`, {
      cache: "no-store",
      headers: { "X-Agent-Token": token },
      signal: AbortSignal.timeout(requestTimeoutMS),
    });
    if (response.status === 404) return { available: true, value: null };
    if (!response.ok) return unavailable();
    return { available: true, value: remediationDetailSchema.parse(await boundedJSON(response)) };
  } catch {
    return unavailable();
  }
}

export function verificationSummary(value: AgentVerification): string {
  switch (true) {
    case !value.applied:
      return "the change could not be applied to a clean checkout";
    case value.test_ran && value.tested:
      return "builds, and the service's tests pass";
    case value.test_ran && !value.tested:
      return "builds, but the service's tests fail";
    case value.build_ran && value.built && !value.test_ran:
      return "builds; this service declares no tests, so nothing was proven beyond that it compiles";
    case value.build_ran && !value.built:
      return "does not build";
    case value.timed_out:
      return "applied, but the sandbox hit its time limit before it could be verified";
    default:
      return "applied, but this service declares neither a build nor a test command, so nothing was verified";
  }
}

export function recommendationLabel(run: AgentInvestigation | null): string {
  if (!run || run.status !== "done" || run.result.verdict.decision === "UNKNOWN") return "No recommendation";
  return `Recommend ${run.result.verdict.decision.toLowerCase()}`;
}

export function executionSummary(run: AgentInvestigation | null, remediation: AgentRemediation | null): string {
  if (remediation) return `Remediation ${remediation.status}`;
  if (run?.status === "done" && run.result.verdict.decision === "ROLLBACK") return "Not run — recommendation only";
  return "Not started";
}

export async function getAgentInvestigations(
  investigationIDs: readonly string[],
): Promise<Record<string, AgentInvestigationState>> {
  const ids = [...new Set(investigationIDs)].slice(0, maxOverviewInvestigations);
  const states = await Promise.all(ids.map((id) => getAgentInvestigation(id)));
  return Object.fromEntries(ids.map((id, index) => [id, states[index]]));
}
