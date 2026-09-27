import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseConfig } from "./config";
import { headerQuotas, resetQuotaCache } from "./quota";
import { SessionStore } from "./routing";
import { createApp } from "./server";

let dir = "";
const saved: Record<string, string | undefined> = {};

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-failover-"));
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

describe("same-request quota failover", () => {
  it("retries on another provider when OpenCode Go returns a weekly limit 429", async () => {
    const config = parseConfig({
      defaultProvider: "opencode-go",
      providers: [
        {
          name: "opencode-go",
          type: "both",
          baseUrl: "https://opencode.ai/zen/go/v1",
          apiKey: "go-key",
          billing: "subscription",
          models: ["deepseek-v4.1-flash"],
        },
        {
          name: "deepseek",
          type: "openai",
          baseUrl: "https://api.deepseek.com/v1",
          apiKey: "ds-key",
          models: ["deepseek-v4.1-flash"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["deepseek-v4.1-flash"],
          execute: ["deepseek-v4.1-flash"],
          utility: ["deepseek-v4.1-flash"],
          chat: ["deepseek-v4.1-flash"],
        },
      },
    });

    let upstreamHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      // Brain
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "execute", confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      upstreamHits += 1;
      const body = typeof init?.body === "string" ? init.body : "";
      if (url.includes("opencode.ai")) {
        return new Response(
          JSON.stringify({
            type: "error",
            error: {
              type: "GoUsageLimitError",
              message: "Weekly usage limit reached. Resets in 1hr 0min.",
            },
          }),
          { status: 429, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("deepseek.com")) {
        expect(body).toContain("deepseek");
        return new Response(
          JSON.stringify({
            id: "chatcmpl-1",
            choices: [{ message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
            usage: { prompt_tokens: 1, completion_tokens: 1 },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("unexpected", { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("deepseek");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    expect(upstreamHits).toBeGreaterThanOrEqual(2);
    expect(headerQuotas()["opencode-go"]?.windows[0]?.usedPercent).toBe(100);
  });

  it("failovers ChatGPT Desktop (/responses) onto OpenRouter when Codex hits usage_limit_reached", async () => {
    const config = parseConfig({
      defaultProvider: "chatgpt-subscription",
      providers: [
        {
          name: "chatgpt-subscription",
          type: "responses",
          baseUrl: "https://chatgpt.com/backend-api/codex",
          apiKey: "codex-key",
          billing: "subscription",
          models: ["gpt-6-astra"],
        },
        {
          name: "openrouter",
          type: "both",
          baseUrl: "https://openrouter.ai/api/v1",
          apiKey: "or-key",
          models: ["openai/gpt-6-astra", "google/gemini-3.8-flash"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["gpt-6-astra", "openai/gpt-6-astra", "gemini-3-8-flash"],
          execute: ["gpt-6-astra", "openai/gpt-6-astra", "gemini-3-8-flash"],
          utility: ["gemini-3-8-flash"],
          chat: ["gemini-3-8-flash"],
        },
      },
    });

    let upstreamHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "plan", confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      upstreamHits += 1;
      if (url.includes("chatgpt.com")) {
        return new Response(
          JSON.stringify({
            error: {
              type: "usage_limit_reached",
              message: "The usage limit has been reached",
              plan_type: "plus",
              resets_at: Math.floor(Date.now() / 1000) + 11_163,
              resets_in_seconds: 11163,
            },
          }),
          { status: 429, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("openrouter.ai")) {
        const body = typeof init?.body === "string" ? JSON.parse(init.body) : {};
        // Bridged Responses clients must leave as Chat Completions, not /responses.
        expect(url).toContain("/chat/completions");
        expect(body.messages).toBeDefined();
        return new Response(
          [
            'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}\n\n',
            'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}\n\n',
            "data: [DONE]\n\n",
          ].join(""),
          { status: 200, headers: { "content-type": "text/event-stream" } },
        );
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        stream: true,
        input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "hi" }] }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("openrouter");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    expect(upstreamHits).toBeGreaterThanOrEqual(2);
    const text = await response.text();
    expect(text).toContain("response.output_text.delta");
    expect(headerQuotas()["chatgpt-subscription"]?.windows[0]?.usedPercent).toBe(100);
  });

  it("failovers on DeepSeek 402 Insufficient Balance onto the next healthy model", async () => {
    const config = parseConfig({
      defaultProvider: "deepseek",
      providers: [
        {
          name: "deepseek",
          type: "both",
          baseUrl: "https://api.deepseek.com/v1",
          apiKey: "ds-key",
          models: ["deepseek-flash"],
        },
        {
          name: "openrouter",
          type: "both",
          baseUrl: "https://openrouter.ai/api/v1",
          apiKey: "or-key",
          models: ["google/gemini-3.8-flash"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["deepseek-flash", "gemini-3-8-flash"],
          execute: ["deepseek-flash", "gemini-3-8-flash"],
          utility: ["gemini-3-8-flash"],
          chat: ["gemini-3-8-flash"],
        },
      },
    });

    let deepseekHits = 0;
    let openrouterHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        const body = JSON.parse((init?.body as string) ?? "{}") as {
          state?: { routings?: Array<{ id: string }> };
        };
        const choice = body.state?.routings?.[0]?.id ?? "none_of_the_above";
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice, confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("deepseek.com")) {
        deepseekHits += 1;
        return new Response(
          JSON.stringify({
            error: {
              message: "Insufficient Balance",
              type: "unknown_error",
              param: null,
              code: "invalid_request_error",
            },
          }),
          { status: 402, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("openrouter.ai")) {
        openrouterHits += 1;
        return new Response(
          [
            'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}\n\n',
            'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}\n\n',
            "data: [DONE]\n\n",
          ].join(""),
          { status: 200, headers: { "content-type": "text/event-stream" } },
        );
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        stream: true,
        input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "hi" }] }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("openrouter");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    expect(deepseekHits).toBe(1);
    expect(openrouterHits).toBe(1);
    expect(headerQuotas().deepseek?.windows[0]?.usedPercent).toBe(100);
  });

  it("failovers Claude rate_limit_error when unified headers say the 5h window is spent", async () => {
    // Anthropic's spend envelope is only `type: rate_limit_error` — not in the structured
    // quota-token allow-list. The same 429 carries `anthropic-ratelimit-unified-*-status:
    // rejected`; that header snapshot must trigger same-request failover onto the next
    // model in the plan chain instead of returning 429 to the client.
    const config = parseConfig({
      defaultProvider: "claude-subscription",
      providers: [
        {
          name: "claude-subscription",
          type: "anthropic",
          baseUrl: "https://api.anthropic.com",
          apiKey: "claude-key",
          billing: "subscription",
          models: ["claude-opus-5-5"],
        },
        {
          name: "chatgpt-subscription",
          type: "both",
          baseUrl: "https://api.openai.com/v1",
          apiKey: "openai-key",
          billing: "subscription",
          models: ["gpt-6-astra"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["claude-opus-5-5", "gpt-6-astra"],
          execute: ["claude-opus-5-5", "gpt-6-astra"],
          utility: ["gpt-6-astra"],
          chat: ["gpt-6-astra"],
        },
      },
    });

    let claudeHits = 0;
    let openaiHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "plan", confidence: 0.95 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("api.anthropic.com")) {
        claudeHits += 1;
        return new Response(
          JSON.stringify({
            type: "error",
            error: {
              type: "rate_limit_error",
              message:
                "This request would exceed your account's rate limit. Please try again later.",
            },
          }),
          {
            status: 429,
            headers: {
              "content-type": "application/json",
              "anthropic-ratelimit-unified-5h-utilization": "1",
              "anthropic-ratelimit-unified-5h-reset": String(Math.floor(Date.now() / 1000) + 3600),
              "anthropic-ratelimit-unified-5h-status": "rejected",
              "anthropic-ratelimit-unified-7d-utilization": "0.33",
              "anthropic-ratelimit-unified-7d-status": "allowed",
            },
          },
        );
      }
      if (url.includes("api.openai.com")) {
        openaiHits += 1;
        const body = typeof init?.body === "string" ? init.body : "";
        expect(body).toContain("gpt-6-astra");
        return new Response(
          JSON.stringify({
            id: "chatcmpl-1",
            choices: [{ message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
            usage: { prompt_tokens: 1, completion_tokens: 1 },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("chatgpt-subscription");
    expect(response.headers.get("x-jevonian-model")).toBe("gpt-6-astra");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    expect(claudeHits).toBe(1);
    expect(openaiHits).toBe(1);
    expect(headerQuotas()["claude-subscription"]?.windows[0]).toMatchObject({
      id: "5h",
      usedPercent: 100,
      status: "rejected",
    });
  });

  it("failovers a Devin daily-quota 500 (classified quota) onto the next provider", async () => {
    const config = parseConfig({
      defaultProvider: "devin-subscription",
      providers: [
        {
          name: "devin-subscription",
          type: "devin",
          baseUrl: "https://api.devin.ai",
          apiKey: "devin-key",
          billing: "subscription",
          models: ["claude-opus-5-5-high", "swe-2-max"],
        },
        {
          name: "chatgpt-subscription",
          type: "both",
          baseUrl: "https://api.openai.com/v1",
          apiKey: "openai-key",
          billing: "subscription",
          models: ["gpt-6-astra"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["claude-opus-5-5-high", "gpt-6-astra"],
          execute: ["claude-opus-5-5-high", "gpt-6-astra"],
          utility: ["gpt-6-astra"],
          chat: ["gpt-6-astra"],
        },
      },
    });

    let devinHits = 0;
    let openaiHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "plan", confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("devin.ai")) {
        devinHits += 1;
        // The shape a spent account produces: an internal-looking 500 whose body names the
        // quota. This is the exact wording seen on the failing turn.
        return new Response(
          JSON.stringify({
            code: "internal",
            message:
              "Your daily usage quota has been exhausted. Visit https://app.devin.ai/settings/usage to purchase on-demand usage or turn on auto-reload. (trace ID: bbdd07ca49202cec286b54dbb6f602a8)",
          }),
          { status: 500, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("api.openai.com")) {
        openaiHits += 1;
        return new Response(
          JSON.stringify({
            id: "chatcmpl-1",
            choices: [{ message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
            usage: { prompt_tokens: 1, completion_tokens: 1 },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("chatgpt-subscription");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    // The 500 passes through the transient retry budget before failover (Devin's 500s are
    // sometimes blips), so it is hit more than once — but the turn still lands elsewhere.
    expect(devinHits).toBeGreaterThanOrEqual(1);
    expect(openaiHits).toBe(1);
    // The Devin refusal is recorded as a rejected window so the next turn skips it without
    // burning a probe.
    expect(headerQuotas()["devin-subscription"]?.windows[0]).toMatchObject({
      usedPercent: 100,
      status: "rejected",
    });
  });

  it("failovers a Devin model_blocked trailer onto the next provider", async () => {
    const config = parseConfig({
      defaultProvider: "devin-subscription",
      providers: [
        {
          name: "devin-subscription",
          type: "devin",
          baseUrl: "https://api.devin.ai",
          apiKey: "devin-key",
          billing: "subscription",
          models: ["claude-opus-5-5-high", "swe-2-max"],
        },
        {
          name: "chatgpt-subscription",
          type: "both",
          baseUrl: "https://api.openai.com/v1",
          apiKey: "openai-key",
          billing: "subscription",
          models: ["gpt-6-astra"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["claude-opus-5-5-high", "gpt-6-astra"],
          execute: ["claude-opus-5-5-high", "gpt-6-astra"],
          utility: ["gpt-6-astra"],
          chat: ["gpt-6-astra"],
        },
      },
    });

    let devinHits = 0;
    let openaiHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "plan", confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("devin.ai")) {
        devinHits += 1;
        return new Response(
          JSON.stringify({
            code: "permission_denied",
            message: "Please visit /upgrade to access this model",
          }),
          { status: 401, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("api.openai.com")) {
        openaiHits += 1;
        return new Response(
          JSON.stringify({
            id: "chatcmpl-1",
            choices: [{ message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
            usage: { prompt_tokens: 1, completion_tokens: 1 },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("chatgpt-subscription");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    expect(devinHits).toBe(1);
    expect(openaiHits).toBe(1);
  });

  it("failovers an unknown-shape 429 that matches no allow-listed token", async () => {
    const config = parseConfig({
      defaultProvider: "custom",
      providers: [
        {
          name: "custom",
          type: "openai",
          baseUrl: "https://api.custom.example/v1",
          apiKey: "custom-key",
          models: ["custom-model"],
        },
        {
          name: "chatgpt-subscription",
          type: "both",
          baseUrl: "https://api.openai.com/v1",
          apiKey: "openai-key",
          billing: "subscription",
          models: ["gpt-6-astra"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["custom-model", "gpt-6-astra"],
          execute: ["custom-model", "gpt-6-astra"],
          utility: ["gpt-6-astra"],
          chat: ["gpt-6-astra"],
        },
      },
    });

    let customHits = 0;
    let openaiHits = 0;
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "plan", confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("custom.example")) {
        customHits += 1;
        // A vendor whose error envelope is not on the allow-list: still a 429, still a
        // verdict about the account, still worth routing around.
        return new Response(
          JSON.stringify({ err: "account_throttled", reason: "monthly cap reached" }),
          { status: 429, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("api.openai.com")) {
        openaiHits += 1;
        return new Response(
          JSON.stringify({
            id: "chatcmpl-1",
            choices: [{ message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
            usage: { prompt_tokens: 1, completion_tokens: 1 },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response(`unexpected ${url}`, { status: 500 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    expect(response.headers.get("x-jevonian-provider")).toBe("chatgpt-subscription");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");
    expect(customHits).toBe(1);
    expect(openaiHits).toBe(1);
    expect(headerQuotas().custom?.windows[0]).toMatchObject({
      usedPercent: 100,
      status: "rejected",
    });
  });

  it("surfaces the error only after every routable provider has been refused", async () => {
    const config = parseConfig({
      defaultProvider: "provider-a",
      providers: [
        {
          name: "provider-a",
          type: "openai",
          baseUrl: "https://a.example/v1",
          apiKey: "a-key",
          models: ["model-a"],
        },
        {
          name: "provider-b",
          type: "openai",
          baseUrl: "https://b.example/v1",
          apiKey: "b-key",
          models: ["model-b"],
        },
        {
          name: "provider-c",
          type: "openai",
          baseUrl: "https://c.example/v1",
          apiKey: "c-key",
          models: ["model-c"],
        },
      ],
      routing: {
        mode: "auto",
        brains: [{ channel: "typesafe", apiKeyEnv: "TYPESAFE_API_KEY", timeoutMs: 1_000 }],
        tiers: {
          plan: ["model-a", "model-b", "model-c"],
          execute: ["model-a", "model-b", "model-c"],
          utility: ["model-c"],
          chat: ["model-c"],
        },
      },
    });

    const hits = { a: 0, b: 0, c: 0 };
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      if (url.includes("typesafe") || url.includes("systemone") || url.includes("evaluation")) {
        return new Response(
          JSON.stringify({
            model: "jev-1.13.0",
            answers: { model: { choice: "plan", confidence: 0.9 } },
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.includes("a.example")) hits.a += 1;
      else if (url.includes("b.example")) hits.b += 1;
      else if (url.includes("c.example")) hits.c += 1;
      // Every host answers 429 with a wording no allow-list knows.
      return new Response(JSON.stringify({ err: "nope" }), { status: 429 });
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "jevonian/auto",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    // Every provider got exactly one shot, and only then is the refusal surfaced. The old
    // fixed cap of 2 would have returned after provider-b.
    expect(hits).toEqual({ a: 1, b: 1, c: 1 });
    expect(response.status).toBe(429);
    expect(response.headers.get("x-jevonian-quota-failovers")).toBe("2");
  });
});
