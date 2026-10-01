import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseConfig } from "./config";
import { readRecords } from "./ledger";
import { captureQuotaHeaders, resetQuotaCache } from "./quota";
import { SessionStore } from "./routing";
import { createApp } from "./server";

let dir = "";
const saved: Record<string, string | undefined> = {};

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-effortlog-"));
  for (const key of ["JEVONIAN_DATA_DIR", "JEVONIAN_LEDGER", "TYPESAFE_API_KEY"]) {
    saved[key] = process.env[key];
  }
  process.env.JEVONIAN_DATA_DIR = dir;
  process.env.JEVONIAN_LEDGER = join(dir, "ledger.jsonl");
  process.env.TYPESAFE_API_KEY = "test-key";
  resetQuotaCache();
});

afterEach(() => {
  vi.unstubAllGlobals();
  for (const [key, value] of Object.entries(saved)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  resetQuotaCache();
  rmSync(dir, { recursive: true, force: true });
});

const config = (routing: Record<string, unknown> = {}) =>
  parseConfig({
    defaultProvider: "sub",
    providers: [
      {
        name: "sub",
        type: "openai",
        baseUrl: "http://127.0.0.1:1/v1",
        apiKey: "test",
        models: ["m"],
      },
    ],
    routing: {
      mode: "auto",
      tiers: { plan: ["m"], execute: ["m"] },
      brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY" }],
      capacities: { m: { efforts: ["low", "medium", "high"] } },
      ...routing,
    },
  });

/** Routes the brain to a fixed level, then records the body that reached the provider. */
function stub(routerEffort: string, onUpstream?: (body: Record<string, unknown>) => void) {
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    const body = JSON.parse((init.body as string) ?? "{}") as Record<string, unknown>;
    if (String(url).includes("systemone")) {
      return new Response(
        JSON.stringify({
          model: "jev",
          answers: {
            model: { choice: "m", confidence: 0.9 },
            effort: { choice: routerEffort, confidence: 0.9 },
          },
        }),
        { status: 200 },
      );
    }
    onUpstream?.(body);
    return new Response(
      JSON.stringify({
        id: "chatcmpl-1",
        object: "chat.completion",
        model: "m",
        choices: [
          { index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" },
        ],
        usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 },
      }),
      { status: 200, headers: { "content-type": "application/json" } },
    );
  });
}

async function send(
  body: Record<string, unknown>,
  options: { routing?: Record<string, unknown>; headers?: Record<string, string> } = {},
) {
  // The admin app serves /v1 without a key when none exist, which keeps this test about routing.
  const app = createApp({ config: config(options.routing) } as never, new SessionStore(60_000));
  const response = await app.request("/v1/chat/completions", {
    method: "POST",
    headers: { "content-type": "application/json", ...options.headers },
    body: JSON.stringify({
      model: "auto",
      messages: [{ role: "user", content: "fix it" }],
      ...body,
    }),
  });
  return { response, records: readRecords().filter((record) => record.kind !== "brain") };
}

describe("the ledger records the thinking level actually sent", () => {
  it("records what the router chose", async () => {
    let upstream: Record<string, unknown> = {};
    stub("high", (body) => {
      upstream = body;
    });
    const { response, records } = await send({});
    expect(response.status).toBe(200);
    expect(upstream.reasoning_effort).toBe("high");
    expect(records.at(-1)?.effort).toBe("high");
    expect(records.at(-1)?.effortNote).toBeUndefined();
  });

  it("records the client's level and explains the override", async () => {
    let upstream: Record<string, unknown> = {};
    stub("low", (body) => {
      upstream = body;
    });
    const { records } = await send({ reasoning_effort: "max" });
    // The client was explicit, so its level survives untouched.
    expect(upstream.reasoning_effort).toBe("max");
    // And the log says so, instead of claiming the router's "low" was used.
    expect(records.at(-1)?.effort).toBe("max");
    expect(records.at(-1)?.effortNote).toContain('client set "max"');
  });

  it("falls back to the configured default when the brain answers no level", async () => {
    let upstream: Record<string, unknown> = {};
    vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
      const body = JSON.parse((init.body as string) ?? "{}") as Record<string, unknown>;
      if (String(url).includes("systemone")) {
        return new Response(
          JSON.stringify({ model: "jev", answers: { model: { choice: "m", confidence: 0.9 } } }),
          { status: 200 },
        );
      }
      upstream = body;
      return new Response(
        JSON.stringify({
          id: "chatcmpl-1",
          object: "chat.completion",
          model: "m",
          choices: [
            { index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" },
          ],
          usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 },
        }),
        { status: 200, headers: { "content-type": "application/json" } },
      );
    });
    const { records } = await send({}, { routing: { defaultEffort: "low" } });
    expect(upstream.reasoning_effort).toBe("low");
    expect(records.at(-1)?.effort).toBe("low");
  });

  it("records nothing when no level is applied at all", async () => {
    let upstream: Record<string, unknown> = {};
    vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
      const body = JSON.parse((init.body as string) ?? "{}") as Record<string, unknown>;
      if (String(url).includes("systemone")) {
        return new Response(
          JSON.stringify({ model: "jev", answers: { model: { choice: "m", confidence: 0.9 } } }),
          { status: 200 },
        );
      }
      upstream = body;
      return new Response(
        JSON.stringify({
          id: "chatcmpl-1",
          object: "chat.completion",
          model: "m",
          choices: [
            { index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" },
          ],
          usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 },
        }),
        { status: 200, headers: { "content-type": "application/json" } },
      );
    });
    // No brain effort question, no default, and a model that states no levels: the router
    // applies nothing, so the ledger says nothing rather than inventing a level.
    const { records } = await send({}, { routing: { brainPicksEffort: false, capacities: {} } });
    expect(upstream.reasoning_effort).toBeUndefined();
    expect(records.at(-1)?.effort).toBeUndefined();
  });

  it("reports the level on the response headers too", async () => {
    stub("medium");
    const { response } = await send({});
    expect(response.headers.get("x-jevonian-effort")).toBe("medium");
  });
});

it("feeds successful upstream cache usage into the next routing consultation", async () => {
  const store = new SessionStore(60_000);
  const app = createApp({ config: config() } as never, store);
  const states: Array<Record<string, unknown>> = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    const body = JSON.parse(typeof init.body === "string" ? init.body : "{}");
    if (String(url).includes("systemone")) {
      states.push(body.state);
      return Response.json({ answers: { model: { choice: "plan", confidence: 0.9 } } });
    }
    return Response.json({
      choices: [{ message: { role: "assistant", content: "ok" } }],
      usage: {
        prompt_tokens: 100,
        completion_tokens: 1,
        prompt_tokens_details: { cached_tokens: 80 },
      },
    });
  });
  const request = () =>
    app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", "x-session-id": "cache" },
      body: JSON.stringify({ model: "auto", messages: [{ role: "user", content: "fix it" }] }),
    });
  expect((await request()).status).toBe(200);
  expect(store.get("cache", Date.now())?.cache).toMatchObject({
    uncachedInputTokens: 20,
    cacheReadTokens: 80,
    success: true,
  });
  const second = await request();
  expect(second.headers.get("x-jevonian-cache-state")).toBe("hot");
  expect(JSON.stringify(states.at(-1))).toContain('"observedHitRatio":0.8');
  expect(
    readRecords()
      .filter((r) => r.kind !== "brain")
      .at(-1)?.cache?.state,
  ).toBe("hot");
});

describe("cache affinity keeps a conversation where it was answered", () => {
  const twoProviders = () =>
    parseConfig({
      defaultProvider: "a",
      providers: [
        { name: "a", type: "openai", baseUrl: "http://127.0.0.1:1/v1", apiKey: "test", models: ["m"] },
        { name: "b", type: "openai", baseUrl: "http://127.0.0.1:2/v1", apiKey: "test", models: ["m"] },
      ],
      routing: {
        mode: "auto",
        tiers: { plan: ["m"], execute: ["m"] },
        providers: { m: ["a", "b"] },
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY" }],
      },
    });

  /** Seeds one provider's 7-day window: the one that renews sooner wins reset-aware order. */
  function seedRenewal(name: string, hours: number): void {
    captureQuotaHeaders(
      {
        name,
        type: "anthropic",
        baseUrl: "https://api.anthropic.com",
        auth: "oauth",
        oauthSource: "claude-code",
        models: [],
      } as never,
      new Headers({
        "anthropic-ratelimit-unified-7d-utilization": "5",
        "anthropic-ratelimit-unified-7d-reset": String(
          Math.floor(Date.now() / 1000) + hours * 3_600,
        ),
      }),
    );
  }

  function appWith(store: SessionStore) {
    vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
      if (String(url).includes("systemone")) {
        const body = JSON.parse((init.body as string) ?? "{}") as Record<string, unknown>;
        // Follow the offered order: the brain keeps whatever routing put first, which is what
        // the router already decided for affinity. This isolates the keep from the brain.
        const routings = ((body.state as Record<string, unknown>)?.routings ?? []) as Array<{
          id: string;
        }>;
        return Response.json({
          answers: { model: { choice: routings[0]?.id ?? "plan", confidence: 0.9 } },
        });
      }
      return Response.json({
        choices: [{ message: { role: "assistant", content: "ok" } }],
        usage: {
          prompt_tokens: 100,
          completion_tokens: 1,
          prompt_tokens_details: { cached_tokens: 90 },
        },
      });
    });
    return createApp({ config: twoProviders() } as never, store);
  }

  const midTurn = {
    model: "auto",
    messages: [
      { role: "user", content: "fix it" },
      {
        role: "assistant",
        content: "",
        tool_calls: [{ id: "1", type: "function", function: { name: "f", arguments: "{}" } }],
      },
      { role: "tool", tool_call_id: "1", content: "ok" },
    ],
  };

  it("keeps the answering provider across a turn when its cache was worth staying for", async () => {
    const store = new SessionStore(600_000);
    // Turn one: "a" renews soonest, so reset-aware order sends the first request there.
    seedRenewal("a", 2);
    seedRenewal("b", 100);
    const app = appWith(store);
    const first = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", "x-session-id": "aff" },
      body: JSON.stringify({ model: "auto", messages: [{ role: "user", content: "fix it" }] }),
    });
    expect(first.status).toBe(200);
    expect(first.headers.get("x-jevonian-provider")).toBe("a");

    // "a"'s window renews, so a fresh route would now move the turn to "b". The conversation
    // is mid-turn and "a" read 90 tokens of the vendor's cache, so it must stay there.
    seedRenewal("a", 120);
    seedRenewal("b", 1);
    const second = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", "x-session-id": "aff" },
      body: JSON.stringify(midTurn),
    });
    expect(second.status).toBe(200);
    expect(second.headers.get("x-jevonian-provider")).toBe("a");
    expect(second.headers.get("x-jevonian-cache-keep")).toBe("turn");
    expect(readRecords().filter((r) => r.kind !== "brain").at(-1)?.cacheKeep).toBe("turn");
  });

  it("honours an explicit affinity=off request", async () => {
    const store = new SessionStore(600_000);
    seedRenewal("a", 2);
    seedRenewal("b", 100);
    const app = appWith(store);
    await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", "x-session-id": "off" },
      body: JSON.stringify({ model: "auto", messages: [{ role: "user", content: "fix it" }] }),
    });
    // The cache is warm on "a", but the caller said not to stay for it: the turn follows the
    // resets instead, which now favour "b".
    seedRenewal("a", 120);
    seedRenewal("b", 1);
    const second = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-session-id": "off",
        "x-jevonian-affinity": "off",
      },
      body: JSON.stringify(midTurn),
    });
    expect(second.headers.get("x-jevonian-cache-keep")).toBe("off");
    expect(second.headers.get("x-jevonian-provider")).toBe("b");
  });
});
