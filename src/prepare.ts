/**
 * The outgoing request body for one upstream attempt.
 *
 * Every wire decision about the body lives here, in the order it must run: fold the client's
 * body onto the upstream's wire, write the router's thinking level, apply prompt hygiene,
 * normalize what strict hosts reject, run the token saver, restore stripped reasoning, then the
 * per-provider envelope quirks. `forward` only asks for a prepared body and sends it; it does
 * not know which wire needed which pass.
 */

import { anthropicToChatRequest, chatToAnthropic, normalizeAnthropicPrefill } from "./anthropic";
import {
  adaptiveEffort,
  anthropicThinkingSupport,
  fitThinkingMaxTokens,
} from "./anthropic-thinking";
import { effectiveCapabilities, isReasoningEffort, type ReasoningEffort } from "./capabilities";
import type { Config, Provider } from "./config";
import { chatToGemini } from "./gemini";
import { CLAUDE_CODE_SYSTEM_PROMPT } from "./oauth";
import { rewritePromptBodies } from "./prompt-policy";
import { needsReasoningPassback, repairReasoningContent } from "./reasoning-passback";
import { chatToResponses, ensureResponsesCallIds, responsesToChatRequest } from "./responses";
import type { RequestKind, RouteDecision } from "./routing";
import { saveTokens, warnSaverUnavailable } from "./saver";
import { normalizeOpenAIMessages } from "./wire";
import { ensureWorkbuddySystem, isWorkbuddyAiSource } from "./workbuddy";

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

/**
 * Writes the router's chosen thinking level into an outgoing body, in the field the target wire
 * expects. A level the client set itself wins: the caller was explicit, and overriding an
 * instruction with a guess is worse than ignoring the router's choice.
 *
 * `none` is expressed as an explicit off rather than by omitting the field, because a model that
 * defaults to thinking would otherwise keep thinking.
 */
export function withEffort(
  body: Record<string, unknown>,
  effort: ReasoningEffort | undefined,
  wire: RequestKind,
  clientEffort?: string,
): Record<string, unknown> {
  const target = stripForeignEffort(body, wire);
  if (wire === "anthropic") {
    // Normalised on every Anthropic body, client-set levels included: newer models answer the
    // legacy shapes with a 400, and a request that cannot be sent is worse than a translated one.
    if (!effort || clientEffort) return normalizeAnthropicThinking(target);
    return anthropicWithEffort(target, effort);
  }
  if (!effort || clientEffort) return target;
  if (wire === "responses") {
    // The Responses wire spells "off" as a null reasoning object.
    if (effort === "none") return { ...target, reasoning: null };
    return { ...target, reasoning: { effort } };
  }
  return { ...target, reasoning_effort: effort };
}

/** The thinking-level field each wire uses. */
const EFFORT_FIELD: Record<RequestKind, string> = {
  anthropic: "thinking",
  openai: "reasoning_effort",
  responses: "reasoning",
};

const EFFORT_FIELDS = Object.values(EFFORT_FIELD);

/**
 * Drops thinking-level fields that do not belong to `wire`.
 *
 * A body can reach us spelled for a different wire than the endpoint it arrived on:
 * Codex sends the Chat Completions `reasoning_effort` on native `/v1/responses`
 * calls for custom model catalogs, and forwarding that spelling to a Responses
 * upstream is rejected outright with "Unsupported parameter: reasoning_effort".
 * Reducing every outgoing body to its own wire's field makes a mislabelled client
 * body routable instead of fatal, and keeps a wire from inheriting a spelling the
 * provider's schema does not have.
 */
export function stripForeignEffort(
  body: Record<string, unknown>,
  wire: RequestKind,
): Record<string, unknown> {
  const keep = EFFORT_FIELD[wire];
  const foreign = EFFORT_FIELDS.filter((field) => field !== keep && body[field] !== undefined);
  if (foreign.length === 0) return body;
  const next = { ...body };
  for (const field of foreign) delete next[field];
  return next;
}

/** Thinking budgets in tokens for the Anthropic wire, by depth. */
const EFFORT_BUDGET: Record<string, number> = {
  minimal: 1_024,
  low: 2_048,
  medium: 8_192,
  high: 16_384,
  xhigh: 24_576,
  max: 32_768,
  ultra: 32_768,
};

function effortBudget(effort: ReasoningEffort): number {
  return EFFORT_BUDGET[effort] ?? 32_768;
}

/**
 * A Chat Completions body (native, or folded from Responses) as an Anthropic Messages body.
 *
 * `chatToAnthropic` has no Chat-side thinking field to carry, so the client's own
 * `reasoning_effort` would be dropped and Claude would run without thinking even when the
 * client asked for `high`. The client's level is therefore applied here as if the router had
 * chosen it — it still wins over the router's choice — and written in the shape the model takes.
 * `max_tokens` is then made consistent with that thinking configuration.
 */
export function bridgedAnthropicBody(
  chatBody: Record<string, unknown>,
  options: {
    model: string;
    stream: boolean;
    effort?: ReasoningEffort;
    clientEffort?: ReasoningEffort;
    maxOutput?: number;
  },
): Record<string, unknown> {
  const base = { ...chatToAnthropic(chatBody), model: options.model, stream: options.stream };
  const effort = options.clientEffort ?? options.effort;
  const max = chatBody.max_completion_tokens ?? chatBody.max_tokens;
  // Claude 4.6+ rejects a conversation ending on an assistant turn ("prefill"). A Chat client can
  // re-send its last assistant reply, so drop the trailing prefill before the body leaves — after
  // the thinking pass, which reads `model` but not the message list.
  const fitted = fitThinkingMaxTokens(withEffort(base, effort, "anthropic"), {
    clientSetMax: typeof max === "number" && max > 0,
    maxOutput: options.maxOutput,
  });
  return normalizeAnthropicPrefill(fitted);
}

/** Writes `output_config.effort`, keeping any other `output_config` keys the body carries. */
function withOutputEffort(body: Record<string, unknown>, effort: string): Record<string, unknown> {
  return { ...body, output_config: { ...asRecord(body.output_config), effort } };
}

/**
 * The router's level in the shape the target Claude model accepts.
 *
 * Legacy models take a token budget. Adaptive models (Claude 4.6+) take
 * `thinking: {type: "adaptive"}` plus `output_config.effort`. "Off" stays an explicit
 * `disabled` where the model allows it, so a model that thinks by default cannot keep thinking
 * silently; always-on models reject `disabled`, so they get the lowest effort instead.
 */
function anthropicWithEffort(
  body: Record<string, unknown>,
  effort: ReasoningEffort,
): Record<string, unknown> {
  const support = anthropicThinkingSupport(body.model);
  if (!support.adaptive) {
    if (effort === "none") return { ...body, thinking: { type: "disabled" } };
    return { ...body, thinking: { type: "enabled", budget_tokens: effortBudget(effort) } };
  }
  if (effort === "none" && !support.rejectsDisabled) {
    return { ...body, thinking: { type: "disabled" } };
  }
  // Keep a client's `display` choice; everything else in `thinking` is the router's to set.
  const display = asRecord(body.thinking).display;
  return withOutputEffort(
    { ...body, thinking: { type: "adaptive", ...(display !== undefined ? { display } : {}) } },
    adaptiveEffort(effort, support),
  );
}

/**
 * Translates thinking shapes the target model rejects — typically sent by a client targeting an
 * older model — into the adaptive equivalent: `disabled` on always-on models becomes the lowest
 * effort, and a `budget_tokens` request on models without extended thinking becomes the nearest
 * effort level. An `output_config.effort` the client already set is kept.
 */
export function normalizeAnthropicThinking(body: Record<string, unknown>): Record<string, unknown> {
  const thinking = asRecord(body.thinking);
  const support = anthropicThinkingSupport(body.model);
  const disabled = thinking.type === "disabled" && support.rejectsDisabled;
  const enabled = thinking.type === "enabled" && support.rejectsEnabled;
  if (!disabled && !enabled) return body;

  const { budget_tokens: budget, type: _type, ...rest } = thinking;
  // `display` is invalid alongside `disabled` but valid with `adaptive`, so keep what remains.
  const next = { ...body, thinking: { ...rest, type: "adaptive" } };
  if (typeof asRecord(body.output_config).effort === "string") return next;
  let level: ReasoningEffort = "low";
  if (enabled && typeof budget === "number") {
    // The shallowest level whose budget covers the request, so thinking is never cut short.
    const match = Object.entries(EFFORT_BUDGET).find(([, tokens]) => tokens >= budget);
    level = match && isReasoningEffort(match[0]) ? match[0] : "max";
  }
  return withOutputEffort(next, adaptiveEffort(level, support));
}

/** Reads an adaptive `output_config.effort` back as a router level, or undefined. */
function adaptiveEffortInBody(
  body: Record<string, unknown>,
  hint?: ReasoningEffort,
): ReasoningEffort | undefined {
  const effort = asRecord(body.output_config).effort;
  if (typeof effort !== "string" || !isReasoningEffort(effort)) return undefined;
  // The router's own level wins when it is what was written (e.g. `minimal` sent as `low`);
  // `none` is excluded, because an always-on model sent `low` really does think.
  if (hint && hint !== "none") {
    const support = anthropicThinkingSupport(body.model);
    if (adaptiveEffort(hint, support) === effort) return hint;
  }
  return effort;
}

/**
 * The thinking level an outgoing body actually carries, read back from whichever field the wire
 * uses. This is what the log reports, so the ledger states the level the model was really sent
 * rather than the level the router meant to send — the two differ when the client set its own
 * level, or when the model takes no level at all.
 *
 * `hint` disambiguates budgets that collide (Anthropic takes tokens, and `max` and `ultra` share
 * a budget), so a level is never reported as a shallower one by accident.
 */
export function effortInBody(
  body: Record<string, unknown>,
  wire: RequestKind,
  hint?: ReasoningEffort,
): ReasoningEffort | undefined {
  if (wire === "anthropic") {
    const thinking = asRecord(body.thinking);
    if (thinking.type === "disabled") return "none";
    const adaptive = adaptiveEffortInBody(body, hint);
    if (adaptive) return adaptive;
    const budget = thinking.budget_tokens;
    if (typeof budget !== "number") return undefined;
    if (hint && EFFORT_BUDGET[hint] === budget) return hint;
    const match = Object.entries(EFFORT_BUDGET).find(([, tokens]) => tokens === budget);
    return match && isReasoningEffort(match[0]) ? match[0] : undefined;
  }
  if (wire === "responses") {
    if (body.reasoning === null) return "none";
    const effort = asRecord(body.reasoning).effort;
    return typeof effort === "string" && isReasoningEffort(effort) ? effort : undefined;
  }
  const effort = body.reasoning_effort;
  return typeof effort === "string" && isReasoningEffort(effort) ? effort : undefined;
}

/**
 * The thinking level the client asked for itself, in whatever field its wire uses. Checked
 * before the router's own choice so an explicit instruction is never overridden — and so the
 * log reports the client's level rather than the one the router would have applied.
 */
export function clientEffortOf(
  body: Record<string, unknown>,
  kind: RequestKind,
): ReasoningEffort | undefined {
  if (kind === "anthropic") {
    const thinking = asRecord(body.thinking);
    if (thinking.type === "disabled") return "none";
    // A client on adaptive thinking states its level in `output_config.effort`.
    const adaptive = adaptiveEffortInBody(body);
    if (adaptive) return adaptive;
    const budget = thinking.budget_tokens;
    if (typeof budget !== "number") return undefined;
    const match = Object.entries(EFFORT_BUDGET).find(([, tokens]) => tokens === budget);
    return match && isReasoningEffort(match[0]) ? match[0] : undefined;
  }
  return effortInBody(body, kind === "responses" ? "responses" : "openai");
}

function applyClaudeCodeSystem(body: Record<string, unknown>): Record<string, unknown> {
  const next = { ...body };
  injectClaudeCodeSystem(next);

  const model = typeof next.model === "string" ? next.model.toLowerCase() : "";
  const support = anthropicThinkingSupport(model);

  if (!support.adaptive) {
    // `context_management` and `output_config` are adaptive-thinking-only; a legacy model
    // rejects them outright.
    delete next.context_management;
    delete next.output_config;
    // An `adaptive` thinking shape (Claude 4.6+) must not reach a legacy model. Keep a
    // legacy-valid `enabled`/`disabled` shape: `withEffort` writes exactly that for these
    // models, and deleting it here — after `withEffort` ran — would silently discard the
    // thinking level the router chose.
    if (asRecord(next.thinking).type === "adaptive") delete next.thinking;

    if (Array.isArray(next.messages)) {
      next.messages = next.messages.map((m: unknown) => {
        if (
          typeof m === "object" &&
          m !== null &&
          (m as Record<string, unknown>).role === "system"
        ) {
          return {
            ...(m as Record<string, unknown>),
            role: "user",
          };
        }
        return m;
      });
    }
  }

  return next;
}

function injectClaudeCodeSystem(body: Record<string, unknown>): void {
  const system = body.system;
  const prompt = { type: "text", text: CLAUDE_CODE_SYSTEM_PROMPT };
  if (typeof system === "string") {
    // Native Anthropic clients still send a bare string; mark the user system
    // (or the Claude Code prompt alone) so OAuth turns get the same cache hits.
    body.system =
      system.length > 0
        ? [prompt, { type: "text", text: system, cache_control: { type: "ephemeral" } }]
        : [{ ...prompt, cache_control: { type: "ephemeral" } }];
    return;
  }
  if (Array.isArray(system)) {
    body.system = [prompt, ...system];
    return;
  }
  body.system = [{ ...prompt, cache_control: { type: "ephemeral" } }];
}

/** A client image Devin wire cannot carry; surfaced to the client as a 400. */
export class DevinImageError extends Error {}

const DEVIN_INLINE_IMAGE = /^data:image\/[a-z0-9.+-]+;base64,[a-z0-9+/=]+$/i;

/** Devin's protobuf encoder accepts image_url parts containing inline base64 data URLs only. */
function devinImageUrl(url: unknown): string {
  const value = typeof url === "string" ? url : asRecord(url).url;
  if (typeof value !== "string" || !DEVIN_INLINE_IMAGE.test(value.replace(/\s/g, ""))) {
    throw new DevinImageError(
      "Devin only supports inline base64 images; remote URLs and file IDs are not supported",
    );
  }
  return value;
}

function devinAnthropicImage(block: Record<string, unknown>): Record<string, unknown> {
  const source = asRecord(block.source);
  if (
    source.type !== "base64" ||
    typeof source.media_type !== "string" ||
    typeof source.data !== "string"
  ) {
    throw new DevinImageError(
      "Devin only supports Anthropic inline base64 image sources, not remote URLs",
    );
  }
  return {
    type: "image_url",
    image_url: { url: devinImageUrl(`data:${source.media_type};base64,${source.data}`) },
  };
}

/** Fold each multimodal block separately: Devin encodes images per turn, not between text spans. */
function devinAnthropicMessages(body: Record<string, unknown>, model: string): unknown[] {
  const base = anthropicToChatRequest(body, model);
  const messages = Array.isArray(base.messages) ? base.messages : [];
  const incoming = Array.isArray(body.messages) ? body.messages : [];
  if (
    !incoming.some(
      (raw) =>
        Array.isArray(asRecord(raw).content) &&
        (asRecord(raw).content as unknown[]).some((block) => {
          const entry = asRecord(block);
          return (
            entry.type === "image" ||
            (entry.type === "tool_result" &&
              Array.isArray(entry.content) &&
              entry.content.some((part) => asRecord(part).type === "image"))
          );
        }),
    )
  ) {
    return messages;
  }
  const system = anthropicToChatRequest({ system: body.system, messages: [] }, model);
  const result: unknown[] = Array.isArray(system.messages) ? [...system.messages] : [];
  for (const raw of incoming) {
    const message = asRecord(raw);
    if (message.role !== "user" && message.role !== "assistant") continue;
    if (!Array.isArray(message.content)) {
      result.push(
        ...(anthropicToChatRequest({ messages: [message] }, model).messages as unknown[]),
      );
      continue;
    }
    for (const rawBlock of message.content) {
      const block = asRecord(rawBlock);
      if (block.type === "image") {
        if (message.role !== "user")
          throw new DevinImageError("Devin only supports images in user messages");
        result.push({ role: "user", content: [devinAnthropicImage(block)] });
      } else if (block.type === "tool_result") {
        const parts = Array.isArray(block.content) ? block.content : [];
        if (parts.some((part) => asRecord(part).type === "image")) {
          throw new DevinImageError("Devin does not support images in Anthropic tool results");
        }
        result.push(
          ...(anthropicToChatRequest({ messages: [{ ...message, content: [block] }] }, model)
            .messages as unknown[]),
        );
      } else {
        result.push(
          ...(anthropicToChatRequest({ messages: [{ ...message, content: [block] }] }, model)
            .messages as unknown[]),
        );
      }
    }
  }
  return result;
}

function devinResponsesMessages(body: Record<string, unknown>, model: string): unknown[] {
  const converted = responsesToChatRequest(body, model);
  const input = Array.isArray(body.input) ? body.input : [];
  const hasImages = input.some(
    (raw) =>
      Array.isArray(asRecord(raw).content) &&
      (asRecord(raw).content as unknown[]).some((part) => asRecord(part).type === "input_image"),
  );
  if (!hasImages) return converted.messages as unknown[];
  const system = responsesToChatRequest({ instructions: body.instructions }, model);
  const messages: unknown[] = Array.isArray(system.messages) ? [...system.messages] : [];
  for (const raw of input) {
    const item = asRecord(raw);
    const content = Array.isArray(item.content) ? item.content : null;
    if (!content || !content.some((part) => asRecord(part).type === "input_image")) {
      messages.push(...(responsesToChatRequest({ input: [raw] }, model).messages as unknown[]));
      continue;
    }
    if (item.role === "assistant" || item.role === "system") {
      throw new DevinImageError("Devin only supports images in user messages");
    }
    for (const rawPart of content) {
      const part = asRecord(rawPart);
      if (part.type === "input_image") {
        if (part.file_id !== undefined) {
          throw new DevinImageError(
            "Devin does not support Responses image file IDs; supply inline base64 image_url instead",
          );
        }
        messages.push({
          role: "user",
          content: [{ type: "image_url", image_url: { url: devinImageUrl(part.image_url) } }],
        });
      } else {
        messages.push(
          ...(responsesToChatRequest({ input: [{ ...item, content: [rawPart] }] }, model)
            .messages as unknown[]),
        );
      }
    }
  }
  return messages;
}

/** The Chat Completions body Devin's wire module encodes, keeping tools and image block order. */
function devinChatBody(
  body: Record<string, unknown>,
  clientKind: RequestKind,
  model: string,
): Record<string, unknown> {
  if (clientKind === "responses") {
    return {
      ...responsesToChatRequest(body, model),
      messages: devinResponsesMessages(body, model),
      model,
    };
  }
  if (clientKind === "anthropic") {
    return {
      ...anthropicToChatRequest(body, model),
      messages: devinAnthropicMessages(body, model),
      model,
    };
  }
  // The native Chat Completions path can carry image_url parts too; Devin's encoder silently
  // skips non-data URLs, so reject them before the request reaches the wire module.
  for (const raw of Array.isArray(body.messages) ? body.messages : []) {
    const message = asRecord(raw);
    const content = Array.isArray(message.content) ? message.content : [];
    for (const rawPart of content) {
      const part = asRecord(rawPart);
      if (part.type === "image_url") devinImageUrl(part.image_url);
    }
  }
  return { ...body, model };
}

/** The Chat Completions body Cursor's wire module encodes, mirroring `devinChatBody`. */
function cursorChatBody(
  body: Record<string, unknown>,
  clientKind: RequestKind,
  model: string,
): Record<string, unknown> {
  if (clientKind === "responses") {
    return {
      ...responsesToChatRequest(body, model),
      messages: devinResponsesMessages(body, model),
      model,
    };
  }
  if (clientKind === "anthropic") {
    return {
      ...anthropicToChatRequest(body, model),
      messages: devinAnthropicMessages(body, model),
      model,
    };
  }
  return { ...body, model };
}

/**
 * The inputs a body preparation needs: what routing decided, what the client sent and on which
 * wire, and what the upstream can take. No ledger, no trace, no transport — the result is a
 * body plus the reporting fields the caller attaches to the ledger row.
 */
export interface UpstreamPrepInput {
  config: Config;
  provider: Provider;
  decision: RouteDecision;
  clientKind: RequestKind;
  clientStream: boolean;
  /** The client body, after any ingress translation (Claude Gateway, compaction retry). */
  body: Record<string, unknown>;
  /** Whether the upstream itself streams — some wires only stream regardless of the client. */
  upstreamStream: boolean;
  /** Which wire the provider answers on and any bridge the turn folds through. */
  planned: { wire: RequestKind; bridge?: string };
  /** True for providers that fold through Chat Completions into OpenAI Responses. */
  translated: boolean;
  /** The resolved auth, when the Gemini envelope needs the OAuth project id. */
  auth?: { project?: string };
}

/** What came out of preparation: the body, and the bookkeeping the ledger row carries. */
export interface UpstreamPrep {
  /** The assembled body, before the per-wire `payloadFor` quirks. */
  body: Record<string, unknown>;
  /** The effort level the outgoing body actually carries, for the ledger. */
  sentEffort?: string;
  /** Why the sent level differs from the router's choice. */
  effortNote?: string;
  /** Tokens the saver removed, reported on the ledger row. */
  savedTokens?: number;
  /** Reasoning passback wants the upstream stream scanned for `reasoning_content`. */
  passbackReasoning: boolean;
  /** The messages the passback capture restores reasoning into. */
  passbackMessages: Record<string, unknown>[];
  /** The model's output ceiling, which Devin's encoder also needs. */
  maxOutput?: number;
}

/**
 * The prepared body as the JSON one wire sends. Wire-only quirks live here, not in the body:
 * the Claude Code system prompt belongs to the OAuth Anthropic wire only, WorkBuddy wants a
 * leading system message on Chat, and Claude 4.6+ rejects a conversation that ends on a plain
 * assistant turn ("prefill").
 */
export function payloadFor(
  prep: Pick<UpstreamPrep, "body">,
  wire: RequestKind,
  provider: Provider,
): Record<string, unknown> {
  let payload = prep.body;
  if (wire === "anthropic" && provider.auth === "oauth") payload = applyClaudeCodeSystem(payload);
  if (wire === "openai" && isWorkbuddyAiSource(provider.oauthSource)) {
    payload = ensureWorkbuddySystem(payload);
  }
  if (wire === "anthropic") payload = normalizeAnthropicPrefill(payload);
  return payload;
}

/**
 * Builds the body one upstream attempt will send.
 *
 * The caller owns the attempt: it prepares, then turns `body` into a wire payload with
 * `payloadFor`. `forward` keeps only the transport — the wire the provider answers on, the
 * retries, the refusal classification — and reads the reporting fields back onto the ledger
 * row.
 */
export async function prepareUpstream(input: UpstreamPrepInput): Promise<UpstreamPrep> {
  const { config, provider, decision, clientKind, clientStream, body, upstreamStream } = input;
  const devinWire = provider.type === "devin";
  const cursorWire = provider.type === "cursor";
  const geminiWire = provider.type === "gemini";
  const workbuddyWire = isWorkbuddyAiSource(provider.oauthSource);
  const upstreamKind = input.planned.wire;
  const bridgeToAnthropic = input.planned.bridge === "to-anthropic";
  const bridgeToOpenAI = input.planned.bridge === "to-openai";
  const maxOutput = effectiveCapabilities(
    decision.model,
    config.routing.capacities?.[decision.model],
  ).maxOutput;

  const bodyFor = (wire: RequestKind): Record<string, unknown> => {
    // The client's own level, in whatever field its wire uses. Detected per wire so the router
    // never overrides an explicit instruction, and so the log can say who chose the level.
    const clientEffort = clientEffortOf(body, clientKind);
    // Devin and Cursor, like Gemini, win over the OpenAI bridge: their wire modules encode a
    // Chat Completions body into Connect-RPC protobuf, so every client folds onto that body.
    // Effort is part of the model ids for both, so no effort field is written.
    if (devinWire) return devinChatBody(body, clientKind, decision.model);
    if (cursorWire) return cursorChatBody(body, clientKind, decision.model);
    if (wire === "anthropic" && bridgeToAnthropic) {
      // Responses clients fold through Chat Completions first (same two-hop as
      // Responses→Antigravity), then chatToAnthropic builds the Messages body.
      const chatBody =
        clientKind === "responses" ? responsesToChatRequest(body, decision.model) : body;
      return bridgedAnthropicBody(chatBody, {
        model: decision.model,
        stream: upstreamStream,
        effort: decision.effort,
        clientEffort,
        maxOutput,
      });
    }
    // Gemini must win over the OpenAI bridge: planUpstreamWire sets wire=openai +
    // bridge=to-openai for Responses→Antigravity so the body can be folded through
    // Chat Completions first, but the egress envelope is still Gemini (`contents`,
    // not `messages`). Returning the bridged chat body here used to POST OpenAI JSON
    // at generateContent and get INVALID_ARGUMENT Unknown name "messages".
    if (geminiWire) {
      const chatBody =
        clientKind === "responses"
          ? responsesToChatRequest(body, decision.model)
          : clientKind === "anthropic"
            ? anthropicToChatRequest(body, decision.model)
            : {
                ...body,
                messages: normalizeOpenAIMessages(
                  (Array.isArray(body.messages) ? body.messages : []) as Record<string, unknown>[],
                ),
              };
      return {
        project: input.auth?.project ?? "default-cli-project",
        model: decision.model,
        userAgent: "antigravity",
        requestId: crypto.randomUUID(),
        request: chatToGemini(chatBody),
      };
    }
    if (wire === "openai" && bridgeToOpenAI) {
      if (clientKind === "responses") {
        return withEffort(
          { ...responsesToChatRequest(body, decision.model), stream: upstreamStream },
          decision.effort,
          "openai",
          clientEffort,
        );
      }
      return withEffort(
        {
          ...anthropicToChatRequest(body, decision.model),
          // Reply is folded into one Anthropic message; an SSE dialect bridge is not wired yet.
          stream: false,
        },
        decision.effort,
        "openai",
        clientEffort,
      );
    }
    if (input.translated) {
      return withEffort(
        chatToResponses(body, decision.model),
        decision.effort,
        "responses",
        clientEffort,
      );
    }
    const native = withEffort(
      { ...body, model: decision.model },
      decision.effort,
      // The body is already in the client's own shape here, so the effort field
      // must use that wire's spelling. Mapping everything non-Anthropic to
      // "openai" wrote `reasoning_effort` into a native Responses body, which
      // the upstream rejects with "Unsupported parameter: reasoning_effort".
      wire,
      clientEffort,
    );
    // A router-written budget can exceed the client's own `max_tokens`, which Anthropic rejects.
    return wire === "anthropic"
      ? fitThinkingMaxTokens(native, { clientSetMax: true, maxOutput })
      : native;
  };

  let upstreamBody = bodyFor(upstreamKind);
  // OpenAI Responses rejects empty call_id / name (minLength 1) and call_id longer than 64
  // chars. Sanitize before egress — bridged history can leave "" or oversized ids on
  // function_call(_output) items.
  if (upstreamKind === "responses") upstreamBody = ensureResponsesCallIds(upstreamBody);
  // Prompt hygiene runs last, so every wire's own assembly (the Chat fold, the Anthropic
  // bridge) sees the rewritten text. The Devin wire applies its built-ins again while encoding;
  // both passes are idempotent.
  upstreamBody = rewritePromptBodies(upstreamBody, config.promptPolicy);
  // OpenAI Chat Completions sanitizer: normalize Anthropic-style blocks (tool_use / tool_result)
  // inside `content` arrays into standard OpenAI tool_calls and tool messages.
  if (upstreamKind === "openai" && Array.isArray(upstreamBody.messages)) {
    upstreamBody.messages = normalizeOpenAIMessages(
      upstreamBody.messages as Record<string, unknown>[],
    );
  }
  // The token saver compresses prior tool results on the fully assembled upstream body.
  let savedTokens: number | undefined;
  if (config.tokenSaver.enabled) {
    const saved = await saveTokens(upstreamBody, config.tokenSaver);
    warnSaverUnavailable(config.tokenSaver, saved.stats.unavailable === true);
    if (saved.stats.savedTokens > 0) {
      upstreamBody = saved.body;
      savedTokens = saved.stats.savedTokens;
    }
  }
  // DeepSeek / Kimi thinking mode: clients often drop `reasoning_content` after tool calls.
  const passbackReasoning =
    upstreamKind === "openai" &&
    !devinWire &&
    needsReasoningPassback(decision.provider, decision.model, provider.baseUrl);
  if (passbackReasoning) {
    upstreamBody = repairReasoningContent(upstreamBody, decision.session).body;
  }
  const passbackMessages = Array.isArray(upstreamBody.messages)
    ? (upstreamBody.messages as Record<string, unknown>[])
    : [];

  // The log reports the level the model was actually sent, read back from the body rather than
  // from the router's intent.
  const sentEffort =
    geminiWire || devinWire || cursorWire
      ? undefined
      : effortInBody(upstreamBody, upstreamKind, decision.effort);
  const effortNote =
    decision.effort && sentEffort && sentEffort !== decision.effort
      ? `client set "${sentEffort}"; router chose "${decision.effort}"`
      : decision.effortNote;

  if (provider.type === "responses") {
    upstreamBody.stream = upstreamStream;
    if (provider.auth === "oauth") upstreamBody.store = false;
  }
  if (workbuddyWire) upstreamBody.stream = true;
  if (
    clientKind === "openai" &&
    provider.type === "openai" &&
    clientStream &&
    provider.injectStreamUsage &&
    upstreamBody.stream_options === undefined
  ) {
    upstreamBody.stream_options = { include_usage: true };
  }

  return {
    body: upstreamBody,
    ...(sentEffort ? { sentEffort } : {}),
    ...(effortNote ? { effortNote } : {}),
    ...(savedTokens !== undefined ? { savedTokens } : {}),
    passbackReasoning,
    passbackMessages,
    maxOutput,
  };
}
