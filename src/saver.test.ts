import { describe, expect, it } from "vite-plus/test";

import {
  compressToolResult,
  DEFAULT_TOKEN_SAVER,
  parseTokenSaver,
  saveTokens,
} from "./saver";

function lines(prefix: string, count: number): string {
  return Array.from({ length: count }, (_, i) => `${prefix} ${i}`).join("\n");
}

describe("parseTokenSaver", () => {
  it("defaults to enabled with the default settings", () => {
    expect(parseTokenSaver(undefined)).toEqual(DEFAULT_TOKEN_SAVER);
    expect(parseTokenSaver(null)).toEqual(DEFAULT_TOKEN_SAVER);
    expect(parseTokenSaver({})).toEqual(DEFAULT_TOKEN_SAVER);
  });

  it("honours explicit flags", () => {
    expect(parseTokenSaver({ enabled: false }).enabled).toBe(false);
    expect(parseTokenSaver({ dedupeLines: false }).dedupeLines).toBe(false);
    expect(parseTokenSaver({ stripNoise: false }).stripNoise).toBe(false);
    expect(parseTokenSaver({ maxChars: 5_000 }).maxChars).toBe(5_000);
  });

  it("clamps a destructive maxChars", () => {
    // 0 or a tiny cap would leave only the "removed N chars" marker; clamp instead.
    expect(parseTokenSaver({ maxChars: 0 }).maxChars).toBe(500);
    expect(parseTokenSaver({ maxChars: 42 }).maxChars).toBe(500);
  });
});

describe("compressToolResult", () => {
  const config = DEFAULT_TOKEN_SAVER;

  it("leaves short text untouched", () => {
    const text = "ok\nall tests passed";
    expect(compressToolResult(text, config)).toBe(text);
  });

  it("collapses runs of identical lines", () => {
    const repeated = Array.from({ length: 30 }, () => "… downloading").join("\n");
    const text = `${lines("line", 8)}\n${repeated}\n${lines("tail", 4)}`;
    const out = compressToolResult(text, config);
    expect(out).toContain("×29");
    expect(out.length).toBeLessThan(text.length);
  });

  it("strips noise lines", () => {
    const noisy = [
      ...Array.from({ length: 6 }, (_, i) => `real ${i}`),
      "npm warn deprecated foo",
      "npm warn deprecated bar",
      "  45% | downloading",
      ...Array.from({ length: 6 }, (_, i) => `real ${i + 6}`),
    ].join("\n");
    const out = compressToolResult(noisy, config);
    expect(out).not.toContain("npm warn");
    expect(out).not.toContain("downloading");
    expect(out).toContain("real 0");
  });

  it("truncates output beyond maxChars keeping head and tail", () => {
    const text = `${"h".repeat(20_000)}${"m".repeat(20_000)}${"t".repeat(20_000)}`;
    const out = compressToolResult(text, { ...config, maxChars: 10_000 });
    expect(out.length).toBeLessThan(11_000);
    expect(out).toContain("hhhh");
    expect(out).toContain("tttt");
    expect(out).toContain("removed");
    expect(out).not.toContain("mmmm");
  });

  it("never returns a larger body than the input", () => {
    // Just over the clamp floor: head+tail+marker exceeds the input, so abridge must refuse.
    const text = "x".repeat(510);
    const out = compressToolResult(text, { ...config, maxChars: 500 });
    expect(out).toBe(text);
  });

  it("never grows a small output", () => {
    const text = "one line";
    expect(compressToolResult(text, config)).toBe(text);
  });
});

describe("saveTokens", () => {
  const config = DEFAULT_TOKEN_SAVER;
  const bigOutput = `${lines("PASS ok", 40)}\n${Array.from({ length: 50 }, () => "Compiling foo").join("\n")}\nnpm warn deprecated x\n${"x".repeat(5_000)}`;

  it("returns the same body when disabled", () => {
    const body = { messages: [{ role: "user", content: "hi" }] };
    const { body: out, stats } = saveTokens(body, "openai", { ...config, enabled: false });
    expect(out).toBe(body);
    expect(stats.savedTokens).toBe(0);
  });

  it("compresses OpenAI tool messages", () => {
    const body = {
      model: "gpt",
      messages: [
        { role: "user", content: "run the tests" },
        { role: "assistant", content: "", tool_calls: [{ id: "c1", type: "function", function: { name: "bash", arguments: "{}" } }] },
        { role: "tool", tool_call_id: "c1", content: bigOutput },
      ],
    };
    const { body: out, stats } = saveTokens(body, "openai", config);
    const messages = out.messages as Array<Record<string, unknown>>;
    const tool = messages[2]!;
    expect(typeof tool.content).toBe("string");
    expect((tool.content as string).length).toBeLessThan(bigOutput.length);
    expect(stats.resultsCompressed).toBe(1);
    expect(stats.savedTokens).toBeGreaterThan(0);
    // Original body is not mutated.
    expect((body.messages[2] as { content: string }).content).toBe(bigOutput);
  });

  it("compresses OpenAI tool messages with array content", () => {
    const body = {
      messages: [
        { role: "assistant", tool_calls: [{ id: "c1", type: "function", function: { name: "bash", arguments: "{}" } }] },
        {
          role: "tool",
          tool_call_id: "c1",
          content: [{ type: "text", text: bigOutput }],
        },
      ],
    };
    const { body: out, stats } = saveTokens(body, "openai", config);
    const tool = (out.messages as Array<Record<string, unknown>>)[1]!;
    const blocks = tool.content as Array<Record<string, unknown>>;
    expect((blocks[0]!.text as string).length).toBeLessThan(bigOutput.length);
    expect(stats.resultsCompressed).toBe(1);
    expect(stats.savedTokens).toBeGreaterThan(0);
  });

  it("compresses Anthropic tool_result blocks", () => {
    const body = {
      model: "claude",
      messages: [
        { role: "user", content: "run tests" },
        { role: "assistant", content: [{ type: "tool_use", id: "t1", name: "bash", input: {} }] },
        {
          role: "user",
          content: [{ type: "tool_result", tool_use_id: "t1", content: bigOutput }],
        },
      ],
    };
    const { body: out, stats } = saveTokens(body, "anthropic", config);
    const user = (out.messages as Array<Record<string, unknown>>)[2]!;
    const content = user.content as Array<Record<string, unknown>>;
    const result = content[0]!;
    expect(typeof result.content).toBe("string");
    expect((result.content as string).length).toBeLessThan(bigOutput.length);
    expect(stats.savedTokens).toBeGreaterThan(0);
  });

  it("compresses Anthropic tool_result text-block arrays", () => {
    const body = {
      messages: [
        {
          role: "user",
          content: [
            {
              type: "tool_result",
              tool_use_id: "t1",
              content: [{ type: "text", text: bigOutput }],
            },
          ],
        },
      ],
    };
    const { body: out, stats } = saveTokens(body, "anthropic", config);
    const result = ((out.messages as Array<Record<string, unknown>>)[0]!.content as Array<Record<string, unknown>>)[0]!;
    const blocks = result.content as Array<Record<string, unknown>>;
    expect((blocks[0]!.text as string).length).toBeLessThan(bigOutput.length);
    expect(stats.resultsCompressed).toBe(1);
  });

  it("compresses Responses function_call_output items", () => {
    const body = {
      input: [
        { type: "message", role: "user", content: [{ type: "input_text", text: "go" }] },
        { type: "function_call", call_id: "c1", name: "bash", arguments: "{}" },
        { type: "function_call_output", call_id: "c1", output: bigOutput },
      ],
    };
    const { body: out, stats } = saveTokens(body, "responses", config);
    const items = out.input as Array<Record<string, unknown>>;
    const output = items[2]!;
    expect((output.output as string).length).toBeLessThan(bigOutput.length);
    expect(stats.savedTokens).toBeGreaterThan(0);
  });

  it("leaves bodies without tool results untouched", () => {
    const body = { messages: [{ role: "user", content: "hi" }] };
    const { body: out, stats } = saveTokens(body, "openai", config);
    expect(out).toBe(body);
    expect(stats.resultsCompressed).toBe(0);
    expect(stats.savedTokens).toBe(0);
  });

  it("sums savings across several results", () => {
    const body = {
      messages: [
        { role: "tool", tool_call_id: "a", content: bigOutput },
        { role: "tool", tool_call_id: "b", content: bigOutput },
      ],
    };
    const { stats } = saveTokens(body, "openai", config);
    expect(stats.resultsCompressed).toBe(2);
  });
});
