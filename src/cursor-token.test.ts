import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { cursorToken } from "./cursor";

/**
 * `cursorToken` renews a stale sign-in by running `cursor-agent status`. Two Cursor accounts
 * must not share that renewal: the CLI renews whichever account it is signed into, and the
 * re-read that follows would hand one account the other's token. These tests stand in for the
 * CLI by mocking `child_process` and count how many renewals each case triggers.
 */
const cli = vi.hoisted(() => ({
  statusCalls: 0,
  /** auth.json path -> the token `cursor-agent status` should leave behind. */
  renewTo: {} as Record<string, string>,
}));

vi.mock("node:child_process", async () => {
  const { writeFileSync } = await import("node:fs");
  return {
    execFile: (
      _file: string,
      args: string[],
      _options: unknown,
      callback: (error: unknown, stdout?: { stdout: string; stderr: string }) => void,
    ) => {
      // Keychain lookups find nothing, so the auth file is the credential that is read.
      if (args[0] === "find-generic-password") {
        callback(null, { stdout: "", stderr: "" });
        return;
      }
      if (args[0] === "status") {
        cli.statusCalls += 1;
        for (const [path, token] of Object.entries(cli.renewTo)) {
          writeFileSync(path, JSON.stringify({ accessToken: token }), "utf8");
        }
        callback(null, { stdout: "", stderr: "" });
        return;
      }
      callback(null, { stdout: "", stderr: "" });
    },
  };
});

/** A JWT whose `exp` is in the past, so the token reads as stale. */
function expiredJwt(): string {
  const payload = Buffer.from(JSON.stringify({ exp: 1 })).toString("base64url");
  return `header.${payload}.signature`;
}

function writeAuth(dir: string, token: string): string {
  mkdirSync(dir, { recursive: true });
  const path = join(dir, "auth.json");
  writeFileSync(path, JSON.stringify({ accessToken: token }), "utf8");
  return path;
}

let home = "";
let binDir = "";
const previousPath = process.env.PATH;
const previousAuth = process.env.JEVONIAN_CURSOR_AUTH;

beforeEach(() => {
  home = mkdtempSync(join(tmpdir(), "jev-cursor-token-"));
  // `cursorExecutable()` finds the CLI by walking PATH; a plain file named cursor-agent is
  // enough, since the exec itself is mocked.
  binDir = join(home, "bin");
  mkdirSync(binDir, { recursive: true });
  writeFileSync(join(binDir, "cursor-agent"), "", "utf8");
  process.env.PATH = binDir;
  delete process.env.JEVONIAN_CURSOR_AUTH;
  cli.statusCalls = 0;
  cli.renewTo = {};
});

afterEach(() => {
  if (previousPath === undefined) delete process.env.PATH;
  else process.env.PATH = previousPath;
  if (previousAuth === undefined) delete process.env.JEVONIAN_CURSOR_AUTH;
  else process.env.JEVONIAN_CURSOR_AUTH = previousAuth;
  rmSync(home, { recursive: true, force: true });
});

describe("cursorToken renewal", () => {
  it("gives each account its own token when two renew at once", async () => {
    const workPath = writeAuth(join(home, "work"), expiredJwt());
    const homePath = writeAuth(join(home, "home"), expiredJwt());
    cli.renewTo = { [workPath]: "fresh-work", [homePath]: "fresh-home" };

    const [work, homeToken] = await Promise.all([
      cursorToken({ credentialsPath: workPath }),
      cursorToken({ credentialsPath: homePath }),
    ]);

    expect(work).toBe("fresh-work");
    expect(homeToken).toBe("fresh-home");
    // One renewal per account: sharing a single `cursor-agent status` is what mixed them up.
    expect(cli.statusCalls).toBe(2);
  });

  it("shares one renewal between concurrent callers of the same account", async () => {
    const path = writeAuth(join(home, "solo"), expiredJwt());
    cli.renewTo = { [path]: "fresh-solo" };

    const [first, second] = await Promise.all([
      cursorToken({ credentialsPath: path }),
      cursorToken({ credentialsPath: path }),
    ]);

    expect(first).toBe("fresh-solo");
    expect(second).toBe("fresh-solo");
    expect(cli.statusCalls).toBe(1);
  });

  it("returns a fresh token without renewing", async () => {
    const path = writeAuth(join(home, "fresh"), "still-good");

    expect(await cursorToken({ credentialsPath: path })).toBe("still-good");
    expect(cli.statusCalls).toBe(0);
  });
});
