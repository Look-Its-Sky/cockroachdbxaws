import { GetQueueAttributesCommand, SQSClient } from "@aws-sdk/client-sqs";
import { analysisOverviewSchema, type AnalysisOverview } from "./schema";

export type ComponentState<T> =
  | { available: true; value: T }
  | { available: false; reason: "unavailable" | "misconfigured" };

export interface DashboardOverview {
  collectedAt: string;
  analysis: ComponentState<AnalysisOverview>;
  cloudwatch: ComponentState<{ ready: boolean; metrics: Record<string, number> }>;
  outbox: ComponentState<{ ready: boolean }>;
  queues: ComponentState<{ available: number; inFlight: number; deadLetter: number }>;
}

const timeout = 4_000;

async function safeFetch(url: string | undefined): Promise<Response> {
  if (!url) throw new Error("misconfigured");
  return fetch(url, { cache: "no-store", signal: AbortSignal.timeout(timeout) });
}

function parseMetrics(text: string): Record<string, number> {
  const values: Record<string, number> = {};
  for (const line of text.split("\n")) {
    if (!line.startsWith("static_log_analysis_")) continue;
    const [name, raw, extra] = line.trim().split(/\s+/);
    const value = Number(raw);
    if (!extra && /^static_log_analysis_[a-z0-9_]+$/.test(name) && Number.isFinite(value) && value >= 0) values[name] = value;
  }
  return values;
}

async function analysis(): Promise<AnalysisOverview> {
  const response = await safeFetch(process.env.ANALYSIS_OVERVIEW_URL);
  if (!response.ok) throw new Error("unavailable");
  return analysisOverviewSchema.parse(await response.json());
}

async function worker(url: string | undefined): Promise<{ ready: boolean }> {
  const response = await safeFetch(url);
  return { ready: response.ok && (await response.text()).trim() === "ready" };
}

async function cloudwatch(): Promise<{ ready: boolean; metrics: Record<string, number> }> {
  const [health, metrics] = await Promise.all([
    worker(process.env.CLOUDWATCH_HEALTH_URL),
    safeFetch(process.env.CLOUDWATCH_METRICS_URL).then(async (response) => {
      if (!response.ok) throw new Error("unavailable");
      return parseMetrics(await response.text());
    }),
  ]);
  return { ready: health.ready, metrics };
}

async function queues(): Promise<{ available: number; inFlight: number; deadLetter: number }> {
  const queueUrl = process.env.SLA_OUTBOX_QUEUE_URL;
  const deadLetterUrl = process.env.SLA_OUTBOX_DEAD_LETTER_QUEUE_URL;
  if (!queueUrl || !deadLetterUrl || !process.env.AWS_REGION) throw new Error("misconfigured");
  const client = new SQSClient({ region: process.env.AWS_REGION });
  const names = ["ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"] as const;
  const [queue, deadLetter] = await Promise.all([
    client.send(new GetQueueAttributesCommand({ QueueUrl: queueUrl, AttributeNames: [...names] })),
    client.send(new GetQueueAttributesCommand({ QueueUrl: deadLetterUrl, AttributeNames: [...names] })),
  ]);
  return {
    available: Number(queue.Attributes?.ApproximateNumberOfMessages ?? 0),
    inFlight: Number(queue.Attributes?.ApproximateNumberOfMessagesNotVisible ?? 0),
    deadLetter: Number(deadLetter.Attributes?.ApproximateNumberOfMessages ?? 0),
  };
}

async function settle<T>(promise: Promise<T>): Promise<ComponentState<T>> {
  try {
    return { available: true, value: await promise };
  } catch (error) {
    return { available: false, reason: error instanceof Error && error.message === "misconfigured" ? "misconfigured" : "unavailable" };
  }
}

export async function getDashboardOverview(): Promise<DashboardOverview> {
  const [analysisState, cloudwatchState, outboxState, queueState] = await Promise.all([
    settle(analysis()),
    settle(cloudwatch()),
    settle(worker(process.env.OUTBOX_HEALTH_URL)),
    settle(queues()),
  ]);
  return {
    collectedAt: new Date().toISOString(),
    analysis: analysisState,
    cloudwatch: cloudwatchState,
    outbox: outboxState,
    queues: queueState,
  };
}

export { parseMetrics };
