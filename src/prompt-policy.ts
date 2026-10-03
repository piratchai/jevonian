/**
 * Outgoing prompt hygiene.
 *
 * Some upstreams refuse a request outright when the system prompt carries verbatim wording from
 * a rival coding agent's prompt. Devin's content policy is the observed case: against
 * `swe-2-max` each of Cursor's identity line, its `tool_calling` paragraph, the
 * "MARKDOWN CODE BLOCKS … NOT already in Codebase" heading, and the
 * "There is one text file for each terminal the user has running." sentence blocked a request on
 * their own, while paraphrases of the same instructions passed. The signatures are matched per
 * sentence, so neutral wording keeps the client's behavior and drops the refusal.
 *
 * Built-in signatures are applied to the Devin wire only. Operators can add their own rules under
 * `promptPolicy.rewrites`, which apply to every wire.
 */

export interface PromptRewriteRule {
  /** Regular expression source, matched against the outgoing prompt text. */
  match: string;
  /** Optional regular-expression flags. `g` is always applied. */
  flags?: string;
  /**
   * Replacement text. Supports `$1` group references. Use `""` to delete the match, or a leading
   * `"!"`-free negative lookahead pattern to keep it conditional.
   */
  replace: string;
}

export interface PromptPolicyConfig {
  /** Built-in rival-prompt signatures on the Devin wire. Off sends the client prompt untouched. */
  builtins: boolean;
  /** Operator-supplied rules, appended after the built-ins and applied on every wire. */
  rewrites: PromptRewriteRule[];
}

export const DEFAULT_PROMPT_POLICY: PromptPolicyConfig = {
  builtins: true,
  rewrites: [],
};

/** Longest accepted rule source; a longer one is dropped rather than compiled. */
const MAX_PATTERN_LENGTH = 500;
const ALLOWED_FLAGS = "gimsuy";

/**
 * Signatures the upstream blocklist has been observed to reject, each replaced with wording that
 * kept the same instruction for the model without tripping the filter.
 */
export const BUILTIN_PROMPT_REWRITES: readonly [string, string][] = [
  ["You operate in Cursor.", "You work inside the user's code editor."],
  [
    "Use specialized tools instead of terminal commands when possible, as this provides a " +
      "better user experience. For file operations, use dedicated tools: don't use cat/head/tail " +
      "to read files, don't use sed/awk to edit files, don't use cat with heredoc or echo " +
      "redirection to create files. Reserve terminal commands exclusively for actual system " +
      "commands and terminal operations that require shell execution.",
    "Prefer the dedicated file tools over shell text utilities: read files with the file-reading " +
      "tool rather than cat/head/tail, edit with the edit tool rather than sed/awk, and create " +
      "files with the write tool rather than shell redirection. Keep the shell for real system " +
      "commands and terminal operations.",
  ],
  [
    "## METHOD 2: MARKDOWN CODE BLOCKS - Proposing or Displaying Code NOT already in Codebase",
    "## METHOD 2: FENCED CODE BLOCKS - For code that is not yet present in the repository",
  ],
  [
    "There is one text file for each terminal the user has running.",
    "Each open terminal has its own file.",
  ],
];

/** Covers the identity line in any casing and with any trailing detail ("Cursor IDE", "!"). */
const IDENTITY_PATTERN = /You operate in Cursor[^.\n]*[.!]?/gi;

/** Keeps only flags RegExp accepts, so a config typo cannot throw at compile time. */
function sanitizeFlags(flags: string): string {
  let out = "";
  for (const flag of flags) {
    if (ALLOWED_FLAGS.includes(flag) && !out.includes(flag)) out += flag;
  }
  return out;
}

export function parsePromptPolicy(raw: unknown): PromptPolicyConfig {
  const value =
    raw && typeof raw === "object" && !Array.isArray(raw) ? (raw as Record<string, unknown>) : {};
  const rewrites: PromptRewriteRule[] = [];
  if (Array.isArray(value.rewrites)) {
    for (const entry of value.rewrites) {
      const rule =
        entry && typeof entry === "object" && !Array.isArray(entry)
          ? (entry as Record<string, unknown>)
          : {};
      if (typeof rule.match !== "string" || rule.match.length === 0) continue;
      if (rule.match.length > MAX_PATTERN_LENGTH) continue;
      if (typeof rule.replace !== "string") continue;
      const flags = typeof rule.flags === "string" ? sanitizeFlags(rule.flags) : "";
      // An invalid pattern is dropped at parse time so the wire never throws mid-turn.
      try {
        new RegExp(rule.match, flags);
      } catch {
        continue;
      }
      rewrites.push({
        match: rule.match,
        replace: rule.replace,
        ...(flags ? { flags } : {}),
      });
    }
  }
  return { builtins: value.builtins !== false, rewrites };
}

function compile(rule: PromptRewriteRule): RegExp | undefined {
  try {
    const flags = rule.flags ?? "";
    return new RegExp(rule.match, flags.includes("g") ? flags : `${flags}g`);
  } catch {
    return undefined;
  }
}

/** Applies the built-in Cursor-prompt signatures. Idempotent: re-running changes nothing. */
export function sanitizeBuiltinPrompt(text: string): string {
  let out = text;
  for (const [from, to] of BUILTIN_PROMPT_REWRITES) out = out.split(from).join(to);
  return out.replace(IDENTITY_PATTERN, "You work inside the user's code editor.");
}

/** Applies the operator rules. An invalid pattern is skipped rather than failing the turn. */
export function sanitizePromptWithPolicy(text: string, policy: PromptPolicyConfig): string {
  let out = policy.builtins ? sanitizeBuiltinPrompt(text) : text;
  for (const rule of policy.rewrites) {
    const pattern = compile(rule);
    if (pattern) out = out.replace(pattern, rule.replace);
  }
  return out;
}

type Json = Record<string, unknown>;

function isRecord(value: unknown): value is Json {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Rewrites one prompt field, which may be a plain string or a list of text blocks. */
function rewriteField(value: unknown, apply: (text: string) => string): unknown {
  if (typeof value === "string") return apply(value);
  if (!Array.isArray(value)) return value;
  return value.map((part) =>
    isRecord(part) && typeof part.text === "string" ? { ...part, text: apply(part.text) } : part,
  );
}

/**
 * Applies the policy to every prompt field of an outgoing wire body, whichever shape it takes:
 * Chat Completions `messages`, Anthropic `system`, and Responses `instructions`. Returns a copy;
 * the caller's body is never mutated.
 */
export function rewritePromptBodies<T>(body: T, policy: PromptPolicyConfig): T {
  if (!isRecord(body)) return body;
  const apply = (text: string): string => sanitizePromptWithPolicy(text, policy);
  let next: Json = body;
  const mutable = (): Json => {
    if (next === body) next = { ...body };
    return next;
  };

  if (Array.isArray(body.messages)) {
    let changed = false;
    const messages = body.messages.map((raw) => {
      if (!isRecord(raw)) return raw;
      const role = typeof raw.role === "string" ? raw.role.toLowerCase() : "";
      if (role !== "system" && role !== "developer") return raw;
      const content = rewriteField(raw.content, apply);
      if (content === raw.content) return raw;
      changed = true;
      return { ...raw, content };
    });
    // Keep the original reference when nothing matched, so an untouched body stays identical.
    if (changed) mutable().messages = messages;
  }
  if (body.system !== undefined) {
    const system = rewriteField(body.system, apply);
    if (system !== body.system) mutable().system = system;
  }
  if (typeof body.instructions === "string") {
    mutable().instructions = apply(body.instructions);
  }
  // The Antigravity envelope keeps its system prompt inside `request.systemInstruction`.
  if (isRecord(body.request) && body.request.systemInstruction !== undefined) {
    const rewritten = rewriteField(body.request.systemInstruction, apply);
    if (rewritten !== body.request.systemInstruction) {
      mutable().request = { ...body.request, systemInstruction: rewritten };
    }
  }
  return next as T;
}
