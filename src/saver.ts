/**
 * Deterministic tool-result compression, in the spirit of RTK (Rust Token Killer).
 *
 * RTK sits in front of an agent's shell and compresses command stdout before the agent sees
 * it. Jevonian is an HTTP proxy, so it cannot intercept the command itself — but every agent
 * turn re-sends the whole conversation, and the bulky part is always the same: prior tool
 * results (test logs, `git status`, file reads, command output). This module shrinks those
 * tool results inside the outgoing request body, before egress, using only deterministic
 * rules: dedupe repeated lines, drop noise lines, cap long outputs. Nothing is summarised or
 * rewritten by a model, so an exact error string or file path survives verbatim.
 *
 * Savings are estimated per result with `estimateTokens` (the same estimator the rest of the
 * codebase calibrates against) and recorded on the turn's ledger row, so `jevonian report`
 * and the dashboard can answer "how many tokens did the saver keep out of the prompt".
 */

import { estimateTokens } from "./compaction";
import type { RequestKind } from "./routing";

export interface TokenSaverConfig {
  /** Master switch. Off leaves every request body untouched. */
  enabled: boolean;
  /** Drop tool outputs beyond this many characters, keeping head + tail. */
  maxChars: number;
  /** Collapse runs of identical lines (progress spam, separators). */
  dedupeLines: boolean;
  /** Drop known noise lines (npm warnings, download progress, blank-only lines). */
  stripNoise: boolean;
}

/** Smallest per-result cap the parser accepts; smaller would leave only the removal marker. */
const MIN_MAX_CHARS = 500;

export const DEFAULT_TOKEN_SAVER: TokenSaverConfig = {
  enabled: true,
  maxChars: 30_000,
  dedupeLines: true,
  stripNoise: true,
};

export function parseTokenSaver(raw: unknown): TokenSaverConfig {
  const value =
    raw && typeof raw === "object" && !Array.isArray(raw) ? (raw as Record<string, unknown>) : {};
  const maxChars = value.maxChars;
  return {
    enabled: value.enabled !== false,
    // A cap below ~500 chars leaves only the "removed N chars" marker — the tool output is
    // gone entirely. Clamp rather than honouring a destructive setting.
    maxChars:
      typeof maxChars === "number" && Number.isFinite(maxChars)
        ? Math.max(MIN_MAX_CHARS, Math.floor(maxChars))
        : DEFAULT_TOKEN_SAVER.maxChars,
    dedupeLines: value.dedupeLines !== false,
    stripNoise: value.stripNoise !== false,
  };
}

export interface SaverStats {
  /** Tool results whose text was replaced by a shorter one. */
  resultsCompressed: number;
  /** Estimated input tokens kept out of the request. */
  savedTokens: number;
  /** Characters removed across all compressed results. */
  charsBefore: number;
  charsAfter: number;
}

export interface SaveResult {
  /** The rewritten body, or the original reference when nothing qualified. */
  body: Record<string, unknown>;
  stats: SaverStats;
}

const NOISE_PATTERNS: readonly RegExp[] = [
  /^\s*$/,
  /^\s*npm warn\b/i,
  /^\s*\d+%\s*(?:\||\[|Downloading|downloading|Extracting|extracting)/,
  /^\s*(?:downloaded|extracted)\s+\d+[\d.]*\s*(?:KB|MB|GB|bytes)/i,
  /^\s*(?:info|note):\s*(?:download|extract|compile|checking)/i,
  /^\s*(?:\||\/|-|\\)\s*$/, // spinner frames
];

function isNoiseLine(line: string): boolean {
  return NOISE_PATTERNS.some((pattern) => pattern.test(line));
}

/**
 * Collapse consecutive duplicate lines into `line × n` and drop noise lines. Runs only when
 * the text is long enough to bother, so a two-line tool result passes through untouched.
 */
function compressLines(text: string, dedupe: boolean, stripNoise: boolean): string {
  const lines = text.split("\n");
  if (lines.length < 8) return text;
  const out: string[] = [];
  let repeat = 0;
  let previous: string | undefined;
  const flushRepeat = (): void => {
    if (repeat > 1 && previous !== undefined) {
      out[out.length - 1] = `${previous}  (×${repeat})`;
    }
    repeat = 0;
  };
  for (const line of lines) {
    if (stripNoise && isNoiseLine(line)) {
      flushRepeat();
      previous = undefined;
      continue;
    }
    if (dedupe && previous !== undefined && line === previous) {
      repeat += 1;
      continue;
    }
    flushRepeat();
    previous = line;
    out.push(line);
  }
  flushRepeat();
  const joined = out.join("\n");
  return joined === text ? text : joined;
}

/**
 * Keep the first and last parts of an oversized tool output and mark the middle as removed.
 * The marker tells the model the output was truncated and by how much, so it can re-run or
 * read the file again rather than assume the content ended.
 */
function abridge(text: string, maxChars: number): string {
  if (text.length <= maxChars) return text;
  const head = Math.floor(maxChars * 0.7);
  const tail = Math.floor(maxChars * 0.2);
  const omitted = text.length - head - tail;
  const next = `${text.slice(0, head)}\n[… jevonian removed ${omitted} chars; re-run the tool or read the file if needed …]\n${text.slice(-tail)}`;
  // A cap so small the marker alone outweighs the content is a misconfiguration, not a
  // compression — leave the text alone rather than enlarging the request.
  return next.length < text.length ? next : text;
}

/**
 * One tool-result text through the saver's stages. Returns the original string when nothing
 * changed so callers can keep the body by reference.
 */
export function compressToolResult(text: string, config: TokenSaverConfig): string {
  let out = text;
  if (config.stripNoise || config.dedupeLines) {
    out = compressLines(out, config.dedupeLines, config.stripNoise);
  }
  if (out.length > config.maxChars && config.maxChars >= 0) {
    out = abridge(out, config.maxChars);
  }
  return out;
}

type Json = Record<string, unknown>;

function isRecord(value: unknown): value is Json {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * The text a message part carries, whichever wire field it lives in. Anthropic tool results
 * keep theirs in `content` (string or text blocks); OpenAI `tool` messages use `content`;
 * Responses `function_call_output` items use `output`.
 */
function extractText(part: Json, field: "content" | "output"): { text: string; key: string } | undefined {
  const raw = part[field];
  if (typeof raw === "string") return { text: raw, key: field };
  if (Array.isArray(raw)) {
    const block = raw.find((item) => isRecord(item) && typeof item.text === "string");
    if (isRecord(block) && typeof block.text === "string") {
      return { text: block.text, key: field };
    }
  }
  return undefined;
}

/** Write a compressed text back into the same shape the part carried it in. */
function injectText(part: Json, field: string, text: string): Json {
  const raw = part[field];
  if (typeof raw === "string") return { ...part, [field]: text };
  if (Array.isArray(raw)) {
    let replaced = false;
    const next = raw.map((item) => {
      if (replaced || !isRecord(item) || typeof item.text !== "string") return item;
      replaced = true;
      return { ...item, text };
    });
    return { ...part, [field]: replaced ? next : raw };
  }
  return part;
}

/**
 * Rewrites tool results inside one message list. Shared by the three wire shapes, which all
 * differ only in where the result text lives and what it is called.
 */
function compressMessages(
  messages: unknown[],
  kind: RequestKind,
  config: TokenSaverConfig,
  stats: SaverStats,
): unknown[] {
  return messages.map((raw) => {
    if (!isRecord(raw)) return raw;

    // OpenAI `role: "tool"` with array content is handled by the dedicated branch below;
    // letting the generic array branch claim it first would skip compression entirely.
    const isToolMessage = raw.role === "tool" && typeof raw.tool_call_id === "string";

    // Anthropic: user messages carry `tool_result` content blocks.
    if (!isToolMessage && Array.isArray(raw.content)) {
      let changed = false;
      const content = raw.content.map((blockRaw) => {
        const block = isRecord(blockRaw) ? blockRaw : {};
        if (block.type !== "tool_result") return blockRaw;
        const found = extractText(block, "content");
        if (!found) return blockRaw;
        const next = compressToolResult(found.text, config);
        if (next === found.text) return blockRaw;
        changed = true;
        stats.resultsCompressed += 1;
        stats.charsBefore += found.text.length;
        stats.charsAfter += next.length;
        stats.savedTokens += Math.max(0, estimateTokens(found.text) - estimateTokens(next));
        return injectText(block, found.key, next);
      });
      return changed ? { ...raw, content } : raw;
    }

    // OpenAI chat: `role: "tool"` messages hold the output in `content`
    // (string or `[{ type: "text", text }]` — `extractText` handles both).
    if (isToolMessage) {
      const found = extractText(raw, "content");
      if (!found) return raw;
      const next = compressToolResult(found.text, config);
      if (next === found.text) return raw;
      stats.resultsCompressed += 1;
      stats.charsBefore += found.text.length;
      stats.charsAfter += next.length;
      stats.savedTokens += Math.max(0, estimateTokens(found.text) - estimateTokens(next));
      return injectText(raw, found.key, next);
    }

    // Responses: `type: "function_call_output"` items hold the output in `output`.
    if (raw.type === "function_call_output") {
      const found = extractText(raw, "output");
      if (!found) return raw;
      const next = compressToolResult(found.text, config);
      if (next === found.text) return raw;
      stats.resultsCompressed += 1;
      stats.charsBefore += found.text.length;
      stats.charsAfter += next.length;
      stats.savedTokens += Math.max(0, estimateTokens(found.text) - estimateTokens(next));
      return injectText(raw, found.key, next);
    }

    // Anthropic `system`/`tool_use` and everything else passes through untouched.
    void kind;
    return raw;
  });
}

/**
 * Shrinks prior tool results inside a request body, on whichever wire the body speaks.
 * Returns the body plus how much the saver removed; the caller records `savedTokens` on the
 * ledger row. The function never throws — a malformed message is left as it arrived.
 */
export function saveTokens(
  body: Record<string, unknown>,
  kind: RequestKind,
  config: TokenSaverConfig,
): SaveResult {
  const stats: SaverStats = {
    resultsCompressed: 0,
    savedTokens: 0,
    charsBefore: 0,
    charsAfter: 0,
  };
  if (!config.enabled) return { body, stats };

  // The messages list is `messages` on Chat/Anthropic bodies and `input` on Responses bodies.
  const key = Array.isArray(body.messages) ? "messages" : Array.isArray(body.input) ? "input" : undefined;
  if (!key) return { body, stats };
  const next = compressMessages(body[key] as unknown[], kind, config, stats);
  if (stats.resultsCompressed === 0) return { body, stats };
  return { body: { ...body, [key]: next }, stats };
}
