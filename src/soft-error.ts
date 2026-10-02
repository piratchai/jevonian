/**
 * Last-resort "soft" completions for streaming turns.
 *
 * A coding agent's harness treats a hard failure badly: an abrupt socket close, a JSON 5xx, or
 * an SSE `{ error }` mid-agent loop can make it roll the whole turn back — sometimes wiping the
 * visible user message and scrambling the conversation context. The failure is real, but it is
 * Jevonian's, not the model's, so it must not cost the user their turn.
 *
 * When Jevonian still owns the HTTP response, it answers with a normal assistant message that
 * explains the failure and ends the stream cleanly (`finish_reason: stop` / `message_stop` /
 * `response.completed`). The turn survives, the user can retry, and the ledger still records the
 * real error. This is deliberately the last resort: it only runs when there is nothing better to
 * say.
 */

import type { RequestKind } from "./routing";

/** Prefix kept stable so users can recognise a Jevonian-generated message at a glance. */
export const SOFT_ERROR_PREFIX = "Jevonian hit an internal error";

/** One-line assistant message describing a failure, safe to show the user. */
export function softErrorMessage(reason: string): string {
  const detail = reason.trim().replace(/\s+/g, " ").slice(0, 300);
  const suffix = detail.length > 0 ? `: ${detail}` : "";
  return `${SOFT_ERROR_PREFIX}${suffix}. The turn was stopped safely — please retry.`;
}

/**
 * Replace every occurrence of a known credential with `[REDACTED]`.
 *
 * An upstream error body can echo the `Authorization` header it just rejected, and that body is
 * shown to the user inside the soft message. Values shorter than a plausible credential are
 * skipped so a config mistake cannot turn a common substring into `[REDACTED]`.
 */
export function redactSecrets(text: string, secrets: Array<string | undefined>): string {
  let safe = text;
  for (const secret of secrets) {
    if (typeof secret !== "string" || secret.length < 8) continue;
    // Cover the JSON-escaped echo too, not just the raw credential.
    for (const variant of [secret, JSON.stringify(secret).slice(1, -1)]) {
      if (variant) safe = safe.split(variant).join("[REDACTED]");
    }
  }
  return safe;
}

const encoder = new TextEncoder();

function sse(payload: unknown): Uint8Array {
  return encoder.encode(`data: ${JSON.stringify(payload)}\n\n`);
}

function sseEvent(type: string, payload: unknown): Uint8Array {
  return encoder.encode(`event: ${type}\ndata: ${JSON.stringify(payload)}\n\n`);
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function number(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function chatChunk(
  model: string,
  delta: Record<string, unknown>,
  finishReason: string | null,
): Record<string, unknown> {
  return {
    id: `chatcmpl-soft-${crypto.randomUUID().replace(/-/g, "").slice(0, 16)}`,
    object: "chat.completion.chunk",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, delta, finish_reason: finishReason }],
  };
}

/**
 * Where the soft block goes on a wire that may already have streamed content.
 *
 * A failure that lands after the upstream opened a content block / output item cannot reuse
 * index 0: strict Anthropic and Responses clients reject a duplicate index. `index` is the next
 * free slot, and `closeIndex` / `closeItem` terminate whatever was still open before the soft
 * block opens, so the wire stays well-formed.
 */
export interface SoftCompletionOptions {
  /** The wire already emitted its stream opener (`message_start` / `response.created` / role). */
  started?: boolean;
  /** Anthropic content-block index / Responses output index for the soft block. Defaults to 0. */
  index?: number;
  /** Anthropic: an open content block to stop before the soft block opens. */
  closeIndex?: number;
  /** Responses: an open output item to finish before the soft item opens. */
  closeItem?: { outputIndex: number; item: Record<string, unknown>; text: string };
}

/**
 * Chunks that end a Chat Completions stream as a normal assistant turn carrying `message`.
 * Emits the role opener only when the wire has not opened yet; a stream that already sent
 * content continues without it.
 */
export function softChatCompletion(
  model: string,
  message: string,
  options: SoftCompletionOptions = {},
): Uint8Array[] {
  const chunks: Uint8Array[] = [];
  if (!options.started) {
    chunks.push(sse(chatChunk(model, { role: "assistant", content: "" }, null)));
  }
  chunks.push(sse(chatChunk(model, { content: message }, null)));
  chunks.push(sse(chatChunk(model, {}, "stop")));
  chunks.push(encoder.encode("data: [DONE]\n\n"));
  return chunks;
}

/** Anthropic Messages events that end the stream cleanly with a text block. */
export function softAnthropicCompletion(
  model: string,
  message: string,
  options: SoftCompletionOptions = {},
): Uint8Array[] {
  const chunks: Uint8Array[] = [];
  const index = options.index ?? 0;
  const id = `msg_soft_${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`;
  if (options.closeIndex !== undefined) {
    chunks.push(
      sseEvent("content_block_stop", { type: "content_block_stop", index: options.closeIndex }),
    );
  }
  if (!options.started) {
    chunks.push(
      sseEvent("message_start", {
        type: "message_start",
        message: {
          id,
          type: "message",
          role: "assistant",
          model,
          content: [],
          stop_reason: null,
          stop_sequence: null,
          usage: { input_tokens: 0, output_tokens: 0 },
        },
      }),
    );
  }
  chunks.push(
    sseEvent("content_block_start", {
      type: "content_block_start",
      index,
      content_block: { type: "text", text: "" },
    }),
    sseEvent("content_block_delta", {
      type: "content_block_delta",
      index,
      delta: { type: "text_delta", text: message },
    }),
    sseEvent("content_block_stop", { type: "content_block_stop", index }),
    sseEvent("message_delta", {
      type: "message_delta",
      delta: { stop_reason: "end_turn", stop_sequence: null },
      usage: { input_tokens: 0, output_tokens: 0 },
    }),
    sseEvent("message_stop", { type: "message_stop" }),
  );
  return chunks;
}

/** Terminate an output item left open by a mid-stream failure, so the wire stays well-formed. */
function closeResponsesItem(
  close: NonNullable<SoftCompletionOptions["closeItem"]>,
  chunks: Uint8Array[],
): void {
  const item = close.item;
  const itemId = asString(item.id);
  if (item.type === "function_call") {
    chunks.push(
      sseEvent("response.function_call_arguments.done", {
        type: "response.function_call_arguments.done",
        item_id: itemId,
        output_index: close.outputIndex,
        arguments: close.text,
      }),
      sseEvent("response.output_item.done", {
        type: "response.output_item.done",
        output_index: close.outputIndex,
        item: { ...item, arguments: close.text, status: "completed" },
      }),
    );
    return;
  }
  if (item.type === "message") {
    chunks.push(
      sseEvent("response.output_text.done", {
        type: "response.output_text.done",
        item_id: itemId,
        output_index: close.outputIndex,
        content_index: 0,
        text: close.text,
      }),
      sseEvent("response.content_part.done", {
        type: "response.content_part.done",
        item_id: itemId,
        output_index: close.outputIndex,
        content_index: 0,
        part: { type: "output_text", text: close.text },
      }),
      sseEvent("response.output_item.done", {
        type: "response.output_item.done",
        output_index: close.outputIndex,
        item: {
          ...item,
          status: "completed",
          content: [{ type: "output_text", text: close.text }],
        },
      }),
    );
    return;
  }
  // A reasoning item or anything else: close it as it stands, without inventing content.
  chunks.push(
    sseEvent("response.output_item.done", {
      type: "response.output_item.done",
      output_index: close.outputIndex,
      item: { ...item, status: "completed" },
    }),
  );
}

/** OpenAI Responses events that end the stream as a completed assistant message. */
export function softResponsesCompletion(
  model: string,
  message: string,
  options: SoftCompletionOptions = {},
): Uint8Array[] {
  const chunks: Uint8Array[] = [];
  const outputIndex = options.index ?? 0;
  const id = `resp_soft_${crypto.randomUUID().replace(/-/g, "").slice(0, 16)}`;
  const itemId = `msg_${id}`;
  const skeleton = (status: string, output: unknown[]) => ({
    id,
    object: "response",
    created_at: Math.floor(Date.now() / 1000),
    status,
    model,
    output,
    usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
  });
  if (!options.started) {
    chunks.push(
      sseEvent("response.created", {
        type: "response.created",
        response: skeleton("in_progress", []),
      }),
      sseEvent("response.in_progress", {
        type: "response.in_progress",
        response: skeleton("in_progress", []),
      }),
    );
  }
  if (options.closeItem) closeResponsesItem(options.closeItem, chunks);
  // The item and its content part are opened even when the stream already started: a delta
  // without a preceding `.added` is dropped by Codex, and reusing a closed index is rejected.
  chunks.push(
    sseEvent("response.output_item.added", {
      type: "response.output_item.added",
      output_index: outputIndex,
      item: {
        type: "message",
        id: itemId,
        role: "assistant",
        status: "in_progress",
        content: [],
      },
    }),
    sseEvent("response.content_part.added", {
      type: "response.content_part.added",
      item_id: itemId,
      output_index: outputIndex,
      content_index: 0,
      part: { type: "output_text", text: "" },
    }),
    sseEvent("response.output_text.delta", {
      type: "response.output_text.delta",
      item_id: itemId,
      output_index: outputIndex,
      content_index: 0,
      delta: message,
    }),
    sseEvent("response.output_text.done", {
      type: "response.output_text.done",
      item_id: itemId,
      output_index: outputIndex,
      content_index: 0,
      text: message,
    }),
    sseEvent("response.content_part.done", {
      type: "response.content_part.done",
      item_id: itemId,
      output_index: outputIndex,
      content_index: 0,
      part: { type: "output_text", text: message },
    }),
    sseEvent("response.output_item.done", {
      type: "response.output_item.done",
      output_index: outputIndex,
      item: {
        type: "message",
        id: itemId,
        role: "assistant",
        status: "completed",
        content: [{ type: "output_text", text: message }],
      },
    }),
    sseEvent("response.completed", {
      type: "response.completed",
      response: skeleton("completed", [
        {
          type: "message",
          id: itemId,
          role: "assistant",
          status: "completed",
          content: [{ type: "output_text", text: message }],
        },
      ]),
    }),
  );
  return chunks;
}

/** The completion chunks for a client wire. */
export function softCompletionChunks(
  kind: RequestKind,
  model: string,
  message: string,
  options: SoftCompletionOptions = {},
): Uint8Array[] {
  if (kind === "anthropic") return softAnthropicCompletion(model, message, options);
  if (kind === "responses") return softResponsesCompletion(model, message, options);
  return softChatCompletion(model, message, options);
}

/** A streaming response body that only contains a soft completion, for a pre-stream failure. */
export function softCompletionStream(
  kind: RequestKind,
  model: string,
  message: string,
): ReadableStream<Uint8Array> {
  const chunks = softCompletionChunks(kind, model, message);
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(chunk);
      controller.close();
    },
  });
}

/** One parsed SSE event: its JSON `data:` payload, a `[DONE]` sentinel, or neither. */
interface ParsedEvent {
  json?: Record<string, unknown>;
  done?: boolean;
}

function parseEvent(event: string): ParsedEvent {
  for (const line of event.split("\n")) {
    if (!line.startsWith("data:")) continue;
    const raw = line.slice(5).trim();
    if (raw.length === 0) continue;
    if (raw === "[DONE]") return { done: true };
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (typeof parsed === "object" && parsed !== null) {
        return { json: parsed as Record<string, unknown> };
      }
    } catch {
      // A malformed data line is not a terminal signal; keep looking at the others.
    }
  }
  return { done: false };
}

/** True when an event is the wire's terminal failure frame, which must never reach the client. */
function isErrorEvent(kind: RequestKind, json: Record<string, unknown>): boolean {
  const type = asString(json.type);
  if (kind === "openai") return json.error !== undefined && json.error !== null;
  if (kind === "anthropic") return type === "error";
  return type === "response.failed" || type === "error";
}

function errorReason(kind: RequestKind, json: Record<string, unknown>): string {
  const raw = json.error;
  if (typeof raw === "string" && raw.length > 0) return raw;
  if (kind === "openai" || kind === "anthropic") {
    return asString(asRecord(raw).message) || "upstream error";
  }
  const nested = asString(asRecord(asRecord(json.response).error).message);
  return nested || asString(asRecord(raw).message) || "upstream response failed";
}

/** Progress of a streamed turn, used to place the soft block and to spot a real finish. */
interface SoftStreamState {
  /** The client has seen the wire's stream opener (or any forwarded event). */
  started: boolean;
  finished: boolean;
  failureReason?: string;
  /** Anthropic: highest content block index seen, and the block still open (if any). */
  maxBlockIndex: number;
  openBlockIndex?: number;
  /** Responses: highest output index seen, and the item still open (if any). */
  maxOutputIndex: number;
  openItem?: { outputIndex: number; item: Record<string, unknown>; text: string };
}

function newState(): SoftStreamState {
  return { started: false, finished: false, maxBlockIndex: -1, maxOutputIndex: -1 };
}

/**
 * Fold one parsed event into the stream state.
 *
 * A real finish is recognised by its parsed `type` or a non-null `finish_reason`, never by a
 * substring: a marker buried more than a carry-length into a frame used to be missed, which
 * appended a second answer to an already-finished turn.
 */
function applyEvent(
  kind: RequestKind,
  json: Record<string, unknown>,
  state: SoftStreamState,
): void {
  const type = asString(json.type);
  if (kind === "openai") {
    // An error frame was never forwarded, so it does not open the stream.
    if (json.error !== undefined && json.error !== null) {
      state.failureReason = errorReason(kind, json);
      return;
    }
    state.started = true;
    const choices = Array.isArray(json.choices) ? json.choices : [];
    for (const raw of choices) {
      const finish = asRecord(raw).finish_reason;
      if (typeof finish === "string" && finish.length > 0) state.finished = true;
    }
    return;
  }
  if (kind === "anthropic") {
    if (type === "error") {
      state.failureReason = errorReason(kind, json);
      return;
    }
    if (type === "message_start") state.started = true;
    if (type === "content_block_start") {
      state.started = true;
      const index = number(json.index);
      state.maxBlockIndex = Math.max(state.maxBlockIndex, index);
      state.openBlockIndex = index;
      return;
    }
    if (type === "content_block_stop") {
      if (state.openBlockIndex === number(json.index)) state.openBlockIndex = undefined;
      return;
    }
    if (type === "message_stop") state.finished = true;
    return;
  }
  // responses
  if (type === "response.failed" || type === "error") {
    state.failureReason = errorReason(kind, json);
    return;
  }
  if (type === "response.created") state.started = true;
  if (type === "response.output_item.added") {
    state.started = true;
    const outputIndex = number(json.output_index);
    state.maxOutputIndex = Math.max(state.maxOutputIndex, outputIndex);
    state.openItem = { outputIndex, item: asRecord(json.item), text: "" };
    return;
  }
  if (state.openItem && number(json.output_index) === state.openItem.outputIndex) {
    if (
      type === "response.output_text.delta" ||
      type === "response.function_call_arguments.delta"
    ) {
      state.openItem.text += asString(json.delta);
      return;
    }
    if (type === "response.output_text.done") {
      state.openItem.text = asString(json.text);
      return;
    }
    if (type === "response.output_item.done") state.openItem = undefined;
    return;
  }
  if (type === "response.completed" || type === "response.incomplete" || type === "response.done") {
    state.finished = true;
  }
}

/** Past this many bytes without a frame boundary, the text is forwarded rather than buffered. */
const MAX_CARRY = 1 << 20;
/** Bytes kept when the carry overflows, so a frame split at the boundary is still parsed. */
const CARRY_TAIL = 1 << 16;

/**
 * Wraps an upstream SSE body so a mid-stream failure (a dropped socket, a proxy that speaks the
 * wrong protocol, a truncated read) ends as a normal assistant message instead of an abrupt
 * close the harness can roll back.
 *
 * A clean upstream that never emitted a finish marker is treated the same way: the turn is
 * ended with the soft message rather than left dangling. When the stream already finished
 * normally, the bytes pass through untouched. A terminal error frame — OpenAI `{ error }`,
 * Anthropic `type: "error"`, Responses `response.failed` — is swallowed and replaced by the soft
 * completion, so no hard failure ever reaches a streaming client.
 *
 * This is a `pull`-driven reader rather than a `pipeThrough` transform on purpose: a transform
 * would have to drain the whole upstream into memory to survive a source error (there is no
 * `flush` when the readable side errors), which is exactly the kind of unbounded buffering that
 * hurts with many concurrent sessions. Pulling one chunk at a time keeps backpressure intact;
 * only a single incomplete SSE event is ever held back.
 */
export function withSoftCompletion(
  source: ReadableStream<Uint8Array>,
  kind: RequestKind,
  model: string,
  message: string,
  onSoft?: (reason?: string) => void,
  /** Applied to the upstream error text before it is shown to the user or recorded. */
  redact?: (text: string) => string,
): ReadableStream<Uint8Array> {
  const reader = source.getReader();
  const decoder = new TextDecoder();
  const state = newState();
  let carry = "";
  let settled = false;
  /** Chunks enqueued so far; used to tell whether a `pull` iteration produced output. */
  let emitted = 0;

  const softFinish = (controller: ReadableStreamDefaultController<Uint8Array>): void => {
    if (settled) return;
    settled = true;
    const reason = state.failureReason;
    const text = reason ? softErrorMessage(redact ? redact(reason) : reason) : message;
    const chunks = softCompletionChunks(kind, model, text, {
      started: state.started,
      index: (kind === "anthropic" ? state.maxBlockIndex : state.maxOutputIndex) + 1,
      ...(kind === "anthropic" && state.openBlockIndex !== undefined
        ? { closeIndex: state.openBlockIndex }
        : {}),
      ...(kind === "responses" && state.openItem ? { closeItem: state.openItem } : {}),
    });
    for (const chunk of chunks) {
      controller.enqueue(chunk);
      emitted += 1;
    }
    // The reason is redacted here too: the ledger is read by the dashboard and shown to users.
    onSoft?.(reason ? (redact ? redact(reason) : reason) : undefined);
    controller.close();
  };

  const forwardEvent = (
    event: string,
    controller: ReadableStreamDefaultController<Uint8Array>,
  ): void => {
    // A terminal failure already decided this turn: nothing after it may be forwarded, not even a
    // `[DONE]` — the soft completion carries its own terminator.
    if (state.failureReason !== undefined) return;
    const parsed = parseEvent(event);
    if (parsed.done) {
      state.finished = true;
      controller.enqueue(encoder.encode(`${event}\n\n`));
      emitted += 1;
      return;
    }
    if (parsed.json) {
      if (isErrorEvent(kind, parsed.json)) {
        // Never hand a hard failure back. If the turn already finished the frame is dropped
        // outright; otherwise it becomes the reason the soft completion reports.
        if (!state.finished) applyEvent(kind, parsed.json, state);
        return;
      }
      applyEvent(kind, parsed.json, state);
    }
    controller.enqueue(encoder.encode(`${event}\n\n`));
    emitted += 1;
  };

  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      // A `pull` that returns without enqueuing is not always re-invoked by the runtime, so this
      // loops until it has produced output, closed the turn, or the upstream is done. Only a
      // single incomplete SSE event is ever held in `carry`, keeping memory bounded.
      while (!settled) {
        const before = emitted;
        let step: Awaited<ReturnType<typeof reader.read>>;
        try {
          step = await reader.read();
        } catch {
          // The upstream died mid-stream: close the turn softly instead of erroring the client.
          if (state.finished) {
            settled = true;
            controller.close();
            return;
          }
          softFinish(controller);
          return;
        }
        // A client hang-up can cancel the stream while a read is in flight; that read can still
        // resolve here, and enqueueing on a canceled controller would throw.
        if (settled) return;
        const decoded = step.done
          ? carry + decoder.decode()
          : carry + decoder.decode(step.value, { stream: true });
        const events = decoded.replace(/\r\n/g, "\n").split("\n\n");
        carry = events.pop() ?? "";
        for (const event of events) {
          if (settled) return;
          forwardEvent(event, controller);
        }
        if (settled) return;
        if (carry.length > MAX_CARRY) {
          // A frame that never completes (a mislabeled non-SSE body, a wedged upstream) must not
          // buffer without bound: forward the excess and keep searching for a boundary in the tail.
          controller.enqueue(encoder.encode(carry.slice(0, carry.length - CARRY_TAIL)));
          emitted += 1;
          carry = carry.slice(-CARRY_TAIL);
        }
        if (step.done) {
          // A trailing incomplete frame cannot be parsed, so it is dropped rather than forwarded
          // as an invalid event — unless the upstream already failed, where nothing is forwarded.
          if (carry.trim().length > 0 && state.failureReason === undefined && !state.finished) {
            controller.enqueue(encoder.encode(carry));
            emitted += 1;
          }
          carry = "";
          if (state.finished) {
            settled = true;
            controller.close();
            return;
          }
          softFinish(controller);
          return;
        }
        // A terminal error ends the turn: stop pulling and close it softly now rather than wait
        // for an upstream that may hold the socket open after the frame.
        if (state.failureReason !== undefined && !state.finished) {
          void reader.cancel().catch(() => undefined);
          softFinish(controller);
          return;
        }
        if (emitted > before) return;
      }
    },
    cancel(reason) {
      // A client hang-up is not Jevonian's failure to report.
      settled = true;
      return reader.cancel(reason).catch(() => undefined);
    },
  });
}
