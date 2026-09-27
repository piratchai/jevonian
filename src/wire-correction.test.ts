import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseConfig, parseModelEntries, mergeModelEntries } from "./config";
import { SessionStore } from "./routing";
import { createApp } from "./server";
import { normalizeOpenAIMessages } from "./wire";

describe("model entry parse", () => {
  it("accepts bare strings and { id, wire } objects", () => {
    expect(parseModelEntries(["a", { id: "b", wire: "anthropic" }, { model: "c" }])).toEqual([
      { id: "a" },
      { id: "b", wire: "anthropic" },
      { id: "c" },
    ]);
  });

  it("preserves wire pins when the UI saves bare ids", () => {
    const merged = mergeModelEntries(
      [{ id: "keep", wire: "responses" }, { id: "drop" }],
      ["keep", "new"],
    );
    expect(merged).toEqual([{ id: "keep", wire: "responses" }, { id: "new" }]);
  });
});

describe("OpenCode Responses passthrough", () => {
  let dir = "";
  const saved: Record<string, string | undefined> = {};

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "jevonian-wire-"));
    for (const key of ["JEVONIAN_DATA_DIR", "JEVONIAN_LEDGER"]) {
      saved[key] = process.env[key];
    }
    process.env.JEVONIAN_DATA_DIR = dir;
    process.env.JEVONIAN_LEDGER = join(dir, "ledger.jsonl");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    rmSync(dir, { recursive: true, force: true });
  });

  it("forwards Codex /responses to OpenCode /responses without a Chat bridge", async () => {
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
      ],
      routing: { mode: "off" },
    });

    const hits: string[] = [];
    vi.stubGlobal("fetch", async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
      const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
      hits.push(url);
      const body = typeof init?.body === "string" ? JSON.parse(init.body) : {};
      expect(url).toContain("/responses");
      expect(url).not.toContain("/chat/completions");
      expect(url).not.toContain("/messages");
      expect(body.input).toBeDefined();
      expect(body.messages).toBeUndefined();
      return new Response(
        [
          'event: response.created\ndata: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}\n\n',
          'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"ok"}\n\n',
          'event: response.completed\ndata: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}\n\n',
        ].join(""),
        { status: 200, headers: { "content-type": "text/event-stream" } },
      );
    });

    const app = createApp({ config }, new SessionStore(60_000));
    const response = await app.request("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "deepseek-v4.1-flash",
        stream: true,
        input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "hi" }] }],
      }),
    });

    expect(response.status).toBe(200);
    expect(hits).toEqual(["https://opencode.ai/zen/go/v1/responses"]);
    expect(await response.text()).toContain("response.output_text.delta");
  });
});

describe("normalizeOpenAIMessages", () => {
  it("leaves standard string messages untouched", () => {
    const input = [
      { role: "system", content: "You are helpful." },
      { role: "user", content: "Hello world" },
      { role: "assistant", content: "Hi there!" },
    ];
    expect(normalizeOpenAIMessages(input)).toEqual(input);
  });

  it("joins text arrays into a single string for strict OpenAI backends", () => {
    const input = [
      {
        role: "user",
        content: [
          { type: "text", text: "Line 1" },
          { type: "text", text: "Line 2" },
        ],
      },
    ];
    expect(normalizeOpenAIMessages(input)).toEqual([
      { role: "user", content: "Line 1\nLine 2" },
    ]);
  });

  it("translates assistant tool_use blocks to OpenAI tool_calls", () => {
    const input = [
      {
        role: "assistant",
        content: [
          { type: "text", text: "Checking files..." },
          {
            type: "tool_use",
            id: "call_abc123",
            name: "read_file",
            input: { path: "app.ts" },
          },
        ],
      },
    ];
    expect(normalizeOpenAIMessages(input)).toEqual([
      {
        role: "assistant",
        content: "Checking files...",
        tool_calls: [
          {
            id: "call_abc123",
            type: "function",
            function: {
              name: "read_file",
              arguments: JSON.stringify({ path: "app.ts" }),
            },
          },
        ],
      },
    ]);
  });

  it("translates user tool_result blocks to OpenAI role: tool messages", () => {
    const input = [
      {
        role: "user",
        content: [
          {
            type: "tool_result",
            tool_use_id: "call_abc123",
            content: "file content here",
          },
        ],
      },
    ];
    expect(normalizeOpenAIMessages(input)).toEqual([
      {
        role: "tool",
        tool_call_id: "call_abc123",
        content: "file content here",
      },
    ]);
  });

  it("preserves multimodal image_url content items", () => {
    const input = [
      {
        role: "user",
        content: [
          { type: "text", text: "Look at this image" },
          { type: "image_url", image_url: { url: "data:image/png;base64,abc" } },
        ],
      },
    ];
    expect(normalizeOpenAIMessages(input)).toEqual([
      {
        role: "user",
        content: [
          { type: "text", text: "Look at this image" },
          { type: "image_url", image_url: { url: "data:image/png;base64,abc" } },
        ],
      },
    ]);
  });
});

