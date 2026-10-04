/**
 * Layered timeouts for upstream model requests.
 *
 * A single attempt against a wedged provider used to have no ceiling at all: the ledger shows
 * `workbuddy-ai-subscription` attempts of 70–100s each, and a stream that never produced its
 * first byte lingering for 23 minutes (`latencyMs` 1,397,018) before the soft close fired. A
 * coding-agent turn cannot afford either — the whole point of the router is to notice a bad
 * host early and move the turn to a healthy one.
 *
 * Four independent clocks bound an attempt:
 *
 * - `connectMs` — the TCP/TLS (or proxy CONNECT) handshake, enforced by undici.
 * - `headersMs` — time to the response status line, enforced by undici and, for a stream, also
 *   by an abort signal so a hung host fails before anything is committed to the client.
 * - `totalMs` — the whole request for a non-streaming answer, enforced by an abort signal.
 * - `firstByteMs` / `idleMs` — the pre-commit wait for the first streamed byte, and the maximum
 *   silence between bytes afterwards. Both are enforced in `guardUpstreamStream`.
 *
 * A timeout is a verdict about the host, not the request, so callers treat it like ECONNRESET:
 * one same-host retry, then failover.
 */

export type UpstreamTimeoutPhase = "connect" | "headers" | "total" | "first-byte" | "idle";

/** Raised when one of the layered clocks above fires. `name` is `TimeoutError` on purpose. */
export class UpstreamTimeoutError extends Error {
  readonly code = "JEVONIAN_TIMEOUT";
  readonly phase: UpstreamTimeoutPhase;
  readonly ms: number;

  constructor(phase: UpstreamTimeoutPhase, ms: number) {
    super(`upstream ${phase} timed out after ${ms}ms`);
    this.name = "TimeoutError";
    this.phase = phase;
    this.ms = ms;
  }
}

/**
 * True when a thrown fetch error is one of our timeouts, or undici's own.
 *
 * undici reports `UND_ERR_CONNECT_TIMEOUT` / `UND_ERR_HEADERS_TIMEOUT` / `UND_ERR_BODY_TIMEOUT`
 * underneath the usual `TypeError: fetch failed` wrapper, and an abort carries our
 * {@link UpstreamTimeoutError} as the rejection itself — both shapes are recognised here.
 */
export function isTimeoutError(error: unknown): boolean {
  let current: unknown = error;
  for (let depth = 0; current instanceof Error && depth < 6; depth += 1) {
    const failure = current as Error & { code?: unknown };
    if (failure.name === "TimeoutError") return true;
    const code = failure.code;
    if (
      code === "JEVONIAN_TIMEOUT" ||
      code === "UND_ERR_CONNECT_TIMEOUT" ||
      code === "UND_ERR_HEADERS_TIMEOUT" ||
      code === "UND_ERR_BODY_TIMEOUT"
    ) {
      return true;
    }
    current = (failure as { cause?: unknown }).cause;
  }
  return false;
}

/** The four clocks, in milliseconds. */
export interface UpstreamTimeouts {
  connectMs: number;
  headersMs: number;
  /** Whole request for a non-streaming answer. */
  totalMs: number;
  /** Pre-commit wait for the first streamed byte. */
  firstByteMs: number;
  /** Maximum silence between streamed bytes once the stream has started. */
  idleMs: number;
}

/** Defaults chosen from the live ledger: a healthy first byte lands in seconds, not minutes. */
export const DEFAULT_UPSTREAM_TIMEOUTS: UpstreamTimeouts = {
  connectMs: 15_000,
  headersMs: 60_000,
  totalMs: 90_000,
  firstByteMs: 60_000,
  idleMs: 120_000,
};

const ENV_KEYS: Record<keyof UpstreamTimeouts, string> = {
  connectMs: "JEVONIAN_UPSTREAM_CONNECT_TIMEOUT_MS",
  headersMs: "JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS",
  totalMs: "JEVONIAN_UPSTREAM_TOTAL_TIMEOUT_MS",
  firstByteMs: "JEVONIAN_UPSTREAM_FIRST_BYTE_TIMEOUT_MS",
  idleMs: "JEVONIAN_UPSTREAM_STREAM_IDLE_TIMEOUT_MS",
};

/** A floor keeps a typo from disabling the protection; a ceiling keeps it finite. */
const MIN_TIMEOUT_MS = 1_000;
const MAX_TIMEOUT_MS = 30 * 60_000;

function readMs(env: NodeJS.ProcessEnv, name: string, fallback: number): number {
  const raw = (env[name] ?? "").trim();
  if (raw.length === 0) return fallback;
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed)) return fallback;
  return Math.min(MAX_TIMEOUT_MS, Math.max(MIN_TIMEOUT_MS, parsed));
}

/** The layered timeouts, with `JEVONIAN_UPSTREAM_*_TIMEOUT_MS` overrides applied. */
export function upstreamTimeouts(env: NodeJS.ProcessEnv = process.env): UpstreamTimeouts {
  return {
    connectMs: readMs(env, ENV_KEYS.connectMs, DEFAULT_UPSTREAM_TIMEOUTS.connectMs),
    headersMs: readMs(env, ENV_KEYS.headersMs, DEFAULT_UPSTREAM_TIMEOUTS.headersMs),
    totalMs: readMs(env, ENV_KEYS.totalMs, DEFAULT_UPSTREAM_TIMEOUTS.totalMs),
    firstByteMs: readMs(env, ENV_KEYS.firstByteMs, DEFAULT_UPSTREAM_TIMEOUTS.firstByteMs),
    idleMs: readMs(env, ENV_KEYS.idleMs, DEFAULT_UPSTREAM_TIMEOUTS.idleMs),
  };
}

/**
 * A signal that aborts with an {@link UpstreamTimeoutError} once `ms` elapses, optionally
 * following a caller's own signal so a client hang-up still cancels the upstream request.
 */
export function timeoutController(
  ms: number,
  phase: UpstreamTimeoutPhase,
  parent?: AbortSignal | null,
): { signal: AbortSignal; clear: () => void } {
  const controller = new AbortController();
  const onAbort = (): void => controller.abort(parent?.reason);
  if (parent) {
    if (parent.aborted) controller.abort(parent.reason);
    else parent.addEventListener("abort", onAbort, { once: true });
  }
  const timer = setTimeout(() => controller.abort(new UpstreamTimeoutError(phase, ms)), ms);
  // Do not keep the process alive solely for a timeout that may never fire.
  timer.unref?.();
  return {
    signal: controller.signal,
    clear: () => {
      clearTimeout(timer);
      parent?.removeEventListener("abort", onAbort);
    },
  };
}

type StreamReadResult = Awaited<ReturnType<ReadableStreamDefaultReader<Uint8Array>["read"]>>;

/** One `reader.read()` bounded by a clock; rejects with an {@link UpstreamTimeoutError}. */
async function readWithTimeout(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  ms: number,
  phase: UpstreamTimeoutPhase,
): Promise<StreamReadResult> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      reader.read(),
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new UpstreamTimeoutError(phase, ms)), ms);
        timer.unref?.();
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

/**
 * Waits for the upstream's first byte before the caller commits the response to the client.
 *
 * Resolves with a stream that replays that first chunk and then enforces an idle timeout
 * between later chunks, so a stream that starts and then wedges is closed rather than left
 * open. Rejects with an {@link UpstreamTimeoutError} (or the reader's own error) when the
 * first byte never arrives — which is the point at which the caller can still fail over.
 */
export async function guardUpstreamStream(
  body: ReadableStream<Uint8Array>,
  options: { firstByteMs: number; idleMs: number },
): Promise<ReadableStream<Uint8Array>> {
  const reader = body.getReader();
  let first: StreamReadResult;
  try {
    first = await readWithTimeout(reader, options.firstByteMs, "first-byte");
  } catch (error) {
    void reader.cancel().catch(() => undefined);
    throw error;
  }
  return new ReadableStream<Uint8Array>({
    start(controller) {
      if (first.done) {
        controller.close();
        return;
      }
      if (first.value) controller.enqueue(first.value);
    },
    async pull(controller) {
      try {
        const step = await readWithTimeout(reader, options.idleMs, "idle");
        if (step.done) {
          controller.close();
          return;
        }
        controller.enqueue(step.value);
      } catch (error) {
        void reader.cancel().catch(() => undefined);
        controller.error(error);
      }
    },
    cancel(reason) {
      return reader.cancel(reason).catch(() => undefined);
    },
  });
}
