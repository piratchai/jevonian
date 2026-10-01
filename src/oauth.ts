import { execFile } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir, userInfo } from "node:os";
import { dirname, join } from "node:path";
import { promisify } from "node:util";

import { cursorAuthPath, cursorToken } from "./cursor";
import { retryingFetch } from "./retry";

export type OAuthSource = "claude-code" | "codex" | "antigravity" | "devin" | "cursor" | "static";

/**
 * Which local sign-in to read, when it is not the agent's own — a second account of the same
 * agent on the same machine. Structurally `ProviderLogin`, declared here so this module does
 * not have to import the config it is parsed into.
 */
export interface LoginSpec {
  home?: string;
  credentialsPath?: string;
  keychainService?: string;
  keychainAccount?: string;
}

/**
 * Identifies a login for the token cache. The source alone is not enough: two providers may
 * read two accounts of the same source, and they must not share one cached token.
 */
function loginKey(source: OAuthSource, login: LoginSpec | undefined): string {
  if (!login) return source;
  const parts = [
    login.home ?? "",
    login.credentialsPath ?? "",
    login.keychainService ?? "",
    login.keychainAccount ?? "",
  ];
  if (parts.every((part) => part === "")) return source;
  return `${source}\u0000${parts.join("\u0000")}`;
}

export const CLAUDE_CODE_SYSTEM_PROMPT =
  "You are Claude Code, Anthropic's official CLI for Claude.";

const execFileAsync = promisify(execFile);
const CLAUDE_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const CODEX_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann";
const ANTIGRAVITY_CLIENT_ID =
  "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com";
const ANTIGRAVITY_CLIENT_SECRET = "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";
const ANTIGRAVITY_KEYCHAIN_SERVICE = "gemini";
const ANTIGRAVITY_KEYCHAIN_ACCOUNT = "antigravity";
const DEVIN_DEFAULT_SERVER_URL = "https://server.codeium.com";
const REFRESH_SKEW_MS = 120_000;

export interface OAuthToken {
  token: string;
  accountId?: string;
  expiresAt?: number;
}

export interface OAuthFailure {
  error: string;
}

interface StoredCredential {
  data: Record<string, unknown>;
  save?: (next: Record<string, unknown>) => Promise<void>;
  label: string;
}

const cache = new Map<string, OAuthToken>();
const pending = new Map<string, Promise<OAuthToken | OAuthFailure>>();

function jwtExpiry(token: string): number | undefined {
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

function fresh(token: OAuthToken): boolean {
  return token.expiresAt === undefined || token.expiresAt - Date.now() > REFRESH_SKEW_MS;
}

async function readKeychain(service: string, account?: string): Promise<string | undefined> {
  if (process.platform !== "darwin") return undefined;
  try {
    const { stdout } = await execFileAsync("security", [
      "find-generic-password",
      "-s",
      service,
      ...(account ? ["-a", account] : []),
      "-w",
    ]);
    const value = stdout.trim();
    return value.length > 0 ? value : undefined;
  } catch {
    return undefined;
  }
}

async function writeKeychain(
  service: string,
  value: string,
  account: string = userInfo().username,
): Promise<void> {
  if (process.platform !== "darwin") return;
  try {
    await execFileAsync("security", [
      "add-generic-password",
      "-U",
      "-s",
      service,
      "-a",
      account,
      "-w",
      value,
    ]);
  } catch {
    return;
  }
}

function claudeCredentialsPath(login?: LoginSpec): string {
  // The source's own override wins over a provider login: an explicit environment is a
  // deliberate local choice, and a login only names a second account in the usual layout.
  if (process.env.JEVONIAN_CLAUDE_CREDENTIALS) return process.env.JEVONIAN_CLAUDE_CREDENTIALS;
  if (login?.credentialsPath) return login.credentialsPath;
  const base = process.env.CLAUDE_CONFIG_DIR ?? login?.home ?? join(homedir(), ".claude");
  return join(base, ".credentials.json");
}

async function readClaudeCredential(login?: LoginSpec): Promise<StoredCredential | undefined> {
  const path = claudeCredentialsPath(login);
  const explicit =
    Boolean(process.env.JEVONIAN_CLAUDE_CREDENTIALS) || Boolean(login?.credentialsPath);
  if (existsSync(path)) {
    try {
      const data = JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
      return {
        data,
        save: async (next) => {
          mkdirSync(dirname(path), { recursive: true });
          writeFileSync(path, `${JSON.stringify(next)}\n`, { mode: 0o600 });
          chmodSync(path, 0o600);
        },
        label: path,
      };
    } catch {
      return undefined;
    }
  }
  // A login that names its own file or directory does not fall back to the keychain: that
  // would serve the wrong account's token under the right account's name.
  if (explicit || login?.home) return undefined;
  const service = login?.keychainService ?? "Claude Code-credentials";
  const value = await readKeychain(service, login?.keychainAccount);
  if (!value) return undefined;
  try {
    const data = JSON.parse(value) as Record<string, unknown>;
    return {
      data,
      save: (next) => writeKeychain(service, JSON.stringify(next), login?.keychainAccount),
      label: `keychain:${service}`,
    };
  } catch {
    return undefined;
  }
}

function codexAuthPath(login?: LoginSpec): string {
  if (process.env.JEVONIAN_CODEX_AUTH) return process.env.JEVONIAN_CODEX_AUTH;
  if (login?.credentialsPath) return login.credentialsPath;
  const base = process.env.CODEX_HOME ?? login?.home ?? join(homedir(), ".codex");
  return join(base, "auth.json");
}

function readCodexCredential(login?: LoginSpec): StoredCredential | undefined {
  const path = codexAuthPath(login);
  if (!existsSync(path)) return undefined;
  try {
    const data = JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
    return {
      data,
      save: async (next) => {
        mkdirSync(dirname(path), { recursive: true });
        writeFileSync(path, `${JSON.stringify(next, null, 2)}\n`, { mode: 0o600 });
        chmodSync(path, 0o600);
      },
      label: path,
    };
  } catch {
    return undefined;
  }
}

function parseKeyringPayload(value: string): Record<string, unknown> | undefined {
  const trimmed = value.trim();
  const payload = trimmed.startsWith("go-keyring-base64:")
    ? Buffer.from(trimmed.slice("go-keyring-base64:".length), "base64").toString("utf8")
    : trimmed;
  try {
    const data = JSON.parse(payload) as unknown;
    return typeof data === "object" && data !== null
      ? (data as Record<string, unknown>)
      : undefined;
  } catch {
    return undefined;
  }
}

async function readAntigravityCredential(login?: LoginSpec): Promise<StoredCredential | undefined> {
  const override = login?.credentialsPath ?? process.env.JEVONIAN_ANTIGRAVITY_TOKEN;
  if (override && override.trim().length > 0) {
    const path = override.trim();
    if (!existsSync(path)) return undefined;
    try {
      const data = parseKeyringPayload(readFileSync(path, "utf8"));
      if (!data) return undefined;
      return {
        data,
        save: async (next) => {
          writeFileSync(path, `${JSON.stringify(next, null, 2)}\n`, { mode: 0o600 });
          chmodSync(path, 0o600);
        },
        label: path,
      };
    } catch {
      return undefined;
    }
  }
  if (process.platform !== "darwin") return undefined;
  const service = login?.keychainService ?? ANTIGRAVITY_KEYCHAIN_SERVICE;
  const account = login?.keychainAccount ?? ANTIGRAVITY_KEYCHAIN_ACCOUNT;
  try {
    const { stdout } = await execFileAsync("security", [
      "find-generic-password",
      "-s",
      service,
      "-a",
      account,
      "-w",
    ]);
    const data = parseKeyringPayload(stdout);
    if (!data) return undefined;
    return {
      data,
      save: (next) =>
        writeKeychain(
          service,
          `go-keyring-base64:${Buffer.from(JSON.stringify(next)).toString("base64")}`,
          account,
        ),
      label: `keychain:${service}/${account}`,
    };
  } catch {
    return undefined;
  }
}

interface AntigravityTokens {
  accessToken: string;
  refreshToken: string;
  expiresAt?: number;
}

function antigravityTokens(data: Record<string, unknown>): AntigravityTokens | undefined {
  const token =
    typeof data.token === "object" && data.token !== null
      ? (data.token as Record<string, unknown>)
      : {};
  const accessToken = typeof token.access_token === "string" ? token.access_token : "";
  if (!accessToken) return undefined;
  const refreshToken = typeof token.refresh_token === "string" ? token.refresh_token : "";
  const expiry = typeof token.expiry === "string" ? Date.parse(token.expiry) : undefined;
  const expiresAt = expiry !== undefined && !Number.isNaN(expiry) ? expiry : undefined;
  return { accessToken, refreshToken, ...(expiresAt === undefined ? {} : { expiresAt }) };
}

async function refreshAntigravity(
  refreshToken: string,
): Promise<{ accessToken: string; expiresAt: number } | undefined> {
  try {
    const body = new URLSearchParams({
      grant_type: "refresh_token",
      client_id: ANTIGRAVITY_CLIENT_ID,
      client_secret: ANTIGRAVITY_CLIENT_SECRET,
      refresh_token: refreshToken,
    });
    const response = await retryingFetch("https://oauth2.googleapis.com/token", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: body.toString(),
    });
    if (!response.ok) return undefined;
    const json = (await response.json()) as Record<string, unknown>;
    const accessToken = typeof json.access_token === "string" ? json.access_token : "";
    if (!accessToken) return undefined;
    const expiresIn = typeof json.expires_in === "number" ? json.expires_in : 3600;
    return { accessToken, expiresAt: Date.now() + expiresIn * 1000 };
  } catch {
    return undefined;
  }
}

async function resolveAntigravity(login?: LoginSpec): Promise<OAuthToken | OAuthFailure> {
  const credential = await readAntigravityCredential(login);
  if (!credential) {
    return {
      error:
        "Antigravity credentials not found. Sign in with the Antigravity IDE or `agy` CLI, or set JEVONIAN_ANTIGRAVITY_TOKEN.",
    };
  }
  const tokens = antigravityTokens(credential.data);
  if (!tokens) {
    return { error: "Antigravity credential has an unexpected shape." };
  }
  if (tokens.expiresAt === undefined || tokens.expiresAt - Date.now() > REFRESH_SKEW_MS) {
    return {
      token: tokens.accessToken,
      ...(tokens.expiresAt === undefined ? {} : { expiresAt: tokens.expiresAt }),
    };
  }
  if (!tokens.refreshToken) {
    return {
      error: "Antigravity token is expired and has no refresh token. Run `agy` to sign in again.",
    };
  }
  const refreshed = await refreshAntigravity(tokens.refreshToken);
  if (!refreshed) {
    return { error: "Antigravity token refresh failed. Run `agy` to sign in again." };
  }
  const token =
    typeof credential.data.token === "object" && credential.data.token !== null
      ? (credential.data.token as Record<string, unknown>)
      : {};
  const next = {
    ...credential.data,
    token: {
      ...token,
      access_token: refreshed.accessToken,
      expiry: new Date(refreshed.expiresAt).toISOString(),
    },
  };
  if (credential.save) {
    try {
      await credential.save(next);
    } catch {
      // in-memory cache still carries the refreshed token
    }
  }
  return { token: refreshed.accessToken, expiresAt: refreshed.expiresAt };
}

export function resolveAntigravityProject(): string {
  const override = process.env.JEVONIAN_ANTIGRAVITY_PROJECT;
  if (override && override.trim().length > 0) return override.trim();
  try {
    const path = join(homedir(), ".gemini", "antigravity-cli", "cache", "default_project_id.txt");
    if (existsSync(path)) {
      const value = readFileSync(path, "utf8").trim();
      if (value.length > 0) return value;
    }
  } catch {
    // fall through to the shared default
  }
  return "default-cli-project";
}

/**
 * Where Devin CLI (`devin auth login`) keeps its session token.
 * `JEVONIAN_DEVIN_CREDENTIALS` wins; otherwise `%APPDATA%\devin` on Windows and
 * `$XDG_DATA_HOME/devin` (default `~/.local/share/devin`) elsewhere.
 */
export function devinCredentialsPath(login?: LoginSpec): string {
  const override = process.env.JEVONIAN_DEVIN_CREDENTIALS;
  if (override && override.trim().length > 0) return override.trim();
  if (login?.credentialsPath) return login.credentialsPath;
  if (process.platform === "win32") {
    if (login?.home) return join(login.home, "credentials.toml");
    const appData = process.env.APPDATA ?? join(homedir(), "AppData", "Roaming");
    return join(appData, "devin", "credentials.toml");
  }
  if (login?.home) return join(login.home, "credentials.toml");
  const xdg = process.env.XDG_DATA_HOME;
  const base = xdg && xdg.trim().length > 0 ? xdg.trim() : join(homedir(), ".local", "share");
  return join(base, "devin", "credentials.toml");
}

function unescapeTomlBasic(value: string): string {
  return value.replace(/\\(["\\nrt])/g, (_, char: string) => {
    if (char === "n") return "\n";
    if (char === "r") return "\r";
    if (char === "t") return "\t";
    return char;
  });
}

/**
 * Parse the flat `key = "value"` lines of Devin's credentials.toml. Tables, arrays, and
 * multi-line strings are not used by that file and are ignored.
 */
export function parseFlatToml(text: string): Record<string, string> {
  const result: Record<string, string> = {};
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith("#") || line.startsWith("[")) continue;
    const basic = /^([A-Za-z0-9_.-]+)\s*=\s*"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$/.exec(line);
    if (basic?.[1] !== undefined && basic[2] !== undefined) {
      result[basic[1]] = unescapeTomlBasic(basic[2]);
      continue;
    }
    const literal = /^([A-Za-z0-9_.-]+)\s*=\s*'([^']*)'\s*(?:#.*)?$/.exec(line);
    if (literal?.[1] !== undefined && literal[2] !== undefined) {
      result[literal[1]] = literal[2];
    }
  }
  return result;
}

function readDevinCredential(login?: LoginSpec): Record<string, string> | undefined {
  const path = devinCredentialsPath(login);
  if (!existsSync(path)) return undefined;
  try {
    return parseFlatToml(readFileSync(path, "utf8"));
  } catch {
    return undefined;
  }
}

/** Devin session tokens do not expire and there is no refresh flow: when the server rejects
 * one, the user signs in again with `devin auth login` and the next resolve re-reads the file.
 */
function resolveDevin(login?: LoginSpec): Promise<OAuthToken | OAuthFailure> {
  const data = readDevinCredential(login);
  if (!data) {
    return Promise.resolve({
      error:
        "Devin credentials not found. Sign in with `devin auth login`, or set JEVONIAN_DEVIN_CREDENTIALS.",
    });
  }
  const token = data.windsurf_api_key?.trim() ?? "";
  if (!token) {
    return Promise.resolve({
      error:
        "Devin credential has an unexpected shape (no windsurf_api_key). Run `devin auth login` again.",
    });
  }
  return Promise.resolve({ token });
}

/** Devin API server from credentials.toml (`api_server_url`), or the public default. */
export function resolveDevinServerUrl(login?: LoginSpec): string {
  const value = readDevinCredential(login)?.api_server_url?.trim();
  return value && value.length > 0 ? value.replace(/\/+$/, "") : DEVIN_DEFAULT_SERVER_URL;
}

/**
 * Cursor keeps its sign-in in `cursor-agent`'s own store: the macOS keychain when it is there,
 * otherwise the CLI's `auth.json`. A status read is what renews a token about to run out, so
 * the only thing to do here is report whether there is something to read at all.
 */
function hasCursorCredential(login?: LoginSpec): boolean {
  const override = process.env.JEVONIAN_CURSOR_AUTH;
  if (override && override.trim().length > 0) return existsSync(override.trim());
  if (existsSync(cursorAuthPath(login))) return true;
  return process.platform === "darwin";
}

async function resolveCursor(login?: LoginSpec): Promise<OAuthToken | OAuthFailure> {
  try {
    const token = await cursorToken(login);
    const expiresAt = jwtExpiry(token);
    return { token, ...(expiresAt === undefined ? {} : { expiresAt }) };
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
}

interface RefreshedClaude {
  accessToken: string;
  refreshToken: string;
  expiresAt: number;
}

async function refreshClaude(refreshToken: string): Promise<RefreshedClaude | undefined> {
  try {
    const response = await retryingFetch("https://console.anthropic.com/v1/oauth/token", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        grant_type: "refresh_token",
        refresh_token: refreshToken,
        client_id: CLAUDE_CLIENT_ID,
      }),
    });
    if (!response.ok) return undefined;
    const json = (await response.json()) as Record<string, unknown>;
    const accessToken = typeof json.access_token === "string" ? json.access_token : "";
    if (!accessToken) return undefined;
    const expiresIn = typeof json.expires_in === "number" ? json.expires_in : 3600;
    return {
      accessToken,
      refreshToken: typeof json.refresh_token === "string" ? json.refresh_token : refreshToken,
      expiresAt: Date.now() + expiresIn * 1000,
    };
  } catch {
    return undefined;
  }
}

async function resolveClaude(login?: LoginSpec): Promise<OAuthToken | OAuthFailure> {
  const credential = await readClaudeCredential(login);
  if (!credential) {
    return {
      error:
        "Claude Code credentials not found. Sign in with `claude` first, or point JEVONIAN_CLAUDE_CREDENTIALS at a credentials file.",
    };
  }
  const oauth = (credential.data.claudeAiOauth ?? {}) as Record<string, unknown>;
  const accessToken = typeof oauth.accessToken === "string" ? oauth.accessToken : "";
  const refreshToken = typeof oauth.refreshToken === "string" ? oauth.refreshToken : "";
  const expiresAt = typeof oauth.expiresAt === "number" ? oauth.expiresAt : jwtExpiry(accessToken);
  if (accessToken && (expiresAt === undefined || expiresAt - Date.now() > REFRESH_SKEW_MS)) {
    return { token: accessToken, ...(expiresAt === undefined ? {} : { expiresAt }) };
  }
  if (!refreshToken) {
    return {
      error:
        "Claude Code token is expired and has no refresh token. Run `claude` to sign in again.",
    };
  }
  const refreshed = await refreshClaude(refreshToken);
  if (!refreshed) {
    return { error: "Claude OAuth refresh failed. Run `claude` to sign in again." };
  }
  const next = {
    ...credential.data,
    claudeAiOauth: {
      ...oauth,
      accessToken: refreshed.accessToken,
      refreshToken: refreshed.refreshToken,
      expiresAt: refreshed.expiresAt,
    },
  };
  if (credential.save) {
    try {
      await credential.save(next);
    } catch {
      // in-memory cache still carries the refreshed token
    }
  }
  return { token: refreshed.accessToken, expiresAt: refreshed.expiresAt };
}

interface RefreshedCodex {
  accessToken: string;
  refreshToken: string;
  idToken?: string;
  expiresAt?: number;
}

async function refreshCodex(refreshToken: string): Promise<RefreshedCodex | undefined> {
  try {
    const response = await retryingFetch("https://auth.openai.com/oauth/token", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        client_id: CODEX_CLIENT_ID,
        grant_type: "refresh_token",
        refresh_token: refreshToken,
        scope: "openid profile email",
      }),
    });
    if (!response.ok) return undefined;
    const json = (await response.json()) as Record<string, unknown>;
    const accessToken = typeof json.access_token === "string" ? json.access_token : "";
    if (!accessToken) return undefined;
    return {
      accessToken,
      refreshToken: typeof json.refresh_token === "string" ? json.refresh_token : refreshToken,
      ...(typeof json.id_token === "string" ? { idToken: json.id_token } : {}),
      ...(jwtExpiry(accessToken) === undefined ? {} : { expiresAt: jwtExpiry(accessToken) }),
    };
  } catch {
    return undefined;
  }
}

async function resolveCodex(login?: LoginSpec): Promise<OAuthToken | OAuthFailure> {
  const credential = readCodexCredential(login);
  if (!credential) {
    return {
      error: "Codex credentials not found. Sign in with `codex` first, or set JEVONIAN_CODEX_AUTH.",
    };
  }
  const tokens = (credential.data.tokens ?? {}) as Record<string, unknown>;
  const accessToken = typeof tokens.access_token === "string" ? tokens.access_token : "";
  const refreshToken = typeof tokens.refresh_token === "string" ? tokens.refresh_token : "";
  const accountId = typeof tokens.account_id === "string" ? tokens.account_id : undefined;
  const expiresAt = accessToken ? jwtExpiry(accessToken) : undefined;
  if (accessToken && (expiresAt === undefined || expiresAt - Date.now() > REFRESH_SKEW_MS)) {
    return {
      token: accessToken,
      ...(accountId ? { accountId } : {}),
      ...(expiresAt ? { expiresAt } : {}),
    };
  }
  if (!refreshToken) {
    return {
      error: "Codex token is expired and has no refresh token. Run `codex` to sign in again.",
    };
  }
  const refreshed = await refreshCodex(refreshToken);
  if (!refreshed) {
    return { error: "Codex OAuth refresh failed. Run `codex` to sign in again." };
  }
  const next = {
    ...credential.data,
    tokens: {
      ...tokens,
      access_token: refreshed.accessToken,
      refresh_token: refreshed.refreshToken,
      ...(accountId ? { account_id: accountId } : {}),
      ...(refreshed.idToken ? { id_token: refreshed.idToken } : {}),
    },
    last_refresh: new Date().toISOString(),
  };
  if (credential.save) {
    try {
      await credential.save(next);
    } catch {
      // in-memory cache still carries the refreshed token
    }
  }
  return {
    token: refreshed.accessToken,
    ...(accountId ? { accountId } : {}),
    ...(refreshed.expiresAt ? { expiresAt: refreshed.expiresAt } : {}),
  };
}

/**
 * Drops a cached token so the next resolve re-reads the sign-in — the account was refused, or
 * signed in again. Passing the login limits it to that account; omitting it clears every login
 * of the source, which is what a caller who does not know which one failed wants.
 */
export function invalidateOAuthToken(source: OAuthSource, login?: LoginSpec): void {
  if (login) {
    cache.delete(loginKey(source, login));
    return;
  }
  cache.delete(source);
  const prefix = `${source}\u0000`;
  for (const key of cache.keys()) {
    if (key.startsWith(prefix)) cache.delete(key);
  }
}

/**
 * Reports whether the local credential file for a live OAuth source exists and holds a token.
 * Used for status display only; it never hits the network.
 */
export function hasOAuthCredential(source: OAuthSource, login?: LoginSpec): boolean {
  if (source === "static") return false;
  if (source === "codex") return readCodexCredential(login) !== undefined;
  if (source === "devin") return Boolean(readDevinCredential(login)?.windsurf_api_key?.trim());
  if (source === "cursor") return hasCursorCredential(login);
  if (source === "antigravity") {
    const override = login?.credentialsPath ?? process.env.JEVONIAN_ANTIGRAVITY_TOKEN;
    if (override && existsSync(override.trim())) return true;
    // Keychain-backed, and a named service cannot be checked without reading it; the best an
    // existence check can say on macOS is "maybe", which is what the default said already.
    return process.platform === "darwin";
  }
  if (process.env.JEVONIAN_CLAUDE_CREDENTIALS) {
    return existsSync(process.env.JEVONIAN_CLAUDE_CREDENTIALS);
  }
  if (existsSync(claudeCredentialsPath(login))) return true;
  // Otherwise the sign-in may be in the keychain, where an existence check cannot reach it.
  return process.platform === "darwin";
}

export function resolveOAuthToken(options: {
  source: OAuthSource;
  staticToken?: string;
  login?: LoginSpec;
}): Promise<OAuthToken | OAuthFailure> {
  if (options.source === "static") {
    if (!options.staticToken) {
      return Promise.resolve({ error: "No OAuth token stored for this provider." });
    }
    return Promise.resolve({ token: options.staticToken });
  }
  // Keyed by login, not just source: two providers may read two accounts of one agent, and
  // sharing one cached token would serve the wrong account's quota window.
  const key = loginKey(options.source, options.login);
  const cached = cache.get(key);
  if (cached && fresh(cached)) return Promise.resolve(cached);
  const inflight = pending.get(key);
  if (inflight) return inflight;
  const login = options.login;
  const resolve = (): Promise<OAuthToken | OAuthFailure> =>
    options.source === "claude-code"
      ? resolveClaude(login)
      : options.source === "codex"
        ? resolveCodex(login)
        : options.source === "devin"
          ? resolveDevin(login)
          : options.source === "cursor"
            ? resolveCursor(login)
            : resolveAntigravity(login);
  const task = resolve().then((result) => {
    if (!("error" in result)) cache.set(key, result);
    return result;
  });
  pending.set(key, task);
  void task.finally(() => pending.delete(key));
  return task;
}

export function oauthCredentialLabel(source: OAuthSource): string {
  if (source === "claude-code") return "Claude Code credentials";
  if (source === "codex") return "Codex credentials";
  if (source === "antigravity") return "Antigravity credentials";
  if (source === "devin") return "Devin credentials";
  if (source === "cursor") return "Cursor credentials";
  return "stored token";
}
