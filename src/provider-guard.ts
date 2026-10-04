/**
 * Per-provider concurrency limits and circuit breakers.
 *
 * A single wedged host must not be able to absorb every in-flight turn. Before this existed,
 * all upstream traffic shared one global dispatcher with no per-host cap and no notion of a
 * provider being unhealthy, so a hung subscription could stack requests until timeouts fired.
 *
 * Two small pieces of in-memory state fix that per provider:
 *
 * - A **semaphore** caps how many attempts are in flight at once. When saturated the router
 *   routes to the next candidate instead of queueing behind a slow host.
 * - A **circuit breaker** opens after consecutive failures and stays open for a cooldown, then
 *   lets a single half-open probe through. A provider that recovers is reinstated immediately;
 *   one that keeps failing is skipped without paying a fresh timeout each turn.
 *
 * All state is process-local and bounded by the number of configured providers.
 */

const DEFAULT_CONCURRENCY = 8;
const DEFAULT_FAILURE_THRESHOLD = 3;
const DEFAULT_COOLDOWN_MS = 30_000;

/** A floor/ceiling keeps a bad env value from disabling or exploding the guard. */
function readInt(
  env: NodeJS.ProcessEnv,
  name: string,
  fallback: number,
  min: number,
  max: number,
): number {
  const raw = (env[name] ?? "").trim();
  if (raw.length === 0) return fallback;
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed)) return fallback;
  return Math.min(max, Math.max(min, parsed));
}

export function providerConcurrency(env: NodeJS.ProcessEnv = process.env): number {
  return readInt(env, "JEVONIAN_PROVIDER_CONCURRENCY", DEFAULT_CONCURRENCY, 1, 256);
}

export function providerBreakerThreshold(env: NodeJS.ProcessEnv = process.env): number {
  return readInt(env, "JEVONIAN_PROVIDER_BREAKER_THRESHOLD", DEFAULT_FAILURE_THRESHOLD, 1, 100);
}

export function providerBreakerCooldownMs(env: NodeJS.ProcessEnv = process.env): number {
  return readInt(
    env,
    "JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS",
    DEFAULT_COOLDOWN_MS,
    1_000,
    30 * 60_000,
  );
}

interface BreakerState {
  failures: number;
  /** Epoch ms the breaker may half-open; 0 while closed. */
  openUntil: number;
  /** True while a single half-open probe is in flight. */
  probing: boolean;
}

const inFlight = new Map<string, number>();
const breakers = new Map<string, BreakerState>();

/** Clears every counter and breaker. Tests call this between cases. */
export function resetProviderGuards(): void {
  inFlight.clear();
  breakers.clear();
}

/** True when the provider has room for one more attempt; a successful acquire must be released. */
export function tryAcquire(provider: string, env: NodeJS.ProcessEnv = process.env): boolean {
  const limit = providerConcurrency(env);
  const current = inFlight.get(provider) ?? 0;
  if (current >= limit) return false;
  inFlight.set(provider, current + 1);
  return true;
}

/** Releases a slot taken by {@link tryAcquire}. A no-op when nothing was held. */
export function release(provider: string): void {
  const current = inFlight.get(provider) ?? 0;
  if (current <= 0) return;
  if (current === 1) inFlight.delete(provider);
  else inFlight.set(provider, current - 1);
}

/** Why {@link beginProviderAttempt} refused a host. */
export type ProviderGuardBlock = "breaker-open" | "saturated";

/**
 * Atomically takes a concurrency slot and, when the breaker is half-open, the single probe.
 *
 * Returns a block reason when the host must be skipped; otherwise the caller must pair the
 * call with {@link endProviderAttempt} so the slot (and any probe) is released.
 */
export function beginProviderAttempt(
  provider: string,
  env: NodeJS.ProcessEnv = process.env,
  now: number = Date.now(),
): ProviderGuardBlock | undefined {
  if (breakerIsOpen(provider, now)) return "breaker-open";
  const state = breakers.get(provider);
  // Past the cooldown but a probe is already out: everyone else waits for its verdict.
  if (state && state.openUntil > 0 && state.probing) return "breaker-open";
  if (!tryAcquire(provider, env)) return "saturated";
  if (state && state.openUntil > 0) state.probing = true;
  return undefined;
}

/** Releases the slot from {@link beginProviderAttempt} and feeds the outcome into the breaker. */
export function endProviderAttempt(provider: string, ok: boolean): void {
  recordOutcome(provider, ok);
  release(provider);
}

/** In-flight attempts for a provider, for tests and diagnostics. */
export function inFlightCount(provider: string): number {
  return inFlight.get(provider) ?? 0;
}

/**
 * Pure read: true while the breaker is open and no probe is due yet.
 *
 * Unlike {@link breakerAllows}, this has no side effects — safe to call from routing while
 * ranking candidates, where opening a half-open probe would be wrong.
 */
export function breakerIsOpen(provider: string, now: number = Date.now()): boolean {
  const state = breakers.get(provider);
  if (!state || state.openUntil === 0) return false;
  return now < state.openUntil;
}

/**
 * Whether a turn may attempt this provider.
 *
 * Returns true while the breaker is closed, and once per cooldown lets a single probe through
 * (half-open). A provider with no recorded failures is always allowed.
 */
export function breakerAllows(provider: string, now: number = Date.now()): boolean {
  const state = breakers.get(provider);
  if (!state || state.openUntil === 0) return true;
  if (now < state.openUntil) return false;
  // Half-open: exactly one probe may go out; the rest wait for its verdict.
  if (state.probing) return false;
  state.probing = true;
  return true;
}

/** Feeds an attempt's outcome back into the breaker. */
export function recordOutcome(provider: string, ok: boolean): void {
  const state = breakers.get(provider) ?? { failures: 0, openUntil: 0, probing: false };
  if (ok) {
    state.failures = 0;
    state.openUntil = 0;
    state.probing = false;
  } else {
    state.failures += 1;
    state.probing = false;
    if (state.failures >= providerBreakerThreshold()) {
      state.openUntil = Date.now() + providerBreakerCooldownMs();
    }
  }
  breakers.set(provider, state);
}

/** Snapshot for diagnostics and tests. */
export function breakerState(provider: string): { failures: number; open: boolean } {
  const state = breakers.get(provider);
  return {
    failures: state?.failures ?? 0,
    open: state !== undefined && state.openUntil > Date.now(),
  };
}
