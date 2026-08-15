import { describe, expect, it } from "vitest";
import { parseMetrics } from "./overview";

describe("Prometheus metric projection", () => {
  it("accepts only finite unlabelled service metrics", () => {
    expect(parseMetrics(`# HELP x x\nstatic_log_analysis_journal_bytes 42\nother_metric 8\nstatic_log_analysis_bad{service="secret"} 5\n`)).toEqual({
      static_log_analysis_journal_bytes: 42,
    });
  });
});
