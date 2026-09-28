import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";

import { parseConfig } from "./config";
import { DEFAULT_KEV_PORT, kevBaseUrl, parseKevPort, stopKev, withKevBrain } from "./kev";

let dir = "";
let previousData: string | undefined;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-kev-"));
  previousData = process.env.JEVONIAN_DATA_DIR;
  process.env.JEVONIAN_DATA_DIR = dir;
});

afterEach(() => {
  if (previousData === undefined) delete process.env.JEVONIAN_DATA_DIR;
  else process.env.JEVONIAN_DATA_DIR = previousData;
  rmSync(dir, { recursive: true, force: true });
});

describe("parseKevPort", () => {
  it("defaults when the flag is absent or empty", () => {
    expect(parseKevPort(undefined)).toBe(DEFAULT_KEV_PORT);
    expect(parseKevPort("")).toBe(DEFAULT_KEV_PORT);
  });

  it("accepts a valid port and rejects everything else", () => {
    expect(parseKevPort("9000")).toBe(9000);
    expect(parseKevPort("0")).toBeUndefined();
    expect(parseKevPort("70000")).toBeUndefined();
    expect(parseKevPort("80abc")).toBeUndefined();
    expect(parseKevPort("-1")).toBeUndefined();
  });
});

describe("withKevBrain", () => {
  const hosted = { channel: "typesafe", timeoutMs: 15_000, minConfidence: 0.6 };

  it("adds a primary Kev brain with the channel's confidence default", () => {
    const config = parseConfig({ routing: { brains: [hosted] } });
    const next = withKevBrain(config, 9000, true);
    expect(next.routing.brains.map((brain) => brain.channel)).toEqual(["kev", "typesafe"]);
    expect(next.routing.brains[0]).toMatchObject({
      channel: "kev",
      baseUrl: "http://127.0.0.1:9000/v1/systemone",
      model: "kev-latest",
      minConfidence: 0.4,
    });
  });

  it("re-points an existing Kev brain instead of adding a second one", () => {
    const config = parseConfig({
      routing: {
        brains: [
          hosted,
          { channel: "kev", baseUrl: kevBaseUrl(), timeoutMs: 3_000, minConfidence: 0.5 },
        ],
      },
    });
    const kept = withKevBrain(config, 9001, false);
    expect(kept.routing.brains.map((brain) => brain.channel)).toEqual(["typesafe", "kev"]);
    // The user's own tuning survives a re-point.
    expect(kept.routing.brains[1]).toMatchObject({
      baseUrl: "http://127.0.0.1:9001/v1/systemone",
      timeoutMs: 3_000,
      minConfidence: 0.5,
    });
    const promoted = withKevBrain(config, 9001, true);
    expect(promoted.routing.brains.map((brain) => brain.channel)).toEqual(["kev", "typesafe"]);
  });
});

describe("stopKev", () => {
  it("does not signal a pid that is not a Kev server", () => {
    // The test runner's own pid is alive but its command line is not kev.serve; if the
    // identity check failed, this test would SIGTERM itself.
    const pidFile = join(dir, "kev.pid");
    writeFileSync(pidFile, String(process.pid));
    stopKev();
    expect(existsSync(pidFile)).toBe(false);
  });
});
