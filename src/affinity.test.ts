import { describe, expect, it } from "vite-plus/test";

import {
  applyCacheKeep,
  CACHE_WORTH_TOKENS,
  cacheAffinityKeep,
  type CacheAffinityMode,
  type CacheKeep,
  type SessionState,
  type TierPick,
} from "./routing";

const NOW = 1_000_000;

function session(
  cache?: Partial<NonNullable<SessionState["cache"]>>,
): SessionState {
  return {
    phase: "plan",
    provider: "warm",
    model: "glm-5.2",
    turns: 2,
    updatedAt: NOW,
    ...(cache
      ? {
          cache: {
            provider: "warm",
            model: "glm-5.2",
            at: NOW,
            uncachedInputTokens: 100,
            cacheReadTokens: cache.cacheReadTokens ?? 0,
            cacheWriteTokens: cache.cacheWriteTokens ?? 0,
            success: cache.success ?? true,
          },
        }
      : {}),
  };
}

const candidates = (...providers: string[]): TierPick[] =>
  providers.map((provider) => ({ provider, model: "glm-5.2" }));

function keep(input: {
  previous?: SessionState;
  mode?: CacheAffinityMode;
  withinTurn?: boolean;
  now?: number;
  ttlMs?: number;
}): CacheKeep {
  return cacheAffinityKeep({
    previous: input.previous,
    mode: input.mode ?? "auto",
    withinTurn: input.withinTurn ?? false,
    candidates: candidates("warm", "other"),
    now: input.now ?? NOW,
    cacheTtlMs: input.ttlMs,
  });
}

describe("cacheAffinityKeep", () => {
  it("keeps whoever answered when it read enough of the vendor's cache", () => {
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    const verdict = keep({ previous });
    expect(verdict.keep).toBe(true);
    expect(verdict.reason).toBe("cache");
    expect(verdict.cacheRead).toBe(CACHE_WORTH_TOKENS);
  });

  it("keeps within a turn whatever the cache says: the prefix is still being built", () => {
    const previous = session({ cacheReadTokens: 0 });
    const verdict = keep({ previous, withinTurn: true });
    expect(verdict.keep).toBe(true);
    expect(verdict.reason).toBe("turn");
  });

  it("does not keep a turn that answered with too little cache to be worth the move", () => {
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS - 1 });
    const verdict = keep({ previous });
    expect(verdict.keep).toBe(false);
    expect(verdict.reason).toBe("no-cache");
  });

  it("does not keep one whose cache the vendor has dropped", () => {
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    const verdict = keep({ previous, now: NOW + 5 * 60_000, ttlMs: 5 * 60_000 });
    expect(verdict.keep).toBe(false);
    expect(verdict.reason).toBe("cold");
  });

  it("does not keep when whoever answered is no longer a candidate", () => {
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    const verdict = cacheAffinityKeep({
      previous,
      mode: "auto",
      withinTurn: false,
      candidates: candidates("other"),
      now: NOW,
    });
    expect(verdict.reason).toBe("gone");
  });

  it("session mode stays whatever the cache says", () => {
    const previous = session({ cacheReadTokens: 0 });
    const verdict = keep({ previous, mode: "session" });
    expect(verdict.keep).toBe(true);
    expect(verdict.reason).toBe("session");
  });

  it("turn mode lets a new turn route afresh", () => {
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    const verdict = keep({ previous, mode: "turn" });
    expect(verdict.keep).toBe(false);
    expect(verdict.reason).toBe("new-turn");
  });

  it("off mode never keeps", () => {
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    expect(keep({ previous, mode: "off" }).reason).toBe("off");
  });

  it("first means nobody has answered yet", () => {
    expect(keep({}).reason).toBe("first");
  });
});

describe("applyCacheKeep", () => {
  it("moves whoever answered to the front", () => {
    const picks = candidates("cold", "warm", "other");
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    const verdict = keep({ previous });
    const ordered = applyCacheKeep(picks, previous, verdict);
    expect(ordered.map((pick) => pick.provider)).toEqual(["warm", "cold", "other"]);
  });

  it("leaves the order alone when the keep verdict is no", () => {
    const picks = candidates("cold", "warm", "other");
    const previous = session({ cacheReadTokens: 0 });
    const verdict = keep({ previous });
    expect(applyCacheKeep(picks, previous, verdict)).toBe(picks);
  });

  it("leaves a pool it is already first in alone", () => {
    const picks = candidates("warm", "cold");
    const previous = session({ cacheReadTokens: CACHE_WORTH_TOKENS });
    expect(applyCacheKeep(picks, previous, keep({ previous }))).toBe(picks);
  });
});
