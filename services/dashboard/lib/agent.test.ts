import { afterEach, describe, expect, it, vi } from "vitest";
import {
  getAgentHealth,
  getAgentInvestigation,
  getAgentInvestigations,
  getAgentRemediation,
  getAgentRepositories,
  getAgentRemediations,
  executionSummary,
  recommendationLabel,
  verificationSummary,
} from "./agent";

const investigationID = "019fe44a-c712-745b-b804-5e7604f02b79";

function upstreamRecord() {
  return {
    investigation_id: investigationID,
    incident_id: "07dc8373c25fec51c145d7ce1827d682fbdc81c0aa2d7ab1a8305b51dfe9aa36",
    correlation_id: investigationID,
    service_id: "frontend-web",
    status: "done",
    result: {
      answer: "raw model prose must not cross the dashboard boundary",
      verdict: { decision: "ROLLBACK", confidence: 0.72, source: "declared" },
      sources: 2,
      iterations: 1,
      trace: [
        {
          iteration: 1,
          tool: "select_query",
          arguments: { query: "SELECT secret FROM private" },
          raw_input: "private input",
          output: "private output",
          duration_ms: 42,
          failed: false,
        },
      ],
      progress: [{
        stage: "tool",
        status: "started",
        iteration: 1,
        tool: "select_query",
        at: "2026-08-16T06:49:01Z",
        arguments: { query: "SELECT secret FROM private" },
        output: "private output",
        model_text: "private reasoning",
      }],
      grounding: ["private precedent"],
      truncated: false,
    },
    started_at: "2026-08-16T06:48:59Z",
    finished_at: "2026-08-16T06:49:08Z",
    error: "private provider error",
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.AGENT_API_URL;
  delete process.env.AGENT_API_TOKEN;
});

describe("agent dashboard boundary", () => {
  it("keeps authentication server-side and strips unsafe agent fields", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    const fetchMock = vi.fn(async (_input: string | URL | Request, init?: RequestInit) => {
      expect(init?.headers).toEqual({ "X-Agent-Token": "server-only-token" });
      return Response.json(upstreamRecord());
    });
    vi.stubGlobal("fetch", fetchMock);

    const state = await getAgentInvestigation(investigationID);

    expect(state).toEqual({
      available: true,
      value: {
        investigation_id: investigationID,
        incident_id: "07dc8373c25fec51c145d7ce1827d682fbdc81c0aa2d7ab1a8305b51dfe9aa36",
        correlation_id: investigationID,
        service_id: "frontend-web",
        status: "done",
        result: {
          verdict: { decision: "ROLLBACK", confidence: 0.72, source: "declared" },
          sources: 2,
          iterations: 1,
          trace: [{ iteration: 1, tool: "select_query", duration_ms: 42, failed: false }],
          progress: [{
            stage: "tool",
            status: "started",
            iteration: 1,
            tool: "select_query",
            at: "2026-08-16T06:49:01Z",
          }],
          truncated: false,
        },
        started_at: "2026-08-16T06:48:59Z",
        finished_at: "2026-08-16T06:49:08Z",
      },
    });
    expect(fetchMock).toHaveBeenCalledWith(
      `http://agent.internal:8080/agent/${investigationID}`,
      expect.objectContaining({ cache: "no-store" }),
    );
  });

  it("treats a valid not-found response as not delivered yet", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    vi.stubGlobal("fetch", vi.fn(async () => new Response("not found", { status: 404 })));

    await expect(getAgentInvestigation(investigationID)).resolves.toEqual({ available: true, value: null });
  });

  it("bounds overview fan-out and keeps one failed lookup from hiding siblings", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    const ids = Array.from({ length: 14 }, (_, index) =>
      `019fe44a-c712-7${String(index).padStart(3, "0")}-b804-5e7604f02b79`,
    );
    const fetchMock = vi.fn(async (input: string | URL | Request) => {
      const id = String(input).split("/").at(-1);
      if (id === ids[1]) return new Response("upstream failed", { status: 503 });
      return Response.json({ ...upstreamRecord(), investigation_id: id, correlation_id: id });
    });
    vi.stubGlobal("fetch", fetchMock);

    const runs = await getAgentInvestigations(ids);

    expect(fetchMock).toHaveBeenCalledTimes(10);
    expect(Object.keys(runs)).toHaveLength(10);
    expect(runs[ids[0]]?.available).toBe(true);
    expect(runs[ids[1]]).toEqual({ available: false, reason: "unavailable" });
  });

  it("reports health independently from authenticated investigation reads", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080/";
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ message: "pong" })));

    await expect(getAgentHealth()).resolves.toEqual({ available: true, value: { ready: true } });
    await expect(getAgentInvestigation(investigationID)).resolves.toEqual({ available: false, reason: "misconfigured" });
  });

  it("rejects an upstream body above the dashboard response ceiling", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    vi.stubGlobal("fetch", vi.fn(async () => new Response("x".repeat(256 * 1024 + 1))));

    await expect(getAgentInvestigation(investigationID)).resolves.toEqual({ available: false, reason: "unavailable" });
  });

  it("admits bounded repository capability metadata and strips executable commands", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({
      count: 1,
      repositories: [{
        repository: {
          service_id: "frontend-web",
          owner: "example",
          repo: "frontend",
          default_branch: "main",
          subdirectory: "apps/web",
          runtime_image: "node:24",
          setup_command: "private setup command",
          build_command: "private build command",
          test_command: "private test command",
        },
        has_tests: true,
        url: "https://github.com/example/frontend",
      }],
    })));

    await expect(getAgentRepositories()).resolves.toEqual({
      available: true,
      value: [{
        service_id: "frontend-web",
        owner: "example",
        repo: "frontend",
        default_branch: "main",
        subdirectory: "apps/web",
        runtime_image: "node:24",
        has_tests: true,
        url: "https://github.com/example/frontend",
      }],
    });
  });

  it("keeps remediation lists small and excludes candidates and raw errors", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    const fetchMock = vi.fn(async () => Response.json({
      count: 1,
      remediations: [{
        investigation_id: investigationID,
        incident_id: "incident-1",
        service_id: "frontend-web",
        status: "failed",
        triage: { status: "confirmed", commit: "abc1234", evidence: "private source excerpt" },
        candidate_count: 3,
        candidates: [{ diff: "must not cross a list boundary" }],
        decided: false,
        error: "private provider error",
        started_at: "2026-08-16T06:48:59Z",
        finished_at: "2026-08-16T06:49:08Z",
      }],
    }));
    vi.stubGlobal("fetch", fetchMock);

    const result = await getAgentRemediations(999);

    expect(fetchMock).toHaveBeenCalledWith(
      "http://agent.internal:8080/remediations?limit=50",
      expect.objectContaining({ headers: { "X-Agent-Token": "server-only-token" } }),
    );
    expect(result).toEqual({
      available: true,
      value: [{
        investigation_id: investigationID,
        incident_id: "incident-1",
        service_id: "frontend-web",
        status: "failed",
        triage: { status: "confirmed", commit: "abc1234" },
        candidate_count: 3,
        decided: false,
        started_at: "2026-08-16T06:48:59Z",
        finished_at: "2026-08-16T06:49:08Z",
      }],
    });
  });

  it("normalizes the Go API's nil remediation slice to an empty list", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ count: 0, remediations: null })));

    await expect(getAgentRemediations()).resolves.toEqual({ available: true, value: [] });
  });

  it("returns bounded remediation review facts while stripping raw logs and decision documents", async () => {
    process.env.AGENT_API_URL = "http://agent.internal:8080";
    process.env.AGENT_API_TOKEN = "server-only-token";
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({
      investigation_id: investigationID,
      incident_id: "incident-1",
      service_id: "frontend-web",
      status: "done",
      repository: {
        service_id: "frontend-web",
        owner: "example",
        repo: "frontend",
        default_branch: "main",
        runtime_image: "node:24",
        setup_command: "private setup command",
      },
      triage: {
        status: "confirmed",
        commit: "abc1234",
        commit_exists: true,
        commit_subject: "fix frontend failure",
        files: ["apps/web/page.tsx"],
        evidence: "bounded explanation",
        confidence: 0.8,
      },
      candidates: [{
        id: "019fe44a-c712-745b-b804-5e7604f02b80",
        investigation_id: investigationID,
        incident_id: "incident-1",
        service_id: "frontend-web",
        strategy: "minimal",
        summary: "Guard the missing value.",
        rationale: "The failure begins at the unchecked access.",
        files: ["apps/web/page.tsx"],
        diff: "diff --git a/apps/web/page.tsx b/apps/web/page.tsx",
        verification: {
          applied: true,
          build_ran: true,
          built: true,
          test_ran: true,
          tested: true,
          timed_out: false,
          duration_ms: 42,
          log: "private build output",
        },
        repairs: 1,
        status: "proposed",
        error: "private provider error",
        created_at: "2026-08-16T06:48:59Z",
      }],
      decided: true,
      decision: {
        id: "019fe44a-c712-745b-b804-5e7604f02b81",
        investigation_id: investigationID,
        chosen_candidate_id: "019fe44a-c712-745b-b804-5e7604f02b80",
        document: "private embedded incident context",
        notes: "private note",
        decided_at: "2026-08-16T07:00:00Z",
        indexed_at: "2026-08-16T07:00:01Z",
      },
      started_at: "2026-08-16T06:48:59Z",
      finished_at: "2026-08-16T06:49:08Z",
    })));

    const state = await getAgentRemediation(investigationID);

    expect(state.available).toBe(true);
    if (!state.available || !state.value) throw new Error("expected remediation");
    expect(state.value.candidates[0]).not.toHaveProperty("error");
    expect(state.value.candidates[0].verification).not.toHaveProperty("log");
    expect(state.value.decision).toEqual({
      id: "019fe44a-c712-745b-b804-5e7604f02b81",
      chosen_candidate_id: "019fe44a-c712-745b-b804-5e7604f02b80",
      decided_at: "2026-08-16T07:00:00Z",
      indexed_at: "2026-08-16T07:00:01Z",
    });
    expect(verificationSummary(state.value.candidates[0].verification)).toBe("builds, and the service's tests pass");
  });

  it("describes verification without claiming an unrun build or test failed", () => {
    expect(verificationSummary({
      applied: true,
      build_ran: false,
      built: false,
      test_ran: false,
      tested: false,
      timed_out: false,
      duration_ms: 0,
    })).toBe("applied, but this service declares neither a build nor a test command, so nothing was verified");
  });

  it("distinguishes a rollback recommendation from an executed remediation", () => {
    const run = agentInvestigationForPresentation("ROLLBACK");

    expect(recommendationLabel(run)).toBe("Recommend rollback");
    expect(executionSummary(run, null)).toBe("Not run — recommendation only");
    expect(executionSummary(agentInvestigationForPresentation("HOTFIX"), null)).toBe("Not started");
  });
});

function agentInvestigationForPresentation(decision: "ROLLBACK" | "HOTFIX") {
  return {
    investigation_id: investigationID,
    incident_id: "incident-1",
    status: "done" as const,
    result: {
      verdict: { decision, source: "declared" as const },
      sources: 0,
      iterations: 1,
      trace: [],
      progress: [],
      truncated: false,
    },
    started_at: "2026-08-16T06:48:59Z",
    finished_at: "2026-08-16T06:49:08Z",
  };
}
