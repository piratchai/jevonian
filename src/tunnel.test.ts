import type { ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vite-plus/test";

import { normalizeTunnelUrl, parseTunnel } from "./config";
import {
  buildTunnelCommand,
  commandMatches,
  extractTunnelUrl,
  TunnelManager,
  type TunnelHost,
} from "./tunnel";
import { augmentPath } from "./user-path";

describe("normalizeTunnelUrl / parseTunnel", () => {
  it("accepts full URLs and bare hostnames", () => {
    expect(normalizeTunnelUrl("https://casqued.ngrok-free.dev/")).toBe(
      "https://casqued.ngrok-free.dev",
    );
    expect(normalizeTunnelUrl("casqued-dominique-memorably.ngrok-free.dev")).toBe(
      "https://casqued-dominique-memorably.ngrok-free.dev",
    );
    expect(normalizeTunnelUrl("not a url")).toBeUndefined();
    expect(parseTunnel({ enabled: true, provider: "ngrok", url: "my.ngrok-free.dev" })).toEqual({
      enabled: true,
      provider: "ngrok",
      url: "https://my.ngrok-free.dev",
    });
    expect(parseTunnel({ enabled: false, provider: "ngrok", url: "" })).toEqual({
      enabled: false,
      provider: "ngrok",
    });
  });
});

describe("extractTunnelUrl", () => {
  it("finds provider URLs in process output", () => {
    expect(
      extractTunnelUrl(
        "cloudflare",
        "2026-09-19 INF |  https://plain-iris-tunnel.trycloudflare.com  |",
      ),
    ).toBe("https://plain-iris-tunnel.trycloudflare.com");
    expect(
      extractTunnelUrl(
        "ngrok",
        't=2026-09-19 level=info msg="url=https://ab12-1-2-3.ngrok-free.app"',
      ),
    ).toBe("https://ab12-1-2-3.ngrok-free.app");
    expect(
      extractTunnelUrl(
        "ngrok",
        't=2026-09-21 level=info msg="url=https://casqued-dominique-memorably.ngrok-free.dev"',
      ),
    ).toBe("https://casqued-dominique-memorably.ngrok-free.dev");
    expect(extractTunnelUrl("custom", "listening at https://bore.example.com:8080 now")).toBe(
      "https://bore.example.com:8080",
    );
    expect(extractTunnelUrl("cloudflare", "no url here")).toBeUndefined();
  });
});

describe("buildTunnelCommand", () => {
  it("builds provider commands", () => {
    expect(buildTunnelCommand({ enabled: true, provider: "cloudflare" }, 8788)).toEqual({
      command: "cloudflared",
      args: ["tunnel", "--url", "http://127.0.0.1:8788", "--no-autoupdate"],
    });
    expect(buildTunnelCommand({ enabled: true, provider: "ngrok" }, 8788)).toEqual({
      command: "ngrok",
      args: ["http", "127.0.0.1:8788", "--log", "stdout"],
    });
    expect(
      buildTunnelCommand(
        {
          enabled: true,
          provider: "ngrok",
          url: "https://casqued-dominique-memorably.ngrok-free.dev",
        },
        8788,
      ),
    ).toEqual({
      command: "ngrok",
      args: [
        "http",
        "127.0.0.1:8788",
        "--url",
        "https://casqued-dominique-memorably.ngrok-free.dev",
        "--log",
        "stdout",
      ],
    });
    expect(
      buildTunnelCommand(
        { enabled: true, provider: "custom", command: "bore local {port} --to bore.pub" },
        8788,
      ),
    ).toEqual({ command: "sh", args: ["-c", "bore local 8788 --to bore.pub"] });
    expect(buildTunnelCommand({ enabled: true, provider: "custom" }, 8788)).toEqual({
      error: "custom tunnel needs a command",
    });
  });
});

interface FakeChild {
  child: ChildProcess;
  stdout: EventEmitter;
  stderr: EventEmitter;
  exit: (code: number) => void;
}

function fakeSpawn(pid = 0): {
  spawn: (command: string, args: string[], options?: { env?: NodeJS.ProcessEnv }) => ChildProcess;
  last: () => FakeChild | undefined;
  lastEnv: () => NodeJS.ProcessEnv | undefined;
  lastArgs: () => { command: string; args: string[] } | undefined;
} {
  let last: FakeChild | undefined;
  let lastEnv: NodeJS.ProcessEnv | undefined;
  let lastArgs: { command: string; args: string[] } | undefined;
  const spawn = (
    command: string,
    args: string[],
    options?: { env?: NodeJS.ProcessEnv },
  ): ChildProcess => {
    lastEnv = options?.env;
    lastArgs = { command, args };
    const child = new EventEmitter() as unknown as ChildProcess;
    const stdout = new EventEmitter();
    const stderr = new EventEmitter();
    Object.assign(child, {
      pid: pid || undefined,
      stdout,
      stderr,
      killed: false,
      kill: () => Object.assign(child, { killed: true }),
    });
    last = { child, stdout, stderr, exit: (code) => child.emit("exit", code) };
    return child;
  };
  return { spawn, last: () => last, lastEnv: () => lastEnv, lastArgs: () => lastArgs };
}

describe("TunnelManager", () => {
  it("reports the URL from process output and stops cleanly", () => {
    const fake = fakeSpawn();
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
    });
    expect(manager.status().status).toBe("off");
    manager.start();
    expect(manager.status().status).toBe("starting");
    const child = fake.last();
    child?.stdout.emit("data", Buffer.from("https://cool-name.trycloudflare.com\n"));
    expect(manager.status()).toMatchObject({
      status: "on",
      url: "https://cool-name.trycloudflare.com",
      publicPort: 8788,
    });
    manager.stop();
    expect(manager.status().status).toBe("off");
    child?.exit(0);
    expect(manager.status().status).toBe("off");
  });

  it("reports an error when the process exits before a url", () => {
    const fake = fakeSpawn();
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
    });
    manager.start();
    fake.last()?.exit(1);
    expect(manager.status().status).toBe("error");
    expect(manager.status().error).toContain("exited");
  });

  it("does not hand HTTPS_PROXY to the tunnel child", () => {
    const previous = process.env.HTTPS_PROXY;
    process.env.HTTPS_PROXY = "http://127.0.0.1:1082";
    try {
      const fake = fakeSpawn();
      const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
        spawnFn: fake.spawn as never,
        timeoutMs: 1_000,
      });
      manager.start();
      expect(fake.lastEnv()?.HTTPS_PROXY).toBeUndefined();
      expect(fake.lastEnv()?.PATH).toBe(augmentPath(process.env.PATH));
    } finally {
      if (previous === undefined) delete process.env.HTTPS_PROXY;
      else process.env.HTTPS_PROXY = previous;
    }
  });

  it("finds Homebrew bin dirs even under a launchd PATH", () => {
    const previous = process.env.PATH;
    process.env.PATH = "/usr/bin:/bin:/usr/sbin:/sbin";
    try {
      const fake = fakeSpawn();
      const manager = new TunnelManager({ enabled: true, provider: "ngrok" }, 8787, {
        spawnFn: fake.spawn as never,
        timeoutMs: 1_000,
      });
      manager.start();
      expect(fake.lastEnv()?.PATH).toBe(augmentPath("/usr/bin:/bin:/usr/sbin:/sbin"));
    } finally {
      process.env.PATH = previous;
    }
  });

  it("uses a configured static url for stable hostnames", () => {
    const fake = fakeSpawn();
    const manager = new TunnelManager(
      {
        enabled: true,
        provider: "custom",
        command: "cloudflared tunnel run my-named-tunnel",
        url: "https://ai.example.com",
      },
      8787,
      { spawnFn: fake.spawn as never, timeoutMs: 1_000 },
    );
    manager.start();
    expect(manager.status()).toMatchObject({
      status: "on",
      url: "https://ai.example.com",
    });
  });

  it("binds an ngrok reserved domain when url is set", () => {
    const fake = fakeSpawn();
    const domain = "https://casqued-dominique-memorably.ngrok-free.dev";
    const manager = new TunnelManager({ enabled: true, provider: "ngrok", url: domain }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
    });
    manager.start();
    expect(manager.status()).toMatchObject({ status: "on", url: domain });
    expect(fake.lastArgs()).toEqual({
      command: "ngrok",
      args: ["http", "127.0.0.1:8788", "--url", domain, "--log", "stdout"],
    });
  });

  it("ignores a leftover url for cloudflare quick tunnels", () => {
    const fake = fakeSpawn();
    const manager = new TunnelManager(
      {
        enabled: true,
        provider: "cloudflare",
        url: "https://stale.ngrok-free.dev",
      },
      8787,
      { spawnFn: fake.spawn as never, timeoutMs: 1_000 },
    );
    manager.start();
    expect(manager.status().status).toBe("starting");
    expect(manager.status().url).toBeUndefined();
  });

  it("keeps the custom command error without spawning", () => {
    const fake = fakeSpawn();
    const manager = new TunnelManager({ enabled: true, provider: "custom" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
    });
    manager.start();
    expect(manager.status().status).toBe("error");
    expect(manager.status().error).toContain("needs a command");
    expect(fake.last()).toBeUndefined();
  });
});

interface FakeHost extends TunnelHost {
  terminated: number[];
}

function fakeHost(alive = true): FakeHost {
  const terminated: number[] = [];
  return {
    terminated,
    alive: () => alive,
    identify: () => true,
    terminate: (pid) => {
      terminated.push(pid);
    },
  };
}

function recordPath(): string {
  return join(mkdtempSync(join(tmpdir(), "jevonian-tunnel-")), "tunnel-state.json");
}

describe("TunnelManager restart adoption", () => {
  it("reuses a live tunnel instead of spawning a new one", () => {
    const statePath = recordPath();
    writeFileSync(
      statePath,
      JSON.stringify({
        pid: 4242,
        provider: "cloudflare",
        publicPort: 8788,
        command: "cloudflared tunnel --url http://127.0.0.1:8788 --no-autoupdate",
        url: "https://steady-name.trycloudflare.com",
        startedAt: "2026-09-20T00:00:00.000Z",
      }),
    );
    const fake = fakeSpawn();
    const host = fakeHost();
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    expect(manager.start()).toMatchObject({
      status: "on",
      url: "https://steady-name.trycloudflare.com",
      startedAt: "2026-09-20T00:00:00.000Z",
    });
    expect(fake.last()).toBeUndefined();
    manager.stop();
    expect(host.terminated).toEqual([4242]);
    expect(existsSync(statePath)).toBe(false);
  });

  it("replaces a stale record when the process is gone", () => {
    const statePath = recordPath();
    writeFileSync(
      statePath,
      JSON.stringify({
        pid: 4242,
        provider: "cloudflare",
        publicPort: 8788,
        command: "cloudflared tunnel --url http://127.0.0.1:8788 --no-autoupdate",
        url: "https://dead-name.trycloudflare.com",
        startedAt: "2026-09-20T00:00:00.000Z",
      }),
    );
    const fake = fakeSpawn();
    const host = fakeHost(false);
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    manager.start();
    expect(fake.last()).toBeDefined();
    expect(manager.status().status).toBe("starting");
    expect(host.terminated).toEqual([]);
  });

  it("restarts when the provider or port no longer matches the record", () => {
    const statePath = recordPath();
    writeFileSync(
      statePath,
      JSON.stringify({
        pid: 4242,
        provider: "ngrok",
        publicPort: 9999,
        command: "ngrok http 9999 --log stdout",
        url: "https://old.ngrok-free.app",
        startedAt: "2026-09-20T00:00:00.000Z",
      }),
    );
    const fake = fakeSpawn();
    const host = fakeHost();
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    manager.start();
    expect(fake.last()).toBeDefined();
    expect(host.terminated).toEqual([]);
  });

  it("writes the url into the record and clears it on explicit stop", () => {
    const statePath = recordPath();
    const fake = fakeSpawn(12345);
    const host = fakeHost();
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    manager.start();
    fake.last()?.stdout.emit("data", Buffer.from("https://fresh-name.trycloudflare.com\n"));
    expect(JSON.parse(readFileSync(statePath, "utf8"))).toMatchObject({
      provider: "cloudflare",
      publicPort: 8788,
      url: "https://fresh-name.trycloudflare.com",
    });
    manager.stop();
    expect(existsSync(statePath)).toBe(false);
  });
});

describe("commandMatches", () => {
  it("matches the sh -c wrapper against the exec'd command line", () => {
    // `sh -c cloudflared …` execs into cloudflared; `ps` never shows the wrapper.
    const recorded = "sh -c cloudflared tunnel --config /home/u/t.yml run";
    const real = "cloudflared tunnel --config /home/u/t.yml run";
    expect(commandMatches(real, recorded)).toBe(true);
    expect(commandMatches(recorded, recorded)).toBe(true);
  });

  it("matches quoted and exec-prefixed wrappers", () => {
    expect(
      commandMatches("bore local 8788 --to bore.pub", 'sh -c "bore local 8788 --to bore.pub"'),
    ).toBe(true);
    expect(commandMatches("cloudflared tunnel run", "sh -c exec cloudflared tunnel run")).toBe(
      true,
    );
  });

  it("does not match a different command that shares a prefix", () => {
    // A shorter configured command must not match a longer unrelated invocation.
    expect(commandMatches("cloudflared tunnel --config other.yml run", "cloudflared tunnel")).toBe(
      false,
    );
    expect(commandMatches("ngrok http 9999 --log stdout", "sh -c ngrok http 8788")).toBe(false);
  });
});

describe("TunnelManager orphan sweep", () => {
  it("reaps a stale process running the configured command on start", () => {
    const statePath = recordPath();
    const fake = fakeSpawn(7777);
    const terminated: number[] = [];
    const host: FakeHost = {
      terminated,
      alive: () => false, // no recorded pid alive — orphan is not the record's pid
      identify: () => false,
      terminate: (pid) => {
        terminated.push(pid);
      },
      // An orphan cloudflared from a crashed serve still runs the exact command.
      find: (command) => (command.includes("cloudflared") ? [5150] : []),
    };
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    manager.start();
    // The orphan was swept before the new spawn.
    expect(terminated).toEqual([5150]);
    expect(fake.last()).toBeDefined();
    manager.stop();
  });

  it("does not kill an unrelated process that only shares a prefix", () => {
    const statePath = recordPath();
    const fake = fakeSpawn(7777);
    const terminated: number[] = [];
    const host: FakeHost = {
      terminated,
      alive: () => false,
      identify: () => false,
      terminate: (pid) => {
        terminated.push(pid);
      },
      // Some other tunnel on a different config — the find impl only reports a
      // pid when the host's own matcher says the line equals the command, so a
      // prefix-only match returns nothing here.
      find: () => [],
    };
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    manager.start();
    expect(terminated).toEqual([]);
    manager.stop();
  });

  it("reaps duplicates but keeps the adopted tunnel on restart", () => {
    // The leak this sweep exists for: earlier restarts left duplicates of the same command
    // running beside the recorded one. Adoption alone kept them all alive.
    const statePath = recordPath();
    writeFileSync(
      statePath,
      JSON.stringify({
        pid: 4242,
        provider: "cloudflare",
        publicPort: 8788,
        command: "cloudflared tunnel --url http://127.0.0.1:8788 --no-autoupdate",
        url: "https://steady-name.trycloudflare.com",
        startedAt: "2026-09-20T00:00:00.000Z",
      }),
    );
    const fake = fakeSpawn();
    const terminated: number[] = [];
    const host: FakeHost = {
      terminated,
      alive: () => true,
      identify: () => true,
      terminate: (pid) => {
        terminated.push(pid);
      },
      find: () => [4242, 5150, 5151],
    };
    const manager = new TunnelManager({ enabled: true, provider: "cloudflare" }, 8787, {
      spawnFn: fake.spawn as never,
      timeoutMs: 1_000,
      statePath,
      host,
    });
    expect(manager.start()).toMatchObject({ status: "on" });
    expect(fake.last()).toBeUndefined();
    expect(terminated).toEqual([5150, 5151]);
  });
});
