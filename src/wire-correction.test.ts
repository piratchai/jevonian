import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { parseConfig, parseModelEntries, mergeModelEntries } from "./config";
import { readRecords, resetLedgerCache } from "./ledger";
import { SessionStore } from "./routing";
import { createApp } from "./server";
import {
  normalizeOpenAIMessages,
  sanitizeOpenAIChatResponse,
  sanitizeOpenAIChatStream,
} from "./wire";

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

  it("prunes older historical images when multiple images accumulate across turns", () => {
    const input = [
      {
        role: "user",
        content: [
          { type: "image_url", image_url: { url: "data:image/png;base64,old1" } },
        ],
      },
      {
        role: "assistant",
        content: "I see screenshot 1.",
      },
      {
        role: "user",
        content: [
          { type: "image_url", image_url: { url: "data:image/png;base64,old2" } },
        ],
      },
      {
        role: "assistant",
        content: "I see screenshot 2.",
      },
      {
        role: "user",
        content: [
          { type: "text", text: "Look at latest screenshot" },
          { type: "image_url", image_url: { url: "data:image/png;base64,latest" } },
        ],
      },
    ];

    const result = normalizeOpenAIMessages(input);
    // The first image should be pruned and replaced with text placeholder
    expect(result[0]).toEqual({
      role: "user",
      content: "[Previous screenshot omitted to prevent multimodal timeout]",
    });
    // The second and third images should be preserved (MAX_IMAGES_TO_KEEP = 2)
    expect(result[2]).toEqual({
      role: "user",
      content: [
        { type: "image_url", image_url: { url: "data:image/png;base64,old2" } },
      ],
    });
    expect(result[4]).toEqual({
      role: "user",
      content: [
        { type: "text", text: "Look at latest screenshot" },
        { type: "image_url", image_url: { url: "data:image/png;base64,latest" } },
      ],
    });
  });

  it("converts Anthropic image blocks into OpenAI image_url format", () => {
    const input = [
      {
        role: "user",
        content: [
          {
            type: "image",
            source: {
              type: "base64",
              media_type: "image/jpeg",
              data: "jpegdata123",
            },
          },
        ],
      },
    ];

    expect(normalizeOpenAIMessages(input)).toEqual([
      {
        role: "user",
        content: [
          {
            type: "image_url",
            image_url: {
              url: "data:image/jpeg;base64,jpegdata123",
            },
          },
        ],
      },
    ]);
  });

  it("converts Anthropic URL image blocks into OpenAI image_url format", () => {
    const input = [
      {
        role: "user",
        content: [
          {
            type: "image",
            source: {
              type: "url",
              url: "https://example.com/screenshot.png",
            },
          },
        ],
      },
    ];

    expect(normalizeOpenAIMessages(input)).toEqual([
      {
        role: "user",
        content: [
          {
            type: "image_url",
            image_url: {
              url: "https://example.com/screenshot.png",
            },
          },
        ],
      },
    ]);
  });

  it("strips empty tool_calls arrays from messages to satisfy strict OpenAI validators", () => {
    const input = [
      {
        role: "assistant",
        content: "Both files are truncated; let me continue reading.",
        tool_calls: [],
      },
      {
        role: "user",
        content: "Have you done?",
      },
      {
        role: "assistant",
        content: [
          { type: "text", text: "Checking next file..." },
        ],
        tool_calls: [],
      },
    ];

    const result = normalizeOpenAIMessages(input);
    expect(result[0]).toEqual({
      role: "assistant",
      content: "Both files are truncated; let me continue reading.",
    });
    expect("tool_calls" in result[0]).toBe(false);
    expect(result[2]).toEqual({
      role: "assistant",
      content: "Checking next file...",
    });
    expect("tool_calls" in result[2]).toBe(false);
  });
});

describe("OpenAI Chat tool call streaming sanitization", () => {
  it("strips empty id: '' and name: '' from streaming tool_call deltas", async () => {
    const rawEvents = [
      'data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_123","type":"function","index":0,"function":{"name":"get_weather","arguments":""}}]}}]}\n\n',
      'data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"","type":"function","index":0,"function":{"arguments":"{\\"city\\": "}}]}}]}\n\n',
      'data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"","type":"function","index":0,"function":{"arguments":"\\"Tokyo\\"}"}}]}}]}\n\n',
      'data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":""},"index":0,"type":"function","id":""}]}}]}\n\n',
      "data: [DONE]\n\n",
    ].join("");

    const encoder = new TextEncoder();
    const decoder = new TextDecoder();
    const inputStream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(rawEvents));
        controller.close();
      },
    });

    const outputStream = inputStream.pipeThrough(sanitizeOpenAIChatStream());
    const reader = outputStream.getReader();
    let result = "";
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      result += decoder.decode(value, { stream: true });
    }

    // Chunk 1 preserves id and name
    expect(result).toContain('"id":"call_123"');
    expect(result).toContain('"name":"get_weather"');

    // Chunks 2, 3, 4 strip id: "" so OpenCode does not fail with "tool call delta is missing id or name"
    const lines = result.split("\n").filter((l) => l.startsWith("data:") && !l.includes("[DONE]"));
    expect(lines).toHaveLength(4);

    const chunk2 = JSON.parse(lines[1].slice(5));
    expect(chunk2.choices[0].delta.tool_calls[0].id).toBeUndefined();
    expect(chunk2.choices[0].delta.tool_calls[0].function.arguments).toBe('{"city": ');

    const chunk3 = JSON.parse(lines[2].slice(5));
    expect(chunk3.choices[0].delta.tool_calls[0].id).toBeUndefined();

    const chunk4 = JSON.parse(lines[3].slice(5));
    expect(chunk4.choices[0].delta.tool_calls[0].id).toBeUndefined();
  });

  it("sanitizes non-streaming responses with empty id or name", () => {
    const raw = {
      choices: [
        {
          message: {
            role: "assistant",
            tool_calls: [
              {
                id: "",
                type: "function",
                function: { name: "", arguments: "{}" },
              },
            ],
          },
        },
      ],
    };
    const sanitized = sanitizeOpenAIChatResponse(raw);
    const tc = (sanitized.choices as any[])[0].message.tool_calls[0];
    expect(tc.id).toBeUndefined();
    expect(tc.function.name).toBeUndefined();
  });

  it("records status 200 and retains token usage when client cancels reader after receiving tool calls", async () => {
    const testDir = mkdtempSync(join(tmpdir(), "jevonian-cancel-test-"));
    const ledgerFile = join(testDir, "ledger.jsonl");
    const oldDir = process.env.JEVONIAN_DATA_DIR;
    const oldLedger = process.env.JEVONIAN_LEDGER;
    process.env.JEVONIAN_DATA_DIR = testDir;
    process.env.JEVONIAN_LEDGER = ledgerFile;
    resetLedgerCache();

    try {
      const config = parseConfig({
        defaultProvider: "qwen",
        providers: [
          {
            name: "qwen",
            type: "openai",
            baseUrl: "https://mock.openai.com/v1",
            apiKey: "test-key",
            models: ["qwen3.8-flash"],
          },
        ],
        routing: { mode: "off" },
      });

      vi.stubGlobal("fetch", async () => {
        const enc = new TextEncoder();
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(
              enc.encode(
                'data: {"id":"chat-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\\"cmd\\":\\"ls\\"}"}}]},"finish_reason":null}]}\n\n',
              ),
            );
            controller.enqueue(
              enc.encode(
                'data: {"id":"chat-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":350,"completion_tokens":45,"total_tokens":395}}\n\n',
              ),
            );
            controller.enqueue(enc.encode("data: [DONE]\n\n"));
          },
        });
        return new Response(stream, {
          status: 200,
          headers: { "content-type": "text/event-stream" },
        });
      });

      const app = createApp({ config }, new SessionStore(60_000));
      const response = await app.request("/v1/chat/completions", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          model: "qwen3.8-flash",
          stream: true,
          messages: [{ role: "user", content: "run ls" }],
        }),
      });

      expect(response.status).toBe(200);
      const reader = response.body?.getReader();
      expect(reader).toBeDefined();

      // Read the tool call chunks
      await reader?.read();
      await reader?.read();

      // Simulate OpenCode canceling the reader to execute the tool locally
      await reader?.cancel();

      // Wait a brief moment for the cancel transform to complete
      await new Promise((resolve) => setTimeout(resolve, 50));

      const records = readRecords();
      expect(records).toHaveLength(1);
      const record = records[0];
      expect(record.status).toBe(200);
      expect(record.promptTokens).toBe(350);
      expect(record.completionTokens).toBe(45);
      expect(record.error).toBeUndefined();
    } finally {
      if (oldDir === undefined) delete process.env.JEVONIAN_DATA_DIR;
      else process.env.JEVONIAN_DATA_DIR = oldDir;
      if (oldLedger === undefined) delete process.env.JEVONIAN_LEDGER;
      else process.env.JEVONIAN_LEDGER = oldLedger;
      resetLedgerCache();
      rmSync(testDir, { recursive: true, force: true });
    }
  });
});


