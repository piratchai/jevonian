/**
 * WorkBuddy AI (international) subscription — Magpie-parity.
 *
 * Speaks OpenAI Chat Completions at `https://www.workbuddy.ai/v2` under the account's
 * access token, with the same headers WorkBuddy's desktop client sends. Credentials come
 * from a Jevonian browser sign-in (stored under ~/.config/jevonian), or from a plaintext
 * desktop session file when credential protection is off.
 */

import { randomBytes } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

import { openBrowserOnce } from "./browser";
import type { LoginSpec } from "./oauth";
import { dataDir } from "./paths";
import { retryingFetch } from "./retry";

export const WORKBUDDY_AI_ID = "workbuddy-ai";
export const WORKBUDDY_AI_ENDPOINT = "https://www.workbuddy.ai";
export const WORKBUDDY_AI_BASE_URL = `${WORKBUDDY_AI_ENDPOINT}/v2`;
export const WORKBUDDY_UA_VERSION = "5.5.6";
export const WORKBUDDY_APP_VERSION = "2.0.0";
export const WORKBUDDY_SYSTEM_PROMPT = "You are a helpful assistant.";

const REFRESH_SKEW_MS = 60_000;
const SIGNIN_POLL_MS = 1_000;
const SIGNIN_TIMEOUT_MS = 5 * 60_000;
const WB_RETRY_TOKEN = 11217;
const WB_RETRY_ACCOUNT = 12151;

export interface WorkbuddyCreds {
  uid: string;
  accessToken: string;
  refreshToken: string;
  expiresAt: number;
  refreshExpiresAt: number;
  domain: string;
  tokenType?: string;
  nickname?: string;
}

export function isWorkbuddyAiSource(source: string | undefined): boolean {
  return source === WORKBUDDY_AI_ID;
}

export function workbuddyDesktopAuthPath(): string {
  const home = homedir();
  const base =
    process.platform === "darwin"
      ? join(home, "Library", "Application Support", "CodeBuddyExtension")
      : process.platform === "win32"
        ? join(home, "AppData", "Local", "CodeBuddyExtension")
        : join(home, ".local", "share", "CodeBuddyExtension");
  return join(base, "Data", "Public", "auth", "workbuddy-desktop-ai.info");
}

export function workbuddySessionPath(login?: LoginSpec): string {
  if (process.env.JEVONIAN_WORKBUDDY_AI_AUTH?.trim()) {
    return process.env.JEVONIAN_WORKBUDDY_AI_AUTH.trim();
  }
  if (login?.credentialsPath) return login.credentialsPath;
  const base = process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config");
  return join(base, "jevonian", "workbuddy-ai.json");
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

/** Plain-string JSON field; encrypted envelopes (`$wbEncrypted`) read as empty. */
export function workbuddyPlainString(raw: unknown): string {
  return typeof raw === "string" ? raw : "";
}

function domainOf(creds: Pick<WorkbuddyCreds, "domain">): string {
  return creds.domain || "www.workbuddy.ai";
}

function nearExpiry(expiresAt: number): boolean {
  if (!expiresAt) return false;
  return Date.now() >= expiresAt - REFRESH_SKEW_MS;
}

export function parseWorkbuddySession(data: Record<string, unknown>): WorkbuddyCreds | undefined {
  // Jevonian-stored shape (flat).
  const flatAccess = workbuddyPlainString(data.accessToken);
  const flatUid = workbuddyPlainString(data.uid);
  if (flatAccess && flatUid) {
    return {
      uid: flatUid,
      accessToken: flatAccess,
      refreshToken: workbuddyPlainString(data.refreshToken),
      expiresAt: typeof data.expiresAt === "number" ? data.expiresAt : 0,
      refreshExpiresAt: typeof data.refreshExpiresAt === "number" ? data.refreshExpiresAt : 0,
      domain: workbuddyPlainString(data.domain) || "www.workbuddy.ai",
      ...(workbuddyPlainString(data.tokenType)
        ? { tokenType: workbuddyPlainString(data.tokenType) }
        : {}),
      ...(workbuddyPlainString(data.nickname)
        ? { nickname: workbuddyPlainString(data.nickname) }
        : {}),
    };
  }

  // Desktop `workbuddy-desktop-ai.info` shape.
  const account = asRecord(data.account);
  const auth = asRecord(data.auth);
  const uid = workbuddyPlainString(account.uid);
  const access = workbuddyPlainString(auth.accessToken);
  if (!uid || !access) return undefined;
  const now = Date.now();
  let expiresAt = typeof auth.expiresAt === "number" ? auth.expiresAt : 0;
  let refreshExpiresAt = typeof auth.refreshExpiresAt === "number" ? auth.refreshExpiresAt : 0;
  if (!expiresAt && typeof auth.expiresIn === "number" && auth.expiresIn > 0) {
    expiresAt = now + auth.expiresIn * 1000;
  }
  if (!refreshExpiresAt && typeof auth.refreshExpiresIn === "number" && auth.refreshExpiresIn > 0) {
    refreshExpiresAt = now + auth.refreshExpiresIn * 1000;
  }
  return {
    uid,
    accessToken: access,
    refreshToken: workbuddyPlainString(auth.refreshToken),
    expiresAt,
    refreshExpiresAt,
    domain: workbuddyPlainString(auth.domain) || "www.workbuddy.ai",
    ...(workbuddyPlainString(auth.tokenType)
      ? { tokenType: workbuddyPlainString(auth.tokenType) }
      : {}),
    ...(workbuddyPlainString(account.nickname)
      ? { nickname: workbuddyPlainString(account.nickname) }
      : {}),
  };
}

function readJsonFile(path: string): Record<string, unknown> | undefined {
  if (!existsSync(path)) return undefined;
  try {
    const raw = JSON.parse(readFileSync(path, "utf8")) as unknown;
    return typeof raw === "object" && raw !== null ? (raw as Record<string, unknown>) : undefined;
  } catch {
    return undefined;
  }
}

export function readWorkbuddySession(login?: LoginSpec): WorkbuddyCreds | undefined {
  const session = readJsonFile(workbuddySessionPath(login));
  if (session) {
    const parsed = parseWorkbuddySession(session);
    if (parsed) return parsed;
  }
  // An explicit path (env or second-account login) is the only place to look — do not fall
  // through to the desktop app's file and silently pick up a different account.
  const override = process.env.JEVONIAN_WORKBUDDY_AI_AUTH?.trim();
  if (override || login?.credentialsPath) return undefined;
  const desktop = readJsonFile(workbuddyDesktopAuthPath());
  if (!desktop) return undefined;
  return parseWorkbuddySession(desktop);
}

export function hasWorkbuddyCredential(login?: LoginSpec): boolean {
  return readWorkbuddySession(login) !== undefined;
}

export function saveWorkbuddySession(creds: WorkbuddyCreds, login?: LoginSpec): void {
  const path = workbuddySessionPath(login);
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, `${JSON.stringify(creds, null, 2)}\n`, { mode: 0o600 });
  chmodSync(path, 0o600);
}

function requestId(): string {
  return randomBytes(16).toString("hex");
}

/** Auth + product headers every WorkBuddy AI call carries. */
export function workbuddyAuthHeaders(creds: WorkbuddyCreds): Record<string, string> {
  return {
    authorization: `Bearer ${creds.accessToken}`,
    "x-user-id": creds.uid,
    "x-domain": domainOf(creds),
    "x-product": "SaaS",
    "x-ide-type": "WorkBuddy",
    "user-agent": `WorkBuddy/${WORKBUDDY_UA_VERSION}`,
  };
}

/** Extra client headers Magpie sends only for WorkBuddy AI chats. */
export function workbuddyClientHeaders(session?: string): Record<string, string> {
  const fromSession =
    session && session.length > 0 ? session.replace(/[^a-zA-Z0-9]/g, "").slice(0, 32) : "";
  const conversation = fromSession || requestId();
  const message = requestId();
  return {
    "x-requested-with": "XMLHttpRequest",
    "x-agent-intent": "craft",
    "x-agent-type": "main",
    "x-ide-name": "WorkBuddy",
    "x-ide-version": WORKBUDDY_UA_VERSION,
    "x-conversation-id": conversation,
    "x-conversation-request-id": conversation,
    "x-conversation-message-id": message,
    "x-request-id": message,
  };
}

export function applyWorkbuddyHeaders(
  headers: Record<string, string>,
  creds: WorkbuddyCreds,
  session?: string,
): void {
  const auth = workbuddyAuthHeaders(creds);
  for (const [key, value] of Object.entries(auth)) {
    headers[key] ??= value;
  }
  // Live token + account always win over a stale preset override.
  headers.authorization = auth.authorization;
  headers["x-user-id"] = auth["x-user-id"]!;
  headers["x-domain"] = auth["x-domain"]!;
  const client = workbuddyClientHeaders(session);
  for (const [key, value] of Object.entries(client)) {
    headers[key] ??= value;
  }
}

/**
 * WorkBuddy refuses a chat whose first message is not a system prompt. Inject a generic one
 * when the client omitted it (scripts and plain OpenAI clients often do).
 */
export function ensureWorkbuddySystem(body: Record<string, unknown>): Record<string, unknown> {
  if (!Array.isArray(body.messages) || body.messages.length === 0) return body;
  const first = asRecord(body.messages[0]);
  if (first.role === "system") return body;
  return {
    ...body,
    messages: [{ role: "system", content: WORKBUDDY_SYSTEM_PROMPT }, ...body.messages],
  };
}

class WorkbuddyApiError extends Error {
  readonly code: number;

  constructor(code: number, message: string) {
    super(message || `WorkBuddy error ${code}`);
    this.name = "WorkbuddyApiError";
    this.code = code;
  }
}

async function wbCall<T>(
  method: string,
  url: string,
  headers: Record<string, string>,
  body?: unknown,
): Promise<T> {
  const response = await retryingFetch(url, {
    method,
    headers: {
      accept: "application/json",
      "user-agent": `WorkBuddy/${WORKBUDDY_UA_VERSION}`,
      ...headers,
      ...(body !== undefined ? { "content-type": "application/json" } : {}),
    },
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
  });
  const text = await response.text();
  let json: Record<string, unknown> = {};
  try {
    json = asRecord(JSON.parse(text));
  } catch {
    if (!response.ok) throw new Error(`WorkBuddy ${response.status}: ${text.slice(0, 200)}`);
    throw new Error("WorkBuddy returned non-JSON");
  }
  const code = typeof json.code === "number" ? json.code : response.ok ? 0 : response.status;
  if (code !== 0) {
    throw new WorkbuddyApiError(code, typeof json.msg === "string" ? json.msg : `error ${code}`);
  }
  return json.data as T;
}

function mergeRefreshed(current: WorkbuddyCreds, got: Record<string, unknown>): WorkbuddyCreds {
  const now = Date.now();
  const access = workbuddyPlainString(got.accessToken);
  if (!access) throw new Error("WorkBuddy gave no refreshed token");
  let expiresAt = typeof got.expiresAt === "number" ? got.expiresAt : 0;
  let refreshExpiresAt = typeof got.refreshExpiresAt === "number" ? got.refreshExpiresAt : 0;
  if (!expiresAt && typeof got.expiresIn === "number" && got.expiresIn > 0) {
    expiresAt = now + got.expiresIn * 1000;
  }
  if (!refreshExpiresAt && typeof got.refreshExpiresIn === "number" && got.refreshExpiresIn > 0) {
    refreshExpiresAt = now + got.refreshExpiresIn * 1000;
  }
  return {
    ...current,
    accessToken: access,
    refreshToken: workbuddyPlainString(got.refreshToken) || current.refreshToken,
    expiresAt: expiresAt || current.expiresAt,
    refreshExpiresAt: refreshExpiresAt || current.refreshExpiresAt,
    domain: workbuddyPlainString(got.domain) || current.domain,
    ...(workbuddyPlainString(got.tokenType)
      ? { tokenType: workbuddyPlainString(got.tokenType) }
      : {}),
  };
}

export async function refreshWorkbuddyCreds(
  creds: WorkbuddyCreds,
  endpoint = WORKBUDDY_AI_ENDPOINT,
): Promise<WorkbuddyCreds> {
  const got = await wbCall<Record<string, unknown>>(
    "POST",
    `${endpoint.replace(/\/+$/, "")}/v2/plugin/auth/token/refresh`,
    {
      "x-refresh-token": creds.refreshToken,
      "x-auth-refresh-source": "plugin",
      "x-domain": domainOf(creds),
    },
    {},
  );
  return mergeRefreshed(creds, got);
}

/**
 * Fresh credentials for a request. Desktop sessions are never written; Jevonian-stored
 * sessions are updated after a successful refresh.
 */
export async function resolveWorkbuddyCreds(
  login?: LoginSpec,
): Promise<WorkbuddyCreds | { error: string }> {
  const creds = readWorkbuddySession(login);
  if (!creds) {
    return {
      error:
        "WorkBuddy AI credentials not found. Use Discover or Save on the dashboard to sign in (browser), run `jevonian add workbuddy-ai-subscription`, or point JEVONIAN_WORKBUDDY_AI_AUTH at a plaintext session file.",
    };
  }
  if (creds.accessToken && !nearExpiry(creds.expiresAt)) return creds;
  if (!creds.refreshToken || (creds.refreshExpiresAt > 0 && Date.now() >= creds.refreshExpiresAt)) {
    if (creds.accessToken) return creds;
    return { error: "WorkBuddy AI account is signed out; sign in again." };
  }
  try {
    const refreshed = await refreshWorkbuddyCreds(creds);
    // Only write back when the session lives in Jevonian's store (not the desktop file).
    const sessionPath = workbuddySessionPath(login);
    const desktopPath = workbuddyDesktopAuthPath();
    if (sessionPath !== desktopPath) {
      try {
        saveWorkbuddySession(refreshed, login);
      } catch {
        // in-memory still carries the refreshed token
      }
    }
    return refreshed;
  } catch (error) {
    if (creds.accessToken) return creds;
    return {
      error: error instanceof Error ? error.message : `WorkBuddy refresh failed: ${String(error)}`,
    };
  }
}

export interface WorkbuddySignInResult {
  creds: WorkbuddyCreds;
  user: string;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Magpie-style browser sign-in: open WorkBuddy's auth URL, poll until the token and account
 * are ready, then persist the session for Jevonian.
 */
export async function signInWorkbuddyAi(options?: {
  endpoint?: string;
  login?: LoginSpec;
  open?: (url: string) => void;
  pollMs?: number;
  timeoutMs?: number;
  signal?: AbortSignal;
}): Promise<WorkbuddySignInResult> {
  const endpoint = (options?.endpoint ?? WORKBUDDY_AI_ENDPOINT).replace(/\/+$/, "");
  const pollMs = options?.pollMs ?? SIGNIN_POLL_MS;
  const timeoutMs = options?.timeoutMs ?? SIGNIN_TIMEOUT_MS;
  const deadline = Date.now() + timeoutMs;

  const stateHeaders = {
    "x-no-authorization": "true",
    "x-no-user-id": "true",
    "x-no-enterprise-id": "true",
    "x-no-department-info": "true",
  };
  const state = await wbCall<{ state?: string; authUrl?: string }>(
    "POST",
    `${endpoint}/v2/plugin/auth/state?platform=workbuddy-ai`,
    stateHeaders,
    {},
  );
  if (!state.state || !state.authUrl?.startsWith("https://")) {
    throw new Error("WorkBuddy gave no sign-in page");
  }
  const authUrl = new URL(state.authUrl);
  authUrl.searchParams.set("version", WORKBUDDY_APP_VERSION);
  authUrl.searchParams.set("loginSessionId", requestId());
  const url = authUrl.toString();
  if (options?.open) options.open(url);
  else openBrowserOnce(url);

  const poll = async <T>(
    path: string,
    headers: Record<string, string>,
    retryCode: number,
  ): Promise<T> => {
    for (;;) {
      if (options?.signal?.aborted) throw new Error("WorkBuddy sign-in aborted");
      if (Date.now() > deadline) throw new Error("the sign-in expired; start again");
      await sleep(pollMs);
      try {
        return await wbCall<T>("GET", `${endpoint}${path}`, headers);
      } catch (error) {
        if (error instanceof WorkbuddyApiError && error.code === retryCode) continue;
        throw error;
      }
    }
  };

  const token = await poll<Record<string, unknown>>(
    `/v2/plugin/auth/token?state=${encodeURIComponent(state.state)}`,
    {},
    WB_RETRY_TOKEN,
  );
  let creds = mergeRefreshed(
    {
      uid: "",
      accessToken: "",
      refreshToken: "",
      expiresAt: 0,
      refreshExpiresAt: 0,
      domain: "www.workbuddy.ai",
    },
    token,
  );
  const account = await poll<{ uid?: string; nickname?: string; phoneNumber?: string }>(
    `/v2/plugin/login/account?state=${encodeURIComponent(state.state)}`,
    {
      authorization: `Bearer ${creds.accessToken}`,
      "x-domain": domainOf(creds),
      "x-no-user-id": "true",
      "x-no-enterprise-id": "true",
    },
    WB_RETRY_ACCOUNT,
  );
  if (!account.uid) throw new Error("WorkBuddy gave no account");
  const nickname =
    (typeof account.nickname === "string" && account.nickname.trim()) ||
    (typeof account.phoneNumber === "string" && account.phoneNumber.trim()) ||
    account.uid;
  creds = { ...creds, uid: account.uid, nickname };
  saveWorkbuddySession(creds, options?.login);
  return { creds, user: nickname };
}

export async function fetchWorkbuddyModels(
  creds: WorkbuddyCreds,
  endpoint = WORKBUDDY_AI_ENDPOINT,
): Promise<string[]> {
  const root = endpoint.replace(/\/+$/, "");
  const response = await retryingFetch(`${root}/v3/config`, {
    method: "GET",
    headers: {
      ...workbuddyAuthHeaders(creds),
      "user-agent": `CLI/${WORKBUDDY_APP_VERSION} WorkBuddy/${WORKBUDDY_UA_VERSION}`,
      "x-requested-with": "XMLHttpRequest",
      accept: "application/json",
    },
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`WorkBuddy config: HTTP ${response.status}`);
  const json = asRecord(JSON.parse(text));
  if (typeof json.code === "number" && json.code !== 0) {
    throw new Error(`WorkBuddy config: ${typeof json.msg === "string" ? json.msg : json.code}`);
  }
  const data = asRecord(json.data ?? json);
  const models = modelsFromWorkbuddyConfig(data);
  if (models.length === 0) {
    throw new Error("WorkBuddy's config lists no models for its CLI agent");
  }
  return models;
}

/** Prefer the CLI agent's model ids; fall back to the top-level product catalog. */
export function modelsFromWorkbuddyConfig(data: Record<string, unknown>): string[] {
  const agents = Array.isArray(data.agents) ? data.agents : [];
  const cli = agents.find((raw) => asRecord(raw).name === "cli");
  const fromCli = Array.isArray(asRecord(cli).models)
    ? (asRecord(cli).models as unknown[]).filter((id): id is string => typeof id === "string")
    : [];
  if (fromCli.length > 0) return fromCli;
  const catalog = Array.isArray(data.models) ? data.models : [];
  return catalog.map((raw) => workbuddyPlainString(asRecord(raw).id)).filter((id) => id.length > 0);
}

export interface WorkbuddyQuota {
  windows: Array<{ id: string; label: string; usedPercent: number; note?: string }>;
  plan?: string;
}

export async function fetchWorkbuddyQuota(
  creds: WorkbuddyCreds,
  endpoint = WORKBUDDY_AI_ENDPOINT,
): Promise<WorkbuddyQuota> {
  const root = endpoint.replace(/\/+$/, "");
  const summary = await wbCall<{
    Packages?: Array<{
      CycleTotalCapacity?: unknown;
      CycleUsedCapacity?: unknown;
    }>;
    IsPaidUser?: boolean;
  }>("POST", `${root}/billing/meter/get-user-resource-summary`, workbuddyAuthHeaders(creds), {});
  let total = 0;
  let used = 0;
  for (const pkg of summary.Packages ?? []) {
    total += Number(pkg.CycleTotalCapacity ?? 0) || 0;
    used += Number(pkg.CycleUsedCapacity ?? 0) || 0;
  }
  const windows =
    total > 0
      ? [
          {
            id: "credits",
            label: "Credits",
            usedPercent: Math.min(100, Math.max(0, (100 * used) / total)),
            note: `${compactNumber(used)} / ${compactNumber(total)}`,
          },
        ]
      : [];
  return {
    windows,
    plan: summary.IsPaidUser ? "Pro" : "Free",
  };
}

function compactNumber(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}k`;
  return String(Math.round(value * 100) / 100);
}

/**
 * Fold an OpenAI Chat Completions SSE body into one non-streaming completion. WorkBuddy
 * refuses non-stream requests, so non-stream clients get the stream assembled here.
 */
export async function foldOpenAIChatStream(
  body: ReadableStream<Uint8Array> | null,
  model: string,
): Promise<Record<string, unknown>> {
  if (!body) {
    return {
      id: `chatcmpl-workbuddy`,
      object: "chat.completion",
      created: Math.floor(Date.now() / 1000),
      model,
      choices: [{ index: 0, message: { role: "assistant", content: "" }, finish_reason: "stop" }],
      usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
    };
  }
  const decoder = new TextDecoder();
  let buffer = "";
  let id = `chatcmpl-workbuddy`;
  let content = "";
  let reasoning = "";
  let finishReason: string | null = "stop";
  let usage: Record<string, unknown> | undefined;
  const toolCalls = new Map<
    number,
    { id: string; type: string; function: { name: string; arguments: string } }
  >();

  const consume = (text: string): void => {
    buffer += text;
    let index = buffer.indexOf("\n\n");
    while (index !== -1) {
      const event = buffer.slice(0, index);
      buffer = buffer.slice(index + 2);
      for (const line of event.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const data = line.slice(5).trim();
        if (!data || data === "[DONE]") continue;
        try {
          const parsed = asRecord(JSON.parse(data));
          if (typeof parsed.id === "string" && parsed.id) id = parsed.id;
          if (parsed.usage && typeof parsed.usage === "object") {
            usage = asRecord(parsed.usage);
          }
          const choices = Array.isArray(parsed.choices) ? parsed.choices : [];
          const choice = asRecord(choices[0]);
          if (typeof choice.finish_reason === "string") finishReason = choice.finish_reason;
          const delta = asRecord(choice.delta);
          if (typeof delta.content === "string") content += delta.content;
          if (typeof delta.reasoning_content === "string") reasoning += delta.reasoning_content;
          if (Array.isArray(delta.tool_calls)) {
            for (const raw of delta.tool_calls) {
              const call = asRecord(raw);
              const idx = typeof call.index === "number" ? call.index : 0;
              const existing = toolCalls.get(idx) ?? {
                id: "",
                type: "function",
                function: { name: "", arguments: "" },
              };
              if (typeof call.id === "string" && call.id) existing.id = call.id;
              if (typeof call.type === "string" && call.type) existing.type = call.type;
              const fn = asRecord(call.function);
              if (typeof fn.name === "string" && fn.name) existing.function.name = fn.name;
              if (typeof fn.arguments === "string") existing.function.arguments += fn.arguments;
              toolCalls.set(idx, existing);
            }
          }
        } catch {
          continue;
        }
      }
      index = buffer.indexOf("\n\n");
    }
  };

  const reader = body.getReader();
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      consume(decoder.decode(value, { stream: true }));
    }
    consume(decoder.decode());
  } finally {
    reader.releaseLock();
  }

  const message: Record<string, unknown> = {
    role: "assistant",
    content: content.length > 0 ? content : null,
  };
  if (reasoning.length > 0) message.reasoning_content = reasoning;
  if (toolCalls.size > 0) {
    message.tool_calls = [...toolCalls.entries()].sort(([a], [b]) => a - b).map(([, call]) => call);
  }
  return {
    id,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message, finish_reason: finishReason }],
    usage: usage ?? { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  };
}

/** Test helper: cache-bust path used by unit tests via dataDir override. */
export function workbuddyCacheDir(): string {
  return join(dataDir(), "workbuddy-ai");
}
