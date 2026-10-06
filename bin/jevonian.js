#!/usr/bin/env node
// The npm entry is a fetcher and exec wrapper: it downloads the verified native
// release for this platform and execs it. It never runs a router in Node.
import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { chmod, mkdir, readFile, realpath, rename, rm, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const RELEASE_BASE = "https://github.com/xinyao27/jevonian/releases/download";
const MAX_BINARY_BYTES = 128 * 1024 * 1024;

export function assetName(platform, arch) {
  const architectures = { x64: "amd64", arm64: "arm64" };
  if (!["darwin", "linux", "win32"].includes(platform) || !architectures[arch]) {
    throw new Error(`Unsupported native platform: ${platform}/${arch}`);
  }
  return `jevonian-${platform === "win32" ? "windows" : platform}-${architectures[arch]}${platform === "win32" ? ".exe" : ""}`;
}

export function checksumFor(manifest, asset) {
  const lines = manifest.split(/\r?\n/).map((line) => line.trim().split(/\s+/));
  const matches = lines.filter(
    (parts) => parts.length === 2 && parts[1].replace(/^\*/, "") === asset,
  );
  if (matches.length !== 1 || !/^[a-f\d]{64}$/i.test(matches[0][0])) {
    throw new Error(`Missing, duplicate, or invalid release checksum for ${asset}`);
  }
  return matches[0][0].toLowerCase();
}

async function getBytes(fetchImpl, url, limit) {
  const parsed = new URL(url);
  if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password)
    throw new Error("Invalid release URL");
  const response = await fetchImpl(url, {
    signal: AbortSignal.timeout(120_000),
    headers: { "user-agent": "jevonian-npm-shim" },
  });
  if (!response.ok) throw new Error(`Release download returned ${response.status}`);
  if (Number(response.headers.get("content-length")) > limit) {
    await response.body?.cancel();
    throw new Error("Release download is too large");
  }
  if (!response.body) throw new Error("Empty release response");
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.length;
      if (size > limit) throw new Error("Release download is too large");
      chunks.push(Buffer.from(value));
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
  return Buffer.concat(chunks);
}

// All network, process and package inputs are injectable; tests only use temp dirs.
export async function ensureBinary({
  packageRoot,
  version,
  platform = process.platform,
  arch = process.arch,
  fetchImpl = fetch,
  releaseBase = RELEASE_BASE,
}) {
  if (!/^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(version))
    throw new Error("Invalid package version");
  const asset = assetName(platform, arch);
  const directory = join(packageRoot, "native");
  const target = join(directory, asset);
  const marker = `${target}.json`;
  try {
    const state = JSON.parse(await readFile(marker, "utf8"));
    const bytes = await readFile(target);
    if (
      state.version === version &&
      createHash("sha256").update(bytes).digest("hex") === state.sha256
    )
      return target;
  } catch {
    /* Missing or stale cache: download and verify before replacing it. */
  }
  const base = `${releaseBase.replace(/\/$/, "")}/v${version}/`;
  const manifest = await getBytes(fetchImpl, `${base}checksums.txt`, 1024 * 1024);
  const expected = checksumFor(manifest.toString("utf8"), asset);
  const bytes = await getBytes(fetchImpl, `${base}${asset}`, MAX_BINARY_BYTES);
  if (!bytes.length || createHash("sha256").update(bytes).digest("hex") !== expected)
    throw new Error("Release binary checksum mismatch");
  await mkdir(directory, { recursive: true });
  const temporary = `${target}.${randomUUID()}.tmp`;
  const markerTemp = `${marker}.${randomUUID()}.tmp`;
  try {
    await writeFile(temporary, bytes, { mode: 0o755, flag: "wx" });
    await chmod(temporary, 0o755);
    await rename(temporary, target);
    await writeFile(markerTemp, `${JSON.stringify({ version, sha256: expected })}\n`, {
      mode: 0o600,
      flag: "wx",
    });
    await rename(markerTemp, marker);
  } finally {
    await Promise.all([rm(temporary, { force: true }), rm(markerTemp, { force: true })]);
  }
  return target;
}

export function installEnvironment({ entry, nodeExecutable, env = process.env }) {
  const path = entry.replaceAll("\\", "/");
  const global =
    path.includes("/lib/node_modules/jevonian/") ||
    path.includes("/pnpm/global/") ||
    path.includes("/AppData/Roaming/npm/node_modules/jevonian/") ||
    (env.npm_config_global === "true" && path.includes("/node_modules/jevonian/"));
  const channel = global
    ? path.includes("/pnpm/") || path.includes("/.pnpm/")
      ? "pnpm"
      : "npm"
    : "unknown";
  return {
    ...env,
    JEVONIAN_NPM_ENTRY: entry,
    JEVONIAN_NODE_EXECUTABLE: nodeExecutable,
    JEVONIAN_INSTALL_GLOBAL: global ? "1" : "0",
    JEVONIAN_INSTALL_CHANNEL: channel,
  };
}

export async function main({
  entry = fileURLToPath(import.meta.url),
  argv = process.argv.slice(2),
  nodeExecutable = process.execPath,
  env = process.env,
  spawnImpl = spawn,
  fetchImpl = fetch,
  platform = process.platform,
  arch = process.arch,
  packageRoot,
  releaseBase = RELEASE_BASE,
} = {}) {
  entry = await realpath(entry);
  packageRoot ??= dirname(dirname(entry));
  const { version } = JSON.parse(await readFile(join(packageRoot, "package.json"), "utf8"));
  const target = await ensureBinary({
    packageRoot,
    version,
    fetchImpl,
    platform,
    arch,
    releaseBase,
  });
  if (argv.length === 1 && argv[0] === "--download-only") return 0;
  const child = spawnImpl(target, argv, {
    stdio: "inherit",
    env: installEnvironment({ entry, nodeExecutable, env }),
  });
  // Node has no portable exec(2); forwarding stdio, signals and the exit status is
  // the thin wrapper equivalent. Managed services should launch the binary directly.
  const signals = platform === "win32" ? [] : ["SIGINT", "SIGTERM", "SIGHUP"];
  const handlers = signals.map((signal) => {
    const fn = () => child.kill(signal);
    process.on(signal, fn);
    return [signal, fn];
  });
  try {
    return await new Promise((resolvePromise, reject) => {
      child.once("error", reject);
      child.once("exit", (code, signal) =>
        resolvePromise(code ?? { SIGINT: 130, SIGTERM: 143, SIGHUP: 129 }[signal] ?? 1),
      );
    });
  } finally {
    for (const [signal, fn] of handlers) process.off(signal, fn);
  }
}

if (
  process.argv[1] &&
  (await realpath(resolve(process.argv[1])).catch(() => "")) === fileURLToPath(import.meta.url)
) {
  main().then(
    (code) => {
      process.exitCode = code;
    },
    (error) => {
      console.error(`jevonian: ${error.message}`);
      process.exitCode = 1;
    },
  );
}
