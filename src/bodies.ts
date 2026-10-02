import { existsSync, readFileSync } from "node:fs";
import { mkdir, readdir, rm, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";

import { dataDir } from "./paths";

const MAX_BODIES = 1_000;
const PRUNE_EVERY = 100;

/**
 * Captures written since the last sweep, and captures currently queued.
 *
 * A sweep only runs once nothing is queued: the directory is stable at that point, so a burst
 * of captures cannot slip past the cap by trimming the first hundred and then writing the
 * rest. In steady state (one capture per request) that is still one sweep per ~100 writes.
 */
let sincePrune = 0;
let queued = 0;

/**
 * Queued writes, drained in order so captures land oldest-first and a sweep sees a consistent
 * directory. Every link swallows its error: these promises are never awaited by the caller (a
 * request must not fail because a diagnostic dump did), so an unhandled rejection here would
 * otherwise take the serve process down.
 */
let writeChain: Promise<void> = Promise.resolve();

export function bodiesDir(): string {
  return join(dataDir(), "bodies");
}

export function isSafeBodyId(id: string): boolean {
  return /^[0-9a-fA-F-]{8,64}$/.test(id);
}

function captureEnabled(): boolean {
  return process.env.JEVONIAN_CAPTURE_BODIES !== "0";
}

/**
 * Dumps a request/response payload for the log-detail view.
 *
 * Asynchronous on purpose. The caller sits on the turn's critical path, and under many
 * concurrent sessions a synchronous `writeFileSync` of a ~0.5 MB body serialized every session
 * behind every other session's disk write — a capture for one turn blocked the event loop all
 * the others were streaming through. The write is queued and returns immediately; `loadBody`
 * sees it once it lands, and the log detail page only ever reads after the turn has finished.
 */
export function saveBody(id: string, payload: unknown): void {
  if (!captureEnabled() || !isSafeBodyId(id)) return;
  let text: string;
  try {
    text = JSON.stringify(payload);
  } catch {
    return;
  }
  // Resolve the directory at call time, not inside the queued task: a test (or a restart) that
  // repoints JEVONIAN_DATA_DIR between the call and the microtask must not split the mkdir from
  // the path it writes to.
  const dir = bodiesDir();
  const path = join(dir, `${id}.json`);
  queued += 1;
  writeChain = writeChain
    .then(async () => {
      await mkdir(dir, { recursive: true });
      await writeFile(path, text, { mode: 0o600 });
    })
    .catch(() => {})
    .finally(async () => {
      queued -= 1;
      sincePrune += 1;
      if (queued === 0 && sincePrune >= PRUNE_EVERY) {
        sincePrune = 0;
        await pruneBodies(dir);
      }
    });
}

export function loadBody(id: string): unknown {
  if (!isSafeBodyId(id)) return undefined;
  const path = join(bodiesDir(), `${id}.json`);
  if (!existsSync(path)) return undefined;
  try {
    return JSON.parse(readFileSync(path, "utf8")) as unknown;
  } catch {
    return undefined;
  }
}

/**
 * Waits for queued captures to hit disk.
 *
 * Tests and shutdown paths need a deterministic point at which `saveBody` has landed, since
 * the write is fire-and-forget on the request path. Never throws.
 */
export function flushBodies(): Promise<void> {
  return writeChain.catch(() => {});
}

/**
 * Oldest-first trim of the capture directory.
 *
 * Async for the same reason as the write: the live directory holds ~1k files, and the old
 * `readdirSync` + `statSync`-per-entry sweep blocked the event loop for a few milliseconds
 * every hundred captures, which is exactly when many sessions are in flight.
 */
async function pruneBodies(dir: string): Promise<void> {
  try {
    const names = (await readdir(dir)).filter((name) => name.endsWith(".json"));
    if (names.length <= MAX_BODIES) return;
    const entries = await Promise.all(
      names.map(async (name) => ({ name, at: (await stat(join(dir, name))).mtimeMs })),
    );
    entries.sort((left, right) => left.at - right.at);
    for (const entry of entries.slice(0, Math.max(0, entries.length - MAX_BODIES))) {
      await rm(join(dir, entry.name), { force: true });
    }
  } catch {
    return;
  }
}
