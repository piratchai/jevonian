import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseConfig, type Config } from "./config";
import { readRecords, resetLedgerCache } from "./ledger";
import { SessionStore } from "./routing";
import { createApp } from "./server";
import { SOFT_ERROR_PREFIX } from "./soft-error";

let dir = "";
const saved: Record<string, string | undefined> = {};

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-soft-"));
  for (const key of ["JEVONIAN_DATA_DIR", "JEVONIAN_LEDGER"]) {
    saved[key] = process.env[key];
  }
  process.env.JEVONIAN_DATA_DIR = dir;
  process.env.JEVONIAN_LEDGER = join(dir, "ledger.jsonl");
  resetLedgerCache();
});

afterEach(() => {
  vi.unstubAllGlobals();
  resetLedgerCache();
  for (const [key, value] of Object.entries(saved)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  rmSync(dir, { recursive: true, force: true });
});

function config(): Config {
  return parseConfig({
    defaultProvider: "openai-compatible",
    providers: [
      {
        name: "openai-compatible",
        type: "openai",
        baseUrl: "https://api.example.com/v1",
        apiKey: "key",
        models: ["gpt-test"],
      },
    ],
    routing: { mode: "off" },
  });
}

function sse(chunks: string[]): Response {
  return new Response(chunks.join(""), {
    status: 200,
    headers: { "content-type": "text/event-stream" },
  });
}

describe("streaming soft fallback", () => {
  it("answers a streamed Chat request with a soft assistant turn instead of a JSON 5xx", async () => {
    vi.stubGlobal("fetch", async () => {
      return new Response(
        JSON.stringify({ error: { message: "upstream is on fire", type: "server_error" } }),
        { status: 500, headers: { "content-type": "application/json" } },
      );
    });

    const app = createApp({ config: config() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-test",
        stream: true,
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    // A JSON 5xx here is what makes a harness roll the whole user message back.
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("text/event-stream");
    expect(response.headers.get("x-jevonian-soft-error")).toBe("1");

    const text = await response.text();
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text).toContain('"finish_reason":"stop"');
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
  });

  it("closes a dropped stream as a soft assistant turn", async () => {
    vi.stubGlobal("fetch", async () => {
      // A 200 SSE body that delivers a delta and then dies before any finish marker: exactly
      // what a dropped upstream socket looks like to the client.
      let pulls = 0;
      const body = new ReadableStream<Uint8Array>({
        pull(controller) {
          if (pulls === 0) {
            pulls += 1;
            controller.enqueue(
              new TextEncoder().encode('data: {"choices":[{"delta":{"content":"partial"}}]}\n\n'),
            );
            return;
          }
          throw new Error("socket hang up");
        },
      });
      return new Response(body, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    });

    const app = createApp({ config: config() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-test",
        stream: true,
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).toContain("partial");
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
  });

  it("replaces a mid-stream `{ error }` frame with a soft close", async () => {
    vi.stubGlobal("fetch", async () =>
      sse([
        'data: {"choices":[{"delta":{"content":"partial"}}]}\n\n',
        'data: {"error":{"message":"upstream refused","type":"server_error"}}\n\n',
      ]),
    );

    const app = createApp({ config: config() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-test",
        stream: true,
        messages: [{ role: "user", content: "hi" }],
      }),
    });

    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).not.toContain('"error"');
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text).toContain("upstream refused");
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
  });

  it("records the real 502 in the ledger for a failed streamed turn", async () => {
    vi.stubGlobal("fetch", async () =>
      sse([
        'data: {"choices":[{"delta":{"content":"partial"}}]}\n\n',
        'data: {"error":{"message":"upstream refused","type":"server_error"}}\n\n',
      ]),
    );

    const app = createApp({ config: config() }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-test",
        stream: true,
        messages: [{ role: "user", content: "hi" }],
      }),
    });
    await response.text();

    const records = readRecords().filter((record) => record.kind !== "brain");
    expect(records).toHaveLength(1);
    expect(records[0]?.status).toBe(502);
    expect(records[0]?.error).toContain("upstream refused");
  });

  it("redacts a credential echoed in the upstream error body", async () => {
    const key = "sk-live-super-secret-credential";
    const cfg = parseConfig({
      defaultProvider: "openai-compatible",
      providers: [
        {
          name: "openai-compatible",
          type: "openai",
          baseUrl: "https://api.example.com/v1",
          apiKey: key,
          models: ["gpt-test"],
        },
      ],
      routing: { mode: "off" },
    });
    vi.stubGlobal("fetch", async () => {
      return new Response(
        JSON.stringify({ error: { message: `Invalid key ${key}`, type: "auth_error" } }),
        { status: 401, headers: { "content-type": "application/json" } },
      );
    });

    const app = createApp({ config: cfg }, new SessionStore(60_000));
    const response = await app.request("/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-test",
        stream: true,
        messages: [{ role: "user", content: "hi" }],
      }),
    });
    const text = await response.text();
    expect(text).not.toContain(key);
    expect(text).toContain("[REDACTED]");
  });

  it("records a 502 when an Anthropic upstream error is folded for a Responses client", async () => {
    const cfg = parseConfig({
      defaultProvider: "anthropic-host",
      providers: [
        {
          name: "anthropic-host",
          type: "anthropic",
          baseUrl: "https://api.anthropic.com/v1",
          apiKey: "key",
          models: ["claude-test"],
        },
      ],
      routing: { mode: "off" },
    });
    vi.stubGlobal("fetch", async () =>
      sse([
        'event: message_start\ndata: {"type":"message_start","message":{"usage":{"input_tokens":1}}}\n\n',
        'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}\n\n',
        'event: error\ndata: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}\n\n',
      ]),
    );

    const app = createApp({ config: cfg }, new SessionStore(60_000));
    const response = await app.request("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "claude-test",
        stream: true,
        input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "hi" }] }],
      }),
    });
    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).not.toContain("response.failed");
    expect(text).toContain("response.completed");
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text).toContain("Overloaded");

    const records = readRecords().filter((record) => record.kind !== "brain");
    expect(records).toHaveLength(1);
    expect(records[0]?.status).toBe(502);
    expect(records[0]?.error).toContain("Overloaded");
  });

  it("records a 502 when a Chat upstream error is bridged to a Responses client", async () => {
    const cfg = parseConfig({
      defaultProvider: "openai-compatible",
      providers: [
        {
          name: "openai-compatible",
          type: "openai",
          baseUrl: "https://api.example.com/v1",
          apiKey: "key",
          models: ["gpt-test"],
        },
      ],
      routing: { mode: "off" },
    });
    vi.stubGlobal("fetch", async () =>
      sse([
        'data: {"choices":[{"delta":{"content":"partial"}}]}\n\n',
        'data: {"error":{"message":"upstream refused","type":"server_error"}}\n\n',
      ]),
    );

    const app = createApp({ config: cfg }, new SessionStore(60_000));
    const response = await app.request("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-test",
        stream: true,
        input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "hi" }] }],
      }),
    });
    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).not.toContain("response.failed");
    expect(text).toContain("response.completed");

    const records = readRecords().filter((record) => record.kind !== "brain");
    expect(records).toHaveLength(1);
    expect(records[0]?.status).toBe(502);
    expect(records[0]?.error).toContain("upstream refused");
  });

  it("replaces a native Responses response.failed with a soft completion", async () => {
    const cfg = parseConfig({
      defaultProvider: "codex",
      providers: [
        {
          name: "codex",
          type: "responses",
          baseUrl: "https://chatgpt.com/backend-api/codex",
          apiKey: "codex-key",
          models: ["gpt-6-astra"],
        },
      ],
      routing: { mode: "off" },
    });
    vi.stubGlobal("fetch", async () =>
      sse([
        'event: response.created\ndata: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}\n\n',
        'event: response.failed\ndata: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"backend exploded"}}}\n\n',
      ]),
    );

    const app = createApp({ config: cfg }, new SessionStore(60_000));
    const response = await app.request("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "gpt-6-astra",
        stream: true,
        input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "hi" }] }],
      }),
    });
    expect(response.status).toBe(200);
    const text = await response.text();
    expect(text).not.toContain("response.failed");
    expect(text).toContain("response.completed");
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text).toContain("backend exploded");
  });
});
