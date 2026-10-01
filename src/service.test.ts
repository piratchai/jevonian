import { mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vite-plus/test";

import {
  SERVICE_LABEL,
  assertLiveServicePlist,
  buildServicePlist,
  defaultServicePlistPath,
  installedPlistHasPath,
  isManagedByLaunchd,
  passthroughServiceEnv,
  readInstalledServeEntry,
  resolveServeEntry,
  serviceTakeoverRefusal,
} from "./service";
import { augmentPath } from "./user-path";

it("detects our LaunchAgent via XPC_SERVICE_NAME", () => {
  expect(isManagedByLaunchd({ XPC_SERVICE_NAME: SERVICE_LABEL })).toBe(true);
  expect(isManagedByLaunchd({ XPC_SERVICE_NAME: "other.job" })).toBe(false);
  expect(isManagedByLaunchd({})).toBe(false);
});

it("resolves the CLI entry to an absolute path", () => {
  const dir = mkdtempSync(join(tmpdir(), "jev-service-"));
  try {
    const entry = join(dir, "cli.mjs");
    writeFileSync(entry, "// stub\n");
    const resolved = resolveServeEntry({
      execPath: "/usr/local/bin/node",
      argv1: entry,
    });
    expect(resolved.node).toBe("/usr/local/bin/node");
    expect(resolved.entry).toBe(realpathSync(entry));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

it("builds a KeepAlive LaunchAgent plist with serve arguments", () => {
  const plist = buildServicePlist({
    node: "/opt/homebrew/bin/node",
    entry: "/Users/me/jevonian/dist/cli.mjs",
    logPath: "/Users/me/.local/share/jevonian/serve.log",
    env: { JEVONIAN_CONFIG: "/tmp/config.json", PATH: "/opt/homebrew/bin:/usr/bin:/bin" },
  });
  expect(plist).toContain(`<string>${SERVICE_LABEL}</string>`);
  expect(plist).toContain("<string>/opt/homebrew/bin/node</string>");
  expect(plist).toContain("<string>/Users/me/jevonian/dist/cli.mjs</string>");
  expect(plist).toContain("<string>serve</string>");
  expect(plist).toContain("<key>KeepAlive</key>");
  expect(plist).toContain("<true/>");
  expect(plist).toContain("<key>RunAtLoad</key>");
  expect(plist).toContain("JEVONIAN_NO_OPEN");
  expect(plist).toContain("<string>1</string>");
  expect(plist).toContain("JEVONIAN_CONFIG");
  expect(plist).toContain("/tmp/config.json");
  expect(plist).toContain("<key>PATH</key>");
  expect(plist).toContain("/opt/homebrew/bin:/usr/bin:/bin");
  expect(plist).toContain("/Users/me/.local/share/jevonian/serve.log");
});

it("detects whether an installed plist already bakes PATH", () => {
  const dir = mkdtempSync(join(tmpdir(), "jev-plist-path-"));
  try {
    const without = join(dir, "without.plist");
    const withPath = join(dir, "with.plist");
    writeFileSync(
      without,
      buildServicePlist({
        node: "/opt/homebrew/bin/node",
        entry: "/Users/me/jevonian/dist/cli.mjs",
        logPath: "/tmp/serve.log",
      }),
    );
    writeFileSync(
      withPath,
      buildServicePlist({
        node: "/opt/homebrew/bin/node",
        entry: "/Users/me/jevonian/dist/cli.mjs",
        logPath: "/tmp/serve.log",
        env: { PATH: augmentPath("/usr/bin:/bin") },
      }),
    );
    expect(installedPlistHasPath(without)).toBe(false);
    expect(installedPlistHasPath(withPath)).toBe(true);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

it("recognises the transient launchctl bootstrap EIO message", () => {
  expect(
    /Input\/output error|\bBootstrap failed:\s*5\b/i.test(
      "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.",
    ),
  ).toBe(true);
  expect(
    /Input\/output error|\bBootstrap failed:\s*5\b/i.test(
      "Bootstrap failed: 125: Domain does not support specified action",
    ),
  ).toBe(false);
});

it("escapes XML special characters in plist paths", () => {
  const plist = buildServicePlist({
    node: "/tmp/node&bin",
    entry: "/tmp/cli<x>.mjs",
    logPath: '/tmp/log"out".txt',
  });
  expect(plist).toContain("/tmp/node&amp;bin");
  expect(plist).toContain("/tmp/cli&lt;x&gt;.mjs");
  expect(plist).toContain("/tmp/log&quot;out&quot;.txt");
});

it("forwards selected proxy and path env vars into the agent", () => {
  expect(
    passthroughServiceEnv({
      JEVONIAN_CONFIG: "/c.json",
      JEVONIAN_DATA_DIR: "/data",
      JEVONIAN_LEDGER: "/ledger.jsonl",
      JEVONIAN_WEB_DIR: "/web",
      HTTPS_PROXY: "http://127.0.0.1:1082",
      IGNORED: "nope",
    }),
  ).toEqual({
    // Config / data / ledger must never ride along: a sandboxed shell would bake
    // empty temp paths into the production LaunchAgent.
    JEVONIAN_WEB_DIR: "/web",
    HTTPS_PROXY: "http://127.0.0.1:1082",
  });
});

it("parses ProgramArguments back out of an installed plist", () => {
  const dir = mkdtempSync(join(tmpdir(), "jev-plist-"));
  try {
    const plistPath = join(dir, "ai.jevonian.serve.plist");
    writeFileSync(
      plistPath,
      buildServicePlist({
        node: "/opt/homebrew/bin/node",
        entry: "/Users/me/jevonian/dist/cli.mjs",
        logPath: "/tmp/serve.log",
      }),
    );
    expect(readInstalledServeEntry(plistPath)).toEqual({
      node: "/opt/homebrew/bin/node",
      entry: "/Users/me/jevonian/dist/cli.mjs",
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

describe("serviceTakeoverRefusal", () => {
  const checkout = { node: "/usr/local/bin/node", entry: "/Users/me/checkout/dist/cli.mjs" };
  const global = {
    node: "/Users/me/.vite-plus/node",
    entry: "/Users/me/.vite-plus/lib/node_modules/jevonian/dist/cli.mjs",
  };

  it("allows a fresh install when nothing is installed", () => {
    expect(serviceTakeoverRefusal(checkout, undefined)).toBeUndefined();
  });

  it("allows reinstalling the same install", () => {
    expect(serviceTakeoverRefusal(checkout, checkout)).toBeUndefined();
  });

  it("refuses to repoint another install's service", () => {
    const refusal = serviceTakeoverRefusal(checkout, global);
    expect(refusal).toBeDefined();
    expect(refusal).toContain("Refusing to replace it");
    expect(refusal).toContain(global.entry);
    expect(refusal).toContain("JEVONIAN_SERVICE_TAKEOVER=1");
  });

  it("allows the takeover when forced or the escape hatch is set", () => {
    expect(serviceTakeoverRefusal(checkout, global, { force: true })).toBeUndefined();
    expect(serviceTakeoverRefusal(checkout, global, { env: {} })).toBeDefined();
    expect(
      serviceTakeoverRefusal(checkout, global, { env: { JEVONIAN_SERVICE_TAKEOVER: "1" } }),
    ).toBeUndefined();
    expect(
      serviceTakeoverRefusal(checkout, global, { env: { JEVONIAN_SERVICE_TAKEOVER: "true" } }),
    ).toBeUndefined();
    expect(
      serviceTakeoverRefusal(checkout, global, { env: { JEVONIAN_SERVICE_TAKEOVER: "0" } }),
    ).toBeDefined();
  });
});

describe("assertLiveServicePlist", () => {
  it("allows the production LaunchAgents path", () => {
    expect(() => assertLiveServicePlist(defaultServicePlistPath())).not.toThrow();
  });

  it("refuses a redirected plist so a temp override cannot bootout production", () => {
    expect(() => assertLiveServicePlist("/tmp/ai.jevonian.serve.plist")).toThrow(
      /Refusing to control the live LaunchAgent/,
    );
  });
});
