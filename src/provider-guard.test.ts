import { afterEach, describe, expect, it } from "vite-plus/test";

import {
  beginProviderAttempt,
  breakerIsOpen,
  breakerState,
  endProviderAttempt,
  inFlightCount,
  resetProviderGuards,
} from "./provider-guard";

afterEach(() => {
  resetProviderGuards();
  delete process.env.JEVONIAN_PROVIDER_CONCURRENCY;
  delete process.env.JEVONIAN_PROVIDER_BREAKER_THRESHOLD;
  delete process.env.JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS;
});

describe("provider-guard", () => {
  it("caps in-flight attempts per provider", () => {
    process.env.JEVONIAN_PROVIDER_CONCURRENCY = "2";
    expect(beginProviderAttempt("p")).toBeUndefined();
    expect(beginProviderAttempt("p")).toBeUndefined();
    expect(beginProviderAttempt("p")).toBe("saturated");
    expect(inFlightCount("p")).toBe(2);
    endProviderAttempt("p", true);
    expect(beginProviderAttempt("p")).toBeUndefined();
  });

  it("opens the breaker after consecutive failures and cools down", () => {
    process.env.JEVONIAN_PROVIDER_BREAKER_THRESHOLD = "2";
    process.env.JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS = "60000";
    expect(beginProviderAttempt("p")).toBeUndefined();
    endProviderAttempt("p", false);
    expect(beginProviderAttempt("p")).toBeUndefined();
    endProviderAttempt("p", false);
    expect(breakerState("p").open).toBe(true);
    expect(breakerIsOpen("p")).toBe(true);
    expect(beginProviderAttempt("p")).toBe("breaker-open");
  });

  it("closes the breaker after a successful probe", async () => {
    process.env.JEVONIAN_PROVIDER_BREAKER_THRESHOLD = "1";
    // Floor is 1000ms; wait past it so the half-open probe is allowed.
    process.env.JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS = "1000";
    expect(beginProviderAttempt("p")).toBeUndefined();
    endProviderAttempt("p", false);
    expect(breakerIsOpen("p")).toBe(true);
    await new Promise((resolve) => setTimeout(resolve, 1050));
    expect(beginProviderAttempt("p")).toBeUndefined();
    endProviderAttempt("p", true);
    expect(breakerIsOpen("p")).toBe(false);
    expect(breakerState("p").failures).toBe(0);
  });
});
