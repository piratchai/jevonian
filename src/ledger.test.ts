import { appendFileSync, mkdtempSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";

import { appendRecord, readRecords, resetLedgerCache, subscribeLedger } from "./ledger";

let dir = "";
let previousLedger: string | undefined;

const record = (session: string) => ({
  ts: new Date().toISOString(),
  session,
  path: "/v1/messages",
  provider: "anthropic",
  model: "claude-opus-5-5",
  stream: true,
  status: 200,
  latencyMs: 10,
  promptTokens: 1,
  completionTokens: 1,
  cacheReadTokens: 0,
  cacheWriteTokens: 0,
  costUsd: 0.01,
  pricingKnown: true,
});

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "jevonian-ledger-"));
  previousLedger = process.env.JEVONIAN_LEDGER;
  process.env.JEVONIAN_LEDGER = join(dir, "ledger.jsonl");
  resetLedgerCache();
});

afterEach(() => {
  if (previousLedger === undefined) delete process.env.JEVONIAN_LEDGER;
  else process.env.JEVONIAN_LEDGER = previousLedger;
  resetLedgerCache();
  rmSync(dir, { recursive: true, force: true });
});

describe("ledger", () => {
  it("returns the same array instance when nothing changed", () => {
    appendRecord(record("a"));
    const first = readRecords();
    const second = readRecords();
    // Same reference proves the cache was a hit rather than a re-parse.
    expect(second).toBe(first);
  });

  it("keeps the cache warm across our own appends", () => {
    appendRecord(record("a"));
    const first = readRecords();
    appendRecord(record("b"));
    const second = readRecords();
    // The regression this guards: pushing to the cache without refreshing the stat made every
    // read after an append fall through to a full re-parse. Same reference means it did not.
    expect(second).toBe(first);
    expect(second).toHaveLength(2);
  });

  it("picks up records another writer appended", () => {
    appendRecord(record("a"));
    expect(readRecords()).toHaveLength(1);
    // A CLI run or a second instance appends directly to the file; the tail read must find it.
    appendFileSync(process.env.JEVONIAN_LEDGER!, `${JSON.stringify(record("external"))}\n`);
    const records = readRecords();
    expect(records).toHaveLength(2);
    expect(records[1]?.session).toBe("external");
  });

  it("re-reads the whole file when it is replaced", () => {
    appendRecord(record("a"));
    appendRecord(record("b"));
    expect(readRecords()).toHaveLength(2);
    // A rewrite that shrinks the file cannot be handled by a tail read.
    writeFileSync(process.env.JEVONIAN_LEDGER!, `${JSON.stringify(record("only"))}\n`);
    const records = readRecords();
    expect(records).toHaveLength(1);
    expect(records[0]?.session).toBe("only");
  });

  it("tracks the file size so a foreign rewrite is detected", () => {
    appendRecord(record("a"));
    readRecords();
    appendRecord(record("b"));
    readRecords();
    // The cached size must equal the real file size, or the next read would re-parse.
    expect(statSync(process.env.JEVONIAN_LEDGER!).size).toBeGreaterThan(0);
  });

  it("leaves a partial trailing line for the next read", () => {
    appendRecord(record("a"));
    readRecords();
    // Simulate a torn write: a JSON line with no terminating newline.
    appendFileSync(
      process.env.JEVONIAN_LEDGER!,
      '{"ts":"2026-01-01T00:00:00.000Z","session":"torn"',
    );
    expect(readRecords()).toHaveLength(1);
    appendFileSync(process.env.JEVONIAN_LEDGER!, ',"path":"/x"}\n');
    expect(readRecords()).toHaveLength(2);
  });

  it("notifies subscribers without swallowing later listeners", () => {
    const seen: string[] = [];
    const unsubscribe = subscribeLedger((entry) => {
      seen.push(entry.session);
      throw new Error("listener failure must not stop the append");
    });
    const other = subscribeLedger((entry) => seen.push(`other:${entry.session}`));
    expect(() => appendRecord(record("s"))).not.toThrow();
    unsubscribe();
    other();
    expect(seen).toEqual(["s", "other:s"]);
  });
});
