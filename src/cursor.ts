/**
 * Cursor subscription wire core: Connect-RPC + protobuf against Cursor's agent API
 * (`AgentService/Run`), the same transport the `cursor-agent` CLI speaks. This module owns
 * credential resolution, model discovery, request encoding, stream decoding and error
 * classification; it holds no routing or config state.
 *
 * A Cursor account has no plain REST endpoint: the CLI talks a bidirectional Connect stream.
 * The conversation is sent whole as AI SDK messages, each stored as a sha256-named blob the
 * server asks back for as it reads them; the caller's tools are MCP tools reached through
 * Cursor's `CallDynamicTool`, which the server hands back to the client to run.
 */
import { execFile } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { existsSync, readFileSync, readdirSync, realpathSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import type { Usage } from "./pricing";
import { sanitizeBuiltinPrompt } from "./prompt-policy";

/**
 * Which local sign-in to read, when it is not the agent's own — a second Cursor account on the
 * same machine. Structurally `LoginSpec` from ./oauth, declared here so the wire module does
 * not have to import the oauth it is read by.
 */
export interface CursorLogin {
  home?: string;
  credentialsPath?: string;
  keychainService?: string;
  keychainAccount?: string;
}

export const CURSOR_DEFAULT_BASE_URL = "https://api2.cursor.sh";
/** Agent API used when the server config cannot name a region-specific one. */
export const CURSOR_AGENT_FALLBACK = "https://agentn.global.api5.cursor.sh";

const SERVER_CONFIG_PATH = "/aiserver.v1.ServerConfigService/GetServerConfig";
const RUN_PATH = "/agent.v1.AgentService/Run";

/** The pseudo-tool the model calls; Cursor hands each call back to the client. */
const CALL_DYNAMIC_TOOL = "CallDynamicTool";
const MCP_NAMESPACE = "magpie";

const DEFAULT_CLIENT_VERSION = "2026.09.23-86fc751";
const VERSION_PATTERN = /^\d{4}\.\d{2}\.\d{2}-[0-9a-f]+$/;

const MAX_FRAME_BYTES = 64 * 1024 * 1024;
const HEARTBEAT_MS = 5_000;
const REFRESH_SKEW_MS = 5 * 60_000;
const STATUS_TIMEOUT_MS = 30_000;

const utf8 = new TextEncoder();
const textDecoder = new TextDecoder();
const execFileAsync = promisify(execFile);

// ─── protobuf ───────────────────────────────────────────────────────────────

function concat(parts: Uint8Array[]): Uint8Array {
  let length = 0;
  for (const part of parts) length += part.length;
  const out = new Uint8Array(length);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

function varintBytes(value: number): Uint8Array {
  const out: number[] = [];
  let v = Math.max(0, Math.floor(value));
  while (v > 0x7f) {
    out.push((v % 0x80) | 0x80);
    v = Math.floor(v / 0x80);
  }
  out.push(v);
  return Uint8Array.from(out);
}

const tag = (field: number, wire: number): Uint8Array => varintBytes(field * 8 + wire);

/** A protobuf message built field by field. */
export class Pb {
  private readonly parts: Uint8Array[] = [];

  varint(field: number, value: number): Pb {
    this.parts.push(tag(field, 0), varintBytes(value));
    return this;
  }

  bytes(field: number, value: Uint8Array | string): Pb {
    const bytes = typeof value === "string" ? utf8.encode(value) : value;
    this.parts.push(tag(field, 2), varintBytes(bytes.length), bytes);
    return this;
  }

  str(field: number, value: string): Pb {
    return this.bytes(field, value);
  }

  double(field: number, value: number): Pb {
    const bytes = new Uint8Array(8);
    new DataView(bytes.buffer).setFloat64(0, value, true);
    this.parts.push(tag(field, 1), bytes);
    return this;
  }

  build(): Uint8Array {
    return concat(this.parts);
  }
}

interface PbField {
  num: number;
  wire: number;
  n: number;
  data?: Uint8Array;
}

/** Decodes one protobuf message level; a malformed tail is dropped. */
function pbFields(buf: Uint8Array): PbField[] {
  const out: PbField[] = [];
  let pos = 0;
  while (pos < buf.length) {
    const key = readVarint(buf, pos);
    if (!key) break;
    pos = key.next;
    const num = Math.floor(key.value / 8);
    const wire = key.value % 8;
    if (num === 0) break;
    if (wire === 0) {
      const v = readVarint(buf, pos);
      if (!v) break;
      out.push({ num, wire, n: v.value });
      pos = v.next;
    } else if (wire === 2) {
      const len = readVarint(buf, pos);
      if (!len) break;
      pos = len.next;
      if (pos + len.value > buf.length) break;
      out.push({ num, wire, n: 0, data: buf.subarray(pos, pos + len.value) });
      pos += len.value;
    } else if (wire === 1) {
      if (pos + 8 > buf.length) break;
      out.push({ num, wire, n: 0, data: buf.subarray(pos, pos + 8) });
      pos += 8;
    } else if (wire === 5) {
      if (pos + 4 > buf.length) break;
      out.push({ num, wire, n: 0, data: buf.subarray(pos, pos + 4) });
      pos += 4;
    } else {
      break;
    }
  }
  return out;
}

function readVarint(buf: Uint8Array, start: number): { value: number; next: number } | undefined {
  let value = 0;
  let scale = 1;
  for (let i = 0; i < 10; i += 1) {
    const index = start + i;
    if (index >= buf.length) return undefined;
    const byte = buf[index] as number;
    value += (byte & 0x7f) * scale;
    if ((byte & 0x80) === 0) return { value, next: index + 1 };
    scale *= 0x80;
  }
  return undefined;
}

function pbStr(fields: PbField[], num: number): string | undefined {
  const field = fields.find((f) => f.num === num && f.wire === 2);
  return field?.data === undefined ? undefined : textDecoder.decode(field.data);
}

function pbNum(fields: PbField[], num: number): number | undefined {
  return fields.find((f) => f.num === num && f.wire === 0)?.n;
}

/** Encodes a `google.protobuf.Value`. */
function pbValue(value: unknown): Uint8Array {
  if (value === null || value === undefined) return new Pb().varint(1, 0).build();
  if (typeof value === "number") return new Pb().double(2, value).build();
  if (typeof value === "string") return new Pb().str(3, value).build();
  if (typeof value === "boolean") return new Pb().varint(4, value ? 1 : 0).build();
  if (Array.isArray(value)) {
    const list = new Pb();
    for (const entry of value) list.bytes(1, pbValue(entry));
    return new Pb().bytes(6, list.build()).build();
  }
  if (typeof value === "object") {
    const keys = Object.keys(value as Record<string, unknown>).sort();
    const struct = new Pb();
    for (const key of keys) {
      struct.bytes(
        1,
        new Pb()
          .str(1, key)
          .bytes(2, pbValue((value as Record<string, unknown>)[key]))
          .build(),
      );
    }
    return new Pb().bytes(5, struct.build()).build();
  }
  return new Pb().varint(1, 0).build();
}

/** Decodes a `google.protobuf.Value`. */
function pbAny(buf: Uint8Array): unknown {
  for (const field of pbFields(buf)) {
    if (field.num === 1) return null;
    if (field.num === 2 && field.wire === 1 && field.data) {
      return new DataView(field.data.buffer, field.data.byteOffset, 8).getFloat64(0, true);
    }
    if (field.num === 3) return field.data === undefined ? null : textDecoder.decode(field.data);
    if (field.num === 4) return field.n !== 0;
    if (field.num === 5 && field.data) {
      const out: Record<string, unknown> = {};
      for (const entry of pbFields(field.data)) {
        if (entry.num !== 1 || entry.data === undefined) continue;
        let key = "";
        let value: unknown;
        for (const kv of pbFields(entry.data)) {
          if (kv.num === 1 && kv.data !== undefined) key = textDecoder.decode(kv.data);
          if (kv.num === 2 && kv.data !== undefined) value = pbAny(kv.data);
        }
        out[key] = value;
      }
      return out;
    }
    if (field.num === 6 && field.data) {
      const list: unknown[] = [];
      for (const entry of pbFields(field.data)) {
        if (entry.num === 1 && entry.data !== undefined) list.push(pbAny(entry.data));
      }
      return list;
    }
  }
  return null;
}

// ─── Connect framing ────────────────────────────────────────────────────────

/** One Connect envelope: flags byte + 4-byte big-endian length + payload. */
export function encodeCursorFrame(payload: Uint8Array, flags = 0): Uint8Array {
  const out = new Uint8Array(5 + payload.length);
  const view = new DataView(out.buffer);
  view.setUint8(0, flags);
  view.setUint32(1, payload.length, false);
  out.set(payload, 5);
  return out;
}

// ─── CLI discovery and credentials ──────────────────────────────────────────

/** Location of the `cursor-agent` CLI, or "" when it is not installed. */
export function cursorExecutable(): string {
  const names = ["cursor-agent", "agent"];
  for (const name of names) {
    const found = whichSync(name);
    if (!found) continue;
    if (name === "cursor-agent" || isCursorAgent(found)) return found;
  }
  const home = homedir();
  for (const path of [
    join(home, ".local", "bin", "cursor-agent"),
    "/usr/local/bin/cursor-agent",
    "/opt/homebrew/bin/cursor-agent",
  ]) {
    if (existsSync(path) && !isDirectory(path)) return path;
  }
  return "";
}

function whichSync(name: string): string | undefined {
  for (const dir of (process.env.PATH ?? "").split(":")) {
    if (dir.length === 0) continue;
    const candidate = join(dir, name);
    if (isFile(candidate)) return candidate;
  }
  return undefined;
}

function isFile(path: string): boolean {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

function isDirectory(path: string): boolean {
  try {
    return statSync(path).isDirectory();
  } catch {
    return false;
  }
}

function isCursorAgent(path: string): boolean {
  try {
    return realpathSync(path).includes("cursor-agent");
  } catch {
    return false;
  }
}

/** Where `cursor-agent` keeps its sign-in away from a Mac's keychain. */
export function cursorAuthPath(login?: CursorLogin): string {
  const override = process.env.JEVONIAN_CURSOR_AUTH;
  if (override && override.trim().length > 0) return override.trim();
  if (login?.credentialsPath) return login.credentialsPath;
  const home = homedir();
  if (process.platform === "win32") {
    const dir =
      login?.home ?? join(process.env.APPDATA ?? join(home, "AppData", "Roaming"), "Cursor");
    return join(dir, "auth.json");
  }
  if (process.platform === "darwin") return join(login?.home ?? join(home, ".cursor"), "auth.json");
  const dir = login?.home ?? join(process.env.XDG_CONFIG_HOME ?? join(home, ".config"), "cursor");
  return join(dir, "auth.json");
}

async function readKeychainToken(login?: CursorLogin): Promise<string | undefined> {
  if (process.platform !== "darwin") return undefined;
  try {
    const { stdout } = await execFileAsync("security", [
      "find-generic-password",
      "-s",
      login?.keychainService ?? "cursor-access-token",
      "-a",
      login?.keychainAccount ?? "cursor-user",
      "-w",
    ]);
    const token = stdout.trim();
    return token.length > 0 ? token : undefined;
  } catch {
    return undefined;
  }
}

function readAuthFileToken(login?: CursorLogin): string | undefined {
  const path = cursorAuthPath(login);
  if (!existsSync(path)) return undefined;
  try {
    const raw = JSON.parse(readFileSync(path, "utf8")) as { accessToken?: unknown };
    return typeof raw.accessToken === "string" && raw.accessToken.length > 0
      ? raw.accessToken
      : undefined;
  } catch {
    return undefined;
  }
}

/** The access token `cursor-agent` signed in with, or "". */
export async function readCursorToken(login?: CursorLogin): Promise<string> {
  if (process.platform === "darwin") {
    const keychain = await readKeychainToken(login);
    if (keychain) return keychain;
  }
  return readAuthFileToken(login) ?? "";
}

/** Expiry of a JWT, or undefined when it does not say. */
function tokenExpiry(token: string): number | undefined {
  const part = token.split(".")[1];
  if (!part) return undefined;
  try {
    const payload = JSON.parse(Buffer.from(part, "base64url").toString("utf8")) as {
      exp?: unknown;
    };
    return typeof payload.exp === "number" ? payload.exp * 1000 : undefined;
  } catch {
    return undefined;
  }
}

function tokenFresh(token: string): boolean {
  const expiry = tokenExpiry(token);
  return token !== "" && (expiry === undefined || expiry - Date.now() > REFRESH_SKEW_MS);
}

/**
 * Identifies a sign-in for the renewal map. Two Cursor accounts must not share one
 * `cursor-agent status` (it renews whichever account the CLI is signed into, and the re-read
 * that follows would hand one account the other's token). Mirrors the key `oauth.ts` uses for
 * its token cache, kept local so this wire module need not import the oauth it is read by.
 */
function loginKey(login?: CursorLogin): string {
  if (!login) return "";
  return [
    login.home ?? "",
    login.credentialsPath ?? "",
    login.keychainService ?? "",
    login.keychainAccount ?? "",
  ].join("\u0000");
}

/** In-flight renewals, one per sign-in, so concurrent callers share rather than race. */
const renewals = new Map<string, Promise<string>>();

/**
 * The token to call Cursor's API with. A token about to run out is renewed by
 * `cursor-agent status`, which renews whenever it runs.
 *
 * Concurrent callers for the same sign-in share one renewal — but only that sign-in: an account
 * whose renewal is already running must not be handed to a different account's caller.
 */
export async function cursorToken(login?: CursorLogin): Promise<string> {
  const token = await readCursorToken(login);
  if (tokenFresh(token)) return token;
  const path = cursorExecutable();
  if (path !== "") {
    const key = loginKey(login);
    const renewal =
      renewals.get(key) ??
      (() => {
        const task = (async () => {
          try {
            await execFileAsync(path, ["status"], { timeout: STATUS_TIMEOUT_MS });
          } catch {
            // A failed status still often renews; the re-read below decides.
          }
          return readCursorToken(login);
        })();
        renewals.set(key, task);
        void task.finally(() => renewals.delete(key));
        return task;
      })();
    const renewed = await renewal;
    if (renewed !== "") return renewed;
  }
  const latest = await readCursorToken(login);
  if (latest === "") {
    throw new Error(
      "Cursor isn't signed in; run `cursor-agent login`, or point JEVONIAN_CURSOR_AUTH at an auth.json.",
    );
  }
  const expiry = tokenExpiry(latest);
  if (expiry !== undefined && expiry - Date.now() <= 0) {
    throw new Error("Cursor's sign-in has run out; run `cursor-agent login` to sign in again.");
  }
  return latest;
}

/** The CLI version the API is told it is talking to, as `cli-<version>`. */
export function cursorClientVersion(): string {
  let version = "";
  const path = cursorExecutable();
  if (path !== "") {
    const parts = path.split("/");
    const candidate = parts[parts.length - 2] ?? "";
    if (VERSION_PATTERN.test(candidate)) version = candidate;
  }
  if (version === "") {
    const dirs = [join(homedir(), ".local", "share", "cursor-agent", "versions")];
    if (process.platform === "win32") {
      const local = process.env.LOCALAPPDATA;
      if (local) dirs.push(join(local, "cursor-agent", "versions"));
    }
    for (const dir of dirs) {
      for (const entry of readDirNames(dir)) {
        if (VERSION_PATTERN.test(entry) && entry > version) version = entry;
      }
    }
  }
  return `cli-${version === "" ? DEFAULT_CLIENT_VERSION : version}`;
}

function readDirNames(path: string): string[] {
  try {
    return readdirSync(path, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => entry.name);
  } catch {
    return [];
  }
}

// ─── account status ─────────────────────────────────────────────────────────

export interface CursorAccount {
  user: string;
  plan: string;
}

function ansiStrip(text: string): string {
  // eslint-disable-next-line no-control-regex -- matching ESC requires a control character
  return text.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");
}

/** Reads `cursor-agent about --format json`: the email and plan. */
export function parseCursorAbout(out: string): { user: string; plan: string } | undefined {
  let text = ansiStrip(out).trim();
  const brace = text.indexOf("{");
  if (brace > 0) text = text.slice(brace);
  try {
    const about = JSON.parse(text) as { subscriptionTier?: unknown; userEmail?: unknown };
    const user = typeof about.userEmail === "string" ? about.userEmail.trim() : "";
    const plan = typeof about.subscriptionTier === "string" ? about.subscriptionTier.trim() : "";
    return user.length > 0 ? { user, plan } : undefined;
  } catch {
    return undefined;
  }
}

/** Who Cursor's CLI says is signed in, or undefined. */
export async function cursorAccount(): Promise<CursorAccount | undefined> {
  const path = cursorExecutable();
  if (path === "") return undefined;
  try {
    const { stdout } = await execFileAsync(path, ["about", "--format", "json"], {
      timeout: 10_000,
    });
    return parseCursorAbout(stdout);
  } catch {
    return undefined;
  }
}

// ─── models ─────────────────────────────────────────────────────────────────

export interface CursorModel {
  id: string;
  name: string;
  context: number;
}

const CURSOR_MODEL_LINE = /^([A-Za-z0-9][\w.:-]*) - (.+)$/;

/** Parses `cursor-agent models` ("id - Name", one a line). */
export function parseCursorModels(out: string): CursorModel[] {
  const models: CursorModel[] = [];
  for (const raw of ansiStrip(out).split(/\r?\n/)) {
    const match = CURSOR_MODEL_LINE.exec(raw.trim());
    if (!match?.[1] || !match[2]) continue;
    // Names come with zero-width spaces and doubled ones.
    const name = match[2]
      .replace(/\u200b/g, "")
      .split(/\s+/)
      .filter(Boolean)
      .join(" ")
      .trim()
      .replace(/\s*\((?:default|current)\)$/i, "")
      .trim();
    models.push({ id: match[1], name, context: cursorContext(match[1], name) });
  }
  return models;
}

const CURSOR_DEFAULT_CONTEXT = 200_000;

/** Context Cursor lets a model hold, read from its name ("Claude Opus 5.5 1M"). */
export function cursorContext(id: string, name: string): number {
  const million = /\b(\d+)M\b/.exec(name);
  if (million?.[1]) return Number(million[1]) * 1_000_000;
  let base = id.replace(/^cursor-/, "");
  for (;;) {
    const stripped = base.replace(
      /-(?:fast|none|low|medium|high|xhigh|extra-high|max|thinking)$/,
      "",
    );
    if (stripped === base) break;
    base = stripped;
  }
  if (base === "auto") return CURSOR_DEFAULT_CONTEXT;
  return CURSOR_DEFAULT_CONTEXT;
}

export async function fetchCursorModels(): Promise<CursorModel[]> {
  const path = cursorExecutable();
  if (path === "") throw new Error("cursor-agent is not installed");
  const { stdout } = await execFileAsync(path, ["models"], { timeout: 30_000 });
  return parseCursorModels(stdout);
}

// ─── model families (effort / fast / thinking variants) ──────────────────────

const CURSOR_EFFORTS: Array<{ word: string; level: string; label: string }> = [
  { word: "extra-high", level: "xhigh", label: "Extra High" },
  { word: "xhigh", level: "xhigh", label: "Extra High" },
  { word: "minimal", level: "minimal", label: "Minimal" },
  { word: "none", level: "none", label: "None" },
  { word: "low", level: "low", label: "Low" },
  { word: "medium", level: "medium", label: "Medium" },
  { word: "high", level: "high", label: "High" },
  { word: "max", level: "max", label: "Max" },
];

/** Splits a Cursor id into its family and its effort ("" when none is named). */
export function splitCursorId(id: string): { family: string; effort: string } {
  let s = id;
  let fast = false;
  let thinking = false;
  if (s.endsWith("-fast") && s.length > "-fast".length) {
    s = s.slice(0, -"-fast".length);
    fast = true;
  }
  if (s.endsWith("-thinking") && s.length > "-thinking".length) {
    s = s.slice(0, -"-thinking".length);
    thinking = true;
  }
  let effort = "";
  for (const entry of CURSOR_EFFORTS) {
    const suffix = `-${entry.word}`;
    if (s.endsWith(suffix) && s.length > suffix.length) {
      s = s.slice(0, -suffix.length);
      effort = entry.level;
      break;
    }
  }
  if (!thinking && s.endsWith("-thinking") && s.length > "-thinking".length) {
    s = s.slice(0, -"-thinking".length);
    thinking = true;
  }
  if (thinking) s += "-thinking";
  if (fast) s += "-fast";
  return { family: s, effort };
}

function cursorEffortLabel(level: string): string {
  return CURSOR_EFFORTS.find((entry) => entry.level === level)?.label ?? "";
}

/** `name` with the first run of `words` taken out, and whether it had them. */
function withoutWords(name: string, words: string): { name: string; said: boolean } {
  const ns = name.split(/\s+/).filter(Boolean);
  const ws = words.split(/\s+/).filter(Boolean);
  if (ws.length === 0) return { name, said: false };
  for (let i = 0; i + ws.length <= ns.length; i += 1) {
    if (ws.every((word, k) => ns[i + k] === word)) {
      return { name: [...ns.slice(0, i), ...ns.slice(i + ws.length)].join(" "), said: true };
    }
  }
  return { name, said: false };
}

interface CursorFamily {
  id: string;
  variants: CursorModel[];
  efforts: string[];
}

function cursorFamilies(raw: CursorModel[]): CursorFamily[] {
  const out: CursorFamily[] = [];
  const by = new Map<string, CursorFamily>();
  for (const model of raw) {
    const { family, effort } = splitCursorId(model.id);
    let entry = by.get(family);
    if (!entry) {
      entry = { id: family, variants: [], efforts: [] };
      by.set(family, entry);
      out.push(entry);
    }
    entry.variants.push(model);
    entry.efforts.push(effort);
  }
  return out;
}

/** The id for each effort, "" being the one Cursor picks by default. */
function familyByEffort(family: CursorFamily): Record<string, string> {
  const out: Record<string, string> = {};
  let def = family.efforts.indexOf("");
  for (let i = 0; i < family.variants.length; i += 1) {
    const effort = family.efforts[i] as string;
    const variant = family.variants[i] as CursorModel;
    if (out[effort] === undefined) out[effort] = variant.id;
    if (def < 0 && effort !== "") {
      if (!withoutWords(variant.name, cursorEffortLabel(effort)).said) def = i;
    }
  }
  if (def < 0) def = Math.max(0, family.efforts.indexOf("medium"));
  out[""] = (family.variants[def] as CursorModel).id;
  return out;
}

/** Collapses each family to one model: named as its default, with the efforts it has. */
export function collapseCursorModels(raw: CursorModel[]): CursorModel[] {
  const out: CursorModel[] = [];
  for (const family of cursorFamilies(raw)) {
    if (family.variants.length === 1) {
      out.push(family.variants[0] as CursorModel);
      continue;
    }
    const def = familyByEffort(family)[""];
    let name = family.id;
    let context = 0;
    for (let i = 0; i < family.variants.length; i += 1) {
      const variant = family.variants[i] as CursorModel;
      const effort = family.efforts[i] as string;
      if (variant.id === def) name = withoutWords(variant.name, cursorEffortLabel(effort)).name;
      if (variant.context > 0 && (context === 0 || variant.context < context)) {
        context = variant.context;
      }
    }
    out.push({ id: family.id, name, context: context || CURSOR_DEFAULT_CONTEXT });
  }
  return out;
}

// ─── errors ─────────────────────────────────────────────────────────────────

export type CursorErrorKind =
  | "auth"
  | "region"
  | "quota"
  | "rate_limit"
  | "context"
  | "invalid"
  | "capacity"
  | "other";

export interface CursorStreamError {
  status: number;
  kind: CursorErrorKind;
  message: string;
}

const KIND_STATUS: Record<CursorErrorKind, number> = {
  auth: 401,
  region: 403,
  quota: 429,
  rate_limit: 429,
  context: 400,
  invalid: 400,
  capacity: 503,
  other: 502,
};

/**
 * Maps a Connect failure (`{code, message, details}` or a stream trailer's `{error}`) to a
 * routing-relevant kind and the status to surface.
 */
export function classifyCursorError(status: number, body: string): CursorStreamError {
  const parsed = parseFailure(body);
  let message = parsed.message;
  if (parsed.title || parsed.detail) {
    message = `${parsed.title ? `${parsed.title}: ` : ""}${parsed.detail}`.replace(/^:\s*/, "");
  }
  if (message === "" || message === "Error") message = parsed.code;
  const lower = message.toLowerCase();
  if (message === "" && status / 100 === 2) message = "Cursor returned an empty reply";
  if (message === "")
    message = status > 0 ? `Cursor upstream error (HTTP ${status})` : "Cursor upstream error";

  const kind = classifyKind(status, parsed.code, lower);
  return { status: KIND_STATUS[kind], kind, message: message.slice(0, 2000) };
}

function parseFailure(body: string): {
  code: string;
  message: string;
  title: string;
  detail: string;
} {
  const trimmed = body.trim();
  if (trimmed === "") return { code: "", message: "", title: "", detail: "" };
  try {
    const parsed = JSON.parse(trimmed) as Record<string, unknown>;
    const inner =
      parsed.error && typeof parsed.error === "object"
        ? (parsed.error as Record<string, unknown>)
        : parsed;
    const code = typeof inner.code === "string" ? inner.code.toLowerCase() : "";
    let message = typeof inner.message === "string" ? inner.message : "";
    let title = "";
    let detail = "";
    const details = Array.isArray(inner.details) ? inner.details : [];
    for (const entry of details) {
      const debug =
        entry && typeof entry === "object"
          ? ((entry as Record<string, unknown>).debug as Record<string, unknown> | undefined)
          : undefined;
      const inner2 =
        debug && typeof debug.details === "object"
          ? (debug.details as Record<string, unknown>)
          : undefined;
      if (!inner2) continue;
      if (typeof inner2.title === "string") title = inner2.title.trim();
      if (typeof inner2.detail === "string") detail = inner2.detail.trim();
    }
    if (message === "" && typeof parsed.error === "string") message = parsed.error;
    return { code, message, title, detail };
  } catch {
    return { code: "", message: trimmed, title: "", detail: "" };
  }
}

function classifyKind(status: number, code: string, lower: string): CursorErrorKind {
  if (lower.includes("region")) return "region";
  if (code === "permission_denied") return "auth";
  if (
    code === "unauthenticated" ||
    status === 401 ||
    lower.includes("expired") ||
    /sign in|signed out/.test(lower)
  ) {
    return "auth";
  }
  if (
    code === "resource_exhausted" ||
    lower.includes("quota") ||
    lower.includes("rate limit") ||
    lower.includes("usage limit")
  ) {
    return "quota";
  }
  if (
    lower.includes("too long") ||
    lower.includes("context length") ||
    lower.includes("too many tokens")
  ) {
    return "context";
  }
  if (code === "invalid_argument") return "invalid";
  if (
    code === "unavailable" ||
    status === 503 ||
    /overloaded|try again later|at capacity|unavailable/.test(lower)
  ) {
    return "capacity";
  }
  return "other";
}

// ─── conversation encoding ──────────────────────────────────────────────────

export interface CursorToolDef {
  name: string;
  description: string;
  inputSchema: string;
}

export interface CursorPart {
  kind: "text" | "tool-call" | "tool-result" | "image" | "file";
  text?: string;
  id?: string;
  name?: string;
  args?: string;
  callId?: string;
  isError?: boolean;
  data?: string;
  mediaType?: string;
}

export interface CursorMessage {
  role: string;
  parts: CursorPart[];
}

/** The conversation as AI SDK messages, in JSON, plus the blobs they are named by. */
export function cursorMessages(
  system: string,
  messages: CursorMessage[],
  tools: CursorToolDef[],
): { blobs: Uint8Array[]; tools: CursorToolDef[] } {
  const blobs: Uint8Array[] = [];
  const add = (message: Record<string, unknown>): void => {
    blobs.push(utf8.encode(JSON.stringify(message)));
  };
  const catalog = cursorCatalog(tools);
  const fullSystem = `${system}${catalog}`;
  if (fullSystem !== "") add({ role: "system", content: fullSystem });

  let pending: string[] = [];
  const answer = (results: unknown[]): void => {
    const list = [...results];
    for (const id of pending) list.push(cursorResult(id, CURSOR_NO_RESULT, true));
    pending = [];
    if (list.length > 0) add({ role: "tool", content: list });
  };

  for (const message of messages) {
    if (message.role === "assistant") {
      answer([]);
      const content: unknown[] = [];
      for (const part of message.parts) {
        if (part.kind === "text" && part.text) {
          content.push({ type: "text", text: part.text });
        } else if (part.kind === "tool-call") {
          let args: unknown = {};
          if (part.args) {
            try {
              args = JSON.parse(part.args);
            } catch {
              args = {};
            }
          }
          content.push({
            type: "tool-call",
            toolCallId: cursorCallId(part.id ?? ""),
            toolName: CALL_DYNAMIC_TOOL,
            args: {
              namespace: MCP_NAMESPACE,
              toolName: part.name ?? "",
              arguments: args,
            },
          });
          pending.push(part.id ?? "");
        }
      }
      if (content.length > 0) add({ role: "assistant", content });
      continue;
    }
    const results: unknown[] = [];
    const content: unknown[] = [];
    for (const part of message.parts) {
      if (part.kind === "tool-result") {
        const at = pending.indexOf(part.callId ?? "");
        if (at >= 0) {
          pending = [...pending.slice(0, at), ...pending.slice(at + 1)];
          results.push(cursorResult(part.callId ?? "", part.text ?? "", part.isError === true));
        }
      } else if (part.kind === "text" && part.text) {
        content.push({ type: "text", text: part.text });
      } else if (part.kind === "image" && part.data) {
        const bytes = Buffer.from(part.data, "base64");
        content.push({
          type: "image",
          mimeType: part.mediaType ?? "image/png",
          image: { __type: "Uint8Array", hex: bytes.toString("hex") },
        });
      } else if (part.kind === "file" && part.text) {
        content.push({ type: "text", text: part.text });
      }
    }
    answer(results);
    if (content.length > 0) add({ role: "user", content });
  }
  answer([]);
  return { blobs, tools };
}

const CURSOR_NO_RESULT = "Tool use was interrupted and did not produce a result.";

function cursorResult(id: string, text: string, isError: boolean): Record<string, unknown> {
  let result: unknown = text;
  try {
    result = JSON.parse(text);
  } catch {
    result = text;
  }
  const out: Record<string, unknown> = {
    type: "tool-result",
    toolCallId: cursorCallId(id),
    toolName: CALL_DYNAMIC_TOOL,
    result,
    experimental_content: [{ type: "text", text }],
  };
  if (isError) out.isError = true;
  return out;
}

/** Lists the caller's tools for the model, which only sees MCP tools. */
function cursorCatalog(tools: CursorToolDef[]): string {
  if (tools.length === 0) return "";
  const lines = tools
    .map(
      (tool) =>
        `<tool name="${tool.name}">\n${tool.description}\ninput schema: ${tool.inputSchema}\n</tool>`,
    )
    .join("\n");
  return `\n\n<dynamic_tool_catalog>\nThe tools below are available in the MCP namespace "${MCP_NAMESPACE}". Call one with \`${CALL_DYNAMIC_TOOL}\` (namespace "${MCP_NAMESPACE}", toolName, arguments). Their schemas are given here, so there is no need to call \`GetDynamicTools\` first.\n${lines}\n</dynamic_tool_catalog>`;
}

function cursorCallId(id: string): string {
  return id.startsWith("call_") ? id.replace("__fc_", "\nfc_") : id;
}

function toHex(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("hex");
}

/** The URL one Run is POSTed to. */
export function cursorRunUrl(baseUrl: string): string {
  return `${(baseUrl || CURSOR_AGENT_FALLBACK).replace(/\/+$/, "")}${RUN_PATH}`;
}

/** How often the client must tell the server it is still there. */
export const CURSOR_HEARTBEAT_MS = HEARTBEAT_MS;

/** The keep-alive frame the client sends while a Run is open. */
export function cursorHeartbeatFrame(): Uint8Array {
  return new Pb().bytes(7, new Uint8Array()).build();
}

function cursorBlobId(bytes: Uint8Array): Uint8Array {
  return createHash("sha256").update(bytes).digest();
}

function cursorUuid(): string {
  const bytes = randomBytes(16);
  bytes[6] = ((bytes[6] as number) & 0x0f) | 0x40;
  bytes[8] = ((bytes[8] as number) & 0x3f) | 0x80;
  const hex = bytes.toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

// ─── run request ────────────────────────────────────────────────────────────

interface CursorRunRequest {
  body: Uint8Array;
  blobs: Map<string, Uint8Array>;
}

function cursorToolDef(tool: CursorToolDef): Uint8Array {
  const def = new Pb().str(1, tool.name);
  if (tool.description !== "") def.str(2, tool.description);
  try {
    def.bytes(3, pbValue(JSON.parse(tool.inputSchema)));
  } catch {
    // A schema that is not JSON is passed as the raw description string only.
  }
  return def.str(4, MCP_NAMESPACE).str(5, tool.name).str(6, tool.inputSchema).build();
}

/** The Run's opening message, an `AgentClientMessage` with its `run_request`, and its blobs. */
export function buildCursorRun(
  blobs: Uint8Array[],
  lastUser: string,
  tools: CursorToolDef[],
  model: string,
): CursorRunRequest {
  const store = new Map<string, Uint8Array>();
  const put = (bytes: Uint8Array): Uint8Array => {
    const id = cursorBlobId(bytes);
    store.set(toHex(id), bytes);
    return id;
  };

  let state = new Pb();
  for (const blob of blobs) state.bytes(1, put(blob));
  const messageId = cursorUuid();
  const user = new Pb().str(1, lastUser).str(2, messageId).varint(4, 1);
  const turn = new Pb().bytes(1, new Pb().bytes(1, put(user.build())).str(10, messageId).build());
  state = new Pb().bytes(1, state.build()).bytes(8, put(turn.build())).varint(10, 1).str(22, "cli");

  const defs = tools.map(cursorToolDef);
  const env = new Pb()
    .str(1, process.platform)
    .str(2, process.env.TMPDIR ?? "/tmp")
    .str(10, "UTC");
  let requestContext = new Pb().bytes(4, env.build());
  let mcp = new Pb();
  for (const def of defs) {
    requestContext = requestContext.bytes(7, def);
    mcp = mcp.bytes(1, def);
  }
  const action = new Pb().bytes(2, new Pb().bytes(2, requestContext.build()).build());
  const runRequest = new Pb()
    .bytes(1, state.build())
    .bytes(2, action.build())
    .bytes(3, new Pb().str(1, model).str(3, model).str(4, model).build())
    .bytes(4, mcp.build())
    .str(5, cursorUuid())
    .bytes(9, new Pb().str(1, model).build())
    .varint(19, 1);

  return { body: new Pb().bytes(1, runRequest.build()).build(), blobs: store };
}

export function cursorHeaders(
  token: string,
  version: string,
  requestId: string,
): Record<string, string> {
  return {
    "content-type": "application/connect+proto",
    "connect-protocol-version": "1",
    authorization: `Bearer ${token}`,
    "x-cursor-client-version": version,
    "x-cursor-client-type": "cli",
    "x-ghost-mode": "true",
    "x-request-id": requestId,
    "x-cursor-agent-allowed-tools": "mcp_tool_call,get_mcp_tools_tool_call",
  };
}

/** The agent API to run on, as Cursor's server config names it, or the global fallback. */
export async function resolveCursorAgentUrl(token: string, baseUrl?: string): Promise<string> {
  const root = (baseUrl || CURSOR_DEFAULT_BASE_URL).replace(/\/+$/, "");
  try {
    const response = await fetch(`${root}${SERVER_CONFIG_PATH}`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "connect-protocol-version": "1",
        authorization: `Bearer ${token}`,
        "x-cursor-client-version": cursorClientVersion(),
        "x-cursor-client-type": "cli",
        "x-ghost-mode": "true",
      },
      body: "{}",
    });
    if (!response.ok) return CURSOR_AGENT_FALLBACK;
    const config = (await response.json()) as {
      agentUrlConfig?: { agentUrl?: unknown; agentnUrl?: unknown };
    };
    for (const raw of [config.agentUrlConfig?.agentUrl, config.agentUrlConfig?.agentnUrl]) {
      if (typeof raw !== "string") continue;
      try {
        const url = new URL(raw);
        if ((url.protocol === "https:" || url.protocol === "http:") && url.host !== "") {
          return raw.replace(/\/+$/, "");
        }
      } catch {
        // keep looking
      }
    }
  } catch {
    // fall through to the global agent API
  }
  return CURSOR_AGENT_FALLBACK;
}

// ─── stream decoding ────────────────────────────────────────────────────────

export type CursorEvent =
  | { type: "text"; text: string }
  | { type: "thinking"; text: string }
  | { type: "tool"; id: string; name: string; args: string }
  | { type: "usage"; usage: Usage }
  | { type: "error"; error: CursorStreamError }
  | { type: "stop"; toolCalls: boolean };

/** What the Run needs to answer while the server reads the conversation. */
export interface CursorWriter {
  send(frame: Uint8Array): void;
  /** A blob the server asked for, or undefined when it is not one of ours. */
  blob(id: string): Uint8Array | undefined;
}

/**
 * Decodes one Run's server frames. The server may ask the client for a blob
 * (`kv_server_message`) or hand it an MCP tool call to run (`exec_server_message`); both are
 * answered through `writer`.
 */
export class CursorRunDecoder {
  private readonly splitter = new CursorFrameSplitter();
  private calls = 0;
  private listed = 0;
  private said = 0;
  private stopped = false;

  private readonly writer: CursorWriter;

  constructor(writer: CursorWriter) {
    this.writer = writer;
  }

  push(chunk: Uint8Array): CursorEvent[] {
    const events: CursorEvent[] = [];
    for (const frame of this.splitter.push(chunk)) {
      if (frame.end) {
        this.finish(events, frame.payload);
        return events;
      }
      this.applyFrame(frame.payload, events);
      if (this.stopped) return events;
    }
    return events;
  }

  /** Emits the terminal stop event when the transport closed without an end-stream frame. */
  finish(events: CursorEvent[], payload?: Uint8Array): void {
    if (this.stopped) return;
    if (payload) {
      const text = textDecoder.decode(payload).trim();
      if (text !== "" && text !== "{}") {
        const error = classifyCursorError(200, text);
        if (error.status / 100 !== 2) {
          events.push({ type: "error", error });
          this.stopped = true;
          return;
        }
      }
    }
    if (this.said === 0 && this.calls === 0) {
      events.push({
        type: "error",
        error: { status: 502, kind: "other", message: "Cursor returned an empty reply" },
      });
      this.stopped = true;
      return;
    }
    events.push({ type: "stop", toolCalls: this.calls > 0 });
    this.stopped = true;
  }

  private applyFrame(payload: Uint8Array, events: CursorEvent[]): void {
    let fields: PbField[];
    try {
      fields = pbFields(payload);
    } catch {
      return;
    }
    for (const field of fields) {
      if (field.num === 1 && field.data) this.applyInteraction(field.data, events);
      else if (field.num === 2 && field.data) this.applyExec(field.data, events);
      else if (field.num === 4 && field.data) this.applyKv(field.data);
      if (this.stopped) return;
    }
  }

  private applyInteraction(payload: Uint8Array, events: CursorEvent[]): void {
    for (const update of pbFields(payload)) {
      const fields = pbFields(update.data ?? new Uint8Array());
      if (update.num === 1) {
        const text = pbStr(fields, 1);
        if (text) {
          this.said += text.length;
          events.push({ type: "text", text });
        }
      } else if (update.num === 4) {
        const text = pbStr(fields, 1);
        if (text) {
          this.said += text.length;
          events.push({ type: "thinking", text });
        }
      } else if (update.num === 14) {
        events.push({
          type: "usage",
          usage: {
            input: pbNum(fields, 1) ?? 0,
            output: pbNum(fields, 2) ?? 0,
            cacheRead: pbNum(fields, 3) ?? 0,
            cacheWrite: pbNum(fields, 4) ?? 0,
          },
        });
      } else if (update.num === 27) {
        this.listed = pbNum(fields, 1) ?? 0;
      }
    }
  }

  private applyExec(payload: Uint8Array, events: CursorEvent[]): void {
    const fields = pbFields(payload);
    const id = pbNum(fields, 1) ?? 0;
    const execId = pbStr(fields, 15) ?? "";
    const answer = (num: number, result: Uint8Array): void => {
      this.writer.send(
        new Pb()
          .bytes(2, new Pb().varint(1, id).str(15, execId).bytes(num, result).build())
          .build(),
      );
      this.closeExec(id);
    };
    for (const field of fields) {
      if (field.num === 11 && field.data) {
        const entryFields = pbFields(field.data);
        const argMap: Record<string, unknown> = {};
        for (const kv of entryFields) {
          if (kv.num !== 2 || !kv.data) continue;
          const entry = pbFields(kv.data);
          const key = pbStr(entry, 1) ?? "";
          const value = entry.find((f) => f.num === 2);
          argMap[key] = value?.data ? pbAny(value.data) : null;
        }
        let name = pbStr(entryFields, 5) ?? "";
        if (name === "") name = (pbStr(entryFields, 1) ?? "").replace(/^magpie-/, "");
        // An OpenAI model's id is its call's and its item's, a line apart, which no caller
        // would take as an id: the line goes as __.
        let callId = (pbStr(entryFields, 3) ?? "").replace(/\n/g, "__");
        if (callId === "") callId = `call_${randomBytes(12).toString("hex")}`;
        this.calls += 1;
        this.said += JSON.stringify(argMap).length;
        events.push({ type: "tool", id: callId, name, args: JSON.stringify(argMap) });
        if (this.listed > 0 && this.calls >= this.listed) this.finish(events);
        return;
      }
      if (field.num === 36) {
        const server = new Pb().str(1, MCP_NAMESPACE).str(2, MCP_NAMESPACE).str(7, "connected");
        answer(36, new Pb().bytes(1, new Pb().bytes(1, server.build()).build()).build());
        return;
      }
      if (field.num === 10) {
        const env = new Pb()
          .str(1, process.platform)
          .str(2, process.env.TMPDIR ?? "/tmp")
          .str(10, "UTC");
        answer(
          10,
          new Pb()
            .bytes(1, new Pb().bytes(1, new Pb().bytes(4, env.build()).build()).build())
            .build(),
        );
        return;
      }
    }
    // Anything else the server asks of the client (a shell, a file read) is refused.
    this.writer.send(
      new Pb()
        .bytes(5, new Pb().bytes(2, new Pb().varint(1, id).str(2, "not available").build()).build())
        .build(),
    );
    this.closeExec(id);
  }

  private applyKv(payload: Uint8Array): void {
    const fields = pbFields(payload);
    const id = pbNum(fields, 1) ?? 0;
    for (const field of fields) {
      if (field.num === 2 && field.data) {
        const asked = pbStr(pbFields(field.data), 1) ?? "";
        const blob = this.writer.blob(asked);
        const result = blob
          ? new Pb().bytes(1, blob).build()
          : new Pb().bytes(2, new Pb().str(1, "blob not found").build()).build();
        this.writer.send(
          new Pb().bytes(3, new Pb().varint(1, id).bytes(2, result).build()).build(),
        );
      } else if (field.num === 3) {
        this.writer.send(
          new Pb().bytes(3, new Pb().varint(1, id).bytes(3, new Uint8Array()).build()).build(),
        );
      }
    }
  }

  private closeExec(id: number): void {
    this.writer.send(
      new Pb().bytes(5, new Pb().bytes(1, new Pb().varint(1, id).build()).build()).build(),
    );
  }
}

interface CursorFrame {
  end: boolean;
  payload: Uint8Array;
}

/** Incremental Connect envelope splitter. */
class CursorFrameSplitter {
  private buffer: Uint8Array = new Uint8Array(0);

  push(chunk: Uint8Array): CursorFrame[] {
    this.buffer = this.buffer.length === 0 ? chunk : concat([this.buffer, chunk]);
    const frames: CursorFrame[] = [];
    let offset = 0;
    while (this.buffer.length - offset >= 5) {
      const view = new DataView(this.buffer.buffer, this.buffer.byteOffset + offset, 5);
      const flags = view.getUint8(0);
      const length = view.getUint32(1, false);
      if (length > MAX_FRAME_BYTES) throw new Error("Cursor frame exceeds the size limit");
      if (this.buffer.length - offset < 5 + length) break;
      frames.push({
        end: (flags & 0x02) !== 0,
        payload: this.buffer.slice(offset + 5, offset + 5 + length),
      });
      offset += 5 + length;
    }
    this.buffer = offset === 0 ? this.buffer : this.buffer.slice(offset);
    return frames;
  }
}

// ─── stream adapters ────────────────────────────────────────────────────────

function completionId(): string {
  return `chatcmpl-${randomBytes(12).toString("hex")}`;
}

function openaiUsage(usage: Usage): Record<string, unknown> {
  const prompt = usage.input + usage.cacheWrite + usage.cacheRead;
  return {
    prompt_tokens: prompt,
    completion_tokens: usage.output,
    total_tokens: prompt + usage.output,
    prompt_tokens_details: { cached_tokens: usage.cacheRead },
  };
}

/** Keeps the caller's system prompt clean of signatures upstream may refuse. */
export function sanitizeCursorSystemPrompt(text: string): string {
  return sanitizeBuiltinPrompt(text);
}

export interface CursorRunOptions {
  token: string;
  /** The agent API to run on, as {@link resolveCursorAgentUrl} resolved it. */
  agentUrl: string;
  systemPrompt: string;
  messages: CursorMessage[];
  tools: CursorToolDef[];
  model: string;
  lastUser: string;
  requestId: string;
}

export interface CursorRun {
  events: AsyncIterable<CursorEvent>;
  usage: Usage;
  toolCalls: boolean;
  error?: CursorStreamError;
}

/**
 * One Run against Cursor's agent API. The request is a Connect stream that stays open while
 * the server reads the conversation: blobs are served on demand, MCP tool calls are handed
 * back, and a heartbeat keeps the stream alive. Returns once the turn ends or every tool call
 * has been made.
 */
export async function runCursor(options: CursorRunOptions): Promise<CursorRun> {
  const { blobs } = cursorMessages(options.systemPrompt, options.messages, options.tools);
  const run = buildCursorRun(blobs, options.lastUser, options.tools, options.model);

  let controllerRef: ReadableStreamDefaultController<Uint8Array> | undefined;
  let closed = false;
  const send = (frame: Uint8Array): void => {
    if (closed || !controllerRef) return;
    try {
      controllerRef.enqueue(frame);
    } catch {
      closed = true;
    }
  };
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controllerRef = controller;
      controller.enqueue(run.body);
    },
    cancel() {
      closed = true;
    },
  });

  const decoder = new CursorRunDecoder({
    send,
    blob: (id) => run.blobs.get(id),
  });

  const timer = setInterval(() => send(cursorHeartbeatFrame()), CURSOR_HEARTBEAT_MS);
  let response: Response;
  try {
    response = await fetch(cursorRunUrl(options.agentUrl), {
      method: "POST",
      headers: cursorHeaders(options.token, cursorClientVersion(), options.requestId),
      body,
      // A half-duplex request body is what lets the stream stay open while we read.
      duplex: "half",
    } as RequestInit & { duplex: "half" });
  } finally {
    clearInterval(timer);
  }

  if (!response.ok) {
    const text = await response.text().catch(() => "");
    try {
      controllerRef?.close();
    } catch {
      // already closed
    }
    return {
      events: emptyEvents(),
      usage: emptyUsage(),
      toolCalls: false,
      error: classifyCursorError(response.status, text),
    };
  }
  if (!response.body) {
    return {
      events: emptyEvents(),
      usage: emptyUsage(),
      toolCalls: false,
      error: { status: 502, kind: "other", message: "Cursor returned an empty response body" },
    };
  }

  const source = response.body;
  async function* events(): AsyncGenerator<CursorEvent> {
    const reader = source.getReader();
    const queue: CursorEvent[] = [];
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        queue.push(...decoder.push(value));
        while (queue.length > 0) yield queue.shift() as CursorEvent;
      }
      decoder.finish(queue);
      while (queue.length > 0) yield queue.shift() as CursorEvent;
    } finally {
      closed = true;
      clearInterval(timer);
      await reader.cancel().catch(() => undefined);
    }
  }

  return { events: events(), usage: emptyUsage(), toolCalls: false };
}

function emptyUsage(): Usage {
  return { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
}

// eslint-disable-next-line require-yield -- an empty stream is the point
async function* emptyEvents(): AsyncGenerator<CursorEvent> {
  return;
}

/** The last user message's text, which the Run is named by. */
export function cursorLastUser(messages: CursorMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const message = messages[i] as CursorMessage;
    if (message.role !== "user") continue;
    for (const part of message.parts) {
      if (part.kind === "text" && part.text) return part.text;
    }
  }
  return ".";
}

export interface CursorFinish {
  usage: Usage;
  toolCalls: boolean;
  error?: CursorStreamError;
}

/**
 * Collects a Run into a non-streaming `chat.completion`. An error event is reported on
 * `finish.error`; the completion then holds whatever partial output arrived.
 */
export async function cursorChatCompletion(
  model: string,
  events: AsyncIterable<CursorEvent>,
): Promise<{ completion: Record<string, unknown>; finish: CursorFinish }> {
  let content = "";
  let reasoning = "";
  let usage: Usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  let error: CursorStreamError | undefined;
  const calls: Array<{ id: string; name: string; args: string }> = [];
  try {
    for await (const event of events) {
      if (event.type === "text") content += event.text;
      else if (event.type === "thinking") reasoning += event.text;
      else if (event.type === "tool")
        calls.push({ id: event.id, name: event.name, args: event.args });
      else if (event.type === "usage") usage = event.usage;
      else if (event.type === "error") error = event.error;
    }
  } catch (thrown) {
    error ??= {
      status: 502,
      kind: "other",
      message: thrown instanceof Error ? thrown.message : String(thrown),
    };
  }
  const message: Record<string, unknown> = {
    role: "assistant",
    content: content.length > 0 ? content : null,
  };
  if (reasoning.length > 0) message.reasoning_content = reasoning;
  if (calls.length > 0) {
    message.tool_calls = calls.map((call) => ({
      id: call.id,
      type: "function",
      function: { name: call.name, arguments: call.args },
    }));
  }
  const completion: Record<string, unknown> = {
    id: completionId(),
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message, finish_reason: calls.length > 0 ? "tool_calls" : "stop" }],
    usage: openaiUsage(usage),
  };
  return {
    completion,
    finish: { usage, toolCalls: calls.length > 0, ...(error ? { error } : {}) },
  };
}

function chatBodyToCursor(body: Record<string, unknown>): {
  system: string;
  messages: CursorMessage[];
  tools: CursorToolDef[];
} {
  const messages = Array.isArray(body.messages) ? body.messages : [];
  const { system, messages: parts } = chatParts(messages);
  const tools: CursorToolDef[] = [];
  for (const raw of Array.isArray(body.tools) ? body.tools : []) {
    if (typeof raw !== "object" || raw === null) continue;
    const tool = raw as Record<string, unknown>;
    const fn =
      typeof tool.function === "object" && tool.function !== null
        ? (tool.function as Record<string, unknown>)
        : {};
    const name = typeof fn.name === "string" ? fn.name : "";
    if (name.length === 0) continue;
    tools.push({
      name,
      description: typeof fn.description === "string" ? fn.description : "",
      inputSchema: JSON.stringify(fn.parameters ?? { type: "object", properties: {} }),
    });
  }
  return { system, messages: parts, tools };
}

/** The Chat Completions body as the conversation Cursor's Run expects. */
export function cursorConversation(body: Record<string, unknown>): {
  system: string;
  messages: CursorMessage[];
  tools: CursorToolDef[];
} {
  return chatBodyToCursor(body);
}

function chatParts(messages: unknown[]): { system: string; messages: CursorMessage[] } {
  const out: CursorMessage[] = [];
  const system: string[] = [];
  for (const raw of messages) {
    if (typeof raw !== "object" || raw === null) continue;
    const message = raw as Record<string, unknown>;
    const role = typeof message.role === "string" ? message.role : "user";
    if (role === "system" || role === "developer") {
      const text = chatText(message.content);
      if (text.length > 0) system.push(text);
      continue;
    }
    const parts: CursorPart[] = [];
    if (role === "tool" || role === "function") {
      const callId = typeof message.tool_call_id === "string" ? message.tool_call_id : "";
      const text = chatText(message.content);
      parts.push({
        kind: "tool-result",
        callId,
        text: text.length > 0 ? text : "(no output)",
        isError: message.is_error === true,
      });
      out.push({ role: "user", parts });
      continue;
    }
    if (role === "assistant" && Array.isArray(message.tool_calls)) {
      for (const entry of message.tool_calls) {
        if (typeof entry !== "object" || entry === null) continue;
        const call = entry as Record<string, unknown>;
        const fn =
          typeof call.function === "object" && call.function !== null
            ? (call.function as Record<string, unknown>)
            : {};
        const name = typeof fn.name === "string" ? fn.name : "";
        if (name.length === 0) continue;
        parts.push({
          kind: "tool-call",
          id: typeof call.id === "string" ? call.id : "",
          name,
          args:
            typeof fn.arguments === "string" ? fn.arguments : JSON.stringify(fn.arguments ?? {}),
        });
      }
    }
    const content = message.content;
    if (typeof content === "string") {
      if (content.length > 0) parts.push({ kind: "text", text: content });
    } else if (Array.isArray(content)) {
      for (const rawPart of content) {
        if (typeof rawPart === "string") {
          if (rawPart.length > 0) parts.push({ kind: "text", text: rawPart });
          continue;
        }
        if (typeof rawPart !== "object" || rawPart === null) continue;
        const part = rawPart as Record<string, unknown>;
        if (part.type === "text" && typeof part.text === "string") {
          parts.push({ kind: "text", text: part.text });
        } else if (part.type === "image_url") {
          const rawUrl = part.image_url;
          const url =
            typeof rawUrl === "string"
              ? rawUrl
              : typeof rawUrl === "object" && rawUrl !== null
                ? ((rawUrl as Record<string, unknown>).url as string | undefined)
                : undefined;
          const match = url
            ? /^data:([a-z0-9.+-]+\/[a-z0-9.+-]+);base64,(.+)$/is.exec(url.replace(/\s/g, ""))
            : null;
          if (match?.[1] && match[2]) {
            parts.push({ kind: "image", data: match[2], mediaType: match[1].toLowerCase() });
          }
        }
      }
    }
    if (parts.length > 0) out.push({ role: role === "assistant" ? "assistant" : "user", parts });
  }
  return { system: system.join("\n\n"), messages: out };
}

function chatText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) {
    return content === undefined || content === null ? "" : JSON.stringify(content);
  }
  return content
    .map((part) => {
      if (typeof part === "string") return part;
      if (
        typeof part === "object" &&
        part !== null &&
        typeof (part as { text?: unknown }).text === "string"
      ) {
        return (part as { text: string }).text;
      }
      return "";
    })
    .filter((text) => text.length > 0)
    .join("\n");
}

/**
 * Cursor events -> OpenAI `chat.completion.chunk` SSE. Emits the role chunk first, then
 * content / reasoning_content / tool_calls deltas, and a final chunk carrying finish_reason
 * and usage. An error event is emitted instead of the final chunk.
 */
export function cursorToChatStream(
  model: string,
  events: AsyncIterable<CursorEvent>,
  onFinish?: (finish: CursorFinish) => void,
): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  const id = completionId();
  const created = Math.floor(Date.now() / 1000);
  let usage: Usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  let toolCalls = false;
  let error: CursorStreamError | undefined;
  let reported = false;
  const report = (): void => {
    if (reported) return;
    reported = true;
    onFinish?.({ usage, toolCalls, ...(error ? { error } : {}) });
  };
  const chunk = (delta: Record<string, unknown>, finishReason: string | null) => ({
    id,
    object: "chat.completion.chunk",
    created,
    model,
    choices: [{ index: 0, delta, finish_reason: finishReason }],
  });

  return new ReadableStream<Uint8Array>({
    async start(controller) {
      const emit = (payload: Record<string, unknown>): void => {
        controller.enqueue(encoder.encode(`data: ${JSON.stringify(payload)}\n\n`));
      };
      emit(chunk({ role: "assistant", content: "" }, null));
      let index = 0;
      try {
        for await (const event of events) {
          if (event.type === "text") emit(chunk({ content: event.text }, null));
          else if (event.type === "thinking") emit(chunk({ reasoning_content: event.text }, null));
          else if (event.type === "tool") {
            toolCalls = true;
            emit(
              chunk(
                {
                  tool_calls: [
                    {
                      index: index++,
                      id: event.id,
                      type: "function",
                      function: { name: event.name, arguments: event.args },
                    },
                  ],
                },
                null,
              ),
            );
          } else if (event.type === "usage") {
            usage = event.usage;
          } else if (event.type === "error") {
            error = event.error;
            emit({ error: { message: error.message, type: "upstream_error" } });
          }
        }
        if (!error) {
          emit({ ...chunk({}, toolCalls ? "tool_calls" : "stop"), usage: openaiUsage(usage) });
        }
        controller.enqueue(encoder.encode("data: [DONE]\n\n"));
      } catch (thrown) {
        error ??= {
          status: 502,
          kind: "other",
          message: thrown instanceof Error ? thrown.message : String(thrown),
        };
        emit({ error: { message: error.message, type: "upstream_error" } });
      } finally {
        controller.close();
        report();
      }
    },
  });
}
