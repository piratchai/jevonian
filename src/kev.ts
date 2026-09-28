/**
 * `jevonian kev`: guided local deployment of a Kev decision model as the routing brain.
 *
 * Kev (github.com/jaredpalmer/kev, Apache-2.0) serves the same `POST /v1/systemone`
 * contract as TypeSafe Jev, so a local server drops into the `kev` brain channel with no
 * client changes. This module clones the repo into the Jevonian data dir, installs the
 * `serve` extra with uv, manages the server process, and adds the brain to the config.
 *
 * Kev runs through its own `kev.serve` rather than Ollama: each checkpoint is a LoRA
 * adapter plus a pointer head read off the logits in one forward pass, which a GGUF chat
 * runtime cannot serve. On Apple Silicon `kev.serve` uses MLX; on Linux, CUDA or ROCm.
 */
import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";

import { findJevChannel } from "./brain";
import {
  DEFAULT_BRAIN,
  loadConfig,
  parseConfig,
  saveConfig,
  type BrainConfig,
  type Config,
} from "./config";
import { dataDir, kevDir, kevLogPath, kevPidPath } from "./paths";
import { askYesNo, isInteractive } from "./prompt";
import { scrubProxyEnv } from "./proxy";
import { withAugmentedPath } from "./user-path";

const KEV_REPO = "https://github.com/jaredpalmer/kev.git";

/** Default checkpoint: the best accuracy/size trade-off that still runs on a laptop. */
export const DEFAULT_KEV_CHECKPOINT = "jaredpalmer/kev-4b";
export const DEFAULT_KEV_PORT = 8009;

/** Matched against `ps` output so a recycled pid is never mistaken for our server. */
const SERVER_SIGNATURE = "kev.serve";

export function kevBaseUrl(port = DEFAULT_KEV_PORT): string {
  return `http://127.0.0.1:${port}/v1/systemone`;
}

/**
 * Children dial GitHub and Hugging Face directly. A Clash-style `HTTPS_PROXY` resets long
 * downloads (the base model is several GB), the same failure `tunnel.ts` avoids.
 */
function childEnv(): NodeJS.ProcessEnv {
  return withAugmentedPath(scrubProxyEnv(process.env));
}

function run(command: string, args: string[], options: { cwd?: string } = {}): number {
  const result = spawnSync(command, args, { cwd: options.cwd, stdio: "inherit", env: childEnv() });
  return result.status ?? 1;
}

function which(command: string): boolean {
  const result = spawnSync("sh", ["-c", `command -v ${command}`], {
    encoding: "utf8",
    env: childEnv(),
  });
  return result.status === 0 && Boolean((result.stdout ?? "").trim());
}

/** Parses `--port`; undefined when the value is not a usable TCP port. */
export function parseKevPort(raw: string | undefined): number | undefined {
  if (raw === undefined || raw === "") return DEFAULT_KEV_PORT;
  if (!/^\d+$/.test(raw)) return undefined;
  const port = Number.parseInt(raw, 10);
  return port >= 1 && port <= 65_535 ? port : undefined;
}

/** True when `pid` is alive and its command line is a Kev server. */
function isKevProcess(pid: number): boolean {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  if (process.platform === "win32") return true;
  try {
    return execFileSync("ps", ["-ww", "-p", String(pid), "-o", "command="], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).includes(SERVER_SIGNATURE);
  } catch {
    return false;
  }
}

/** The recorded server pid, when that pid still belongs to a Kev server. */
function kevRunningPid(): number | undefined {
  let pid: number;
  try {
    pid = Number.parseInt(readFileSync(kevPidPath(), "utf8").trim(), 10);
  } catch {
    return undefined;
  }
  if (!Number.isFinite(pid) || !isKevProcess(pid)) return undefined;
  return pid;
}

async function kevHealth(port: number): Promise<boolean> {
  try {
    const response = await fetch(`http://127.0.0.1:${port}/v1/models`, {
      signal: AbortSignal.timeout(3_000),
    });
    return response.ok;
  } catch {
    return false;
  }
}

/**
 * Start `kev.serve` detached. There is deliberately no LaunchAgent: the brain is optional
 * infrastructure, and a forgotten launchd job holding several GB of weights is a worse
 * surprise than re-running `jevonian kev --start` after a reboot.
 */
export function startKev(checkpoint: string, port: number): number | undefined {
  const existing = kevRunningPid();
  if (existing) {
    console.log(
      `kev already running (pid ${existing}); stop it first to change checkpoint or port.`,
    );
    return existing;
  }
  const python = join(kevDir(), ".venv", "bin", "python");
  if (!existsSync(python)) {
    console.error(`kev is not installed in ${kevDir()}; run \`jevonian kev\` first.`);
    return undefined;
  }
  mkdirSync(dataDir(), { recursive: true });
  // spawn needs a raw descriptor for stdio; the child keeps its own copy after we close ours.
  const logFd = openSync(kevLogPath(), "a");
  const child = spawn(python, ["-m", "kev.serve", "--run", checkpoint, "--port", String(port)], {
    cwd: kevDir(),
    detached: true,
    stdio: ["ignore", logFd, logFd],
    env: childEnv(),
  });
  closeSync(logFd);
  child.unref();
  if (!child.pid) {
    console.error("failed to start kev.serve");
    return undefined;
  }
  writeFileSync(kevPidPath(), String(child.pid));
  console.log(`kev.serve started (pid ${child.pid}), logging to ${kevLogPath()}`);
  return child.pid;
}

export function stopKev(): void {
  const pid = kevRunningPid();
  if (!pid) {
    console.log("kev is not running.");
  } else {
    // Detached children lead their own process group; signal the group so uvicorn
    // workers go with the server instead of lingering on Linux.
    try {
      process.kill(process.platform === "win32" ? pid : -pid, "SIGTERM");
    } catch {
      try {
        process.kill(pid, "SIGTERM");
      } catch {
        // already gone
      }
    }
    console.log(`stopped kev (pid ${pid})`);
  }
  try {
    unlinkSync(kevPidPath());
  } catch {
    // no pid file
  }
}

export async function kevStatus(port: number): Promise<void> {
  const pid = kevRunningPid();
  const up = await kevHealth(port);
  if (pid && up) console.log(`kev running (pid ${pid}): ${kevBaseUrl(port)}`);
  else if (pid)
    console.log(`kev process ${pid} exists but ${kevBaseUrl(port)} is not answering yet`);
  else if (up)
    console.log(`a server answers on ${kevBaseUrl(port)}, but it was not started by jevonian`);
  else console.log("kev is not running; start it with `jevonian kev --start`");
}

/**
 * Adds a `kev` brain pointing at `port`, or re-points an existing one. With `primary`
 * the Kev brain moves to the front of the failover list, so it decides every routed turn
 * and the hosted channels only answer when it is down.
 */
export function withKevBrain(config: Config, port: number, primary: boolean): Config {
  const preset = findJevChannel("kev");
  const brains = [...config.routing.brains];
  const index = brains.findIndex((brain) => brain.channel === "kev");
  const existing = index >= 0 ? brains[index] : undefined;
  const brain: BrainConfig = {
    ...(existing ?? {
      ...DEFAULT_BRAIN,
      minConfidence: preset?.defaultMinConfidence ?? DEFAULT_BRAIN.minConfidence,
    }),
    channel: "kev",
    baseUrl: kevBaseUrl(port),
    model: existing?.model ?? preset?.model ?? "kev-latest",
  };
  if (index >= 0) brains.splice(index, 1);
  if (primary || index < 0) brains.unshift(brain);
  else brains.splice(index, 0, brain);
  return { ...config, routing: { ...config.routing, brains } };
}

function configureBrain(port: number, primary: boolean): void {
  const config = loadConfig() ?? parseConfig({});
  const hadKev = config.routing.brains.some((brain) => brain.channel === "kev");
  const next = withKevBrain(config, port, primary);
  saveConfig(next);
  const order = next.routing.brains.map((brain) => brain.channel).join(" → ");
  console.log(
    `${hadKev ? "updated" : "added"} the Kev brain (${kevBaseUrl(port)}); brain order: ${order}`,
  );
  console.log("A running Jevonian picks this up on its next restart: `jevonian stop && jevonian`.");
}

function kevHelp(): void {
  console.log(`Usage: jevonian kev [--start] [--stop] [--status] [--run CHECKPOINT] [--port P] [--no-config]

  (no flags)   clone or update kev, install it, ask to start it, add the brain
  --start      also start the server without asking
  --stop       stop the server started by jevonian
  --status     report whether the server answers on --port
  --run        checkpoint: jaredpalmer/kev-4b (default), kev-9b, kev-27b
  --port       server port (default ${DEFAULT_KEV_PORT})
  --no-config  do not add the Kev brain to config.json`);
}

/**
 * Guided setup. Idempotent: an existing checkout is pulled instead of re-cloned, a
 * running server is left alone, and an existing Kev brain is re-pointed, not duplicated.
 */
export async function kevCommand(flags: Record<string, string>): Promise<void> {
  if ("help" in flags || "h" in flags) {
    kevHelp();
    return;
  }
  const port = parseKevPort(flags.port);
  if (port === undefined) {
    console.error(`invalid --port "${flags.port}"; use a number between 1 and 65535.`);
    process.exitCode = 1;
    return;
  }
  if ("stop" in flags) {
    stopKev();
    return;
  }
  if ("status" in flags) {
    await kevStatus(port);
    return;
  }

  const checkpoint = flags.run || DEFAULT_KEV_CHECKPOINT;
  const dir = kevDir();
  mkdirSync(dataDir(), { recursive: true });

  for (const [tool, hint] of [
    ["git", "Install git"],
    ["uv", "Install uv (https://docs.astral.sh/uv/)"],
  ] as const) {
    if (!which(tool)) {
      console.error(`${tool} is required. ${hint}, then re-run \`jevonian kev\`.`);
      process.exitCode = 1;
      return;
    }
  }

  if (existsSync(join(dir, ".git"))) {
    console.log(`updating ${dir}`);
    if (run("git", ["-C", dir, "pull", "--ff-only"]) !== 0) {
      console.error("git pull failed; using the existing checkout.");
    }
  } else {
    console.log(`cloning kev into ${dir}`);
    if (run("git", ["clone", "--depth", "1", KEV_REPO, dir]) !== 0) {
      console.error("clone failed; check network access to github.com.");
      process.exitCode = 1;
      return;
    }
  }

  console.log("installing serve dependencies (uv sync --extra serve)");
  if (run("uv", ["sync", "--extra", "serve"], { cwd: dir }) !== 0) {
    console.error("uv sync failed; see the output above.");
    process.exitCode = 1;
    return;
  }

  const shouldStart =
    "start" in flags || (isInteractive() && (await askYesNo(`Start kev now on :${port}?`, true)));
  if (shouldStart) {
    console.log(
      `The first start downloads ${checkpoint} and its Qwen base model once (~8GB for kev-4b).`,
    );
    const pid = startKev(checkpoint, port);
    if (!pid) {
      process.exitCode = 1;
      return;
    }
    console.log("waiting for /v1/models (up to 5 minutes while weights download)…");
    let up = false;
    for (let attempt = 0; attempt < 150 && !up; attempt += 1) {
      up = await kevHealth(port);
      if (!up) await new Promise((resolve) => setTimeout(resolve, 2_000));
    }
    console.log(
      up ? `kev is up: ${kevBaseUrl(port)}` : `kev has not answered yet; tail ${kevLogPath()}`,
    );
  }

  if ("no-config" in flags) {
    console.log(`\nAdd the brain yourself: channel "Kev (local)", endpoint ${kevBaseUrl(port)}.`);
    return;
  }
  configureBrain(port, true);
}
