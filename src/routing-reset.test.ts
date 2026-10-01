import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";

import { parseConfig } from "./config";
import { accountWindows, captureQuotaHeaders, resetQuotaCache } from "./quota";
import { decideRoute, orderByQuotaReset, SessionStore, type TierPick } from "./routing";

let dir = "";
let previousData: string | undefined;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-reset-"));
  previousData = process.env.JEVONIAN_DATA_DIR;
  process.env.JEVONIAN_DATA_DIR = join(dir, "data");
  resetQuotaCache();
});

afterEach(() => {
  if (previousData === undefined) delete process.env.JEVONIAN_DATA_DIR;
  else process.env.JEVONIAN_DATA_DIR = previousData;
  resetQuotaCache();
  rmSync(dir, { recursive: true, force: true });
});

function configOf(providers: Array<Record<string, unknown>>) {
  const first = providers[0]?.name;
  return parseConfig({
    defaultProvider: typeof first === "string" ? first : "a",
    providers,
    routing: { brains: [] },
  });
}

const provider = (name: string, model: string) => ({
  name,
  type: "openai",
  baseUrl: `http://127.0.0.1:9999/${name}/v1`,
  apiKey: "test",
  models: [model],
});

/**
 * Seeds one provider's account windows the way Anthropic's unified headers do, so the
 * ordering reads exactly what a real subscription would report.
 */
function seed(
  name: string,
  windows: {
    fiveHour?: { used: number; resetsAt: number };
    sevenDay?: { used: number; resetsAt: number };
  },
): void {
  const headers = new Headers();
  if (windows.fiveHour) {
    headers.set("anthropic-ratelimit-unified-5h-utilization", String(windows.fiveHour.used));
    headers.set("anthropic-ratelimit-unified-5h-reset", String(windows.fiveHour.resetsAt));
  }
  if (windows.sevenDay) {
    headers.set("anthropic-ratelimit-unified-7d-utilization", String(windows.sevenDay.used));
    headers.set("anthropic-ratelimit-unified-7d-reset", String(windows.sevenDay.resetsAt));
  }
  captureQuotaHeaders(
    {
      name,
      type: "anthropic",
      baseUrl: "https://api.anthropic.com",
      auth: "oauth",
      oauthSource: "claude-code",
      models: [],
    } as never,
    headers,
  );
}

const hoursFromNow = (hours: number): number => Math.floor(Date.now() / 1000) + hours * 3_600;
const daysFromNow = (days: number): number => hoursFromNow(days * 24);

describe("accountWindows", () => {
  it("reports the account windows longest first", () => {
    seed("a", {
      fiveHour: { used: 20, resetsAt: hoursFromNow(1) },
      sevenDay: { used: 40, resetsAt: daysFromNow(3) },
    });
    const windows = accountWindows({ name: "a", quota: undefined } as never);
    expect(windows.map((window) => window.spanMinutes)).toEqual([10_080, 300]);
    expect(windows[0]?.usedPercent).toBe(40);
    expect(windows[1]?.usedPercent).toBe(20);
  });

  it("leaves out a window whose reset has passed, which is empty again", () => {
    seed("a", {
      fiveHour: { used: 100, resetsAt: hoursFromNow(-1) },
      sevenDay: { used: 30, resetsAt: daysFromNow(2) },
    });
    expect(
      accountWindows({ name: "a", quota: undefined } as never).map((w) => w.spanMinutes),
    ).toEqual([10_080]);
  });

  it("returns nothing for a provider that has never reported a window", () => {
    expect(accountWindows({ name: "unknown", quota: undefined } as never)).toEqual([]);
  });
});

describe("orderByQuotaReset", () => {
  it("puts the allowance that renews soonest first", () => {
    const config = configOf([provider("later", "glm-5.2"), provider("sooner", "glm-5.2")]);
    seed("later", { sevenDay: { used: 5, resetsAt: daysFromNow(6) } });
    seed("sooner", { sevenDay: { used: 5, resetsAt: daysFromNow(1) } });
    const picks: TierPick[] = [
      { provider: "later", model: "glm-5.2" },
      { provider: "sooner", model: "glm-5.2" },
    ];
    expect(orderByQuotaReset(picks, config).map((pick) => pick.provider)).toEqual([
      "sooner",
      "later",
    ]);
  });

  it("keeps the configured order when the two renew in the same hour", () => {
    const config = configOf([provider("first", "glm-5.2"), provider("second", "glm-5.2")]);
    // Ten minutes apart: too close to matter, and reordering would break the warm cache.
    seed("first", { sevenDay: { used: 5, resetsAt: daysFromNow(2) } });
    seed("second", { sevenDay: { used: 5, resetsAt: daysFromNow(2) + 600 } });
    const picks: TierPick[] = [
      { provider: "first", model: "glm-5.2" },
      { provider: "second", model: "glm-5.2" },
    ];
    expect(orderByQuotaReset(picks, config).map((pick) => pick.provider)).toEqual([
      "first",
      "second",
    ]);
  });

  it("lets the longest window decide what soonest means", () => {
    const config = configOf([
      provider("week-later", "glm-5.2"),
      provider("week-sooner", "glm-5.2"),
    ]);
    // The 5h window is the inverse of the 7d one, so only the week can be the tie-breaker.
    seed("week-later", {
      fiveHour: { used: 5, resetsAt: hoursFromNow(1) },
      sevenDay: { used: 5, resetsAt: daysFromNow(6) },
    });
    seed("week-sooner", {
      fiveHour: { used: 5, resetsAt: hoursFromNow(4) },
      sevenDay: { used: 5, resetsAt: daysFromNow(1) },
    });
    const picks: TierPick[] = [
      { provider: "week-later", model: "glm-5.2" },
      { provider: "week-sooner", model: "glm-5.2" },
    ];
    expect(orderByQuotaReset(picks, config).map((pick) => pick.provider)).toEqual([
      "week-sooner",
      "week-later",
    ]);
  });

  it("keeps a window that does not say when it renews behind one that does", () => {
    const config = configOf([provider("silent", "glm-5.2"), provider("known", "glm-5.2")]);
    seed("known", { sevenDay: { used: 5, resetsAt: daysFromNow(3) } });
    const picks: TierPick[] = [
      { provider: "silent", model: "glm-5.2" },
      { provider: "known", model: "glm-5.2" },
    ];
    expect(orderByQuotaReset(picks, config).map((pick) => pick.provider)).toEqual([
      "known",
      "silent",
    ]);
  });

  it("sorts room before low before spent", () => {
    const config = configOf([
      provider("spent", "glm-5.2"),
      provider("low", "glm-5.2"),
      provider("fine", "glm-5.2"),
    ]);
    seed("spent", { sevenDay: { used: 100, resetsAt: daysFromNow(1) } });
    seed("low", { sevenDay: { used: 95, resetsAt: daysFromNow(4) } });
    seed("fine", { sevenDay: { used: 5, resetsAt: daysFromNow(6) } });
    const picks: TierPick[] = [
      { provider: "spent", model: "glm-5.2" },
      { provider: "low", model: "glm-5.2" },
      { provider: "fine", model: "glm-5.2" },
    ];
    expect(orderByQuotaReset(picks, config).map((pick) => pick.provider)).toEqual([
      "fine",
      "low",
      "spent",
    ]);
  });

  it("leaves a single candidate alone without asking about quota", () => {
    const config = configOf([provider("only", "glm-5.2")]);
    const picks: TierPick[] = [{ provider: "only", model: "glm-5.2" }];
    expect(orderByQuotaReset(picks, config)).toBe(picks);
  });
});

describe("decideRoute reset-aware pinning", () => {
  const bodyFor = (model: string) => ({ model, messages: [{ role: "user", content: "hi" }] });

  /** Two providers serve one model; `sooner` refills tomorrow, `later` in six days. */
  function twoAccounts(guard?: Record<string, unknown>) {
    seed("later", { sevenDay: { used: 5, resetsAt: daysFromNow(6) } });
    seed("sooner", { sevenDay: { used: 5, resetsAt: daysFromNow(1) } });
    return parseConfig({
      defaultProvider: "later",
      providers: [provider("later", "glm-5.2"), provider("sooner", "glm-5.2")],
      routing: { brains: [], ...(guard ? { quotaGuard: guard } : {}) },
    });
  }

  async function pick(config: ReturnType<typeof parseConfig>): Promise<string> {
    const result = await decideRoute({
      config,
      body: bodyFor("glm-5.2"),
      headers: {},
      store: new SessionStore(720_000),
      kind: "openai",
    });
    if ("error" in result) throw new Error(result.error);
    return result.provider;
  }

  it("pins a model to the provider whose allowance renews soonest", async () => {
    expect(await pick(twoAccounts())).toBe("sooner");
  });

  it("keeps the configured order when reset-aware routing is off", async () => {
    const off = twoAccounts({ enabled: true, lowPercent: 10, resetAware: false });
    expect(await pick(off)).toBe("later");
  });
});
