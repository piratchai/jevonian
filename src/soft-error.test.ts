import { describe, expect, it, vi } from "vite-plus/test";

import {
  SOFT_ERROR_PREFIX,
  softCompletionStream,
  softErrorMessage,
  withSoftCompletion,
} from "./soft-error";

const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** Drain a byte stream to text. */
async function readText(stream: ReadableStream<Uint8Array>): Promise<string> {
  const chunks: string[] = [];
  const reader = stream.getReader();
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    chunks.push(decoder.decode(value, { stream: true }));
  }
  chunks.push(decoder.decode());
  return chunks.join("");
}

/** A stream that yields `chunks`, then optionally fails the way a dropped socket does. */
function source(
  chunks: Uint8Array[],
  options: { failAfter?: number } = {},
): ReadableStream<Uint8Array> {
  let index = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (options.failAfter !== undefined && index === options.failAfter) {
        throw new Error("socket hang up");
      }
      if (index >= chunks.length) {
        controller.close();
        return;
      }
      controller.enqueue(chunks[index++]);
    },
  });
}

describe("softErrorMessage", () => {
  it("names Jevonian and asks for a retry", () => {
    const message = softErrorMessage("upstream refused");
    expect(message.startsWith(SOFT_ERROR_PREFIX)).toBe(true);
    expect(message).toContain("upstream refused");
    expect(message).toContain("retry");
  });

  it("collapses whitespace and caps the reason", () => {
    const message = softErrorMessage(`a\n\n  b${"x".repeat(500)}`);
    expect(message).toContain("a b");
    expect(message.length).toBeLessThan(SOFT_ERROR_PREFIX.length + 350);
  });

  it("handles an empty reason without a dangling colon", () => {
    expect(softErrorMessage("   ")).toBe(
      `${SOFT_ERROR_PREFIX}. The turn was stopped safely — please retry.`,
    );
  });
});

describe("softCompletionStream", () => {
  it("closes a chat wire with a normal assistant turn", async () => {
    const text = await readText(softCompletionStream("openai", "m", "boom"));
    expect(text).toContain('"role":"assistant"');
    expect(text).toContain('"content":"boom"');
    expect(text).toContain('"finish_reason":"stop"');
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
  });

  it("closes an anthropic wire with message_stop", async () => {
    const text = await readText(softCompletionStream("anthropic", "m", "boom"));
    expect(text).toContain("event: message_start");
    expect(text).toContain("event: message_stop");
    expect(text).toContain('"stop_reason":"end_turn"');
    expect(text).not.toContain("event: error");
  });

  it("closes a responses wire with response.completed", async () => {
    const text = await readText(softCompletionStream("responses", "m", "boom"));
    expect(text).toContain("event: response.created");
    expect(text).toContain("event: response.output_text.delta");
    expect(text).toContain("event: response.completed");
    expect(text).not.toContain("event: response.failed");
  });
});

describe("withSoftCompletion", () => {
  it("appends a soft close when the upstream dies mid-stream", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([encoder.encode('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n')], {
        failAfter: 1,
      }),
      "openai",
      "m",
      softErrorMessage("the upstream stream ended unexpectedly"),
      onSoft,
    );
    const text = await readText(stream);
    expect(text).toContain('"content":"hi"');
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
    expect(onSoft).toHaveBeenCalledTimes(1);
  });

  it("appends a soft close when the upstream ends without a finish marker", async () => {
    const stream = withSoftCompletion(
      source([encoder.encode('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n')]),
      "openai",
      "m",
      softErrorMessage("ended early"),
    );
    const text = await readText(stream);
    expect(text).toContain('"content":"hi"');
    expect(text).toContain(SOFT_ERROR_PREFIX);
  });

  it("passes a normally finished stream through untouched", async () => {
    const body = [
      'data: {"choices":[{"delta":{"content":"hi"}}]}\n\n',
      'data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n',
      "data: [DONE]\n\n",
    ]
      .map((line) => encoder.encode(line))
      .reduce((all, chunk) => {
        const merged = new Uint8Array(all.length + chunk.length);
        merged.set(all);
        merged.set(chunk, all.length);
        return merged;
      }, new Uint8Array());
    const onSoft = vi.fn();
    const stream = withSoftCompletion(source([body]), "openai", "m", "unused", onSoft);
    const text = await readText(stream);
    expect(text).toBe(decoder.decode(body));
    expect(onSoft).not.toHaveBeenCalled();
  });

  it("recognises a finish marker split across two chunks", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([
        encoder.encode('data: {"choices":[{"delta":{},"finish_reason":"st'),
        encoder.encode('op"}]}\n\ndata: [DONE]\n\n'),
      ]),
      "openai",
      "m",
      "unused",
      onSoft,
    );
    await readText(stream);
    expect(onSoft).not.toHaveBeenCalled();
  });

  it("stops answering after the client hangs up", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([encoder.encode('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n')]),
      "openai",
      "m",
      "unused",
      onSoft,
    );
    const reader = stream.getReader();
    await reader.read();
    await reader.cancel();
    expect(onSoft).not.toHaveBeenCalled();
  });

  it("swallows an openai error frame and closes softly with its reason", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([
        encoder.encode('data: {"choices":[{"delta":{"content":"partial"}}]}\n\n'),
        encoder.encode('data: {"error":{"message":"upstream refused","type":"server_error"}}\n\n'),
      ]),
      "openai",
      "m",
      softErrorMessage("stream ended unexpectedly"),
      onSoft,
    );
    const text = await readText(stream);
    expect(text).toContain("partial");
    expect(text).not.toContain('"error"');
    expect(text).toContain(SOFT_ERROR_PREFIX);
    expect(text).toContain("upstream refused");
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
    expect(onSoft).toHaveBeenCalledWith("upstream refused");
  });

  it("swallows an anthropic error event and closes softly", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([
        encoder.encode(
          'event: error\ndata: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}\n\n',
        ),
      ]),
      "anthropic",
      "m",
      softErrorMessage("stream ended unexpectedly"),
      onSoft,
    );
    const text = await readText(stream);
    expect(text).not.toContain('"type":"error"');
    expect(text).toContain("event: message_stop");
    expect(text).toContain("Overloaded");
    expect(onSoft).toHaveBeenCalledWith("Overloaded");
  });

  it("swallows a responses response.failed event and completes the turn", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([
        encoder.encode(
          'event: response.failed\ndata: {"type":"response.failed","response":{"error":{"message":"nope"}}}\n\n',
        ),
      ]),
      "responses",
      "m",
      softErrorMessage("stream ended unexpectedly"),
      onSoft,
    );
    const text = await readText(stream);
    expect(text).not.toContain("response.failed");
    expect(text).toContain("event: response.completed");
    expect(text).toContain("nope");
    expect(onSoft).toHaveBeenCalledWith("nope");
  });

  it("continues an anthropic stream at the next free block index", async () => {
    // A failure after `content_block_start{index:0}` must not emit a second block at index 0:
    // strict Anthropic clients reject the duplicate index.
    const stream = withSoftCompletion(
      source([
        encoder.encode(
          'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n',
        ),
        encoder.encode(
          'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}\n\n',
        ),
      ]),
      "anthropic",
      "m",
      "SOFT",
    );
    const text = await readText(stream);
    const starts = [...text.matchAll(/"type":"content_block_start","index":(\d+)/g)].map((match) =>
      Number(match[1]),
    );
    expect(starts).toEqual([0, 1]);
    // The block left open by the failure is closed before the soft block opens.
    expect(text.indexOf('"index":0}\n\nevent: content_block_start')).toBeGreaterThan(-1);
    expect(text).toContain("event: message_stop");
  });

  it("opens a fresh responses item at the next output index", async () => {
    const stream = withSoftCompletion(
      source([
        encoder.encode(
          'event: response.output_item.added\ndata: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"read","arguments":""}}\n\n',
        ),
        encoder.encode(
          'event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{}"}\n\n',
        ),
      ]),
      "responses",
      "m",
      "SOFT",
    );
    const text = await readText(stream);
    const added = [
      ...text.matchAll(/"type":"response\.output_item\.added","output_index":(\d+)/g),
    ].map((match) => Number(match[1]));
    expect(added).toEqual([0, 1]);
    // The function_call left open is terminated before the soft message item opens.
    expect(text).toContain('"type":"response.function_call_arguments.done"');
    expect(text).toContain('"type":"response.completed"');
  });

  it("does not append a second answer when a finish sits past a 64-byte carry", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([
        encoder.encode('data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n'),
        encoder.encode(`data: {"choices":[{"delta":{"content":"${"x".repeat(200)}"}}]}\n\n`),
      ]),
      "openai",
      "m",
      "unused",
      onSoft,
    );
    const text = await readText(stream);
    expect(text).not.toContain(SOFT_ERROR_PREFIX);
    expect(onSoft).not.toHaveBeenCalled();
  });

  it("does not append a second answer after a [DONE] sentinel", async () => {
    const onSoft = vi.fn();
    const stream = withSoftCompletion(
      source([
        encoder.encode('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n'),
        encoder.encode("data: [DONE]\n\n"),
      ]),
      "openai",
      "m",
      "unused",
      onSoft,
    );
    const text = await readText(stream);
    expect(text.trimEnd().endsWith("data: [DONE]")).toBe(true);
    expect(onSoft).not.toHaveBeenCalled();
  });

  it("does not enqueue after a cancel races an in-flight read", async () => {
    // The cancel happens while the upstream read is still pending; when it resolves, the wrapper
    // must not touch the canceled controller (which would throw).
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const source = new ReadableStream<Uint8Array>({
      async pull(controller) {
        await gate;
        controller.enqueue(encoder.encode('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n'));
      },
    });
    const onSoft = vi.fn();
    const stream = withSoftCompletion(source, "openai", "m", "unused", onSoft);
    const reader = stream.getReader();
    const pending = reader.read();
    await reader.cancel();
    release?.();
    await pending.catch(() => undefined);
    await Promise.resolve();
    expect(onSoft).not.toHaveBeenCalled();
  });
});
