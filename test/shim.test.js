import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import { mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { assetName, checksumFor, ensureBinary, installEnvironment, main } from "../bin/jevonian.js";

const binary = Buffer.from("native executable fixture");
const sum = createHash("sha256").update(binary).digest("hex");
async function sandbox(fn) {
  const dir = await mkdtemp(join(tmpdir(), "jevonian-shim-"));
  try {
    await fn(dir);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}
function fixtureFetch(calls, bad = false) {
  return async (url) => {
    calls.push(url);
    if (url.endsWith("checksums.txt"))
      return new Response(`${bad ? "0".repeat(64) : sum}  jevonian-linux-amd64\n`);
    return new Response(binary);
  };
}
test("platform naming and checksum manifest match Go release contract", () => {
  assert.equal(assetName("darwin", "arm64"), "jevonian-darwin-arm64");
  assert.equal(assetName("win32", "x64"), "jevonian-windows-amd64.exe");
  assert.throws(() => assetName("linux", "ia32"), /Unsupported/);
  assert.equal(checksumFor(`${sum} *jevonian-linux-amd64\n`, "jevonian-linux-amd64"), sum);
  assert.throws(() => checksumFor(`${sum} wrong\n`, "jevonian-linux-amd64"), /checksum/);
  assert.throws(() => checksumFor(`${sum} asset\n${sum} asset`, "asset"), /duplicate/);
});
test("downloads verified binary, caches by version, repairs tampering", async () =>
  sandbox(async (dir) => {
    const calls = [];
    const options = {
      packageRoot: dir,
      version: "0.5.4",
      platform: "linux",
      arch: "x64",
      releaseBase: "https://release.test",
      fetchImpl: fixtureFetch(calls),
    };
    const path = await ensureBinary(options);
    assert.deepEqual(await readFile(path), binary);
    assert.deepEqual(calls, [
      "https://release.test/v0.5.4/checksums.txt",
      "https://release.test/v0.5.4/jevonian-linux-amd64",
    ]);
    await ensureBinary({
      ...options,
      fetchImpl: () => {
        throw new Error("cache made network call");
      },
    });
    await writeFile(path, "tampered");
    await ensureBinary(options);
    assert.deepEqual(await readFile(path), binary);
    assert.equal(calls.length, 4);
  }));
test("checksum failure leaves old executable untouched and no temporary files", async () =>
  sandbox(async (dir) => {
    const options = {
      packageRoot: dir,
      version: "0.5.4",
      platform: "linux",
      arch: "x64",
      fetchImpl: fixtureFetch([]),
    };
    const path = await ensureBinary(options);
    await assert.rejects(
      ensureBinary({ ...options, version: "0.5.5", fetchImpl: fixtureFetch([], true) }),
      /checksum mismatch/,
    );
    assert.deepEqual(await readFile(path), binary);
    assert.equal((await readdir(join(dir, "native"))).length, 2);
    await assert.rejects(
      ensureBinary({ ...options, version: "../../etc" }),
      /Invalid package version/,
    );
  }));
test("registry failure and unsupported platform never spawn a process", async () =>
  sandbox(async (dir) => {
    await assert.rejects(
      ensureBinary({
        packageRoot: dir,
        version: "0.5.4",
        platform: "linux",
        arch: "x64",
        fetchImpl: async () => new Response("missing", { status: 404 }),
      }),
      /404/,
    );
  }));
test("local dependency and source execution do not attest a global install", () => {
  const local = installEnvironment({
    entry: "/work/jevonian/bin/jevonian.js",
    nodeExecutable: "/node",
    env: {},
  });
  assert.equal(local.JEVONIAN_INSTALL_CHANNEL, "unknown");
  const npm = installEnvironment({
    entry: "/prefix/lib/node_modules/jevonian/bin/jevonian.js",
    nodeExecutable: "/prefix/bin/node",
    env: {},
  });
  assert.equal(npm.JEVONIAN_INSTALL_CHANNEL, "npm");
  assert.equal(npm.JEVONIAN_NODE_EXECUTABLE, "/prefix/bin/node");
  const pnpm = installEnvironment({
    entry:
      "/home/me/.local/share/pnpm/global/5/node_modules/.pnpm/jevonian@0.5.4/node_modules/jevonian/bin/jevonian.js",
    nodeExecutable: "/node",
    env: {},
  });
  assert.equal(pnpm.JEVONIAN_INSTALL_CHANNEL, "pnpm");
});
test("thin launcher preserves argv, stdio, injected environment and exit status", async () =>
  sandbox(async (dir) => {
    await writeFile(join(dir, "package.json"), JSON.stringify({ version: "0.5.4" }));
    const entry = join(dir, "jevonian.js");
    await writeFile(entry, "fixture");
    let spawned = 0;
    const code = await main({
      entry,
      packageRoot: dir,
      argv: ["serve", "--foreground"],
      nodeExecutable: "/node",
      env: { CUSTOM: "kept" },
      platform: "linux",
      arch: "x64",
      fetchImpl: fixtureFetch([]),
      spawnImpl: (path, argv, options) => {
        spawned++;
        assert.equal(path, join(dir, "native", "jevonian-linux-amd64"));
        assert.deepEqual(argv, ["serve", "--foreground"]);
        assert.equal(options.stdio, "inherit");
        assert.equal(options.env.CUSTOM, "kept");
        assert.equal(options.env.JEVONIAN_INSTALL_CHANNEL, "unknown");
        const child = new EventEmitter();
        queueMicrotask(() => child.emit("exit", 7, null));
        return child;
      },
    });
    assert.equal(code, 7);
    assert.equal(spawned, 1);
    assert.equal(
      await main({
        entry,
        packageRoot: dir,
        argv: ["--download-only"],
        platform: "linux",
        arch: "x64",
        fetchImpl: () => {
          throw new Error("already cached");
        },
        spawnImpl: () => {
          throw new Error("download spawned");
        },
      }),
      0,
    );
  }));
