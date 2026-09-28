import { spawn } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { connect } from "node:net";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// Dev ports live far from the production defaults (8787 api / 5173 web) so `npm run dev`
// never collides with a running jevonian or another vite dev server.
const APP_PORT = Number(process.env.JEVONIAN_PORT ?? 18888);
const WEB_PORT = Number(process.env.JEVONIAN_WEB_PORT ?? 15174);
const WEB_DEV_URL = `http://127.0.0.1:${WEB_PORT}`;

const children = [];
let shuttingDown = false;

// The dev instance gets its own config and browser-state file so `npm run dev` can save
// routings / providers / keys without touching a running production jevonian.
const DEV_CACHE = join(process.env.XDG_CACHE_HOME ?? join(homedir(), ".cache"), "jevonian");
const DEV_DATA = join(DEV_CACHE, "data-dev");
const DEV_CONFIG = join(DEV_CACHE, "config-dev.json");
const DEV_BROWSER_STATE = join(DEV_CACHE, "browser-state-dev.json");

// Seed the dev config from the real one on first run, so the dashboard has providers to
// show. Tunnel is force-disabled and the listen port overridden to the dev port — the
// real `publicPort` and `listen.port` must never leak into the dev file, or the dev
// instance would collide with the running service on port+1.
if (!existsSync(DEV_CONFIG)) {
  try {
    mkdirSync(dirname(DEV_CONFIG), { recursive: true });
    const base = process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config");
    const realConfig = join(base, "jevonian", "config.json");
    if (existsSync(realConfig)) {
      const seeded = JSON.parse(readFileSync(realConfig, "utf8"));
      seeded.listen = { ...seeded.listen, host: "127.0.0.1", port: APP_PORT };
      if (seeded.tunnel) {
        delete seeded.tunnel.publicPort;
        seeded.tunnel.enabled = false;
      }
      writeFileSync(DEV_CONFIG, JSON.stringify(seeded, null, 2));
    }
  } catch {
    // best-effort — a missing seed just means the dev UI starts empty
  }
}

// Fresh `pnpm dev` should open the dashboard once; HMR restarts keep the marker
// so they reuse the same tab instead of stacking a new one on every edit.
try {
  rmSync(DEV_BROWSER_STATE, { force: true });
} catch {
  // best-effort
}

function run(command, args, env = {}) {
  const child = spawn(command, args, {
    stdio: "inherit",
    cwd: REPO_ROOT,
    env: { ...process.env, ...env },
  });
  child.on("error", (error) => {
    console.error(`failed to start ${command}: ${String(error)}`);
    shutdown(1);
  });
  child.on("exit", (code) => shutdown(code ?? 0));
  children.push(child);
  return child;
}

function shutdown(code = 0) {
  if (shuttingDown) return;
  shuttingDown = true;
  for (const child of children) {
    if (!child.killed) child.kill("SIGTERM");
  }
  setTimeout(() => process.exit(code), 200);
}

process.on("SIGINT", () => shutdown(0));
process.on("SIGTERM", () => shutdown(0));

function portInUse(port) {
  return new Promise((resolve) => {
    const socket = connect({ host: "127.0.0.1", port });
    socket.once("connect", () => {
      socket.destroy();
      resolve(true);
    });
    socket.once("error", () => resolve(false));
  });
}

if (await portInUse(APP_PORT)) {
  console.error(`port ${APP_PORT} is already in use — stop the other Jevonian instance first.`);
  process.exit(1);
}

// `vp` is a local devDep, not on the daemon PATH — resolve it through the repo root so
// `launchctl` / `npm run dev` both find the same binary.
const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const VP_BIN = join(REPO_ROOT, "node_modules", ".bin", "vp");
const TSX_BIN = join(REPO_ROOT, "node_modules", ".bin", "tsx");
const CLI_ENTRY = join(REPO_ROOT, "src", "cli.ts");

run(VP_BIN, ["-C", "web", "dev"]);

setTimeout(() => {
  run(TSX_BIN, ["watch", CLI_ENTRY, "serve", "--foreground", ...process.argv.slice(2)], {
    JEVONIAN_WEB_DEV: WEB_DEV_URL,
    JEVONIAN_PORT: String(APP_PORT),
    JEVONIAN_CONFIG: DEV_CONFIG,
    JEVONIAN_DATA_DIR: DEV_DATA,
    JEVONIAN_BROWSER_STATE: DEV_BROWSER_STATE,
  });
}, 1_200);
