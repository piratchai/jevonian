import { describe, expect, it } from "vite-plus/test";

import {
  classifyCursorError,
  collapseCursorModels,
  cursorContext,
  encodeCursorFrame,
  parseCursorAbout,
  parseCursorModels,
  splitCursorId,
} from "./cursor";

describe("parseCursorAbout", () => {
  it("reads the email and plan from the CLI's JSON", () => {
    expect(parseCursorAbout('{"subscriptionTier":"pro","userEmail":"me@work.dev"}')).toEqual({
      user: "me@work.dev",
      plan: "pro",
    });
  });

  it("skips an update notice printed before the JSON", () => {
    const out =
      '\u001b[32mUpdate available\u001b[0m\n{"userEmail":"a@b.c","subscriptionTier":"free"}';
    expect(parseCursorAbout(out)).toEqual({ user: "a@b.c", plan: "free" });
  });

  it("returns undefined when nobody is named", () => {
    expect(parseCursorAbout('{"subscriptionTier":"pro"}')).toBeUndefined();
    expect(parseCursorAbout("not json")).toBeUndefined();
  });
});

describe("parseCursorModels", () => {
  it("reads `id - Name` lines and strips ANSI and zero-width spaces", () => {
    const out =
      "claude-opus-5.5-high - Claude Opus 5.5 1M\n\u001b[1mgrok-4.7-low\u001b[0m - Grok 4.7\u200b (default)\n";
    expect(parseCursorModels(out).map((m) => m.id)).toEqual([
      "claude-opus-5.5-high",
      "grok-4.7-low",
    ]);
    expect(parseCursorModels(out)[0]?.name).toBe("Claude Opus 5.5 1M");
    expect(parseCursorModels(out)[1]?.name).toBe("Grok 4.7");
  });

  it("gives a 1M model a 1M context and everything else Cursor's default", () => {
    expect(cursorContext("claude-opus-5.5-high", "Claude Opus 5.5 1M")).toBe(1_000_000);
    expect(cursorContext("auto", "Auto")).toBe(200_000);
    expect(cursorContext("gpt-5.5-fast", "GPT-5.5 Fast")).toBe(200_000);
  });
});

describe("splitCursorId", () => {
  it("takes the effort and the fast / thinking marks out of an id", () => {
    expect(splitCursorId("grok-4.7-low-fast")).toEqual({ family: "grok-4.7-fast", effort: "low" });
    expect(splitCursorId("claude-opus-5-thinking-high")).toEqual({
      family: "claude-opus-5-thinking",
      effort: "high",
    });
    expect(splitCursorId("claude-4.6-opus-high-thinking")).toEqual({
      family: "claude-4.6-opus-thinking",
      effort: "high",
    });
    expect(splitCursorId("gpt-5.2")).toEqual({ family: "gpt-5.2", effort: "" });
  });
});

describe("collapseCursorModels", () => {
  it("collapses a family to one model named as its default, without the effort word", () => {
    const collapsed = collapseCursorModels([
      { id: "grok-4.7-low", name: "Grok 4.7 Low", context: 200_000 },
      { id: "grok-4.7-medium", name: "Grok 4.7 Medium", context: 200_000 },
      { id: "grok-4.7-high", name: "Grok 4.7 High", context: 200_000 },
    ]);
    expect(collapsed).toHaveLength(1);
    expect(collapsed[0]?.id).toBe("grok-4.7");
    expect(collapsed[0]?.name).toBe("Grok 4.7");
  });

  it("keeps a family of one exactly as Cursor lists it", () => {
    const collapsed = collapseCursorModels([{ id: "auto", name: "Auto", context: 200_000 }]);
    expect(collapsed).toEqual([{ id: "auto", name: "Auto", context: 200_000 }]);
  });
});

describe("classifyCursorError", () => {
  it("reads a Connect error's detail and maps it to a status", () => {
    const body = JSON.stringify({
      code: "resource_exhausted",
      message: "You have hit your usage limit",
      details: [{ debug: { details: { title: "Usage limit", detail: "try again in 2h" } } }],
    });
    const error = classifyCursorError(429, body);
    expect(error.kind).toBe("quota");
    expect(error.status).toBe(429);
    expect(error.message).toBe("Usage limit: try again in 2h");
  });

  it("tells a regional refusal from a sign-in problem", () => {
    expect(
      classifyCursorError(403, '{"message":"This region is not yet available for your team"}').kind,
    ).toBe("region");
    expect(
      classifyCursorError(401, '{"code":"unauthenticated","message":"token expired"}').kind,
    ).toBe("auth");
  });

  it("falls back to the status text for a body it cannot parse", () => {
    const error = classifyCursorError(503, "upstream is unavailable");
    expect(error.kind).toBe("capacity");
    expect(error.status).toBe(503);
  });
});

describe("encodeCursorFrame", () => {
  it("writes the flags byte and a big-endian length", () => {
    const frame = encodeCursorFrame(new Uint8Array([1, 2, 3]));
    expect(Array.from(frame)).toEqual([0, 0, 0, 0, 3, 1, 2, 3]);
  });
});
