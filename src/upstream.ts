import type { Context } from "hono";

import { proxyNativeCodex, shouldProxyNativeCodex } from "./account";
import { anthropicToChat, anthropicToChatStream, chatToAnthropicMessage } from "./anthropic";
import { resolveProviderAuth, withSessionAffinity, type AuthResolution } from "./auth";
import { saveBody } from "./bodies";
import { decodeBody } from "./body-encoding";
import { askJevRaw } from "./brain";
import { chatToAnthropicStream } from "./chat-anthropic-stream";
import { isClaudeGatewayRequest, resolveClaudeGatewayModel } from "./claude-gateway";
import {
  compact,
  normalizeTranscript,
  reductionRatio,
  reencodeMessages,
  type CompactResult,
  type JevAsker,
  type JevResponse,
} from "./compaction";
import type { Config, Provider } from "./config";
import { findProviderByName } from "./config";
import {
  cursorChatCompletion,
  cursorConversation,
  cursorLastUser,
  cursorToChatStream,
  resolveCursorAgentUrl,
  runCursor,
  type CursorStreamError,
} from "./cursor";
import { cursorModelId } from "./cursor-catalog";
import {
  buildDevinChatRequest,
  classifyDevinError,
  devinChatCompletion,
  devinChatUrl,
  devinHeaders,
  devinToChatStream,
  peekDevinStream,
  stripAgentSystemMessages,
  type DevinErrorKind,
  type DevinStreamError,
} from "./devin";
import {
  geminiChatCompletion,
  geminiEndpoint,
  geminiToChatStream,
  geminiUsage,
  unwrapGemini,
} from "./gemini";
import { appendRecord } from "./ledger";
import { LOCAL_CLIENT_KEYS } from "./local-client";
import { invalidateOAuthToken } from "./oauth";
import { DevinImageError, payloadFor, prepareUpstream } from "./prepare";
import { costOf, type Usage } from "./pricing";
import { beginProviderAttempt, endProviderAttempt } from "./provider-guard";
import {
  captureQuotaHeaders,
  captureUsageLimit,
  isProviderRefusal,
  isRateLimitRefusal,
  markProviderSpent,
  messageSpendSignal,
  providerQuotaHealth,
  PROVIDER_COOLDOWN_MS,
} from "./quota";
import { rememberFromChatCompletion, reasoningCaptureTransform } from "./reasoning-passback";
import { cancelOutcome, trackEvents, type StreamEvent } from "./relay";
import {
  chatCompletionFrom,
  chatResultFromResponse,
  ChatToResponsesBridge,
  chatToResponsesStream,
  isRemoteCompactionV2,
  repairResponsesOutput,
  responsesErrorMessage,
  responsesPassthroughRepairStream,
  responsesToChatStream,
  responsesUsage,
  splitSseEvents,
} from "./responses";
import {
  configuredSameHostRetries,
  describeFailure,
  describeFetchError,
  retryTransient,
  withRetry,
  type RetryAttempt,
  type RetryFailure,
} from "./retry";
import {
  compactionEstimate,
  decideRoute,
  isDesktopRoutedModel,
  nextFromPlan,
  phaseOfModel,
  planKey,
  resolveSessionKey,
  type RequestKind,
  type RouteDecision,
  type RouteSkip,
  type SessionStore,
} from "./routing";
import type { AppEnv } from "./server";
import {
  softCompletionStream,
  softErrorMessage,
  redactSecrets,
  withSoftCompletion,
} from "./soft-error";
import { streamWithKeepalive } from "./stream-keepalive";
import {
  beginRoute,
  beginTry,
  endTry,
  finishRoute,
  firstToken,
  noteDecision,
  weigh,
  type TryCause,
} from "./trace";
import { guardUpstreamStream, timeoutController, upstreamTimeouts } from "./upstream-timeout";
import {
  planUpstreamWire,
  sanitizeOpenAIChatResponse,
  sanitizeOpenAIChatStream,
  upstreamUrlFor,
} from "./wire";
import { foldOpenAIChatStream, isWorkbuddyAiSource } from "./workbuddy";

interface RequestMeta {
  id: string;
  session: string;
  path: string;
  provider: string;
  model: string;
  stream: boolean;
  started: number;
  requestedModel?: string;
  phase?: string;
  routed?: boolean;
  reason?: string;
  store?: SessionStore;
  usageKind?: RequestKind;
  cache?: RouteDecision["cache"];
  switchPenaltyUsd?: number | null;
  /** Why the conversation stayed where it was answered, or moved (cache affinity). */
  cacheKeep?: RouteDecision["cacheKeep"];
  brain?: string;
  confidence?: number;
  canonical?: string;
  billing?: "api" | "subscription";
  effort?: string;
  effortNote?: string;
  skipped?: RouteSkip[];
  keyId?: string;
  keyName?: string;
  /** Transient upstream failures that were retried before this turn was recorded. */
  retries?: number;
  /** Estimated prompt tokens the tool-result saver removed before egress. */
  savedTokens?: number;
  /** Trace this turn is being recorded under, so attempts and TTFT land on the ledger row. */
  traceId?: string;
  /** Set once a ledger row is written so cancel cannot double-record a finished turn. */
  recorded?: boolean;
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/**
 * Non-stream Chat Completions body → Responses API body. Shared by every path that answers a
 * Responses client from a chat-shaped result (OpenAI hosts directly, Anthropic hosts via
 * `anthropicToChat`), so the two cannot drift on ids, tool-call shape, or usage fields.
 */
function chatJsonToResponse(
  chat: Record<string, unknown>,
  context: { model: string; session: string; started: number; usage: Usage },
): Record<string, unknown> {
  const choice = asRecord(asRecord((chat.choices as unknown[])?.[0]).message);
  const toolCalls = Array.isArray(choice.tool_calls) ? choice.tool_calls : [];
  const output: unknown[] = [];
  if (typeof choice.content === "string" && choice.content.length > 0) {
    output.push({
      type: "message",
      role: "assistant",
      content: [{ type: "output_text", text: choice.content }],
    });
  }
  for (const raw of toolCalls) {
    const call = asRecord(raw);
    const fn = asRecord(call.function);
    const callId =
      typeof call.id === "string" && call.id.length > 0
        ? call.id
        : `call_${crypto.randomUUID().replace(/-/g, "").slice(0, 16)}`;
    output.push({
      type: "function_call",
      call_id: callId,
      name: asString(fn.name),
      arguments: typeof fn.arguments === "string" ? fn.arguments : "",
    });
  }
  const { usage } = context;
  return {
    id: `resp_${context.session.slice(0, 16)}`,
    object: "response",
    created_at: Math.floor(context.started / 1000),
    status: "completed",
    model: context.model,
    output,
    usage: {
      input_tokens: usage.input,
      output_tokens: usage.output,
      total_tokens: usage.input + usage.output,
      input_tokens_details: { cached_tokens: usage.cacheRead },
    },
  };
}

/**
 * Codex remote compaction v2 only works on a native Responses host (ChatGPT backend).
 * Bridging to Chat Completions returns a normal message item and Codex aborts with
 * "expected exactly one compaction output item, got 0 from N".
 */
function remoteCompactionDecision(
  config: Config,
  body: Record<string, unknown>,
  headers: Record<string, string | undefined>,
  requestId: string,
): RouteDecision | { error: string; status: number } {
  const providers = config.providers.filter((provider) => provider.type === "responses");
  const preferred =
    providers.find((provider) => provider.oauthSource === "codex") ??
    providers.find((provider) => {
      try {
        return new URL(provider.baseUrl).hostname.includes("chatgpt.com");
      } catch {
        return false;
      }
    }) ??
    providers[0];
  if (!preferred) {
    return {
      error:
        "Remote compaction requires a ChatGPT subscription (Responses) provider. Add chatgpt-subscription under Providers.",
      status: 400,
    };
  }
  const requestedRaw = typeof body.model === "string" ? body.model : "";
  const requested = requestedRaw.replace(/^jevonian\//, "");
  const model = preferred.models.some((entry) => entry.id === requested)
    ? requested
    : preferred.models.find((candidate) => candidate.id.length > 0)?.id;
  if (!model) {
    return {
      error: `Provider "${preferred.name}" has no models configured for remote compaction.`,
      status: 400,
    };
  }
  return {
    model,
    provider: preferred.name,
    phase: phaseOfModel(config, model),
    requestedModel: requestedRaw || model,
    virtual: false,
    routed: true,
    reason: "remote-compaction",
    session: resolveSessionKey(body, headers),
    requestId,
  };
}

const emptyUsage = (): Usage => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });

function number(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function openaiUsage(raw: unknown): Usage {
  const usage = (raw ?? {}) as Record<string, unknown>;
  const details = (usage.prompt_tokens_details ?? {}) as Record<string, unknown>;
  return {
    input: number(usage.prompt_tokens),
    output: number(usage.completion_tokens),
    cacheRead: number(details.cached_tokens),
    cacheWrite: 0,
  };
}

function anthropicUsage(raw: unknown): Usage {
  const usage = (raw ?? {}) as Record<string, unknown>;
  return {
    input: number(usage.input_tokens),
    output: number(usage.output_tokens),
    cacheRead: number(usage.cache_read_input_tokens),
    cacheWrite: number(usage.cache_creation_input_tokens),
  };
}

function applyAnthropicEvent(event: Record<string, unknown>, usage: Usage): void {
  if (event.type === "message_start") {
    const message = (event.message ?? {}) as Record<string, unknown>;
    Object.assign(usage, anthropicUsage(message.usage));
    return;
  }
  if (event.type === "message_delta") {
    const delta = (event.usage ?? {}) as Record<string, unknown>;
    const output = number(delta.output_tokens);
    if (output > 0) usage.output = output;
  }
}

function record(
  meta: RequestMeta,
  status: number,
  usage: Usage,
  costUsd: number | null,
  pricingKnown: boolean,
  error?: string,
): void {
  // A canceled stream's transform `cancel` and a racing `flush` must not both write.
  if (meta.recorded) return;
  meta.recorded = true;
  // The trace ends with the turn, so the ledger row can carry the attempt history. A turn
  // that was never traced (a pinned model with no route decision) simply has no trace.
  const trace = meta.traceId
    ? finishRoute(meta.traceId, { status, ...(error ? { error } : {}) })
    : undefined;
  if (meta.store && status >= 200 && status < 300) {
    meta.store.observeCache(meta.session, {
      provider: meta.provider,
      model: meta.model,
      at: Date.now(),
      uncachedInputTokens:
        meta.usageKind === "anthropic" ? usage.input : Math.max(0, usage.input - usage.cacheRead),
      cacheReadTokens: usage.cacheRead,
      cacheWriteTokens: usage.cacheWrite,
      success: true,
    });
  }
  appendRecord({
    id: meta.id,
    ts: new Date().toISOString(),
    session: meta.session,
    path: meta.path,
    provider: meta.provider,
    model: meta.model,
    stream: meta.stream,
    status,
    latencyMs: Date.now() - meta.started,
    promptTokens: usage.input,
    completionTokens: usage.output,
    cacheReadTokens: usage.cacheRead,
    cacheWriteTokens: usage.cacheWrite,
    costUsd,
    pricingKnown,
    ...(meta.keyId ? { keyId: meta.keyId } : {}),
    ...(meta.keyName ? { keyName: meta.keyName } : {}),
    ...(meta.cache ? { cache: meta.cache } : {}),
    ...(meta.cacheKeep ? { cacheKeep: meta.cacheKeep } : {}),
    ...(meta.switchPenaltyUsd === undefined ? {} : { switchPenaltyUsd: meta.switchPenaltyUsd }),
    ...(meta.billing === "subscription" ? { billing: meta.billing } : {}),
    ...(meta.requestedModel ? { requestedModel: meta.requestedModel } : {}),
    ...(meta.phase ? { phase: meta.phase } : {}),
    ...(meta.routed === undefined ? {} : { routed: meta.routed }),
    ...(meta.reason ? { reason: meta.reason } : {}),
    ...(meta.brain ? { brain: meta.brain } : {}),
    ...(meta.confidence === undefined ? {} : { confidence: meta.confidence }),
    ...(meta.canonical ? { canonical: meta.canonical } : {}),
    ...(meta.effort ? { effort: meta.effort } : {}),
    ...(meta.effortNote ? { effortNote: meta.effortNote } : {}),
    ...(meta.skipped && meta.skipped.length > 0 ? { skipped: meta.skipped } : {}),
    ...(meta.retries ? { retries: meta.retries } : {}),
    ...(meta.savedTokens ? { savedTokens: meta.savedTokens } : {}),
    // Only a turn that needed more than one attempt carries the list: a clean turn stays as
    // small as it was, and a row with no field reads as "not recorded", never "0 tries".
    ...(trace && trace.tries.length > 1 ? { tries: trace.tries } : {}),
    ...(trace && trace.failovers > 0 ? { failovers: trace.failovers } : {}),
    ...(trace?.ttftMs === undefined ? {} : { ttftMs: trace.ttftMs }),
    ...(error ? { error } : {}),
  });
}

function decisionMeta(
  decision: RouteDecision,
  path: string,
  stream: boolean,
  started: number,
  id: string,
  keyId?: string,
  keyName?: string,
): RequestMeta {
  return {
    id,
    traceId: id,
    session: decision.session,
    path,
    provider: decision.provider,
    model: decision.model,
    stream,
    started,
    ...(keyId ? { keyId } : {}),
    ...(keyName ? { keyName } : {}),
    requestedModel: decision.requestedModel,
    phase: decision.phase,
    routed: decision.routed,
    reason: decision.reason,
    cache: decision.cache,
    switchPenaltyUsd: decision.switchPenaltyUsd,
    ...(decision.cacheKeep ? { cacheKeep: decision.cacheKeep } : {}),
    ...(decision.brain ? { brain: decision.brain } : {}),
    ...(decision.confidence === undefined ? {} : { confidence: decision.confidence }),
    ...(decision.canonical ? { canonical: decision.canonical } : {}),
    ...(decision.effort ? { effort: decision.effort } : {}),
    ...(decision.effortNote ? { effortNote: decision.effortNote } : {}),
    ...(decision.skipped && decision.skipped.length > 0 ? { skipped: decision.skipped } : {}),
  };
}

function decisionHeaders(decision: RouteDecision, retries = 0): Record<string, string> {
  return {
    "x-jevonian-model": decision.model,
    "x-jevonian-provider": decision.provider,
    "x-jevonian-phase": decision.phase,
    "x-jevonian-session": decision.session,
    "x-jevonian-reason": decision.reason,
    // Correlates a client's own logs with `/logs/:id` and the routing trace for this turn.
    "x-jevonian-request-id": decision.requestId,
    // Reported only when the turn needed one, so a healthy response stays uncluttered.
    ...(retries > 0 ? { "x-jevonian-retries": String(retries) } : {}),
    ...(decision.cache ? { "x-jevonian-cache-state": decision.cache.state } : {}),
    ...(decision.cacheKeep ? { "x-jevonian-cache-keep": decision.cacheKeep } : {}),
    ...(decision.brain ? { "x-jevonian-brain": decision.brain } : {}),
    ...(decision.brainChannel ? { "x-jevonian-brain-channel": decision.brainChannel } : {}),
    ...(decision.canonical ? { "x-jevonian-canonical": decision.canonical } : {}),
    ...(decision.effort ? { "x-jevonian-effort": decision.effort } : {}),
    ...(decision.effortNote ? { "x-jevonian-effort-note": decision.effortNote } : {}),
    // Skipped models are reported one header per model, never dropped silently.
    ...(decision.skipped && decision.skipped.length > 0
      ? { "x-jevonian-skipped": skippedHeader(decision.skipped) }
      : {}),
  };
}

/** One compact line per withheld model: `provider/model=context(...)`. */
function skippedHeader(skipped: RouteSkip[]): string {
  return skipped
    .map((entry) => `${entry.provider}/${entry.model}=${entry.reason}(${entry.detail})`)
    .join("; ");
}

function errorResponse(c: Context, meta: RequestMeta, status: number, message: string): Response {
  record(meta, status, emptyUsage(), null, true, message);
  // A streaming client that gets a JSON 5xx mid-agent loop can lose the whole turn: the harness
  // treats a hard error as "the response is invalid" and rolls the message back. When the client
  // asked to stream, answer as a completed assistant message instead, so the conversation
  // survives and the user can retry. The ledger row above still records the real failure.
  if (meta.stream) {
    const text = softErrorMessage(message);
    return new Response(softCompletionStream(clientKindOfPath(meta.path), meta.model, text), {
      status: 200,
      headers: {
        "content-type": "text/event-stream",
        "cache-control": "no-store",
        "x-jevonian-soft-error": "1",
      },
    });
  }
  return c.json({ error: { message, type: "jevonian_error" } }, status as 400);
}

/** The client wire a ledger `path` belongs to. Determines the SSE shape a soft close must use. */
function clientKindOfPath(path: string): RequestKind {
  if (path === "/messages") return "anthropic";
  if (path === "/responses") return "responses";
  return "openai";
}

/**
 * An SSE Response that keeps the socket warm during silent thinking and writes a
 * ledger row when the client hangs up before the stream finishes.
 *
 * This is also the single place a mid-stream failure is turned into a normal assistant turn:
 * the harness rolls the whole message back when a stream dies abruptly, so a dropped upstream
 * socket is closed with a soft completion instead (see `./soft-error`). Only a real SSE body is
 * wrapped — a passthrough of some other content type is forwarded untouched.
 */
function streamResponse(
  stream: ReadableStream<Uint8Array> | null,
  meta: RequestMeta,
  headers: Record<string, string>,
  contentType = "text/event-stream",
  onClientCancel?: () => void,
  /** Scrubs provider credentials from an upstream error before it reaches the user. */
  redact?: (text: string) => string,
): Response {
  const isSse = contentType.includes("event-stream");
  let body = stream;
  if (isSse) {
    const kind = clientKindOfPath(meta.path);
    const message = softErrorMessage("the upstream stream ended unexpectedly");
    body = stream
      ? withSoftCompletion(
          stream,
          kind,
          meta.model,
          message,
          (reason) => {
            record(
              meta,
              502,
              emptyUsage(),
              null,
              true,
              reason ?? "stream ended early (soft error)",
            );
          },
          redact,
        )
      : // An upstream that produced no body at all: still answer the turn rather than
        // handing the client an empty 200 it will read as a broken response.
        softCompletionStream(kind, meta.model, message);
  }
  return new Response(
    streamWithKeepalive(body, {
      onClientCancel:
        onClientCancel ?? (() => record(meta, 499, emptyUsage(), null, true, "client canceled")),
      // The first real chunk is the first byte a client can render, which is what makes a
      // first-token measurement meaningful. Keepalive comments never reach this callback.
      onFirstChunk: () => {
        if (meta.traceId) firstToken(meta.traceId);
      },
    }),
    {
      status: 200,
      headers: {
        "content-type": contentType,
        "cache-control": "no-store",
        ...headers,
      },
    },
  );
}

/** One line describing why an upstream attempt is being repeated. */
function describeRetryFailure(failure: RetryFailure): string {
  return describeFailure(failure);
}

/**
 * POSTs an outgoing body, repeating the call while the failure looks transient.
 *
 * A non-ok body is read as part of the attempt rather than by the caller: an unread body holds
 * the pooled socket the next attempt wants, and reading it here means a retried 502 leaves
 * nothing behind. An ok body is left untouched, because it may be an SSE stream.
 *
 * Same-host retries are deliberately short: a transport failure is usually a verdict about this
 * host, and the turn's remaining budget belongs to failover onto a different provider. A stream
 * waits only for response headers here; the first-byte clock runs after the body arrives.
 */
async function postUpstream(
  url: string,
  init: RequestInit,
  onRetry: (info: RetryAttempt) => void,
  options: { stream: boolean } = { stream: false },
): Promise<{ response: Response; text: string }> {
  const retryBudget = configuredSameHostRetries();
  const timeouts = upstreamTimeouts();
  const budgetMs = options.stream ? timeouts.headersMs : timeouts.totalMs;
  const phase = options.stream ? "headers" : "total";
  return withRetry(
    async () => {
      const clock = timeoutController(budgetMs, phase, init.signal);
      try {
        const response = await fetch(url, { ...init, signal: clock.signal });
        return { response, text: response.ok ? "" : await response.text() };
      } finally {
        clock.clear();
      }
    },
    {
      attempts: retryBudget + 1,
      // Only a "the server could not answer" status is repeated here. A 429 is a verdict about
      // quota and belongs to the failover path, which knows how to route the turn elsewhere.
      retryWhen: ({ response }) => retryTransient(response),
      onRetry,
    },
  );
}

/** The result of trying to shrink a body that no configured model could hold. */
type CompactOutcome =
  | { ok: true; body: Record<string, unknown>; stats: CompactResult["stats"] }
  | { ok: false; error: string };

/** A provider's hard context rejection is actionable; other 400s must never rewrite history. */
export function isContextOverflowResponse(status: number, text: string): boolean {
  if (status !== 400 && status !== 413 && status !== 422) return false;
  return /context_length_exceeded|context window|prompt is too long|maximum context length|too many (input )?tokens|input is too long|input tokens exceed/i.test(
    text,
  );
}

/**
 * Shrinks a request body that no model's context window could hold. Compaction drops tool calls
 * and results Jev judges stale and keeps every word of prose verbatim; if the reduction is not
 * worth the churn, the original body is kept rather than sending a mangled history.
 */
async function compactForOverflow(
  config: Config,
  body: Record<string, unknown>,
): Promise<CompactOutcome> {
  const brains = config.routing.brains;
  if (brains.length === 0) return { ok: false, error: "no Jev brain is configured" };
  const messages = normalizeTranscript(body);
  if (messages.length === 0) return { ok: false, error: "the request has no messages to compact" };

  // Compaction asks its own questions, so it needs the raw System One shape rather than the
  // router's model-choice verdict.
  const asker: JevAsker = {
    ask: async (state, questions) => {
      let lastError = "no Jev brain answered";
      for (const brain of brains) {
        try {
          const { answers } = await askJevRaw(
            brain,
            state as unknown as Record<string, unknown>,
            questions,
          );
          return { answers: answers as JevResponse["answers"] };
        } catch (error) {
          lastError = error instanceof Error ? error.message : String(error);
        }
      }
      throw new Error(lastError);
    },
  };

  try {
    const result = await compact(messages, asker, { preserveRecentMessages: 4 });
    if (reductionRatio(result) < 0.05) {
      return {
        ok: false,
        error: `compaction only reduced the history by ${(reductionRatio(result) * 100).toFixed(0)}%`,
      };
    }
    const rewritten = reencodeMessages(body, result.messages);
    if (compactionEstimate(rewritten) > compactionEstimate(body) * 0.9) {
      return { ok: false, error: "compaction did not sufficiently reduce the outgoing request" };
    }
    return { ok: true, body: rewritten, stats: result.stats };
  } catch (error) {
    return { ok: false, error: error instanceof Error ? error.message : String(error) };
  }
}

function requestHeaders(c: Context): Record<string, string | undefined> {
  return Object.fromEntries(c.req.raw.headers.entries());
}

/** Devin usage is exclusive; OpenAI-shaped client payloads expect inclusive prompt counts. */
function inclusiveUsage(usage: Usage): Usage {
  return { ...usage, input: usage.input + usage.cacheRead + usage.cacheWrite };
}

const DEVIN_ERROR_TYPES: Record<DevinErrorKind, string> = {
  quota: "rate_limit_error",
  rate_limit: "rate_limit_error",
  capacity: "overloaded_error",
  internal: "api_error",
  content_policy: "invalid_request_error",
  model_blocked: "invalid_request_error",
  auth: "authentication_error",
  other: "api_error",
};

/**
 * Whether a Devin refusal should send the turn to another provider.
 *
 * A quota or rate-limit verdict obviously should, and so should the ones describing a provider
 * that cannot run this model *now*: a model gated behind a bigger plan, an overloaded backend,
 * an internal fault, or credentials this host will not accept. Every one of those is a fact
 * about Devin, and another provider can usually take the turn. Only a content-policy refusal is
 * left out — that is a judgment about the request text, which is the client's to fix, and
 * re-sending the same prompt elsewhere is not a repair.
 */
function devinShouldFailover(error: DevinStreamError): boolean {
  return error.kind !== "content_policy";
}

/**
 * Records what a Devin refusal means for routing, so the next `decideRoute` walks past it.
 *
 * A quota refusal keeps the provider out until the stated reset, or until a live probe proves
 * the account is alive again. Everything short-lived — a bare rate limit, an overloaded
 * backend, an internal fault, a rejected credential — gets a cooldown instead, because benching
 * any of those indefinitely would take a working subscription out of the pool for a blip. A
 * plan gate is per-model, so it retires just that model and leaves the rest usable.
 */
function markDevinRefusal(provider: Provider, error: DevinStreamError, model: string): void {
  const cooldown = new Date(Date.now() + PROVIDER_COOLDOWN_MS).toISOString();
  // "Reached free model rate limit" caps the free tier only; paid models on the same account
  // keep working, so benching the whole provider would strand them for hours.
  const modelScoped = /\b(?:for this model|for the model|free model rate limit)\b/i.test(
    error.message,
  );
  switch (error.kind) {
    case "quota":
      markProviderSpent(provider, {
        label: "limit",
        ...(error.resetsAt ? { resetsAt: error.resetsAt } : {}),
      });
      return;
    case "rate_limit":
      markProviderSpent(provider, {
        label: "rate-limit",
        resetsAt: error.resetsAt ?? cooldown,
        ...(modelScoped ? { model } : {}),
      });
      return;
    case "model_blocked":
      markProviderSpent(provider, { label: "model-blocked", model, resetsAt: cooldown });
      return;
    case "capacity":
    case "internal":
    case "auth":
      markProviderSpent(provider, { label: error.kind, resetsAt: cooldown });
      return;
    default:
      // `other` and `content_policy` carry no trustworthy verdict about the subscription, so
      // they are failed over (or not) without writing a bench that could outlive the cause.
      return;
  }
}

function devinErrorStatus(error: DevinStreamError): number {
  return error.status >= 400 && error.status <= 599 ? error.status : 502;
}

/** Adds `reasoning_content` as a leading thinking block, which `chatToAnthropicMessage` drops. */
function withThinkingBlock(
  message: Record<string, unknown>,
  completion: Record<string, unknown>,
): Record<string, unknown> {
  const choice = asRecord(asRecord((completion.choices as unknown[])?.[0]).message);
  const thinking = asString(choice.reasoning_content);
  if (thinking.length === 0) return message;
  const content = Array.isArray(message.content) ? message.content : [];
  return { ...message, content: [{ type: "thinking", thinking, signature: "" }, ...content] };
}

/**
 * The wire-specific half of a Connect-RPC subscription (Devin, Cursor): how its refusals are
 * classified, recorded, and spelled for the client. Everything else about answering the client
 * — folding a stream for a non-stream client, relaying it across wires, recording usage, and
 * a client hanging up — is shared by `rpcClientResponse`, so the two wires cannot drift.
 */
interface RpcWire<E extends { kind: string; message: string }> {
  /** Error `type` per refusal kind, as the client's wire spells it. */
  errorTypes: Record<string, string>;
  status(error: E): number;
  shouldFailover(error: E): boolean;
  /** What a refusal means for routing, so the next turn walks past it. */
  markRefusal(error: E): void;
}

function devinWireAdapter(provider: Provider, model: string): RpcWire<DevinStreamError> {
  return {
    errorTypes: DEVIN_ERROR_TYPES,
    status: devinErrorStatus,
    shouldFailover: devinShouldFailover,
    markRefusal: (error) => markDevinRefusal(provider, error, model),
  };
}

function cursorWireAdapter(provider: Provider): RpcWire<CursorStreamError> {
  return {
    errorTypes: CURSOR_ERROR_TYPES,
    status: cursorErrorStatus,
    shouldFailover: cursorShouldFailover,
    markRefusal: (error) => markCursorRefusal(provider, error),
  };
}

/** A classified refusal, in the error envelope of the client's own wire. */
function rpcErrorResponse<E extends { kind: string; message: string }>(
  wire: RpcWire<E>,
  meta: RequestMeta,
  clientKind: RequestKind,
  error: E,
  headers: Record<string, string>,
): Response {
  const status = wire.status(error);
  record(meta, status, emptyUsage(), null, true, `${error.kind}: ${error.message}`.slice(0, 300));
  // A streaming client reads a bare JSON error as a malformed response and can roll the turn
  // back, so it gets the same soft close every other wire gives it. The ledger row above still
  // carries the real refusal.
  if (meta.stream) {
    return new Response(
      softCompletionStream(clientKind, meta.model, softErrorMessage(error.message.slice(0, 300))),
      {
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-store",
          "x-jevonian-soft-error": "1",
          ...headers,
        },
      },
    );
  }
  const type = wire.errorTypes[error.kind] ?? "api_error";
  const payload =
    clientKind === "anthropic"
      ? { type: "error", error: { type, message: error.message } }
      : { error: { message: error.message, type, code: error.kind } };
  return Response.json(payload, { status, headers });
}

/** Re-routed (`failover`), or answered (`response`). */
type RpcOutcome = { kind: "response"; response: Response } | { kind: "failover" };

/** A completion an RPC wire produced, plus how it finished. */
interface RpcFinish<E> {
  usage: Usage;
  error?: E;
}

/**
 * Answers the client from an RPC subscription that already produced its first event. These
 * wires always stream upstream; a non-stream client gets the folded completion instead.
 */
async function rpcClientResponse<E extends { kind: string; message: string }>(input: {
  c: Context<AppEnv>;
  meta: RequestMeta;
  provider: Provider;
  decision: RouteDecision;
  clientKind: RequestKind;
  clientStream: boolean;
  started: number;
  headers: Record<string, string>;
  wire: RpcWire<E>;
  /** Folds the whole upstream into one `chat.completion`. */
  fold: () => Promise<{ completion: Record<string, unknown>; finish: RpcFinish<E> }>;
  /** Relays the upstream as Chat Completions SSE, reporting what it emits and how it ends. */
  relay: (
    onFinish: (finish: RpcFinish<E>) => void,
    onEvent: (event: StreamEvent) => void,
  ) => ReadableStream<Uint8Array>;
  /**
   * Called when a non-stream call ends in a refusal, before the error is surfaced. Returning
   * true tells the caller the turn was re-routed; the caller `continue`s the routing loop.
   */
  onFailover?: () => Promise<boolean>;
}): Promise<RpcOutcome> {
  const { c, meta, provider, decision, clientKind, clientStream, started, headers, wire } = input;
  const model = decision.model;
  const finalize = (finish: RpcFinish<E> | undefined): void => {
    const usage = finish?.usage ?? emptyUsage();
    if (finish?.error) {
      // A streaming answer is already on the wire, so the refusal can only be recorded here.
      // Failover happens on the folded (non-stream) path below, where nothing has been sent.
      if (wire.shouldFailover(finish.error)) wire.markRefusal(finish.error);
      record(
        meta,
        wire.status(finish.error),
        usage,
        null,
        true,
        `${finish.error.kind}: ${finish.error.message}`.slice(0, 300),
      );
      return;
    }
    const cost = costOf(model, usage, new Date(), decision.provider, provider.type);
    record(meta, 200, usage, cost.usd, cost.known);
  };

  if (!clientStream) {
    const { completion, finish } = await input.fold();
    if (finish.error) {
      if (wire.shouldFailover(finish.error)) {
        wire.markRefusal(finish.error);
        // The turn moved to another provider. No ledger row is written for this dead attempt:
        // the loop's next iteration records the turn under the new decision.
        if (await input.onFailover?.()) return { kind: "failover" };
      }
      return {
        kind: "response",
        response: rpcErrorResponse(wire, meta, clientKind, finish.error, headers),
      };
    }
    finalize(finish);
    if (clientKind === "anthropic") {
      const message = chatToAnthropicMessage(completion, model);
      return {
        kind: "response",
        response: c.json(
          {
            ...withThinkingBlock(message, completion),
            usage: {
              input_tokens: finish.usage.input,
              output_tokens: finish.usage.output,
              cache_read_input_tokens: finish.usage.cacheRead,
              cache_creation_input_tokens: finish.usage.cacheWrite,
            },
          },
          200,
          headers,
        ),
      };
    }
    if (clientKind === "responses") {
      return {
        kind: "response",
        response: c.json(
          chatJsonToResponse(completion, {
            model,
            session: decision.session,
            started,
            usage: inclusiveUsage(finish.usage),
          }),
          200,
          headers,
        ),
      };
    }
    return { kind: "response", response: c.json(completion, 200, headers) };
  }

  // The client hanging up is recorded from what it actually saw, before the relay's own cancel
  // runs: a synthetic "aborted" error there must not turn a delivered turn into a 502.
  const tracker = trackEvents();
  const onCancel = (): void => {
    const outcome = cancelOutcome(tracker);
    const cost = costOf(model, outcome.usage, new Date(), decision.provider, provider.type);
    record(meta, outcome.status, outcome.usage, cost.usd, cost.known, outcome.error);
  };
  const feed = (event: StreamEvent): void => tracker.feed(event);
  if (clientKind === "openai") {
    return {
      kind: "response",
      response: streamResponse(input.relay(finalize, feed), meta, headers, undefined, onCancel),
    };
  }
  // The ledger row is written after the last stage flushes, from the wire's exclusive usage:
  // the chat hop has no cache-write field, so letting a later stage's usage win would
  // under-bill.
  let finish: RpcFinish<E> | undefined;
  const toChat = input.relay((result) => {
    finish = result;
  }, feed);
  const next =
    clientKind === "anthropic"
      ? chatToAnthropicStream(model, {
          usage: () => finish?.usage,
          onFinish: () => finalize(finish),
        })
      : chatToResponsesStream(model, () => finalize(finish));
  return {
    kind: "response",
    response: streamResponse(toChat.pipeThrough(next), meta, headers, undefined, onCancel),
  };
}

const CURSOR_ERROR_TYPES: Record<CursorStreamError["kind"], string> = {
  auth: "authentication_error",
  region: "permission_error",
  quota: "rate_limit_error",
  rate_limit: "rate_limit_error",
  context: "invalid_request_error",
  invalid: "invalid_request_error",
  capacity: "overloaded_error",
  other: "api_error",
};

/**
 * Whether a Cursor refusal should send the turn to another provider. Everything Cursor says
 * about the account or the model right now — a regional block, a quota, an overload, a rejected
 * credential — is a fact another provider can usually take the turn for. Only a malformed
 * request is the client's to fix, so it is not failed over.
 */
function cursorShouldFailover(error: CursorStreamError): boolean {
  return error.kind !== "invalid" && error.kind !== "context";
}

function cursorErrorStatus(error: CursorStreamError): number {
  return error.status >= 400 && error.status <= 599 ? error.status : 502;
}

/** Records what a Cursor refusal means for routing, so the next `decideRoute` walks past it. */
function markCursorRefusal(provider: Provider, error: CursorStreamError): void {
  const cooldown = new Date(Date.now() + PROVIDER_COOLDOWN_MS).toISOString();
  switch (error.kind) {
    case "quota":
    case "rate_limit":
      markProviderSpent(provider, { label: "limit", resetsAt: cooldown });
      return;
    case "region":
      markProviderSpent(provider, { label: "region", resetsAt: cooldown });
      return;
    case "capacity":
    case "other":
      markProviderSpent(provider, { label: error.kind, resetsAt: cooldown });
      return;
    case "auth":
      // The token the keychain or auth.json handed out is rejected: drop the cache so the
      // next resolve re-reads it (and `cursor-agent status` can mint a fresh one).
      invalidateOAuthToken("cursor");
      markProviderSpent(provider, { label: "auth", resetsAt: cooldown });
      return;
    default:
      // `invalid` and `context` carry no verdict about the subscription.
      return;
  }
}

async function forward(
  c: Context<AppEnv>,
  config: Config,
  store: SessionStore,
  /** The wire the client speaks. The router may still reach the upstream on another one. */
  clientKind: RequestKind,
): Promise<Response> {
  const started = Date.now();
  const endpoint =
    clientKind === "openai"
      ? "/chat/completions"
      : clientKind === "anthropic"
        ? "/messages"
        : "/responses";

  let body: Record<string, unknown>;
  let decodedBytes: Uint8Array;
  try {
    // Codex sends the /v1/responses body zstd-compressed, so the encoding must
    // be decoded before the JSON can be parsed. Reading the raw bytes keeps the
    // original body available for the upstream request.
    const raw = new Uint8Array(await c.req.arrayBuffer());
    decodedBytes = decodeBody(raw, c.req.header("content-encoding"));
    body = JSON.parse(new TextDecoder().decode(decodedBytes)) as Record<string, unknown>;
  } catch {
    return c.json({ error: { message: "Invalid JSON body", type: "invalid_request_error" } }, 400);
  }

  // ChatGPT Desktop dual catalog: native models keep using OpenAI / the
  // ChatGPT subscription. Only `jevonian/*` (and bare `auto`) stay on Jevonian.
  // A caller holding a real Jevonian key is talking to Jevonian, so a concrete
  // model it names (swe-2-max, claude-opus-4-6-thinking, …) is routed locally
  // instead of being forwarded to OpenAI with that key.
  if (clientKind === "responses" || clientKind === "openai") {
    const model = typeof body.model === "string" ? body.model : "";
    const nativeRoute = c.get("jevoKey") ? false : shouldProxyNativeCodex(model, c.req.raw.headers);
    if (nativeRoute) {
      return proxyNativeCodex(c, nativeRoute, decodedBytes);
    }
    if (model && !isDesktopRoutedModel(model)) {
      const auth = c.req.header("authorization") ?? "";
      const token = auth.replace(/^Bearer\s+/i, "").trim();
      if ((LOCAL_CLIENT_KEYS as readonly string[]).includes(token)) {
        return c.json(
          {
            error: {
              message: "OpenAI models require signing in to ChatGPT or adding an OpenAI API key",
              type: "authentication_error",
            },
          },
          401,
        );
      }
    }
  }

  // Claude Desktop can only ask for the Claude ids it knows, so its gateway
  // profile stands in for Jevonian aliases under those ids. Translate back
  // before routing, so the turn is routed like any other Jevonian request.
  if (clientKind === "anthropic") {
    const stand_in = resolveClaudeGatewayModel(
      typeof body.model === "string" ? body.model : "",
      config,
      isClaudeGatewayRequest(c.req.raw.headers),
    );
    if (stand_in) body = { ...body, model: stand_in };
  }

  const clientStream = body.stream === true;
  const requestId = crypto.randomUUID();
  const keyId = c.get("keyId") as string | undefined;
  const keyName = c.get("keyName") as string | undefined;
  const incomingHeaders = requestHeaders(c);
  // The client's own session header, when it sent one. A resolved session key can be a
  // prompt fingerprint the client does not know, so both are kept for lookup.
  const clientSession =
    incomingHeaders["x-session-id"] ??
    incomingHeaders["x-jevonian-session"] ??
    incomingHeaders["x-opencode-session"];
  let decision: RouteDecision;
  {
    // Codex remote compaction must stay on a native Responses (ChatGPT) upstream.
    // Running it through decideRoute / brain can land on OpenRouter etc., and the
    // Chat Completions bridge then synthesizes a message item instead of `compaction`.
    const initial =
      clientKind === "responses" && isRemoteCompactionV2(body)
        ? remoteCompactionDecision(config, body, incomingHeaders, requestId)
        : await decideRoute({
            config,
            body,
            headers: incomingHeaders,
            store,
            kind: clientKind,
            requestId,
            keyId,
            keyName,
          });
    if ("error" in initial) {
      return c.json(
        { error: { message: initial.error, type: "jevonian_error" } },
        (initial.status ?? 400) as 400,
      );
    }
    decision = initial;
  }

  // The trace exists from the first decision on, so a turn is observable before its first
  // byte and a failed attempt is recorded even when the turn never reaches the ledger.
  beginRoute({
    requestId,
    session: decision.session,
    ...(clientSession && clientSession !== decision.session ? { clientSession } : {}),
    ...(keyId ? { keyId } : {}),
    ...(keyName ? { keyName } : {}),
    path: endpoint,
    requestedModel: decision.requestedModel,
    stream: clientStream,
    startedAt: started,
    phase: decision.phase,
    reason: decision.reason,
    ...(decision.cacheKeep ? { cacheKeep: decision.cacheKeep } : {}),
  });
  if (decision.order && decision.order.length > 0) weigh(requestId, decision.order);

  // A conservative token estimate can be a false positive. Try to compact proactively,
  // but if Jev is unavailable or there are no stale tool results, let the provider make the
  // final call instead of rejecting a request that might fit its actual context window.
  if (decision.contextOverflow && !isRemoteCompactionV2(body)) {
    const compacted = await compactForOverflow(config, body);
    if (compacted.ok) {
      const retry = await decideRoute({
        config,
        body: compacted.body,
        headers: incomingHeaders,
        store,
        kind: clientKind,
        requestId,
        keyId,
        keyName,
      });
      if (!("error" in retry)) {
        body = compacted.body;
        decision = retry;
        noteDecision(requestId, {
          phase: retry.phase,
          reason: retry.reason,
          ...(retry.cacheKeep ? { cacheKeep: retry.cacheKeep } : {}),
        });
        if (retry.order && retry.order.length > 0) weigh(requestId, retry.order);
      }
    }
  }

  let quotaFailovers = 0;
  let overflowRetries = 0;
  /** Passes through the routing loop: the count is what distinguishes initial from failover. */
  let attemptCount = 0;
  /**
   * Every provider/model this turn has already tried and been refused by. Failover keeps
   * walking the routing chain until `decideRoute` can only offer a target from this set —
   * which is the point where the client is genuinely out of options and the refusal is
   * worth surfacing. A fixed attempt cap is not enough: a routing chain with five healthy
   * subscriptions would still be cut off after two, and the client would see a rate-limit
   * error while three usable providers sat idle.
   */
  const triedTargets = new Set<string>([planKey(decision)]);
  /**
   * Re-routes the turn after the current provider refused it for quota. Returns true when a
   * provider/model that has not been tried yet took the turn, so the caller should `continue`
   * the loop.
   */
  const quotaFailover = async (): Promise<boolean> => {
    // Remote compaction v2 only ChatGPT's Responses API can answer. Failover onto
    // OpenRouter/DeepSeek would bridge to Chat Completions and Codex would then
    // see "got 0 compaction items" — or our bridge guard. Keep the upstream error.
    if (clientKind === "responses" && isRemoteCompactionV2(body)) return false;
    const next = nextFromPlan(decision, triedTargets);
    if (!next) return false;
    triedTargets.add(planKey(next));
    decision = { ...next, reason: `${decision.reason}:quota-failover` };
    quotaFailovers += 1;
    noteDecision(requestId, {
      phase: decision.phase,
      reason: decision.reason,
      ...(decision.cacheKeep ? { cacheKeep: decision.cacheKeep } : {}),
    });
    if (next.order && next.order.length > 0) weigh(requestId, next.order);
    return true;
  };
  /**
   * Closes an RPC subscription attempt that refused before streaming. Returns true when the
   * turn moved to another provider (the caller `continue`s), false when the refusal is the
   * client's answer.
   */
  const rpcRefused = async <E extends { kind: string; message: string }>(
    wire: RpcWire<E>,
    error: E,
  ): Promise<boolean> => {
    const refused = wire.shouldFailover(error);
    endTry(requestId, {
      status: wire.status(error),
      fail: refused ? error.kind : `http-${wire.status(error)}`,
    });
    if (!refused) return false;
    wire.markRefusal(error);
    return quotaFailover();
  };
  while (true) {
    const meta = decisionMeta(decision, endpoint, clientStream, started, requestId, keyId, keyName);
    const provider: Provider | undefined = findProviderByName(config, decision.provider);
    if (!provider) {
      return errorResponse(c, meta, 404, `Provider "${decision.provider}" is not configured`);
    }
    // Skip a host the breaker has already condemned, or one that is at capacity, before we
    // burn a try slot on it. The turn's remaining budget belongs to a healthy candidate.
    const blocked = beginProviderAttempt(provider.name);
    if (blocked) {
      if (await quotaFailover()) {
        if (attemptCount > 0) endTry(requestId, { fail: blocked });
        continue;
      }
      return errorResponse(c, meta, 502, `Provider "${provider.name}" is unavailable (${blocked})`);
    }
    // Optimistic until a refusal / transport failure marks the attempt failed. Success paths
    // leave this true so a half-open probe that lands closes the breaker.
    let attemptOk = true;
    try {
      // Every pass through this loop is one upstream attempt. The first is `initial`; a re-route
      // after a refusal or a context retry is a `failover`, which is what the waterfall draws.
      const attemptCause: TryCause = attemptCount === 0 ? "initial" : "failover";
      // Devin and Cursor can re-route from inside their response helpers, past the explicit
      // `endTry` calls below. Close whatever is still open so no attempt is left dangling;
      // `endTry` is a no-op when the previous attempt was already closed.
      if (attemptCount > 0) endTry(requestId, { fail: "refused" });
      attemptCount += 1;
      beginTry(requestId, {
        provider: decision.provider,
        model: decision.model,
        cause: attemptCause,
        ...(decision.effort ? { effort: decision.effort } : {}),
      });
      meta.store = store;
      meta.billing = provider.billing;
      // A failover moved the turn to a new target without re-deciding, so the session's record
      // is brought with it. The turn count and the cache observation are untouched: a failover
      // is not a new turn, and the observation names the provider that measured it.
      if (attemptCount > 1) store.retarget(decision.session, decision, started);

      const translated = provider.type === "responses" && clientKind === "openai";
      const geminiWire = provider.type === "gemini";
      const devinWire = provider.type === "devin";
      const cursorWire = provider.type === "cursor";
      const planned = planUpstreamWire({
        provider,
        client: clientKind,
        model: decision.model,
      });
      if ("error" in planned) {
        return errorResponse(c, meta, 400, planned.error);
      }
      const bridgeToAnthropic = planned.bridge === "to-anthropic";
      const bridgeToOpenAI = planned.bridge === "to-openai";
      if (
        clientKind === "responses" &&
        isRemoteCompactionV2(body) &&
        provider.type !== "responses"
      ) {
        return errorResponse(
          c,
          meta,
          400,
          `Remote compaction requires ChatGPT's Responses API; "${provider.name}" cannot serve it.`,
        );
      }
      let upstreamKind: RequestKind = planned.wire;
      // Devin and Cursor report exclusive usage (uncached input, cache reads, cache writes
      // apart), the same accounting Anthropic uses, so cache observation must not subtract reads
      // from input again.
      meta.usageKind = devinWire || cursorWire ? "anthropic" : upstreamKind;
      // Devin's and Cursor's RPCs only stream; WorkBuddy AI refuses non-stream chats.
      // Non-stream clients get the stream folded into one reply.
      const workbuddyWire = isWorkbuddyAiSource(provider.oauthSource);
      const upstreamStream =
        provider.type === "responses" || devinWire || cursorWire || workbuddyWire
          ? true
          : clientStream;

      let auth = await resolveProviderAuth(provider, upstreamKind, decision.session);
      if (auth.error) return errorResponse(c, meta, 400, auth.error);
      if (devinWire && !auth.token) {
        return errorResponse(c, meta, 400, `Missing Devin token for provider "${provider.name}"`);
      }
      if (cursorWire && !auth.token) {
        return errorResponse(c, meta, 400, `Missing Cursor token for provider "${provider.name}"`);
      }
      withSessionAffinity(auth.headers, provider, decision.session, incomingHeaders);
      // An upstream SSE error frame can echo the credential it rejected; scrub it before the text
      // reaches the user or the ledger. `auth` is read lazily because a 401 refresh reassigns it.
      const redactProvider = (text: string): string =>
        redactSecrets(text, [auth.token, provider.apiKey]);

      saveBody(requestId, {
        kind: "request",
        at: new Date().toISOString(),
        path: endpoint,
        decision: {
          provider: decision.provider,
          model: decision.model,
          requestedModel: decision.requestedModel,
          phase: decision.phase,
          reason: decision.reason,
          cache: decision.cache,
          switchPenaltyUsd: decision.switchPenaltyUsd,
          brain: decision.brain,
          ...(decision.brainChannel ? { brainChannel: decision.brainChannel } : {}),
          ...(decision.canonical ? { canonical: decision.canonical } : {}),
        },
        body,
      });

      let prep;
      try {
        prep = await prepareUpstream({
          config,
          provider,
          decision,
          clientKind,
          clientStream,
          body,
          upstreamStream,
          planned,
          translated,
          auth,
        });
      } catch (error) {
        if (error instanceof DevinImageError) {
          record(meta, 400, emptyUsage(), null, true, error.message);
          const payload =
            clientKind === "anthropic"
              ? { type: "error", error: { type: "invalid_request_error", message: error.message } }
              : { error: { type: "invalid_request_error", message: error.message } };
          return c.json(payload, 400);
        }
        throw error;
      }
      let upstreamBody = prep.body;
      const { passbackReasoning, passbackMessages, maxOutput } = prep;
      if (prep.savedTokens) meta.savedTokens = prep.savedTokens;
      meta.effort = prep.sentEffort;
      if (prep.effortNote) meta.effortNote = prep.effortNote;

      const urlFor = (wire: RequestKind): string =>
        geminiWire
          ? geminiEndpoint(provider.baseUrl, upstreamStream)
          : upstreamUrlFor(provider, wire);
      const upstreamUrl = devinWire ? devinChatUrl(provider.baseUrl) : urlFor(upstreamKind);
      // Built once per wire, not per attempt: a retry repeats the same bytes, which is the whole
      // point of retrying a POST that failed on the network.
      const payload = devinWire
        ? ""
        : JSON.stringify(payloadFor({ body: upstreamBody }, upstreamKind, provider));
      // Devin carries its session token inside the protobuf body, so its request is rebuilt when
      // a 401 forces a fresh token; everything else only swaps headers.
      const requestInit = (current: AuthResolution): RequestInit => {
        if (!devinWire) return { method: "POST", headers: current.headers, body: payload };
        const token = current.token ?? "";
        return {
          method: "POST",
          headers: devinHeaders(token, "stream"),
          body: buildDevinChatRequest(token, upstreamBody, decision.model, {
            // Stable per conversation, so Devin's prompt cache keeps hitting across turns.
            sessionId: decision.session,
            ...(maxOutput ? { maxOutput } : {}),
            builtins: config.promptPolicy.builtins,
          }) as Uint8Array<ArrayBuffer>,
        };
      };

      // A Cursor turn is a Connect stream that stays open both ways, so it is driven here rather
      // than through the shared POST path: the request body is built per attempt and the reply is
      // folded or relayed by `rpcClientResponse`.
      if (cursorWire) {
        const wire = cursorWireAdapter(provider);
        const conversation = cursorConversation(upstreamBody);
        const attempt = await runCursor({
          token: auth.token ?? "",
          agentUrl: await resolveCursorAgentUrl(auth.token ?? "", provider.baseUrl),
          systemPrompt: conversation.system,
          messages: conversation.messages,
          tools: conversation.tools,
          model: cursorModelId(decision.model, decision.effort, false),
          lastUser: cursorLastUser(conversation.messages),
          requestId: crypto.randomUUID(),
        });
        const rpcHeaders = (): Record<string, string> => ({
          ...decisionHeaders(decision, meta.retries),
          ...(quotaFailovers > 0 ? { "x-jevonian-quota-failovers": String(quotaFailovers) } : {}),
        });
        if (attempt.error) {
          if (await rpcRefused(wire, attempt.error)) {
            attemptOk = false;
            continue;
          }
          return rpcErrorResponse(wire, meta, clientKind, attempt.error, rpcHeaders());
        }
        const events = attempt.events;
        const delivered = await rpcClientResponse({
          c,
          meta,
          provider,
          decision,
          clientKind,
          clientStream,
          started,
          wire,
          fold: () => cursorChatCompletion(decision.model, events),
          relay: (onFinish, onEvent) =>
            cursorToChatStream(decision.model, events, onFinish, onEvent),
          onFailover: quotaFailover,
          headers: rpcHeaders(),
        });
        if (delivered.kind === "failover") {
          attemptOk = false;
          continue;
        }
        return delivered.response;
      }

      // A socket reset from a local proxy, a DNS timeout, or a gateway's brief 502 otherwise
      // costs the whole turn — and the same request almost always succeeds on a second attempt.
      // Same-host retries are short on purpose: a host that keeps failing should hand the turn
      // to failover, not burn the whole budget on itself.
      const retryBudget = configuredSameHostRetries();
      const onRetry = ({ attempt, delayMs, failure }: RetryAttempt): void => {
        meta.retries = (meta.retries ?? 0) + 1;
        // The attempt that just failed is closed here, and the retry that follows is opened as
        // its own try, so the waterfall shows the transient failure rather than hiding it.
        endTry(requestId, { fail: describeRetryFailure(failure) });
        beginTry(requestId, {
          provider: decision.provider,
          model: decision.model,
          cause: "retry",
          ...(decision.effort ? { effort: decision.effort } : {}),
        });
        console.warn(
          `upstream retry ${attempt}/${retryBudget} for ${decision.provider} in ${delayMs}ms: ${describeRetryFailure(failure)}`,
        );
      };

      let upstream: Response;
      let failureText = "";
      try {
        ({ response: upstream, text: failureText } = await postUpstream(
          upstreamUrl,
          requestInit(auth),
          onRetry,
          { stream: upstreamStream },
        ));
        if (
          upstream.status === 401 &&
          provider.auth === "oauth" &&
          provider.oauthSource &&
          provider.oauthSource !== "static"
        ) {
          invalidateOAuthToken(provider.oauthSource, provider.login);
          const refreshed = await resolveProviderAuth(provider, upstreamKind, decision.session);
          if (!refreshed.error) {
            auth = refreshed;
            ({ response: upstream, text: failureText } = await postUpstream(
              upstreamUrl,
              requestInit(auth),
              onRetry,
              { stream: upstreamStream },
            ));
          }
        }
      } catch (error) {
        // A transport failure is a verdict about this host, not the request: the same body
        // almost always succeeds on a different provider. Walk the plan before surfacing a 502.
        attemptOk = false;
        endTry(requestId, { fail: `fetch: ${describeFetchError(error)}`.slice(0, 120) });
        markProviderSpent(provider, {
          label: "transport",
          resetsAt: new Date(Date.now() + PROVIDER_COOLDOWN_MS).toISOString(),
        });
        if (await quotaFailover()) continue;
        return errorResponse(c, meta, 502, `Upstream request failed: ${describeFetchError(error)}`);
      }

      captureQuotaHeaders(provider, upstream.headers);

      if (devinWire) {
        // Devin refuses in two ways: a non-200 status, or a 200 whose Connect stream opens with an
        // end-of-stream error trailer before any data. Both classify into one error, so quota
        // failover and the client-facing error share one path.
        const devin = devinWireAdapter(provider, decision.model);
        const rpcHeaders = (): Record<string, string> => ({
          ...decisionHeaders(decision, meta.retries),
          ...(quotaFailovers > 0 ? { "x-jevonian-quota-failovers": String(quotaFailovers) } : {}),
        });
        let failure: DevinStreamError | undefined;
        let stream: ReadableStream<Uint8Array> | undefined;
        let policyRetried = false;
        if (!upstream.ok) {
          failure = classifyDevinError(upstream.status, failureText, auth.token);
        } else if (!upstream.body) {
          failure = {
            status: 502,
            kind: "other",
            message: "Devin returned an empty response body",
          };
        } else {
          const peeked = await peekDevinStream(upstream.body, auth.token);
          if ("error" in peeked) failure = peeked.error;
          else stream = peeked.stream;
        }
        // The wire neutralizes the prompt signatures we know about, but that list trails the
        // client. One retry without the client's system prompt clears wording we have not seen.
        if (failure?.kind === "content_policy" && !policyRetried) {
          policyRetried = true;
          upstreamBody = {
            ...upstreamBody,
            messages: stripAgentSystemMessages(
              Array.isArray(upstreamBody.messages) ? upstreamBody.messages : [],
            ),
          };
          meta.retries = (meta.retries ?? 0) + 1;
          console.warn(
            `devin content policy: retrying ${decision.provider}/${decision.model} without the client system prompt`,
          );
          try {
            ({ response: upstream, text: failureText } = await postUpstream(
              upstreamUrl,
              requestInit(auth),
              onRetry,
              { stream: true },
            ));
            failure = undefined;
            stream = undefined;
            if (!upstream.ok) {
              failure = classifyDevinError(upstream.status, failureText, auth.token);
            } else if (!upstream.body) {
              failure = {
                status: 502,
                kind: "other",
                message: "Devin returned an empty response body",
              };
            } else {
              const peeked = await peekDevinStream(upstream.body, auth.token);
              if ("error" in peeked) failure = peeked.error;
              else stream = peeked.stream;
            }
          } catch (error) {
            failure = {
              status: 502,
              kind: "other",
              message: `Upstream retry failed: ${describeFetchError(error)}`,
            };
          }
        }
        if (failure || !stream) {
          const error = failure ?? {
            status: 502,
            kind: "other" as const,
            message: "Devin returned no stream",
          };
          if (await rpcRefused(devin, error)) {
            attemptOk = false;
            continue;
          }
          return rpcErrorResponse(devin, meta, clientKind, error, rpcHeaders());
        }
        const token = auth.token;
        const body = stream;
        const delivered = await rpcClientResponse({
          c,
          meta,
          provider,
          decision,
          clientKind,
          clientStream,
          started,
          wire: devin,
          fold: () => devinChatCompletion(body, decision.model, token),
          relay: (onFinish, onEvent) =>
            body.pipeThrough(devinToChatStream(decision.model, onFinish, token, onEvent)),
          onFailover: quotaFailover,
          headers: rpcHeaders(),
        });
        // A non-stream Devin call that was refused mid-answer is re-routed rather than returned,
        // so the client only sees an error once no alternative is left.
        if (delivered.kind === "failover") {
          attemptOk = false;
          continue;
        }
        return delivered.response;
      }

      if (!upstream.ok) {
        // Read during the attempt, not here: a body left unread would hold the pooled socket
        // that the next retry needs, and the last attempt's text is what gets reported.
        const text = failureText;
        if (
          overflowRetries === 0 &&
          !isRemoteCompactionV2(body) &&
          isContextOverflowResponse(upstream.status, text)
        ) {
          overflowRetries += 1;
          const shrunk = await compactForOverflow(config, body);
          if (shrunk.ok) {
            const retry = await decideRoute({
              config,
              body: shrunk.body,
              headers: incomingHeaders,
              store,
              kind: clientKind,
              requestId,
              keyId,
              keyName,
            });
            if (!("error" in retry)) {
              body = shrunk.body;
              decision = { ...retry, reason: `${retry.reason}:context-retry` };
              endTry(requestId, { status: upstream.status, fail: "context-overflow" });
              noteDecision(requestId, {
                phase: decision.phase,
                reason: decision.reason,
                ...(decision.cacheKeep ? { cacheKeep: decision.cacheKeep } : {}),
              });
              if (retry.order && retry.order.length > 0) weigh(requestId, retry.order);
              continue;
            }
          }
        }
        // Structured spend tokens (usage_limit_reached, GoUsageLimitError, …) mark the
        // provider exhausted. Claude subscription 429s usually only emit
        // `type: rate_limit_error` — not in that allow-list — but the unified rate-limit
        // headers on the same response already say the window is spent. After capturing
        // those headers, treat an exhausted health bit as the same failover trigger so
        // the next model in the phase chain (gpt-6-astra, …) gets the turn.
        const spent = captureUsageLimit(provider, upstream.status, text);
        // The response's own rate-limit headers may already have recorded the real window
        // (`5h` rejected, with its reset) a few lines above. That reading beats a synthetic
        // cooldown, so it is checked before one is invented.
        const exhausted = providerQuotaHealth(provider).status === "exhausted";
        // Beyond the allow-list: any provider-side refusal is worth trying elsewhere. The
        // client only sees an error once `quotaFailover` reports that no target outside
        // `triedTargets` is left, which is the honest "you really are out" signal.
        const refused = spent || exhausted || isProviderRefusal(upstream.status);
        if (refused && !spent && !exhausted && isRateLimitRefusal(upstream.status)) {
          // Nothing recorded this refusal, so bench the provider briefly: without it the next
          // turn's routing brain would pick the same host straight back up.
          markProviderSpent(provider, {
            label: "rate-limit",
            resetsAt: new Date(Date.now() + PROVIDER_COOLDOWN_MS).toISOString(),
          });
        }
        if (refused && (await quotaFailover())) {
          attemptOk = false;
          endTry(requestId, {
            status: upstream.status,
            fail: spent || exhausted ? "quota" : `http-${upstream.status}`,
          });
          continue;
        }
        endTry(requestId, { status: upstream.status, fail: `http-${upstream.status}` });
        record(meta, upstream.status, emptyUsage(), null, true, text.slice(0, 300));
        // A streaming client must not receive a bare JSON error body: the harness reads that as a
        // malformed response and can drop the turn. Close it as an assistant message instead. The
        // body is redacted first: a provider that rejects a credential often echoes it back, and
        // this text is shown to the user.
        if (clientStream) {
          const reason = redactSecrets(text, [auth.token, provider.apiKey]);
          return new Response(
            softCompletionStream(
              clientKind,
              decision.model,
              softErrorMessage(reason.slice(0, 300)),
            ),
            {
              status: 200,
              headers: {
                "content-type": "text/event-stream",
                "cache-control": "no-store",
                "x-jevonian-soft-error": "1",
                ...decisionHeaders(decision, meta.retries),
                ...(quotaFailovers > 0
                  ? { "x-jevonian-quota-failovers": String(quotaFailovers) }
                  : {}),
              },
            },
          );
        }
        return new Response(text, {
          status: upstream.status,
          headers: {
            "content-type": upstream.headers.get("content-type") ?? "application/json",
            ...decisionHeaders(decision, meta.retries),
            ...(quotaFailovers > 0 ? { "x-jevonian-quota-failovers": String(quotaFailovers) } : {}),
          },
        });
      }

      // A stream that never produces its first byte must fail over *before* the client is
      // committed to this host. Waiting for the soft-error close used to take 23 minutes.
      if (upstreamStream && upstream.body) {
        try {
          const timeouts = upstreamTimeouts();
          const guarded = await guardUpstreamStream(upstream.body, {
            firstByteMs: timeouts.firstByteMs,
            idleMs: timeouts.idleMs,
          });
          upstream = new Response(guarded, {
            status: upstream.status,
            statusText: upstream.statusText,
            headers: upstream.headers,
          });
        } catch (error) {
          attemptOk = false;
          endTry(requestId, {
            fail: `first-byte: ${describeFetchError(error)}`.slice(0, 120),
          });
          markProviderSpent(provider, {
            label: "transport",
            resetsAt: new Date(Date.now() + PROVIDER_COOLDOWN_MS).toISOString(),
          });
          if (await quotaFailover()) continue;
          return errorResponse(
            c,
            meta,
            502,
            `Upstream request failed: ${describeFetchError(error)}`,
          );
        }
      }

      // Follow the wire the upstream actually answered on. `bridgeToAnthropic` covers
      // both dual-wire hosts (Claude on OpenCode) and Anthropic-only OAuth subscriptions.
      // Responses clients take a second hop: Anthropic → Chat Completions → Responses.
      if (
        upstreamKind === "anthropic" &&
        bridgeToAnthropic &&
        (clientKind === "openai" || clientKind === "responses")
      ) {
        if (clientKind === "responses") {
          if (!upstreamStream) {
            const json = (await upstream.json()) as Record<string, unknown>;
            const usage = anthropicUsage(json.usage);
            const cost = costOf(decision.model, usage, new Date(), decision.provider);
            record(meta, 200, usage, cost.usd, cost.known);
            return c.json(
              chatJsonToResponse(anthropicToChat(json, decision.model), {
                model: decision.model,
                session: decision.session,
                started,
                usage,
              }),
              200,
              decisionHeaders(decision, meta.retries),
            );
          }
          // Usage is Anthropic's, captured by the first stage: the Chat hop has no cache-write
          // field, so letting the Responses stage's usage win zeroed cacheRead/cacheWrite and
          // under-billed cached turns. Recorded once, after the last stage flushes.
          const usage = emptyUsage();
          // The Chat hop swallows the upstream `{ error }` and finishes the turn softly, so the
          // failure is only visible here — the Responses stage sees a normal completion. Record
          // it as the real 502 rather than a clean 200.
          let failure: string | undefined;
          const toChat = anthropicToChatStream(decision.model, (finalUsage, chatFailure) => {
            Object.assign(usage, finalUsage);
            failure = chatFailure;
          });
          const toResponses = chatToResponsesStream(decision.model, (result) => {
            const failed = failure ?? result.failure;
            if (failed) {
              record(meta, 502, usage, null, true, failed);
              return;
            }
            const cost = costOf(decision.model, usage, new Date(), decision.provider);
            record(meta, 200, usage, cost.usd, cost.known);
          });
          const streamBody = upstream.body?.pipeThrough(toChat).pipeThrough(toResponses) ?? null;
          return streamResponse(
            streamBody,
            meta,
            decisionHeaders(decision, meta.retries),
            "text/event-stream",
            undefined,
            redactProvider,
          );
        }
        if (!upstreamStream) {
          const json = (await upstream.json()) as Record<string, unknown>;
          const usage = anthropicUsage(json.usage);
          const cost = costOf(decision.model, usage, new Date(), decision.provider);
          record(meta, 200, usage, cost.usd, cost.known);
          return c.json(
            anthropicToChat(json, decision.model),
            200,
            decisionHeaders(decision, meta.retries),
          );
        }
        const usage = emptyUsage();
        const tracker = trackEvents();
        const transform = anthropicToChatStream(
          decision.model,
          (finalUsage, failure) => {
            Object.assign(usage, finalUsage);
            // The refusal was already closed softly for the client; the ledger must still carry
            // the real status instead of a clean 200 for a failed turn.
            if (failure) {
              record(meta, 502, usage, null, true, failure);
              return;
            }
            const cost = costOf(decision.model, usage, new Date(), decision.provider);
            record(meta, 200, usage, cost.usd, cost.known);
          },
          (event) => tracker.feed(event),
        );
        return streamResponse(
          upstream.body?.pipeThrough(transform) ?? null,
          meta,
          decisionHeaders(decision, meta.retries),
          "text/event-stream",
          () => {
            const outcome = cancelOutcome(tracker);
            const cost = costOf(decision.model, outcome.usage, new Date(), decision.provider);
            record(meta, outcome.status, outcome.usage, cost.usd, cost.known, outcome.error);
          },
          redactProvider,
        );
      }

      // Gemini must win over the OpenAI bridge on the way back too. planUpstreamWire
      // labels Antigravity as wire=openai + bridge=to-openai for Responses clients, but
      // the upstream still answers in Gemini shape. Parsing that as Chat Completions
      // yields empty `output: []` / a lone response.completed — Codex then shows "done"
      // with no assistant text.
      if (geminiWire) {
        if (clientKind === "responses") {
          // Antigravity answers in Gemini shape; fold to chat then to Responses SSE/JSON.
          if (!upstreamStream) {
            const json = (await upstream.json()) as Record<string, unknown>;
            const response = unwrapGemini(json);
            const chat = geminiChatCompletion(response, decision.model);
            const usage = geminiUsage(response.usageMetadata);
            const cost = costOf(decision.model, usage, new Date(), decision.provider);
            record(meta, 200, usage, cost.usd, cost.known);
            return c.json(
              chatJsonToResponse(chat, {
                model: decision.model,
                session: decision.session,
                started,
                usage,
              }),
              200,
              decisionHeaders(decision, meta.retries),
            );
          }
          // Stream Gemini → chat SSE → Responses SSE.
          const usage = emptyUsage();
          const toChat = geminiToChatStream(decision.model, (finalUsage) => {
            Object.assign(usage, finalUsage);
          });
          const toResponses = chatToResponsesStream(decision.model, (result) => {
            Object.assign(usage, result.usage);
            const cost = costOf(decision.model, usage, new Date(), decision.provider);
            record(meta, 200, usage, cost.usd, cost.known);
          });
          const body = upstream.body?.pipeThrough(toChat).pipeThrough(toResponses) ?? null;
          return streamResponse(
            body,
            meta,
            decisionHeaders(decision, meta.retries),
            "text/event-stream",
            undefined,
            redactProvider,
          );
        }
        if (!upstreamStream) {
          const json = (await upstream.json()) as Record<string, unknown>;
          const response = unwrapGemini(json);
          const usage = geminiUsage(response.usageMetadata);
          const cost = costOf(decision.model, usage, new Date(), decision.provider);
          record(meta, 200, usage, cost.usd, cost.known);
          return c.json(
            geminiChatCompletion(response, decision.model),
            200,
            decisionHeaders(decision, meta.retries),
          );
        }
        const usage = emptyUsage();
        const tracker = trackEvents();
        const transform = geminiToChatStream(
          decision.model,
          (finalUsage) => {
            Object.assign(usage, finalUsage);
            const cost = costOf(decision.model, usage, new Date(), decision.provider);
            record(meta, 200, usage, cost.usd, cost.known);
          },
          (event) => tracker.feed(event),
        );
        return streamResponse(
          upstream.body?.pipeThrough(transform) ?? null,
          meta,
          decisionHeaders(decision, meta.retries),
          "text/event-stream",
          () => {
            const outcome = cancelOutcome(tracker);
            const cost = costOf(decision.model, outcome.usage, new Date(), decision.provider);
            record(meta, outcome.status, outcome.usage, cost.usd, cost.known, outcome.error);
          },
          redactProvider,
        );
      }

      if (upstreamKind === "openai" && bridgeToOpenAI) {
        if (clientKind === "responses") {
          if (clientStream) {
            const usage = emptyUsage();
            const responsesBridge = new ChatToResponsesBridge(decision.model);
            const transform = chatToResponsesStream(
              decision.model,
              (result) => {
                Object.assign(usage, result.usage);
                // The bridge closes the turn softly for the client; the ledger must still record
                // the real upstream failure rather than a clean 200.
                if (result.failure) {
                  record(meta, 502, usage, null, true, result.failure);
                  return;
                }
                const cost = costOf(decision.model, usage, new Date(), decision.provider);
                record(meta, 200, usage, cost.usd, cost.known);
              },
              responsesBridge,
            );
            return streamResponse(
              upstream.body?.pipeThrough(transform) ?? null,
              meta,
              decisionHeaders(decision, meta.retries),
              "text/event-stream",
              () => {
                const outcome = cancelOutcome({
                  delivered: responsesBridge.delivered,
                  usage: responsesBridge.seenUsage,
                });
                const cost = costOf(decision.model, outcome.usage, new Date(), decision.provider);
                record(meta, outcome.status, outcome.usage, cost.usd, cost.known, outcome.error);
              },
              redactProvider,
            );
          }
          const json = (await upstream.json()) as Record<string, unknown>;
          const usage = openaiUsage(json.usage);
          const cost = costOf(decision.model, usage, new Date(), decision.provider);
          record(meta, 200, usage, cost.usd, cost.known);
          return c.json(
            chatJsonToResponse(json, {
              model: decision.model,
              session: decision.session,
              started,
              usage,
            }),
            200,
            decisionHeaders(decision, meta.retries),
          );
        }
        const json = (await upstream.json()) as Record<string, unknown>;
        const usage = openaiUsage(json.usage);
        const cost = costOf(decision.model, usage, new Date(), decision.provider);
        record(meta, 200, usage, cost.usd, cost.known);
        return c.json(
          chatToAnthropicMessage(json, decision.model),
          200,
          decisionHeaders(decision, meta.retries),
        );
      }

      if (!upstreamStream && upstreamKind !== "responses") {
        let json = (await upstream.json()) as Record<string, unknown>;
        if (passbackReasoning && clientKind === "openai") {
          rememberFromChatCompletion(json, passbackMessages, decision.session);
        }
        if (clientKind === "openai") {
          json = sanitizeOpenAIChatResponse(json);
        }
        const usage =
          clientKind === "openai" ? openaiUsage(json.usage) : anthropicUsage(json.usage);
        const cost = costOf(decision.model, usage, new Date(), decision.provider);
        record(meta, 200, usage, cost.usd, cost.known);
        return c.json(json, 200, decisionHeaders(decision, meta.retries));
      }

      // WorkBuddy forces upstream streaming; fold SSE → JSON for non-stream OpenAI clients.
      if (workbuddyWire && upstreamStream && !clientStream && upstreamKind === "openai") {
        const json = await foldOpenAIChatStream(upstream.body, decision.model);
        const sanitized = sanitizeOpenAIChatResponse(json);
        if (passbackReasoning) {
          rememberFromChatCompletion(sanitized, passbackMessages, decision.session);
        }
        const usage = openaiUsage(sanitized.usage);
        const cost = costOf(decision.model, usage, new Date(), decision.provider);
        record(meta, 200, usage, cost.usd, cost.known);
        return c.json(sanitized, 200, decisionHeaders(decision, meta.retries));
      }

      if (upstreamKind === "responses") {
        if (translated) {
          if (clientStream) {
            const transform = responsesToChatStream(decision.model, (result) => {
              if (result.failure) {
                record(meta, 502, result.usage, null, true, result.failure);
                return;
              }
              const cost = costOf(decision.model, result.usage, new Date(), decision.provider);
              record(meta, 200, result.usage, cost.usd, cost.known);
            });
            return streamResponse(
              upstream.body?.pipeThrough(transform) ?? null,
              meta,
              decisionHeaders(decision, meta.retries),
            );
          }
          const text = await upstream.text();
          const { events } = splitSseEvents(text);
          const completed = [...events]
            .reverse()
            .find((event) => event.type === "response.completed");
          const failure = responsesErrorMessage(events);
          if (!completed || failure) {
            const message = failure ?? "upstream stream ended before completion";
            // The folded stream carries a `response.failed` rather than an HTTP error, so the
            // refusal hides inside a 200. Read it like the body it is: a quota verdict still
            // fails the provider over, while a truncation is surfaced as before.
            if (messageSpendSignal(message)) {
              attemptOk = false;
              markProviderSpent(provider, { label: "limit" });
              if (await quotaFailover()) continue;
            }
            record(meta, 502, emptyUsage(), null, true, message);
            return c.json({ error: { message, type: "jevonian_error" } }, 502);
          }
          const response = asRecord(completed.response);
          const result = chatResultFromResponse(response);
          const cost = costOf(decision.model, result.usage, new Date(), decision.provider);
          record(meta, 200, result.usage, cost.usd, cost.known);
          return c.json(
            chatCompletionFrom(
              result,
              decision.model,
              `chatcmpl-${decision.session.slice(0, 16)}`,
              Math.floor(started / 1000),
            ),
            200,
            decisionHeaders(decision, meta.retries),
          );
        }

        if (!clientStream) {
          const text = await upstream.text();
          const { events } = splitSseEvents(text);
          const completed = [...events]
            .reverse()
            .find((event) => event.type === "response.completed");
          const failure = responsesErrorMessage(events);
          if (!completed || failure) {
            const message = failure ?? "upstream stream ended before completion";
            if (messageSpendSignal(message)) {
              attemptOk = false;
              markProviderSpent(provider, { label: "limit" });
              if (await quotaFailover()) continue;
            }
            record(meta, 502, emptyUsage(), null, true, message);
            return c.json({ error: { message, type: "jevonian_error" } }, 502);
          }
          const response = repairResponsesOutput(asRecord(completed.response), events);
          const usage = responsesUsage(response.usage);
          const cost = costOf(decision.model, usage, new Date(), decision.provider);
          record(meta, 200, usage, cost.usd, cost.known);
          return c.json(response, 200, decisionHeaders(decision, meta.retries));
        }

        const usage = emptyUsage();
        const transform = responsesPassthroughRepairStream((response) => {
          Object.assign(usage, responsesUsage(response.usage));
          const cost = costOf(decision.model, usage, new Date(), decision.provider);
          record(meta, 200, usage, cost.usd, cost.known);
        });
        return streamResponse(
          upstream.body?.pipeThrough(transform) ?? null,
          meta,
          decisionHeaders(decision, meta.retries),
          upstream.headers.get("content-type") ?? "text/event-stream",
          undefined,
          redactProvider,
        );
      }

      const usage = emptyUsage();
      const decoder = new TextDecoder();
      let buffer = "";
      let hasDelivered = false;
      let finished = false;
      let charsOut = 0;

      const consume = (text: string): void => {
        buffer += text;
        let index = buffer.indexOf("\n\n");
        while (index !== -1) {
          const event = buffer.slice(0, index);
          buffer = buffer.slice(index + 2);
          for (const line of event.split("\n")) {
            if (!line.startsWith("data:")) continue;
            const data = line.slice(5).trim();
            if (data === "[DONE]") {
              finished = true;
              continue;
            }
            if (data.length === 0) continue;
            try {
              const parsed = JSON.parse(data) as Record<string, unknown>;
              if (clientKind === "openai") {
                if (parsed.usage !== undefined) {
                  Object.assign(usage, openaiUsage(parsed.usage));
                }
                const choices = parsed.choices as Array<Record<string, unknown>> | undefined;
                if (choices && choices.length > 0) {
                  const choice = choices[0];
                  const delta = choice.delta as Record<string, unknown> | undefined;
                  if (delta) {
                    // A `role`-only or empty first delta is not delivered content — count only
                    // fields a client actually renders, so a cancel right after the opener still
                    // reads as an abandoned request rather than a finished turn.
                    const isContent =
                      typeof delta.content === "string" ||
                      typeof delta.reasoning_content === "string" ||
                      delta.tool_calls !== undefined;
                    if (isContent) hasDelivered = true;
                    if (typeof delta.content === "string") charsOut += delta.content.length;
                    if (typeof delta.reasoning_content === "string")
                      charsOut += delta.reasoning_content.length;
                    if (delta.tool_calls) charsOut += JSON.stringify(delta.tool_calls).length;
                  }
                  if (choice.finish_reason) {
                    hasDelivered = true;
                    finished = true;
                  }
                }
              }
              if (clientKind === "anthropic") {
                applyAnthropicEvent(parsed, usage);
                if (parsed.type === "content_block_delta" || parsed.type === "message_delta") {
                  hasDelivered = true;
                }
                if (parsed.type === "message_stop") {
                  hasDelivered = true;
                  finished = true;
                }
              }
            } catch {
              continue;
            }
          }
          index = buffer.indexOf("\n\n");
        }
      };

      const finalize = (status = 200, error?: string): void => {
        consume(decoder.decode());
        if (usage.output === 0 && charsOut > 0) {
          usage.output = Math.max(1, Math.round(charsOut / 3.5));
        }
        const cost = costOf(decision.model, usage, new Date(), decision.provider);
        record(meta, status, usage, cost.usd, cost.known, error);
      };

      const usageTransform = new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          controller.enqueue(chunk);
          consume(decoder.decode(chunk, { stream: true }));
        },
        flush() {
          finalize(200);
        },
      });

      let stream = upstream.body;
      if (passbackReasoning && clientKind === "openai" && stream) {
        stream = stream.pipeThrough(reasoningCaptureTransform(passbackMessages, decision.session));
      }
      if (clientKind === "openai" && stream) {
        stream = stream.pipeThrough(sanitizeOpenAIChatStream());
      }
      stream = stream?.pipeThrough(usageTransform) ?? null;

      const onCancel = (): void => {
        const outcome = cancelOutcome({ delivered: finished || hasDelivered, usage });
        finalize(outcome.status, outcome.error);
      };

      return streamResponse(
        stream,
        meta,
        decisionHeaders(decision, meta.retries),
        upstream.headers.get("content-type") ?? "text/event-stream",
        onCancel,
        redactProvider,
      );
    } finally {
      endProviderAttempt(provider.name, attemptOk);
    }
  }
}

export function handleOpenAI(
  c: Context<AppEnv>,
  config: Config,
  store: SessionStore,
): Promise<Response> {
  return forward(c, config, store, "openai");
}

export function handleAnthropic(
  c: Context<AppEnv>,
  config: Config,
  store: SessionStore,
): Promise<Response> {
  return forward(c, config, store, "anthropic");
}

export function handleResponses(
  c: Context<AppEnv>,
  config: Config,
  store: SessionStore,
): Promise<Response> {
  return forward(c, config, store, "responses");
}
