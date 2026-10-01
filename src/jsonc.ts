/**
 * Surgical JSONC editing: change only the keys a config edit owns, leaving every
 * other byte — comments, blank lines, key order, 2-vs-4-space indent — untouched.
 *
 * A whole-file `JSON.parse` + `JSON.stringify` round-trip silently reformats the
 * user's `settings.json`. Claude Code (and most agent CLIs) permit `//` comments and
 * trailing commas, so the file is JSONC, not strict JSON. The surgical rule: locate the
 * managed member's value span in the text and replace just that span, or insert one
 * property in the object's existing indent style. Nothing else is ever reordered,
 * deleted, or reprinted.
 *
 * The surface is intentionally narrow — set or remove an object member, including one
 * nested inside an object member like `env`. Keys that cannot be resolved are skipped
 * rather than risk a corrupt file.
 */

export interface JsoncEdit {
  /** Member path, e.g. `["env", "ANTHROPIC_BASE_URL"]`. */
  path: string[];
  /** Value to write; `undefined` removes the member. */
  value: unknown;
}

interface Tok {
  /** `"str"` keys/strings, `"lit"` numbers/true/false/null, single punct chars. */
  kind: "str" | "lit" | "{" | "}" | "[" | "]" | ":" | ",";
  start: number;
  end: number;
  /** Decoded text for strings, raw text for literals/punct. */
  text: string;
}

/** Tokenize JSONC, dropping whitespace and both comment forms. */
function scan(text: string): Tok[] {
  const tokens: Tok[] = [];
  const n = text.length;
  let i = 0;
  while (i < n) {
    const c = text[i];
    if (c === " " || c === "\t" || c === "\n" || c === "\r") {
      i += 1;
      continue;
    }
    if (c === "/" && text[i + 1] === "/") {
      while (i < n && text[i] !== "\n") i += 1;
      continue;
    }
    if (c === "/" && text[i + 1] === "*") {
      i += 2;
      while (i < n && !(text[i] === "*" && text[i + 1] === "/")) i += 1;
      i += 2;
      continue;
    }
    if (c === "{") {
      tokens.push({ kind: "{", start: i, end: i + 1, text: "{" });
      i += 1;
      continue;
    }
    if (c === "}") {
      tokens.push({ kind: "}", start: i, end: i + 1, text: "}" });
      i += 1;
      continue;
    }
    if (c === "[") {
      tokens.push({ kind: "[", start: i, end: i + 1, text: "[" });
      i += 1;
      continue;
    }
    if (c === "]") {
      tokens.push({ kind: "]", start: i, end: i + 1, text: "]" });
      i += 1;
      continue;
    }
    if (c === ":") {
      tokens.push({ kind: ":", start: i, end: i + 1, text: ":" });
      i += 1;
      continue;
    }
    if (c === ",") {
      tokens.push({ kind: ",", start: i, end: i + 1, text: "," });
      i += 1;
      continue;
    }
    if (c === '"') {
      const start = i;
      i += 1;
      let value = "";
      while (i < n && text[i] !== '"') {
        if (text[i] === "\\" && i + 1 < n) {
          const esc = text[i + 1];
          if (esc === "u") {
            value += String.fromCodePoint(parseInt(text.slice(i + 2, i + 6), 16) || 0);
            i += 6;
          } else {
            value +=
              esc === "n"
                ? "\n"
                : esc === "t"
                  ? "\t"
                  : esc === "r"
                    ? "\r"
                    : esc === "b"
                      ? "\b"
                      : esc === "f"
                        ? "\f"
                        : esc;
            i += 2;
          }
          continue;
        }
        value += text[i];
        i += 1;
      }
      i += 1; // closing quote
      tokens.push({ kind: "str", start, end: i, text: value });
      continue;
    }
    // Literal: number, true, false, null.
    const start = i;
    while (
      i < n &&
      !/[\s{}[\]:,"]/.test(text[i]) &&
      !(text[i] === "/" && /[/*]/.test(text[i + 1] ?? ""))
    ) {
      i += 1;
    }
    tokens.push({ kind: "lit", start, end: i, text: text.slice(start, i) });
  }
  return tokens;
}

/**
 * A cursor over the token stream that walks object members. `member()` yields each
 * `{ keyTok, colonTok, valueStart }` at the current object's depth.
 */
interface Member {
  keyTok: Tok;
  /** Start of the value token. */
  valueIndex: number;
  /** End offset of the member's value (after its token / nested block). */
  valueEnd: number;
}

/** Index of the token after the value starting at `vi` (skips nested blocks). */
function valueEndIndex(tokens: Tok[], vi: number): number {
  const t = tokens[vi];
  if (t === undefined) return vi;
  if (t.kind === "{") {
    let depth = 1;
    let j = vi + 1;
    while (j < tokens.length && depth > 0) {
      if (tokens[j].kind === "{") depth += 1;
      else if (tokens[j].kind === "}") depth -= 1;
      j += 1;
    }
    return j;
  }
  if (t.kind === "[") {
    let depth = 1;
    let j = vi + 1;
    while (j < tokens.length && depth > 0) {
      if (tokens[j].kind === "[") depth += 1;
      else if (tokens[j].kind === "]") depth -= 1;
      j += 1;
    }
    return j;
  }
  return vi + 1;
}

/**
 * Iterate the top-level members of the object whose `{` is at `tokens[startIdx]`,
 * or the document root when `startIdx` is `-1` (a bare object literal at the top).
 */
function membersOf(tokens: Tok[], startIdx: number): { members: Member[]; closeIdx: number } {
  const members: Member[] = [];
  // Start scanning just inside the opening brace (or at token 0 for the root object).
  let i = startIdx + 1;
  let closeIdx = tokens.length;
  while (i < tokens.length) {
    const t = tokens[i];
    if (t.kind === "}") {
      closeIdx = i;
      break;
    }
    if (t.kind === "str" && tokens[i + 1]?.kind === ":") {
      const valueIndex = i + 2;
      const endIdx = valueEndIndex(tokens, valueIndex);
      members.push({
        keyTok: t,
        valueIndex,
        valueEnd: tokens[endIdx - 1]?.end ?? tokens[valueIndex]?.end ?? t.end,
      });
      i = endIdx;
      continue;
    }
    i += 1;
  }
  return { members, closeIdx };
}

/**
 * Resolve `path` to a concrete edit site. Returns the member's value span when found,
 * or the insertion point (the closing `}` of the containing object) when the leaf is
 * missing. `null` when a non-final path step is absent or not an object.
 */
function resolvePath(
  tokens: Tok[],
  text: string,
  path: string[],
):
  | { found: true; valueStart: number; valueEnd: number; keyStart: number }
  | { found: false; insertAt: number; insertIndent: string }
  | null {
  let scopeStart = -1; // -1 → root object
  for (let depth = 0; depth < path.length; depth += 1) {
    const startIdx = scopeStart;
    // If descending into a member, startIdx should point at its `{` token.
    const { members, closeIdx } = membersOf(tokens, startIdx);
    const key = path[depth];
    const member = members.find((m) => m.keyTok.text === key);
    const last = depth === path.length - 1;
    if (member && last) {
      return {
        found: true,
        valueStart: tokens[member.valueIndex].start,
        valueEnd: member.valueEnd,
        keyStart: member.keyTok.start,
      };
    }
    if (member && !last) {
      const valueTok = tokens[member.valueIndex];
      if (valueTok?.kind !== "{") return null; // not an object we can descend into
      scopeStart = member.valueIndex;
      continue;
    }
    // Member missing.
    if (!last) return null; // can't create intermediate objects surgically
    const closeTok = tokens[closeIdx];
    const insertAt = closeTok ? closeTok.start : text.length;
    return { found: false, insertAt, insertIndent: detectIndent(text, startIdx, tokens, closeIdx) };
  }
  return null;
}

/** Leading whitespace of the line containing `offset`. */
function lineIndent(text: string, offset: number): string {
  const start = text.lastIndexOf("\n", offset - 1) + 1;
  let i = start;
  while (i < text.length && (text[i] === " " || text[i] === "\t")) i += 1;
  return text.slice(start, i);
}

/** Indent used by members of the object whose `{` is `tokens[startIdx]` (or root if -1). */
function detectIndent(text: string, startIdx: number, tokens: Tok[], closeIdx: number): string {
  const openTok = startIdx >= 0 ? tokens[startIdx] : undefined;
  const first = tokens[startIdx + 1];
  const closeTok = tokens[closeIdx];
  if (openTok && first && first.kind !== "}" && closeTok) {
    const ind = lineIndent(text, first.start);
    if (ind.length > 0) return ind;
  }
  // Empty or single-line object: parent indent + two spaces.
  return `${openTok ? lineIndent(text, openTok.start) : ""}  `;
}

/** Remove `path`'s member, swallowing one adjacent comma so the object stays valid. */
function removeMember(text: string, tokens: Tok[], path: string[]): string {
  const resolved = resolvePath(tokens, text, path);
  if (!resolved || !resolved.found) return text;
  const { keyStart, valueEnd } = resolved;

  // Find the token index of the key to inspect neighbors for commas.
  let keyIdx = -1;
  for (let i = 0; i < tokens.length; i += 1) {
    if (tokens[i].start === keyStart) {
      keyIdx = i;
      break;
    }
  }
  if (keyIdx < 0) return text;

  const afterIdx = valueEndIndex(tokens, keyIdx + 2); // token index just after the value
  const hasFollowingComma = tokens[afterIdx]?.kind === ",";
  const hasPrecedingComma = tokens[keyIdx - 1]?.kind === ",";

  let start = keyStart;
  let end = valueEnd;
  if (hasFollowingComma) {
    // Removing a middle/first member: swallow the comma and the whitespace+newline
    // after it, and back up over the member's leading line indent so no blank or
    // over-indented line is left behind.
    end = tokens[afterIdx].end;
    while (end < text.length && (text[end] === " " || text[end] === "\t")) end += 1;
    if (text[end] === "\n") end += 1;
    // Back up over leading whitespace to (and including) the preceding newline.
    let s = start - 1;
    while (s >= 0 && (text[s] === " " || text[s] === "\t")) s -= 1;
    if (text[s] === "\n") start = s + 1; // keep the newline; drop the indent
  } else if (hasPrecedingComma) {
    // Removing the last member: take the preceding comma plus the whitespace and
    // newline that separated it from the previous member, so the close brace lands
    // on the previous member's own line ending.
    start = tokens[keyIdx - 1].start;
    let s = start - 1;
    while (s >= 0 && (text[s] === " " || text[s] === "\t")) s -= 1;
    if (text[s] === "\n") start = s + 1;
    // Trailing whitespace before the close brace collapses to a single newline+indent.
    while (end < text.length && (text[end] === " " || text[end] === "\t")) end += 1;
    if (text[end] === "\n") {
      // keep the newline and closing-brace indent
      end += 1;
      while (end < text.length && (text[end] === " " || text[end] === "\t")) end += 1;
      return text.slice(0, start) + "\n" + lineIndent(text, end) + text.slice(end);
    }
  }
  return text.slice(0, start) + text.slice(end);
}

/** Insert `key: serialized` at `insertAt` (the containing object's `}`). */
function insertMember(
  text: string,
  path: string[],
  serialized: string,
  insertAt: number,
  indent: string,
): string {
  const key = path[path.length - 1];
  // Last non-whitespace char before insertAt tells us if a comma is needed.
  let prev = insertAt - 1;
  while (prev >= 0 && /\s/.test(text[prev])) prev -= 1;
  const emptyObject = prev >= 0 && text[prev] === "{";
  const needsComma = !emptyObject && text[prev] !== ",";
  const closingIndent = lineIndent(text, insertAt);

  const head = text.slice(0, prev + 1);
  const tail = text.slice(insertAt);
  if (emptyObject) {
    return `${head}\n${indent}"${key}": ${serialized}\n${closingIndent}${tail}`;
  }
  return `${head}${needsComma ? "," : ""}\n${indent}"${key}": ${serialized}\n${closingIndent}${tail}`;
}

/** Apply a single edit, returning updated text (or the original when unresolvable). */
function applyOneEdit(text: string, edit: JsoncEdit): string {
  const tokens = scan(text);
  const resolved = resolvePath(tokens, text, edit.path);
  if (!resolved) return text;

  if (edit.value === undefined) {
    return resolved.found ? removeMember(text, tokens, edit.path) : text;
  }

  const serialized = JSON.stringify(edit.value);
  if (resolved.found) {
    return text.slice(0, resolved.valueStart) + serialized + text.slice(resolved.valueEnd);
  }
  return insertMember(text, edit.path, serialized, resolved.insertAt, resolved.insertIndent);
}

/**
 * Apply `edits` to JSONC `text`. Each edit is resolved against the fresh text so
 * offsets never drift. Unresolvable paths are skipped; the file is never corrupted.
 */
export function applyJsoncEdits(text: string, edits: JsoncEdit[]): string {
  let doc = text;
  for (const edit of edits) doc = applyOneEdit(doc, edit);
  return doc;
}

/** True when `path` resolves to an existing member value in the JSONC text. */
export function jsoncPathExists(text: string, path: string[]): boolean {
  return resolvePath(scan(text), text, path)?.found === true;
}

/**
 * The immediate member keys of the object at `path`, or `null` when the path is
 * absent or is not an object. Lets a caller decide whether a block is now empty
 * without a strict `JSON.parse` that would choke on comments.
 */
export function jsoncObjectKeys(text: string, path: string[]): string[] | null {
  const tokens = scan(text);
  const resolved = resolvePath(tokens, text, path);
  if (!resolved || !resolved.found) return null;
  const valueIdx = tokens.findIndex((t) => t.start === resolved.valueStart);
  if (valueIdx < 0 || tokens[valueIdx].kind !== "{") return null;
  const { members } = membersOf(tokens, valueIdx);
  return members.map((m) => m.keyTok.text);
}
