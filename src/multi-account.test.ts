import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseProviderLogin } from "./config";
import {
  hasOAuthCredential,
  invalidateOAuthToken,
  resolveDevinServerUrl,
  resolveOAuthToken,
  type OAuthFailure,
} from "./oauth";

let dir = "";
let previous: Record<string, string | undefined> = {};

const ENV_KEYS = [
  "JEVONIAN_CLAUDE_CREDENTIALS",
  "JEVONIAN_CODEX_AUTH",
  "JEVONIAN_ANTIGRAVITY_TOKEN",
  "JEVONIAN_DEVIN_CREDENTIALS",
  "CLAUDE_CONFIG_DIR",
  "CODEX_HOME",
] as const;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-multi-"));
  previous = {};
  for (const key of ENV_KEYS) previous[key] = process.env[key];
  // Isolate every source's own override so a provider login is what reads the temp dir, never a
  // machine sign-in that happens to be present.
  for (const key of ENV_KEYS) delete process.env[key];
});

afterEach(() => {
  invalidateOAuthToken("claude-code");
  invalidateOAuthToken("codex");
  invalidateOAuthToken("antigravity");
  invalidateOAuthToken("devin");
  for (const key of ENV_KEYS) {
    if (previous[key] === undefined) delete process.env[key];
    else process.env[key] = previous[key];
  }
  rmSync(dir, { recursive: true, force: true });
  vi.unstubAllGlobals();
});

function jwt(expSeconds: number, sub = ""): string {
  const encode = (value: unknown): string =>
    Buffer.from(JSON.stringify(value)).toString("base64url");
  return `${encode({ alg: "none", typ: "JWT" })}.${encode({ exp: expSeconds, sub })}.sig`;
}

function failure(result: { token?: string } | OAuthFailure): OAuthFailure {
  if ("error" in result) return result;
  throw new Error(`expected failure, got ${result.token ?? "token"}`);
}

function writeClaude(path: string, accessToken: string, fresh = true): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(
    path,
    JSON.stringify({
      claudeAiOauth: {
        accessToken,
        refreshToken: `refresh-${accessToken}`,
        expiresAt: fresh ? Date.now() + 3_600_000 : Date.now() - 1_000,
      },
    }),
  );
}

function writeCodex(path: string, token: string, accountId: string): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(
    path,
    JSON.stringify({
      tokens: { access_token: token, refresh_token: "refresh-1", account_id: accountId },
    }),
  );
}

describe("parseProviderLogin", () => {
  it("returns undefined for an absent or empty login", () => {
    expect(parseProviderLogin(undefined)).toBeUndefined();
    expect(parseProviderLogin(null)).toBeUndefined();
    expect(parseProviderLogin({})).toBeUndefined();
    expect(parseProviderLogin({ label: "   ", home: "  " })).toBeUndefined();
  });

  it("keeps each field it names", () => {
    expect(
      parseProviderLogin({
        label: "work",
        home: "/home/u/.claude-work",
        credentialsPath: "/home/u/creds.json",
        keychainService: "svc",
        keychainAccount: "acct",
      }),
    ).toEqual({
      label: "work",
      home: "/home/u/.claude-work",
      credentialsPath: "/home/u/creds.json",
      keychainService: "svc",
      keychainAccount: "acct",
    });
  });

  it("rejects a relative home and credentials path", () => {
    expect(() => parseProviderLogin({ home: "rel/dir" })).toThrow(/absolute path/);
    expect(() => parseProviderLogin({ credentialsPath: "rel/creds.json" })).toThrow(
      /absolute path/,
    );
    // `~` is a shell convenience, not an absolute path; it is refused rather than expanded.
    expect(() => parseProviderLogin({ home: "~/.claude-work" })).toThrow(/absolute path/);
  });
});

describe("Claude Code second account", () => {
  it("reads a credentials file under login.home", async () => {
    const home = join(dir, ".claude-work");
    writeClaude(join(home, ".credentials.json"), "work-token");
    vi.stubGlobal("fetch", async () => {
      throw new Error("fresh token should not refresh");
    });
    const result = await resolveOAuthToken({
      source: "claude-code",
      login: { home },
    });
    expect("token" in result && result.token).toBe("work-token");
  });

  it("reads an explicit credentialsPath over the default layout", async () => {
    const path = join(dir, "elsewhere.json");
    writeClaude(path, "explicit-token");
    const result = await resolveOAuthToken({
      source: "claude-code",
      login: { credentialsPath: path },
    });
    expect("token" in result && result.token).toBe("explicit-token");
  });

  it("does not fall back to the keychain when a login names a file that is missing", async () => {
    const result = await resolveOAuthToken({
      source: "claude-code",
      login: { credentialsPath: join(dir, "missing.json") },
    });
    expect(failure(result).error).toContain("Claude Code credentials not found");
  });
});

describe("Codex second account", () => {
  it("reads auth.json under login.home", async () => {
    const home = join(dir, ".codex-work");
    const token = jwt(Math.floor(Date.now() / 1000) + 3_600);
    writeCodex(join(home, "auth.json"), token, "acct_work");
    const result = await resolveOAuthToken({ source: "codex", login: { home } });
    expect("token" in result && result.token).toBe(token);
    expect("token" in result && result.accountId).toBe("acct_work");
  });

  it("reads an explicit auth.json path", async () => {
    const path = join(dir, "auth.json");
    const token = jwt(Math.floor(Date.now() / 1000) + 3_600);
    writeCodex(path, token, "acct_explicit");
    const result = await resolveOAuthToken({
      source: "codex",
      login: { credentialsPath: path },
    });
    expect("token" in result && result.accountId).toBe("acct_explicit");
  });
});

describe("Devin second account", () => {
  it("reads credentials.toml under login.home", async () => {
    const home = join(dir, "devin-work");
    mkdirSync(home, { recursive: true });
    writeFileSync(
      join(home, "credentials.toml"),
      'windsurf_api_key = "work-devin"\napi_server_url = "https://server.work.com/"\n',
    );
    const result = await resolveOAuthToken({ source: "devin", login: { home } });
    expect("token" in result && result.token).toBe("work-devin");
    expect(resolveDevinServerUrl({ home })).toBe("https://server.work.com");
  });
});

describe("Independent accounts of one source", () => {
  it("caches and resolves two Codex accounts separately", async () => {
    const workHome = join(dir, ".codex-work");
    const homeHome = join(dir, ".codex-home");
    const workToken = jwt(Math.floor(Date.now() / 1000) + 3_600, "work");
    const homeToken = jwt(Math.floor(Date.now() / 1000) + 3_600, "home");
    writeCodex(join(workHome, "auth.json"), workToken, "acct_work");
    writeCodex(join(homeHome, "auth.json"), homeToken, "acct_home");

    const work = await resolveOAuthToken({ source: "codex", login: { home: workHome } });
    const home = await resolveOAuthToken({ source: "codex", login: { home: homeHome } });
    expect("token" in work && work.accountId).toBe("acct_work");
    expect("token" in home && home.accountId).toBe("acct_home");
    expect("token" in work && work.token).not.toBe("token" in home && home.token);
  });

  it("invalidates one account without touching the other", async () => {
    const workHome = join(dir, ".codex-work");
    const homeHome = join(dir, ".codex-home");
    const fresh1 = jwt(Math.floor(Date.now() / 1000) + 3_600);
    const fresh2 = jwt(Math.floor(Date.now() / 1000) + 3_601);
    writeCodex(join(workHome, "auth.json"), fresh1, "acct_work");
    writeCodex(join(homeHome, "auth.json"), fresh2, "acct_home");
    await resolveOAuthToken({ source: "codex", login: { home: workHome } });
    await resolveOAuthToken({ source: "codex", login: { home: homeHome } });

    // Rotate the work token, invalidate only that account, and confirm the home account is
    // still served from its own cache.
    const rotated = jwt(Math.floor(Date.now() / 1000) + 7_200);
    writeCodex(join(workHome, "auth.json"), rotated, "acct_work");
    invalidateOAuthToken("codex", { home: workHome });
    const work = await resolveOAuthToken({ source: "codex", login: { home: workHome } });
    const home = await resolveOAuthToken({ source: "codex", login: { home: homeHome } });
    expect("token" in work && work.token).toBe(rotated);
    expect("token" in home && home.token).toBe(fresh2);
  });

  it("a bare invalidate clears every login of the source", async () => {
    const workHome = join(dir, ".codex-work");
    writeCodex(join(workHome, "auth.json"), jwt(Math.floor(Date.now() / 1000) + 3_600), "a");
    await resolveOAuthToken({ source: "codex", login: { home: workHome } });
    invalidateOAuthToken("codex");
    const next = jwt(Math.floor(Date.now() / 1000) + 9_999);
    writeCodex(join(workHome, "auth.json"), next, "a");
    const after = await resolveOAuthToken({ source: "codex", login: { home: workHome } });
    expect("token" in after && after.token).toBe(next);
  });
});

describe("hasOAuthCredential with a login", () => {
  it("reports a scoped Codex sign-in only where the file exists", () => {
    const home = join(dir, ".codex-work");
    expect(hasOAuthCredential("codex", { home })).toBe(false);
    writeCodex(join(home, "auth.json"), jwt(Math.floor(Date.now() / 1000) + 3_600), "a");
    expect(hasOAuthCredential("codex", { home })).toBe(true);
  });

  it("reports a scoped Claude sign-in from its credentials file", () => {
    const path = join(dir, "creds.json");
    expect(hasOAuthCredential("claude-code", { credentialsPath: path })).toBe(
      process.platform === "darwin",
    );
    writeClaude(path, "t");
    expect(hasOAuthCredential("claude-code", { credentialsPath: path })).toBe(true);
  });
});
