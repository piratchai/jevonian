import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseConfig } from "./config";
import { readRecords, resetLedgerCache } from "./ledger";
import { resetProviderGuards } from "./provider-guard";
import { resetQuotaCache } from "./quota";
import { SessionStore } from "./routing";
import { createApp } from "./server";
import { UpstreamTimeoutError } from "./upstream-timeout";

let dir = "";
const saved: Record<string, string | undefined> = {};

function socketReset(): TypeError {
  return Object.assign(new TypeError("fetch failed"), {
    cause: Object.assign(new Error("read ECONNRESET"), { code: "ECONNRESET" }),
  });
}

function chatCompletion(content = "ok"): Response {
  return new Response(
    JSON.stringify({
      id: "chatcmpl-1",
      choices: [{ message: { role: "assistant", content }, finish_reason: "stop" }],
      usage: { prompt_tokens: 1, completion_tokens: 1 },
    }),
    { status: 200, headers: { "content-type": "application/json" } },
  );
}

function twoProviderConfig() {
  return parseConfig({
    defaultProvider: "flaky",
    providers: [
      {
        name: "flaky",
        type: "openai",
        baseUrl: "https://flaky.example/v1",
        apiKey: "flaky-key",
        models: ["shared-model"],
      },
      {
        name: "healthy",
        type: "openai",
        baseUrl: "https://healthy.example/v1",
        apiKey: "healthy-key",
        models: ["shared-model"],
      },
    ],
    routing: { mode: "auto" },
  });
}

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-transport-failover-"));
  for (const key of [
    "JEVONIAN_DATA_DIR",
    "JEVONIAN_LEDGER",
    "JEVONIAN_SAME_HOST_RETRIES",
    "JEVONIAN_UPSTREAM_FIRST_BYTE_TIMEOUT_MS",
    "JEVONIAN_UPSTREAM_TOTAL_TIMEOUT_MS",
    "JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS",
  ]) {
    saved[key] = process.env[key];
  }
  process.env.JEVONIAN_DATA_DIR = dir;
  process.env.JEVONIAN_LEDGER = join(dir, "ledger.jsonl");
  // No same-host retry: a transport failure should fail over on the first miss.
  process.env.JEVONIAN_SAME_HOST_RETRIES = "0";
  resetQuotaCache();
  resetLedgerCache();
  resetProviderGuards();
});

afterEach(() => {
  vi.unstubAllGlobals();
  for (const [key, value] of Object.entries(saved)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  resetQuotaCache();
  resetLedgerCache();
  resetProviderGuards();
  rmSync(dir, { recursive: true, force: true });
});

describe("transport failover", () => {
  it("switches provider after ECONNRESET instead of returning 502", async () => {
    const hosts: string[] = [];
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      hosts.push(url);
      if (url.includes("flaky.example")) throw socketReset();
      return chatCompletion("from-healthy");
    });

    const app = createApp({ config: twoProviderConfig() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "shared-model",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      choices: Array<{ message: { content: string } }>;
    };
    expect(body.choices[0]?.message.content).toBe("from-healthy");
    expect(hosts.some((url) => url.includes("flaky.example"))).toBe(true);
    expect(hosts.some((url) => url.includes("healthy.example"))).toBe(true);
    expect(response.headers.get("x-jevonian-provider")).toBe("healthy");
    expect(response.headers.get("x-jevonian-reason") ?? "").toContain("quota-failover");

    const records = readRecords().filter((record) => record.kind !== "brain");
    expect(records[0]?.status).toBe(200);
    expect(records[0]?.provider).toBe("healthy");
  });

  it("fails over when the first streamed byte never arrives", async () => {
    process.env.JEVONIAN_UPSTREAM_FIRST_BYTE_TIMEOUT_MS = "50";
    const hosts: string[] = [];
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      hosts.push(url);
      if (url.includes("flaky.example")) {
        // Headers arrive, but the body never yields a chunk — the first-byte clock must fire.
        return new Response(
          new ReadableStream<Uint8Array>({
            start() {
              /* intentionally never enqueue */
            },
          }),
          { status: 200, headers: { "content-type": "text/event-stream" } },
        );
      }
      return new Response(
        [
          'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}\n\n',
          'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}\n\n',
          "data: [DONE]\n\n",
        ].join(""),
        { status: 200, headers: { "content-type": "text/event-stream" } },
      );
    });

    const app = createApp({ config: twoProviderConfig() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "shared-model",
        stream: true,
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).toContain("ok");
    expect(hosts.some((url) => url.includes("flaky.example"))).toBe(true);
    expect(hosts.some((url) => url.includes("healthy.example"))).toBe(true);
  });

  it("treats UpstreamTimeoutError as a failover trigger", async () => {
    const hosts: string[] = [];
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0]) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      hosts.push(url);
      if (url.includes("flaky.example")) throw new UpstreamTimeoutError("headers", 60_000);
      return chatCompletion("recovered");
    });

    const app = createApp({ config: twoProviderConfig() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "shared-model",
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    expect(hosts.some((url) => url.includes("healthy.example"))).toBe(true);
  });
});
