import {
  appendFileSync,
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  readSync,
  statSync,
} from "node:fs";
import { dirname } from "node:path";

import { ledgerPath } from "./paths";

export interface LedgerRecord {
  id?: string;
  requestId?: string;
  ts: string;
  session: string;
  path: string;
  provider: string;
  model: string;
  stream: boolean;
  status: number;
  latencyMs: number;
  promptTokens: number;
  completionTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  costUsd: number | null;
  pricingKnown: boolean;
  kind?: "request" | "brain";
  billing?: string;
  requestedModel?: string;
  phase?: string;
  routed?: boolean;
  reason?: string;
  brain?: string;
  confidence?: number;
  canonical?: string;
  /** The thinking level actually sent upstream, read back from the outgoing body. */
  effort?: string;
  /** Why that level differs from what the router chose, when it does. */
  effortNote?: string;
  /** Models code withheld from the brain's choice, each with its reason. */
  skipped?: Array<{ model: string; provider: string; reason: string; detail: string }>;
  /** Probabilistic routing estimate, not the measured hit rate for this response. */
  cache?: import("./routing").CacheAffinity;
  /** Why the conversation stayed where it was answered, or moved (cache affinity). */
  cacheKeep?: import("./routing").CacheKeepReason;
  switchPenaltyUsd?: number | null;
  /**
   * Estimated input tokens the tool-result saver removed before egress (`tokenSaver`
   * config, backed by `rtk`). Absent when the saver was off or nothing qualified.
   */
  savedTokens?: number;
  /**
   * Transient upstream failures that were retried before this record was written. Absent when
   * the first attempt succeeded, so a clean turn carries no field at all.
   */
  retries?: number;
  /**
   * Every upstream attempt for this turn, in order. Omitted when the first try succeeded, so a
   * clean turn stays as small as today. A row written before tracing existed has no field at
   * all, which reads as "not recorded" rather than "0 attempts".
   */
  tries?: Array<{
    provider: string;
    model: string;
    cause: "initial" | "retry" | "failover";
    /** Epoch ms the attempt began, so the detail page can draw a waterfall. */
    startedAt?: number;
    status?: number;
    ms?: number;
    /** Milliseconds from the turn starting to this attempt's first streamed content. */
    ttftMs?: number;
    fail?: string;
  }>;
  /** Quota or refusal failovers this turn took before it was served. */
  failovers?: number;
  /** Milliseconds from the request arriving to the first streamed content. */
  ttftMs?: number;
  error?: string;
  /** Jevonian key ID that authorized the request, or "local" / "unauthenticated". */
  keyId?: string;
  /** Snapshot of the key name when the request was made (for display even if revoked). */
  keyName?: string;
}

/**
 * The parsed ledger, kept in sync with our own appends.
 *
 * A running server appends a record per turn, so this cache would otherwise be invalidated
 * by our own writes: the very next read — a dashboard poll, `/stats`, the routing brain's
 * quota check — re-read and re-parsed the whole file. On the live ledger (130 MB / 215k
 * lines) that is ~200 ms of blocked event loop, and with several sessions in flight each
 * turn invalidated the cache for every other session's next read, so the cost compounded.
 *
 * Two things keep it cheap. `appendRecord` advances the cache in place, mtime and size
 * included, so a read after our own write is a hit. And when the file *has* moved for a
 * reason we did not make (a CLI append, a rotated file), `readRecords` parses only the new
 * tail and splices it onto the cached prefix instead of re-reading everything.
 *
 * A foreign rewrite that shortens the file, or one that changes bytes before the cached
 * prefix ends, cannot be detected by size alone — the tail would then be nonsense. That is
 * what `resetLedgerCache` is for, and why the tail parse only runs when the file grew.
 */
let cachedRecords: { path: string; mtimeMs: number; size: number; records: LedgerRecord[] } | null =
  null;

type LedgerListener = (record: LedgerRecord) => void;
const ledgerListeners = new Set<LedgerListener>();

export function subscribeLedger(listener: LedgerListener): () => void {
  ledgerListeners.add(listener);
  return () => {
    ledgerListeners.delete(listener);
  };
}

export function resetLedgerCache(): void {
  cachedRecords = null;
}

export function appendRecord(record: LedgerRecord): void {
  const path = ledgerPath();
  mkdirSync(dirname(path), { recursive: true });
  const line = `${JSON.stringify(record)}\n`;
  appendFileSync(path, line);
  // Keep the cache current, mtime included, so the next `readRecords` is a hit. Pushing the
  // record without refreshing the stat was the bug: the size check below then mismatched on
  // every read and fell through to a full re-parse of the file.
  if (cachedRecords && cachedRecords.path === path) {
    cachedRecords.records.push(record);
    try {
      const st = statSync(path);
      cachedRecords.mtimeMs = st.mtimeMs;
      cachedRecords.size = st.size;
    } catch {
      // If the stat fails, drop the cache so the next read re-reads rather than trusts a
      // stale size. A stat failing right after a successful append is not expected.
      cachedRecords = null;
    }
  }
  for (const listener of ledgerListeners) {
    try {
      listener(record);
    } catch {
      // Ignore listener error
    }
  }
}

/** Parses the non-empty lines of a ledger chunk, skipping anything malformed. */
function parseRecords(text: string, into: LedgerRecord[]): void {
  for (const line of text.split("\n")) {
    if (!line) continue;
    try {
      into.push(JSON.parse(line) as LedgerRecord);
    } catch {
      // ignore malformed line
    }
  }
}

/**
 * Reads bytes `[from, to)` of the ledger without loading the whole file.
 *
 * Returns the decoded chunk and the offset up to the last complete line, so a partially
 * written trailing line is left for the next read rather than parsed and dropped.
 */
function readTail(path: string, from: number, to: number): { text: string; consumed: number } {
  if (to <= from) return { text: "", consumed: from };
  const length = to - from;
  const buffer = Buffer.allocUnsafe(length);
  const fd = openSync(path, "r");
  try {
    let read = 0;
    while (read < length) {
      const n = readSync(fd, buffer, read, length - read, from + read);
      if (n <= 0) break;
      read += n;
    }
    const text = buffer.subarray(0, read).toString("utf8");
    const lastNewline = text.lastIndexOf("\n");
    if (lastNewline < 0) return { text: "", consumed: from };
    const complete = text.slice(0, lastNewline + 1);
    return { text: complete, consumed: from + Buffer.byteLength(complete) };
  } finally {
    closeSync(fd);
  }
}

export function readRecords(): LedgerRecord[] {
  const path = ledgerPath();
  if (!existsSync(path)) {
    cachedRecords = null;
    return [];
  }
  try {
    const st = statSync(path);
    if (cachedRecords && cachedRecords.path === path) {
      if (cachedRecords.mtimeMs === st.mtimeMs && cachedRecords.size === st.size) {
        return cachedRecords.records;
      }
      // The file grew past what we have parsed: parse only the appended tail. This is the
      // foreign-append path (a CLI write, another instance); our own appends already advanced
      // the cache above, so a busy server almost never reaches here.
      if (st.size > cachedRecords.size) {
        const tail = readTail(path, cachedRecords.size, st.size);
        if (tail.text.length > 0) {
          parseRecords(tail.text, cachedRecords.records);
          // `consumed` stops at the last newline: a torn trailing write is left for the next
          // read, so the cache size never lands mid-line.
          cachedRecords = {
            path,
            mtimeMs: st.mtimeMs,
            size: tail.consumed,
            records: cachedRecords.records,
          };
          return cachedRecords.records;
        }
        // A trailing line with no newline yet (a write in progress). Keep the parsed prefix
        // and the offset at the last complete line; the next read resumes from there rather
        // than re-parsing the whole file.
        return cachedRecords.records;
      }
    }
    // Cold read, or the file shrank / was rewritten: parse the whole thing.
    const records: LedgerRecord[] = [];
    parseRecords(readFileSync(path, "utf8"), records);
    cachedRecords = { path, mtimeMs: st.mtimeMs, size: st.size, records };
    return records;
  } catch {
    return [];
  }
}
