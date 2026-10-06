import type { CapturedResponse, LogAttempt, LogDetailResponse, LogRecord } from "@/lib/api";

/** Narrow an unknown to a plain object, or undefined. */
export function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

/** A non-empty string (or a number/boolean rendered as one), or undefined. */
export function textOf(value: unknown): string | undefined {
  if (typeof value === "string" && value.trim()) return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return undefined;
}

/** Append a period unless the value already ends with sentence punctuation. */
export function sentence(value: string): string {
  return /[.!?]$/.test(value) ? value : `${value}.`;
}

/**
 * The captured request body and response for a request record. Older records carry no
 * capture at all, so every field here is optional.
 */
export interface RequestCapture {
  at?: string;
  path?: string;
  decision?: unknown;
  clientRequest?: unknown;
  response?: CapturedResponse;
}

export function requestCapture(detail: LogDetailResponse): RequestCapture {
  const body = asRecord(detail.body);
  if (!body) return {};
  const response = asRecord(body.response);
  return {
    at: textOf(body.at),
    path: textOf(body.path),
    decision: body.decision,
    clientRequest: body.body,
    response: response ? (response as unknown as CapturedResponse) : undefined,
  };
}

/** The verdict and state for this record, whether it is a brain record or a request. */
export function brainVerdictState(detail: LogDetailResponse): {
  verdict?: unknown;
  state?: unknown;
} {
  const body = asRecord(detail.body);
  if (detail.record.kind === "brain") {
    return { verdict: body?.verdict, state: body?.state };
  }
  for (const call of detail.brainCalls) {
    const data = asRecord(call.body);
    if (data?.verdict) return { verdict: data.verdict, state: data.state };
  }
  return {};
}

function promptMessages(body: unknown): Array<{ role: string; content: unknown }> | undefined {
  const record = asRecord(body);
  if (!record) return undefined;
  const source = Array.isArray(record.messages)
    ? record.messages
    : Array.isArray(record.input)
      ? record.input
      : undefined;
  if (!source) return undefined;
  return source.map((raw) => {
    const message = asRecord(raw) ?? {};
    return {
      role: typeof message.role === "string" ? message.role : ((message.type as string) ?? "item"),
      content: message.content ?? message.parts ?? raw,
    };
  });
}

export function contentText(content: unknown): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((block) => {
        const record = asRecord(block);
        if (record && typeof record.text === "string") return record.text;
        return JSON.stringify(block);
      })
      .join("\n");
  }
  return JSON.stringify(content, null, 2);
}

/** Pretty-print JSON text when it parses; otherwise return it unchanged. */
export function prettyJsonText(text: string): { pretty: string; parsed: boolean } {
  try {
    return { pretty: JSON.stringify(JSON.parse(text), null, 2), parsed: true };
  } catch {
    return { pretty: text, parsed: false };
  }
}

/** One-line, human-readable summary of the routing verdict a brain call returned. */
export function verdictSummary(verdict: Record<string, unknown>): string {
  const parts: string[] = [];
  const model = textOf(verdict.model);
  parts.push(model ? `picked ${model}` : "returned no model");
  const effort = textOf(verdict.effort);
  if (effort) parts.push(`thinking ${effort}`);
  if (typeof verdict.confidence === "number") {
    parts.push(`confidence ${(verdict.confidence * 100).toFixed(0)}%`);
  }
  return parts.length > 0 ? sentence(parts.join(" · ")) : "The brain returned a verdict.";
}

/** Ranked option list from a SystemOne probability map, highest first. */
export function probabilityRanking(raw: unknown): Array<{ option: string; score: number }> {
  const record = asRecord(raw);
  if (!record) return [];
  return Object.entries(record)
    .flatMap(([option, value]) =>
      typeof value === "number" && Number.isFinite(value) ? [{ option, score: value }] : [],
    )
    .sort((left, right) => right.score - left.score || left.option.localeCompare(right.option));
}

export function formatPercent(score: number): string {
  return `${(score * 100).toFixed(score >= 0.1 || score === 0 ? 0 : 1)}%`;
}

/** One-line summary of the state the router sent to the brain. */
export function stateSummary(state: Record<string, unknown>): string {
  const parts: string[] = [];
  const message = textOf(state.first_user_message);
  if (message) {
    const trimmed = message.length > 140 ? `${message.slice(0, 140)}…` : message;
    parts.push(`first user message: “${trimmed.replaceAll("\n", " ")}”`);
  }
  const tools = state.tools;
  if (Array.isArray(tools)) parts.push(`${tools.length} tool${tools.length === 1 ? "" : "s"}`);
  const count = (key: string): number | undefined =>
    Array.isArray(state[key]) ? (state[key] as unknown[]).length : undefined;
  const messages = count("messages") ?? count("input");
  if (messages !== undefined) parts.push(`${messages} message${messages === 1 ? "" : "s"}`);
  const transcript = textOf(state.transcript);
  if (transcript) {
    const trimmed = transcript.length > 140 ? `${transcript.slice(0, 140)}…` : transcript;
    parts.push(`transcript: “${trimmed.replaceAll("\n", " ")}”`);
  }
  const session = textOf(state.session);
  if (session) parts.push(`session ${session}`);
  return parts.length > 0
    ? sentence(parts.join(" · "))
    : "The router sent routing state to the brain; expand the raw JSON to inspect it.";
}

/**
 * The short reason an attempt failed, in the operator's words. The stored value is a
 * classification (`quota`, `http-502`, `fetch: ECONNRESET`) so the UI can name it without
 * guessing at a vendor's message.
 */
export function attemptFailLabel(fail: string): string {
  if (fail === "quota") return "quota exhausted";
  if (fail === "client-canceled") return "client canceled";
  if (fail === "context-overflow") return "context overflow";
  if (fail.startsWith("http-")) return `HTTP ${fail.slice(5)}`;
  if (fail.startsWith("fetch:")) return fail.slice(6).trim();
  return fail;
}

/** One row of the routing decision table. */
export interface RoutingRow {
  model: string;
  provider?: string;
  score?: number;
  chosen?: boolean;
  withheld?: { reason: string; detail: string };
}

/**
 * Build the routing decision table: the chosen model first, then the brain's ranked
 * alternatives, then every model that was withheld with its reason. `brainModel` is the
 * verdict's own model id, which may differ from the record's raw wire id; either match
 * removes that ranking entry so the chosen model never appears twice.
 */
export function routingRows(
  record: LogRecord,
  ranking: Array<{ option: string; score: number }>,
  brainModel?: string,
): RoutingRow[] {
  const chosenKeys = new Set(
    [record.model, brainModel].filter((value): value is string => Boolean(value)),
  );
  const chosenScore = ranking.find((entry) => chosenKeys.has(entry.option))?.score;
  const rows: RoutingRow[] = [
    { model: record.model, provider: record.provider, score: chosenScore, chosen: true },
  ];
  for (const entry of ranking) {
    if (chosenKeys.has(entry.option)) continue;
    rows.push({ model: entry.option, score: entry.score });
  }
  for (const entry of record.skipped ?? []) {
    rows.push({
      model: entry.model,
      provider: entry.provider,
      withheld: { reason: entry.reason, detail: entry.detail },
    });
  }
  return rows;
}

/** Placement of one attempt on the shared millisecond axis. */
export interface TimelineBar {
  leftPct: number;
  widthPct: number;
}

export interface TimelineLayout {
  /** True when at least one attempt carried a `startedAt`, so bars sit on a real axis. */
  onAxis: boolean;
  /** Total span the axis is scaled to, in milliseconds. */
  total: number;
  bars: TimelineBar[];
}

/**
 * Place each attempt on a shared axis from the first attempt's start, scaled to the turn
 * latency. When no attempt has a `startedAt`, fall back to widths proportional to each
 * attempt's own duration.
 */
export function timelineLayout(tries: LogAttempt[], latencyMs: number): TimelineLayout {
  const starts = tries
    .map((attempt) => attempt.startedAt)
    .filter((value): value is number => typeof value === "number" && Number.isFinite(value));
  if (starts.length === 0) {
    const max = Math.max(latencyMs, ...tries.map((attempt) => attempt.ms ?? 0), 1);
    return {
      onAxis: false,
      total: max,
      bars: tries.map((attempt) => ({
        leftPct: 0,
        widthPct: Math.max(2, ((attempt.ms ?? 0) / max) * 100),
      })),
    };
  }
  const first = Math.min(...starts);
  const ends = tries.map((attempt) =>
    typeof attempt.startedAt === "number" ? attempt.startedAt - first + (attempt.ms ?? 0) : 0,
  );
  const total = Math.max(latencyMs, ...ends, 1);
  return {
    onAxis: true,
    total,
    bars: tries.map((attempt) => {
      const offset = typeof attempt.startedAt === "number" ? attempt.startedAt - first : 0;
      return {
        leftPct: (offset / total) * 100,
        widthPct: Math.max(((attempt.ms ?? 0) / total) * 100, 1),
      };
    }),
  };
}

function responseBlock(response: CapturedResponse): string[] {
  const lines: string[] = [
    "",
    "## Response",
    `- wire: ${response.wire}`,
    `- status: ${response.status}`,
    `- streamed: ${response.stream ? "yes" : "no"}`,
    ...(response.finishReason ? [`- finish reason: ${response.finishReason}`] : []),
    ...(response.truncated ? ["- truncated: the capture was cut at a size cap"] : []),
    ...(response.error ? [`- error: ${response.error}`] : []),
  ];
  if (response.reasoning) {
    lines.push("", "### Reasoning", "```", response.reasoning, "```");
  }
  lines.push("", "### Text", "```", response.text, "```");
  if (response.toolCalls && response.toolCalls.length > 0) {
    lines.push("", "### Tool calls");
    for (const call of response.toolCalls) {
      lines.push(`- ${call.name}(${call.arguments})`);
    }
  }
  return lines;
}

export function buildBundle(detail: LogDetailResponse): string {
  const { record } = detail;
  const lines: string[] = [
    `# Jevonian ${record.kind === "brain" ? "routing brain call" : "request"} ${record.id ?? ""}`,
    "",
  ];
  lines.push(
    `- time: ${record.ts}`,
    `- status: ${record.status}`,
    `- provider/model: ${record.provider} / ${record.model}`,
    `- requested: ${record.requestedModel ?? "-"}`,
    `- phase: ${record.phase ?? "-"}`,
    `- thinking effort: ${record.effort ?? "default"}${record.effortNote ? ` (${record.effortNote})` : ""}`,
    `- reason: ${record.reason ?? "-"}`,
    ...(record.retries
      ? [`- network retries: ${record.retries} (transient upstream failure, recovered)`]
      : []),
    ...(record.failovers ? [`- failovers: ${record.failovers}`] : []),
    ...(record.ttftMs !== undefined ? [`- first token: ${record.ttftMs}ms`] : []),
    ...(record.tries && record.tries.length > 0
      ? [
          `- attempts: ${record.tries
            .map(
              (attempt) =>
                `${attempt.provider}/${attempt.model} (${attempt.cause}${
                  attempt.status !== undefined ? `, ${attempt.status}` : ""
                }${attempt.fail ? `, ${attemptFailLabel(attempt.fail)}` : ""}${
                  attempt.ms !== undefined ? `, ${attempt.ms}ms` : ""
                })`,
            )
            .join("; ")}`,
        ]
      : []),
    `- brain: ${record.brain ?? "-"}${record.brainChannel ? ` (channel ${record.brainChannel})` : ""}`,
    ...(record.skipped && record.skipped.length > 0
      ? [
          `- models withheld: ${record.skipped
            .map((entry) => `${entry.provider}/${entry.model} (${entry.reason}: ${entry.detail})`)
            .join("; ")}`,
        ]
      : []),
    `- session: ${record.session}`,
    `- tokens: ${record.promptTokens} in / ${record.completionTokens} out / ${record.cacheReadTokens} cached`,
    `- cost: ${record.costUsd ?? "-"}${record.billing ? ` (${record.billing})` : ""}`,
    `- latency: ${record.latencyMs}ms`,
    ...(record.error ? [`- error: ${record.error}`] : []),
    ...(record.requestId ? [`- parent request: ${record.requestId}`] : []),
  );

  const capture = requestCapture(detail);
  if (record.kind === "brain") {
    const body = asRecord(detail.body);
    lines.push(
      "",
      "## Brain state",
      "```json",
      JSON.stringify(body?.state ?? null, null, 2),
      "```",
    );
    lines.push("", "## Verdict", "```json", JSON.stringify(body?.verdict ?? null, null, 2), "```");
    return lines.join("\n");
  }

  const messages = promptMessages(capture.clientRequest);
  lines.push("", "## Prompt");
  if (messages) {
    for (const message of messages) {
      lines.push(`### ${message.role}`, "```", contentText(message.content), "```");
    }
  } else {
    lines.push(
      "```json",
      JSON.stringify(capture.clientRequest ?? detail.body ?? null, null, 2),
      "```",
    );
  }
  lines.push(
    "",
    "## Raw request JSON",
    "```json",
    JSON.stringify(capture.clientRequest ?? null, null, 2),
    "```",
  );

  if (capture.response) {
    lines.push(...responseBlock(capture.response));
  } else {
    lines.push(
      "",
      "## Response",
      "Response not captured for this record (recorded before response capture, or JEVONIAN_CAPTURE_BODIES=0).",
    );
  }

  if (detail.brainCalls.length > 0) {
    lines.push("", `## Routing brain calls (${detail.brainCalls.length}, fallback order)`);
    detail.brainCalls.forEach((call, index) => {
      const data = asRecord(call.body);
      lines.push(
        "",
        `### ${index + 1}. ${call.record.provider} · ${call.record.model} · status ${call.record.status} · ${call.record.latencyMs}ms · ${call.record.promptTokens}/${call.record.completionTokens} tokens`,
      );
      if (data?.verdict) {
        lines.push("verdict:", "```json", JSON.stringify(data.verdict, null, 2), "```");
      }
      if (data?.state) {
        lines.push("state:", "```json", JSON.stringify(data.state, null, 2), "```");
      }
    });
  }
  return lines.join("\n");
}

/** The messages the client sent, for the request side of the exchange. */
export function clientMessages(
  clientRequest: unknown,
): Array<{ role: string; content: unknown }> | undefined {
  return promptMessages(clientRequest);
}
