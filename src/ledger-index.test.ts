import { describe, expect, it } from "vite-plus/test";

import type { LedgerRecord } from "./ledger";
import { LedgerSpendIndex } from "./ledger-index";

function record(
  partial: Partial<LedgerRecord> & Pick<LedgerRecord, "id" | "ts" | "provider">,
): LedgerRecord {
  return {
    session: "s",
    path: "/v1/chat/completions",
    model: "m",
    stream: false,
    status: 200,
    latencyMs: 1,
    promptTokens: 0,
    completionTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
    costUsd: null,
    pricingKnown: false,
    ...partial,
  };
}

describe("LedgerSpendIndex", () => {
  it("sums provider spend inside rolling windows without scanning rows", () => {
    const index = new LedgerSpendIndex();
    const now = Date.parse("2026-10-04T12:00:00.000Z");
    index.add(
      record({
        id: "a",
        ts: "2026-10-04T11:30:00.000Z",
        provider: "p1",
        costUsd: 1.5,
      }),
      now,
    );
    index.add(
      record({
        id: "b",
        ts: "2026-10-03T12:00:00.000Z",
        provider: "p1",
        costUsd: 2,
      }),
      now,
    );
    index.add(
      record({
        id: "c",
        ts: "2026-10-04T11:00:00.000Z",
        provider: "p2",
        costUsd: 9,
      }),
      now,
    );

    expect(index.providerWindow("p1", 5 * 3_600_000, now).costUsd).toBe(1.5);
    expect(index.providerWindow("p1", 86_400_000, now).costUsd).toBe(3.5);
    expect(index.providerWindow("p2", 5 * 3_600_000, now).costUsd).toBe(9);
    expect(index.size).toBe(3);
  });

  it("tracks per-key api vs subscription spend", () => {
    const index = new LedgerSpendIndex();
    const now = Date.now();
    index.add(
      record({
        id: "a",
        ts: new Date(now).toISOString(),
        provider: "p1",
        keyId: "k1",
        costUsd: 3,
        billing: "api",
      }),
      now,
    );
    index.add(
      record({
        id: "b",
        ts: new Date(now).toISOString(),
        provider: "p1",
        keyId: "k1",
        costUsd: 4,
        billing: "subscription",
      }),
      now,
    );

    const all = index.keyAllTime("k1");
    expect(all.apiUsd).toBe(3);
    expect(all.subscriptionUsd).toBe(4);
    expect(all.costUsd).toBe(7);
  });

  it("ignores brain rows", () => {
    const index = new LedgerSpendIndex();
    const now = Date.now();
    index.add(
      record({
        id: "brain",
        ts: new Date(now).toISOString(),
        provider: "brain:typesafe",
        kind: "brain",
        costUsd: 99,
      }),
      now,
    );
    expect(index.size).toBe(0);
    expect(index.providerAllTime("brain:typesafe").costUsd).toBe(0);
  });
});
