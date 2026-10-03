import { createHash } from "node:crypto";

import { rejectsAssistantPrefill } from "./anthropic-thinking";
import { bareModelId } from "./model-id";
import type { Usage } from "./pricing";
import type { StreamEvent } from "./relay";
import { splitSseEvents } from "./responses";
import { softErrorMessage } from "./soft-error";

/** Anthropic rejects `tool_use.id` / `tool_result.tool_use_id` outside this charset. */
const ANTHROPIC_TOOL_ID = /^[a-zA-Z0-9_-]+$/;
const MAX_ANTHROPIC_TOOL_ID_LENGTH = 64;

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/**
 * Map OpenAI / Responses call ids onto Anthropic's `^[a-zA-Z0-9_-]+$` charset.
 * Deterministic so a tool_use and its tool_result stay paired after sanitization. Any id that
 * had to change carries a hash of the original: stripping punctuation alone would fold
 * `call:a` and `call.a` onto one id, and Anthropic rejects duplicate `tool_use` ids in a turn.
 */
export function anthropicToolId(raw: unknown): string {
  const id = asString(raw);
  if (ANTHROPIC_TOOL_ID.test(id) && id.length <= MAX_ANTHROPIC_TOOL_ID_LENGTH) return id;
  const hash = createHash("sha256")
    .update(id.length > 0 ? id : "empty", "utf8")
    .digest("hex");
  const sanitized = id
    .replace(/[^a-zA-Z0-9_-]+/g, "_")
    .replace(/_+/g, "_")
    .replace(/^_|_$/g, "");
  if (sanitized.length === 0) return `tool_${hash.slice(0, 24)}`;
  const suffix = `_${hash.slice(0, 8)}`;
  return `${sanitized.slice(0, MAX_ANTHROPIC_TOOL_ID_LENGTH - suffix.length)}${suffix}`;
}

function number(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

export function needsAnthropicWire(model: string): boolean {
  const tail = bareModelId(model);
  return /^claude/i.test(tail) || /anthropic/i.test(model);
}

function textOf(value: unknown): string {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) {
    return value
      .map((block) => {
        const record = asRecord(block);
        if (typeof record.text === "string") return record.text;
        return "";
      })
      .filter((part) => part.length > 0)
      .join("\n");
  }
  return value === undefined ? "" : JSON.stringify(value);
}

function parseArgs(value: unknown): Record<string, unknown> {
  if (typeof value === "string") {
    try {
      return asRecord(JSON.parse(value));
    } catch {
      return {};
    }
  }
  return asRecord(value);
}

function toolResultContent(value: unknown): string {
  const text = textOf(value);
  return text.length > 0 ? text : "(no output)";
}

function chatToolsToAnthropic(raw: unknown): unknown[] | undefined {
  if (!Array.isArray(raw)) return undefined;
  const tools = raw.flatMap((entry) => {
    const tool = asRecord(entry);
    if (tool.type !== "function") return [];
    const fn = asRecord(tool.function);
    const name = asString(fn.name);
    if (name.length === 0) return [];
    return [
      {
        name,
        ...(typeof fn.description === "string" ? { description: fn.description } : {}),
        input_schema: asRecord(fn.parameters),
      },
    ];
  });
  return tools.length > 0 ? tools : undefined;
}

function toolChoiceFor(choice: unknown): Record<string, unknown> | undefined {
  if (choice === "required") return { type: "any" };
  if (choice === "none") return { type: "none" };
  if (typeof choice === "object" && choice !== null) {
    const name = asString(asRecord(asRecord(choice).function).name);
    if (name.length > 0) return { type: "tool", name };
  }
  return undefined;
}

/** True when an Anthropic message is an assistant turn carrying no `tool_use` block. */
function isPlainAssistantMessage(message: Record<string, unknown>): boolean {
  if (message.role !== "assistant") return false;
  const content = message.content;
  if (typeof content === "string") return true;
  if (!Array.isArray(content)) return true;
  return !content.some((block) => asRecord(block).type === "tool_use");
}

/**
 * Claude 4.6 and later (plus the Fable/Mythos family) reject a conversation whose final message
 * is an assistant turn ("prefill") with 400 `This model does not support assistant message
 * prefill. The conversation must end with a user message.` A Chat or Responses client can
 * legitimately send a trailing assistant turn — Cursor re-sends the last assistant reply, and a
 * client may seed a partial reply — so the bridge would forward it and fail the whole turn.
 *
 * Drops trailing plain-assistant prefill messages on models that reject prefill, so the request
 * ends on the user/tool turn before them. A trailing assistant that carries `tool_use` is left
 * alone: that is an in-flight tool call awaiting its result, not a prefill, and dropping it would
 * orphan the call. Models that still support prefill (Claude 4.5 / Haiku 4.5 and older) are never
 * touched, so a client that deliberately prefills those keeps its steer.
 */
export function normalizeAnthropicPrefill(body: Record<string, unknown>): Record<string, unknown> {
  if (!rejectsAssistantPrefill(body.model)) return body;
  const messages = Array.isArray(body.messages) ? body.messages : null;
  if (!messages || messages.length === 0) return body;
  if (!isPlainAssistantMessage(asRecord(messages[messages.length - 1]))) return body;

  let end = messages.length - 1;
  while (end > 0 && isPlainAssistantMessage(asRecord(messages[end - 1]))) end -= 1;
  const trimmed = messages.slice(0, end);
  // A body that is nothing but assistant turns cannot be trimmed to empty (Anthropic requires at
  // least one message). Relocate the newest one to a user turn instead, flattening to text so no
  // assistant-only block (e.g. `thinking`) ends up in a user turn.
  if (trimmed.length === 0) {
    const only = asRecord(messages[messages.length - 1]);
    // Flatten to text so no assistant-only block (e.g. `thinking`) ends up in a user turn. An
    // assistant turn can be empty (a bare tool-free reply), and Anthropic rejects empty user
    // content, so fall back to a placeholder that still asks the model to continue.
    const text = textOf(only.content).trim();
    return {
      ...body,
      messages: [{ role: "user", content: text.length > 0 ? text : "Continue." }],
    };
  }
  return { ...body, messages: trimmed };
}

export function chatToAnthropic(body: Record<string, unknown>): Record<string, unknown> {
  const messages = Array.isArray(body.messages) ? body.messages : [];
  const systemTexts: string[] = [];
  const converted: Array<{ role: "user" | "assistant"; content: unknown[] }> = [];

  const push = (role: "user" | "assistant", blocks: unknown[]): void => {
    if (blocks.length === 0) return;
    const last = converted[converted.length - 1];
    if (last && last.role === role) last.content.push(...blocks);
    else converted.push({ role, content: blocks });
  };

  for (const raw of messages) {
    const message = asRecord(raw);
    const role = message.role;
    if (role === "system" || role === "developer") {
      const text = textOf(message.content);
      if (text.length > 0) systemTexts.push(text);
      continue;
    }
    if (role === "tool" || role === "function") {
      push("user", [
        {
          type: "tool_result",
          tool_use_id: anthropicToolId(message.tool_call_id),
          content: toolResultContent(message.content),
        },
      ]);
      continue;
    }
    if (role === "assistant") {
      const blocks: unknown[] = [];
      const text = textOf(message.content);
      if (text.length > 0) blocks.push({ type: "text", text });
      const toolCalls = Array.isArray(message.tool_calls) ? message.tool_calls : [];
      for (const rawCall of toolCalls) {
        const call = asRecord(rawCall);
        const fn = asRecord(call.function);
        blocks.push({
          type: "tool_use",
          id: anthropicToolId(call.id),
          name: asString(fn.name),
          input: parseArgs(fn.arguments),
        });
      }
      push("assistant", blocks);
      continue;
    }
    const text = textOf(message.content);
    push("user", text.length > 0 ? [{ type: "text", text }] : []);
  }

  const max = body.max_completion_tokens ?? body.max_tokens;
  const out: Record<string, unknown> = {
    messages: converted,
    max_tokens: typeof max === "number" && max > 0 ? max : 4_096,
  };
  // Prompt caching: mark stable prefixes so Claude subscription turns reuse the
  // system prompt, tool schemas, and conversation prefix across a long agent
  // session. Anthropic allows up to four breakpoints; these three cover the
  // usual Chat Completions shape without needing the client to opt in.
  if (systemTexts.length > 0) {
    out.system = [
      {
        type: "text",
        text: systemTexts.join("\n\n"),
        cache_control: { type: "ephemeral" },
      },
    ];
  }
  if (typeof body.temperature === "number") out.temperature = body.temperature;
  if (typeof body.top_p === "number") out.top_p = body.top_p;
  const stop = body.stop;
  if (typeof stop === "string") out.stop_sequences = [stop];
  else if (Array.isArray(stop)) {
    const sequences = stop.filter((item): item is string => typeof item === "string");
    if (sequences.length > 0) out.stop_sequences = sequences;
  }
  const tools = chatToolsToAnthropic(body.tools);
  if (tools) {
    const last = tools[tools.length - 1] as Record<string, unknown>;
    tools[tools.length - 1] = { ...last, cache_control: { type: "ephemeral" } };
    out.tools = tools;
  }
  const toolChoice = toolChoiceFor(body.tool_choice);
  if (toolChoice) out.tool_choice = toolChoice;
  // Cache up through the last message block so growing agent history still hits
  // the prefix written on the previous turn.
  const lastMessage = converted[converted.length - 1];
  if (lastMessage && lastMessage.content.length > 0) {
    const blocks = lastMessage.content as Array<Record<string, unknown>>;
    const lastBlock = blocks[blocks.length - 1];
    if (lastBlock)
      blocks[blocks.length - 1] = { ...lastBlock, cache_control: { type: "ephemeral" } };
  }
  return out;
}

/**
 * The inverse of {@link chatToAnthropic}: an Anthropic `/messages` body as OpenAI chat.
 *
 * Needed when an Anthropic client (OpenCode's probe) is routed to a `both` provider's
 * OpenAI-only model (DeepSeek on Command Code / OpenCode Go) — that model must leave on
 * `/chat/completions`, then the reply is folded back with {@link chatToAnthropicMessage}.
 */
export function anthropicToChatRequest(
  body: Record<string, unknown>,
  model: string,
): Record<string, unknown> {
  const messages: Array<Record<string, unknown>> = [];
  const system = body.system;
  if (typeof system === "string" && system.length > 0) {
    messages.push({ role: "system", content: system });
  } else if (Array.isArray(system)) {
    const text = system
      .map((block) => textOf(asRecord(block).text ?? block))
      .filter((part) => part.length > 0)
      .join("\n\n");
    if (text.length > 0) messages.push({ role: "system", content: text });
  }

  const incoming = Array.isArray(body.messages) ? body.messages : [];
  for (const raw of incoming) {
    const message = asRecord(raw);
    const role = message.role;
    if (role !== "user" && role !== "assistant") continue;
    if (typeof message.content === "string") {
      messages.push({
        role,
        content: message.content.length > 0 ? message.content : role === "assistant" ? null : "",
      });
      continue;
    }
    const content = Array.isArray(message.content) ? message.content : [];
    const texts: string[] = [];
    const images: Array<Record<string, unknown>> = [];
    const toolCalls: unknown[] = [];
    for (const rawBlock of content) {
      const block = asRecord(rawBlock);
      if (block.type === "text") {
        const text = asString(block.text);
        if (text.length > 0) texts.push(text);
        continue;
      }
      if (block.type === "image" && role === "user") {
        const image = anthropicImageAsChat(block);
        if (image) images.push(image);
        continue;
      }
      if (block.type === "tool_use") {
        toolCalls.push({
          id: asString(block.id),
          type: "function",
          function: {
            name: asString(block.name),
            arguments: JSON.stringify(asRecord(block.input)),
          },
        });
        continue;
      }
      if (block.type === "tool_result") {
        messages.push({
          role: "tool",
          tool_call_id: asString(block.tool_use_id),
          content: toolResultContent(block.content),
        });
      }
    }
    if (role === "assistant") {
      const entry: Record<string, unknown> = {
        role: "assistant",
        content: texts.length > 0 ? texts.join("\n") : null,
      };
      if (toolCalls.length > 0) entry.tool_calls = toolCalls;
      messages.push(entry);
      continue;
    }
    if (images.length > 0) {
      // Images keep their place beside the text as Chat content parts; a text-only turn stays
      // a plain string, which every Chat host accepts.
      messages.push({
        role: "user",
        content: [...texts.map((text) => ({ type: "text", text })), ...images],
      });
      continue;
    }
    if (texts.length > 0) messages.push({ role: "user", content: texts.join("\n") });
  }

  const max = body.max_tokens;
  const out: Record<string, unknown> = {
    model,
    messages,
    max_tokens: typeof max === "number" && max > 0 ? max : 4_096,
    // Without its tools the model cannot call any, and an agent turn silently degrades into
    // a text answer — the tool definitions travel with the conversation.
    ...anthropicToolsAsChat(body),
  };
  if (typeof body.temperature === "number") out.temperature = body.temperature;
  if (typeof body.top_p === "number") out.top_p = body.top_p;
  if (typeof body.stream === "boolean") out.stream = body.stream;
  return out;
}

/** An Anthropic image block as a Chat `image_url` part: inline base64 or a URL source. */
function anthropicImageAsChat(block: Record<string, unknown>): Record<string, unknown> | undefined {
  const source = asRecord(block.source);
  if (
    source.type === "base64" &&
    typeof source.media_type === "string" &&
    typeof source.data === "string"
  ) {
    return {
      type: "image_url",
      image_url: { url: `data:${source.media_type};base64,${source.data}` },
    };
  }
  if (source.type === "url" && typeof source.url === "string") {
    return { type: "image_url", image_url: { url: source.url } };
  }
  return undefined;
}

/** Anthropic `tools` as Chat Completions function tools. Server tools (no schema) are dropped. */
export function anthropicToolsAsChat(body: Record<string, unknown>): Record<string, unknown> {
  const tools = (Array.isArray(body.tools) ? body.tools : []).flatMap((raw) => {
    const tool = asRecord(raw);
    const name = asString(tool.name);
    if (name.length === 0 || typeof tool.input_schema !== "object" || tool.input_schema === null) {
      return [];
    }
    return [
      {
        type: "function",
        function: {
          name,
          ...(typeof tool.description === "string" ? { description: tool.description } : {}),
          parameters: tool.input_schema,
        },
      },
    ];
  });
  const choice = asRecord(body.tool_choice);
  const toolChoice =
    choice.type === "any"
      ? "required"
      : choice.type === "none"
        ? "none"
        : choice.type === "tool" && asString(choice.name).length > 0
          ? { type: "function", function: { name: asString(choice.name) } }
          : choice.type === "auto"
            ? "auto"
            : undefined;
  return {
    ...(tools.length > 0 ? { tools } : {}),
    ...(tools.length > 0 && toolChoice !== undefined ? { tool_choice: toolChoice } : {}),
  };
}

const CHAT_STOP_REASONS: Record<string, string> = {
  stop: "end_turn",
  length: "max_tokens",
  tool_calls: "tool_use",
  content_filter: "refusal",
};

/** Folds an OpenAI chat completion back into Anthropic's `/messages` response shape. */
export function chatToAnthropicMessage(
  response: Record<string, unknown>,
  model: string,
): Record<string, unknown> {
  const choices = Array.isArray(response.choices) ? response.choices : [];
  const choice = asRecord(choices[0]);
  const message = asRecord(choice.message);
  const content: unknown[] = [];
  const text = asString(message.content);
  if (text.length > 0) content.push({ type: "text", text });
  const toolCalls = Array.isArray(message.tool_calls) ? message.tool_calls : [];
  for (const rawCall of toolCalls) {
    const call = asRecord(rawCall);
    const fn = asRecord(call.function);
    content.push({
      type: "tool_use",
      id: asString(call.id),
      name: asString(fn.name),
      input: parseArgs(fn.arguments),
    });
  }
  const usage = asRecord(response.usage);
  const finish = asString(choice.finish_reason);
  return {
    id: asString(response.id) || `msg_${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`,
    type: "message",
    role: "assistant",
    model: asString(response.model) || model,
    content,
    stop_reason: CHAT_STOP_REASONS[finish] ?? "end_turn",
    stop_sequence: null,
    usage: {
      input_tokens: number(usage.prompt_tokens),
      output_tokens: number(usage.completion_tokens),
    },
  };
}

export function anthropicUsage(raw: unknown): Usage {
  const usage = asRecord(raw);
  return {
    input: number(usage.input_tokens),
    output: number(usage.output_tokens),
    cacheRead: number(usage.cache_read_input_tokens),
    cacheWrite: number(usage.cache_creation_input_tokens),
  };
}

const STOP_REASONS: Record<string, string> = {
  end_turn: "stop",
  stop_sequence: "stop",
  max_tokens: "length",
  tool_use: "tool_calls",
  refusal: "content_filter",
};

export interface AnthropicCall {
  id: string;
  name: string;
  arguments: string;
}

export interface AnthropicResult {
  text: string;
  calls: AnthropicCall[];
  finishReason: string;
  usage: Usage;
}

export function anthropicResult(response: Record<string, unknown>): AnthropicResult {
  const content = Array.isArray(response.content) ? response.content : [];
  let text = "";
  const calls: AnthropicCall[] = [];
  for (const raw of content) {
    const block = asRecord(raw);
    if (block.type === "text" && typeof block.text === "string") text += block.text;
    if (block.type === "tool_use") {
      calls.push({
        id: asString(block.id),
        name: asString(block.name),
        arguments: JSON.stringify(block.input ?? {}),
      });
    }
  }
  const stop = asString(response.stop_reason);
  const finishReason = calls.length > 0 ? "tool_calls" : (STOP_REASONS[stop] ?? "stop");
  return { text, calls, finishReason, usage: anthropicUsage(response.usage) };
}

export function anthropicToChat(
  response: Record<string, unknown>,
  model: string,
): Record<string, unknown> {
  const result = anthropicResult(response);
  const message: Record<string, unknown> = {
    role: "assistant",
    content: result.text.length > 0 ? result.text : null,
  };
  if (result.calls.length > 0) {
    message.tool_calls = result.calls.map((call, index) => ({
      index,
      id: call.id || `call_${index}`,
      type: "function",
      function: { name: call.name, arguments: call.arguments },
    }));
  }
  return {
    id: asString(response.id) || `chatcmpl-${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model: asString(response.model) || model,
    choices: [{ index: 0, message, finish_reason: result.finishReason }],
    usage: {
      prompt_tokens: result.usage.input,
      completion_tokens: result.usage.output,
      total_tokens: result.usage.input + result.usage.output,
    },
  };
}

export function anthropicToChatStream(
  model: string,
  onFinish?: (usage: Usage, failure?: string) => void,
  onEvent?: (event: StreamEvent) => void,
): TransformStream<Uint8Array, Uint8Array> {
  const decoder = new TextDecoder();
  const encoder = new TextEncoder();
  const id = `chatcmpl-${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`;
  const created = Math.floor(Date.now() / 1000);
  const toolIndexes = new Map<number, number>();
  let buffer = "";
  let usage: Usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  let roleSent = false;
  let finished = false;
  let failure: string | undefined;

  const chunk = (
    delta: Record<string, unknown>,
    finishReason: string | null,
  ): Record<string, unknown> => ({
    id,
    object: "chat.completion.chunk",
    created,
    model,
    choices: [{ index: 0, delta, finish_reason: finishReason }],
  });

  const emit = (
    payload: Record<string, unknown>,
    controller: TransformStreamDefaultController<Uint8Array>,
  ): void => {
    controller.enqueue(encoder.encode(`data: ${JSON.stringify(payload)}\n\n`));
  };

  // Cache reads ride along as OpenAI's `prompt_tokens_details.cached_tokens`, so a Chat or
  // Responses client downstream still sees them.
  const usagePayload = (): Record<string, unknown> => ({
    prompt_tokens: usage.input,
    completion_tokens: usage.output,
    total_tokens: usage.input + usage.output,
    ...(usage.cacheRead > 0 ? { prompt_tokens_details: { cached_tokens: usage.cacheRead } } : {}),
  });

  const handle = (
    event: Record<string, unknown>,
    controller: TransformStreamDefaultController<Uint8Array>,
  ): void => {
    const type = event.type;
    if (type === "message_start") {
      const message = asRecord(event.message);
      usage = anthropicUsage(message.usage);
      if (!roleSent) {
        emit(chunk({ role: "assistant", content: "" }, null), controller);
        roleSent = true;
      }
      return;
    }
    if (type === "content_block_start") {
      const block = asRecord(event.content_block);
      if (block.type !== "tool_use") return;
      const index = number(event.index);
      const toolIndex = toolIndexes.size;
      toolIndexes.set(index, toolIndex);
      onEvent?.({ kind: "content" });
      emit(
        chunk(
          {
            tool_calls: [
              {
                index: toolIndex,
                id: asString(block.id) || `call_${toolIndex}`,
                type: "function",
                function: { name: asString(block.name), arguments: "" },
              },
            ],
          },
          null,
        ),
        controller,
      );
      return;
    }
    if (type === "content_block_delta") {
      const delta = asRecord(event.delta);
      if (delta.type === "text_delta" && typeof delta.text === "string" && delta.text.length > 0) {
        onEvent?.({ kind: "content" });
        emit(chunk({ content: delta.text }, null), controller);
        return;
      }
      if (delta.type === "input_json_delta" && typeof delta.partial_json === "string") {
        const toolIndex = toolIndexes.get(number(event.index)) ?? 0;
        onEvent?.({ kind: "content" });
        emit(
          chunk(
            { tool_calls: [{ index: toolIndex, function: { arguments: delta.partial_json } }] },
            null,
          ),
          controller,
        );
      }
      return;
    }
    if (type === "message_delta") {
      const delta = asRecord(event.delta);
      const stop = asString(delta.stop_reason);
      const output = number(asRecord(event.usage).output_tokens);
      if (output > 0) usage.output = output;
      if (stop.length > 0 && !finished) {
        onEvent?.({ kind: "usage", usage: { ...usage } });
        onEvent?.({ kind: "finish" });
        finished = true;
        emit(
          {
            ...chunk({}, STOP_REASONS[stop] ?? "stop"),
            usage: usagePayload(),
          },
          controller,
        );
      }
      return;
    }
    if (type === "error") {
      const error = asRecord(event.error);
      failure = asString(error.message) || "upstream error";
      onEvent?.({ kind: "error", message: failure });
    }
  };

  return new TransformStream<Uint8Array, Uint8Array>({
    transform(input, controller) {
      buffer += decoder.decode(input, { stream: true });
      const { events, rest } = splitSseEvents(buffer);
      buffer = rest;
      for (const event of events) handle(event, controller);
    },
    flush(controller) {
      buffer += decoder.decode();
      for (const event of splitSseEvents(buffer).events) handle(event, controller);
      // A finish reason already ended the turn: never append a second one, which would be an
      // invalid duplicate for a strict Chat client. The upstream error is still reported below.
      if (failure && !finished) {
        // Never hand a mid-stream refusal to the client as a bare `{ error }` frame: the harness
        // reads that as an invalid response and can roll the user message back. Finish the turn
        // with a readable assistant message instead; the ledger still records the real failure.
        emit(chunk({ content: softErrorMessage(failure) }, null), controller);
      }
      if (!finished) {
        emit({ ...chunk({}, "stop"), usage: usagePayload() }, controller);
      }
      controller.enqueue(encoder.encode("data: [DONE]\n\n"));
      onFinish?.(usage, failure);
    },
  });
}
