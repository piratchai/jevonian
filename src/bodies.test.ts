import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";

import { flushBodies, isSafeBodyId, loadBody, saveBody } from "./bodies";

let dir = "";
let previousData: string | undefined;
let previousCapture: string | undefined;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-bodies-"));
  previousData = process.env.JEVONIAN_DATA_DIR;
  previousCapture = process.env.JEVONIAN_CAPTURE_BODIES;
  process.env.JEVONIAN_DATA_DIR = dir;
});

afterEach(async () => {
  // Let any queued write finish against the temp dir before it is removed, so the test does
  // not race the very behavior it is asserting.
  await flushBodies();
  if (previousData === undefined) delete process.env.JEVONIAN_DATA_DIR;
  else process.env.JEVONIAN_DATA_DIR = previousData;
  if (previousCapture === undefined) delete process.env.JEVONIAN_CAPTURE_BODIES;
  else process.env.JEVONIAN_CAPTURE_BODIES = previousCapture;
  rmSync(dir, { recursive: true, force: true });
});

describe("bodies", () => {
  it("round-trips a payload", async () => {
    const id = "11111111-2222-3333-4444-555555555555";
    saveBody(id, { kind: "request", body: { messages: [{ role: "user", content: "hi" }] } });
    // The write is async on the request path; flush is the deterministic point at which it
    // has landed.
    await flushBodies();
    expect(loadBody(id)).toEqual({
      kind: "request",
      body: { messages: [{ role: "user", content: "hi" }] },
    });
  });

  it("does not block the caller on the write", () => {
    const id = "12121212-2222-3333-4444-555555555555";
    saveBody(id, { kind: "request" });
    // A synchronous write would already be on disk here; the async contract is that the
    // caller is not made to wait for it.
    expect(loadBody(id)).toBeUndefined();
  });

  it("rejects unsafe ids", async () => {
    expect(isSafeBodyId("../../etc/passwd")).toBe(false);
    expect(isSafeBodyId("nope")).toBe(false);
    expect(loadBody("../../etc/passwd")).toBeUndefined();
    saveBody("../../etc/passwd", { evil: true });
    await flushBodies();
    expect(loadBody("../../etc/passwd")).toBeUndefined();
  });

  it("honors the capture toggle", async () => {
    process.env.JEVONIAN_CAPTURE_BODIES = "0";
    const id = "99999999-2222-3333-4444-555555555555";
    saveBody(id, { kind: "request" });
    await flushBodies();
    expect(loadBody(id)).toBeUndefined();
  });

  it("prunes the oldest captures once the directory exceeds its cap", async () => {
    // 1_005 unique ids trips one prune (every 100 writes) and leaves the directory capped.
    const ids = Array.from({ length: 1_005 }, (_, index) => {
      const hex = index.toString(16).padStart(12, "0");
      return `${hex}-aaaa-bbbb-cccc-dddddddddddd`;
    });
    for (const id of ids) saveBody(id, { kind: "request", index: id });
    await flushBodies();
    const files = readdirSync(join(dir, "bodies")).filter((name) => name.endsWith(".json"));
    expect(files.length).toBeLessThanOrEqual(1_000);
    // The newest capture always survives; a prune removes the oldest.
    expect(loadBody(ids[ids.length - 1]!)).toBeDefined();
  });
});
