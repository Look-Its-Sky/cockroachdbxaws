import { z } from "zod";

const countMap = z.record(z.string(), z.number().int().nonnegative());

export const analysisOverviewSchema = z.object({
  records_seen: z.number().int().nonnegative(),
  incident_families: z.number().int().nonnegative(),
  investigations: countMap,
  outbox: countMap,
  recent_investigations: z.array(
    z.object({
      investigation_id: z.string().uuid(),
      service: z.string().max(128),
      environment: z.string().max(128),
      severity: z.string().max(32),
      state: z.string().max(64),
      trigger_reason: z.string().max(64),
      queued_at: z.string().datetime(),
    }),
  ).max(25),
});

export type AnalysisOverview = z.infer<typeof analysisOverviewSchema>;
