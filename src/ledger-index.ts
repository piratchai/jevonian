/**
 * Incremental rollups over the ledger, so routing and the dashboard never walk the whole file.
 *
 * `readRecords` holds ~240k parsed rows on the live ledger (142 MB), and the old spend path
 * scanned every one of them — once per provider, a dozen times per turn — while the append path
 * invalidated every other session's cache by growing the array. This module keeps the answers
 * that path actually needs (per-provider and per-key money inside rolling windows) in structures
 * that an append can update in constant time.
 *
 * The store is rebuilt from the file when the process starts or when the ledger is replaced (a
 * rewrite, a rotation, a CLI append from another install). Steady state never re-reads the file:
 * `appendRecord` feeds this directly, so our own writes cost one bucket update.
 */

import type { LedgerRecord } from "./ledger";

/** The widest rolling window the store keeps in hourly buckets. */
export const BUCKET_WINDOW_MS = 30 * 86_400_000;
const BUCKET_MS = 3_600_000;
/** Hourly buckets retained: the 30-day window plus one boundary bucket. */
const BUCKET_COUNT = Math.ceil(BUCKET_WINDOW_MS / BUCKET_MS) + 1;

/** A rolling window, in milliseconds. */
export type SpendWindow = number;

/**
 * One hour of money, split the way the ledger splits it.
 *
 * `apiUsd` is what a Jevonian key's own limit is measured against, `subscriptionUsd` is
 * subscription billing, and `costUsd` is the sum the quota guard compares with a plan's cap.
 * All three live on the same bucket because every window query needs the same slice.
 */
export interface SpendBucket {
  /** Epoch ms of the bucket's first millisecond, truncated to the hour. */
  at: number;
  costUsd: number;
  apiUsd: number;
  subscriptionUsd: number;
  requests: number;
}

/** What a windowed query returns. */
export interface SpendWindowTotal {
  costUsd: number;
  apiUsd: number;
  subscriptionUsd: number;
  requests: number;
}

const emptyTotal = (): SpendWindowTotal => ({
  costUsd: 0,
  apiUsd: 0,
  subscriptionUsd: 0,
  requests: 0,
});

/** True when a record carries money the rollups should count. */
function isCosted(record: LedgerRecord): boolean {
  return record.kind !== "brain" && typeof record.provider === "string" && record.provider.length > 0;
}

/**
 * Buckets for one entity (a provider, or a key), addressed by hour.
 *
 * A fixed-size ring rather than a growing array: the store outlives every turn, so the oldest
 * hour is overwritten as the newest arrives. Lookups verify the stored hour matches the one
 * asked for, which is what makes a stale slot from a previous lap safe to reuse.
 */
class BucketRing {
  private readonly buckets: Array<SpendBucket | undefined> = new Array(BUCKET_COUNT);

  /** The bucket for `at`, creating it (and overwriting whatever stale hour it held). */
  at(at: number): SpendBucket {
    const hour = Math.floor(at / BUCKET_MS);
    const slot = ((hour % BUCKET_COUNT) + BUCKET_COUNT) % BUCKET_COUNT;
    const existing = this.buckets[slot];
    if (existing && existing.at === hour * BUCKET_MS) return existing;
    const created: SpendBucket = {
      at: hour * BUCKET_MS,
      costUsd: 0,
      apiUsd: 0,
      subscriptionUsd: 0,
      requests: 0,
    };
    this.buckets[slot] = created;
    return created;
  }

  /** Adds a record to its hour. */
  add(record: LedgerRecord, at: number): void {
    const bucket = this.at(at);
    const cost = record.costUsd ?? 0;
    bucket.costUsd += cost;
    if (record.billing === "subscription") bucket.subscriptionUsd += cost;
    else bucket.apiUsd += cost;
    bucket.requests += 1;
  }

  /**
   * Sums the buckets inside `[from, now]`.
   *
   * A ring may hold a bucket from a previous lap of the same slot, so the walk filters by
   * timestamp rather than trusting the slot's position: a stale bucket whose hour is outside
   * the window is skipped, which is also what makes the overwrite above safe.
   */
  total(from: number, now: number): SpendWindowTotal {
    const total = emptyTotal();
    for (const bucket of this.buckets) {
      // Include any hour that overlaps `[from, now]`. Filtering on `bucket.at < from` alone
      // drops the boundary hour whenever the window starts mid-hour, which under-counted a
      // record that was still inside the rolling window (the quota tests catch this).
      if (!bucket || bucket.at > now || bucket.at + BUCKET_MS <= from) continue;
      total.costUsd += bucket.costUsd;
      total.apiUsd += bucket.apiUsd;
      total.subscriptionUsd += bucket.subscriptionUsd;
      total.requests += bucket.requests;
    }
    return total;
  }

  /** Every bucket still held, oldest first. For callers that draw their own buckets. */
  list(): SpendBucket[] {
    return this.buckets
      .filter((bucket): bucket is SpendBucket => bucket !== undefined)
      .sort((a, b) => a.at - b.at);
  }

  clear(): void {
    this.buckets.fill(undefined);
  }
}

/** One entity's ring plus what fell out of the widest window. */
class EntityRollup {
  private readonly ring = new BucketRing();
  /** Money older than the ring's horizon, so full-history totals stay exact. */
  private older: SpendWindowTotal = emptyTotal();

  add(record: LedgerRecord, at: number, now: number): void {
    const cost = record.costUsd ?? 0;
    if (at < now - BUCKET_WINDOW_MS) {
      this.older.costUsd += cost;
      if (record.billing === "subscription") this.older.subscriptionUsd += cost;
      else this.older.apiUsd += cost;
      this.older.requests += 1;
      return;
    }
    this.ring.add(record, at);
  }

  window(now: number, windowMs: SpendWindow): SpendWindowTotal {
    const total = this.ring.total(now - windowMs, now);
    // Only the widest window includes the folded remainder: a 5h query that counted a 40-day-old
    // record would report spend that already rolled out.
    if (windowMs >= BUCKET_WINDOW_MS) {
      total.costUsd += this.older.costUsd;
      total.apiUsd += this.older.apiUsd;
      total.subscriptionUsd += this.older.subscriptionUsd;
      total.requests += this.older.requests;
    }
    return total;
  }

  allTime(): SpendWindowTotal {
    const total = this.ring.total(Number.NEGATIVE_INFINITY, Number.POSITIVE_INFINITY);
    total.costUsd += this.older.costUsd;
    total.apiUsd += this.older.apiUsd;
    total.subscriptionUsd += this.older.subscriptionUsd;
    total.requests += this.older.requests;
    return total;
  }

  buckets(): SpendBucket[] {
    return this.ring.list();
  }

  clear(): void {
    this.ring.clear();
    this.older = emptyTotal();
  }
}

/**
 * The live rollup. One process-wide store, fed by `ledger.ts` as records land.
 *
 * Every reader goes through {@link ledgerSpendIndex}. A query is cheap enough to run per
 * candidate per turn, which is what retires the old `spendOf` full scan.
 */
export class LedgerSpendIndex {
  private readonly providers = new Map<string, EntityRollup>();
  private readonly keys = new Map<string, EntityRollup>();
  private records = 0;
  private newestAt = 0;

  /** Files one record whose timestamp is near `now`. */
  add(record: LedgerRecord, now: number = Date.now()): void {
    this.file(record, now);
  }

  /**
   * Files a record read from the file during a rebuild.
   *
   * The rebuild passes the newest timestamp it has seen as `now`, so a historical row is
   * classified against the horizon without a `Date.now()` call per row — measurable on 240k.
   */
  addHistorical(record: LedgerRecord, now: number): void {
    this.file(record, now);
  }

  private file(record: LedgerRecord, now: number): void {
    if (!isCosted(record)) return;
    const at = Date.parse(record.ts);
    if (Number.isNaN(at)) return;
    this.records += 1;
    if (at > this.newestAt) this.newestAt = at;
    this.of(this.providers, record.provider).add(record, at, now);
    if (record.keyId) this.of(this.keys, record.keyId).add(record, at, now);
  }

  private of(map: Map<string, EntityRollup>, name: string): EntityRollup {
    let entry = map.get(name);
    if (!entry) {
      entry = new EntityRollup();
      map.set(name, entry);
    }
    return entry;
  }

  /** Money a provider spent inside `windowMs`, ending at `now`. */
  providerWindow(provider: string, windowMs: SpendWindow, now: number): SpendWindowTotal {
    const entry = this.providers.get(provider);
    return entry ? entry.window(now, windowMs) : emptyTotal();
  }

  /** Money a Jevonian key spent inside `windowMs`, ending at `now`. */
  keyWindow(keyId: string, windowMs: SpendWindow, now: number): SpendWindowTotal {
    const entry = this.keys.get(keyId);
    return entry ? entry.window(now, windowMs) : emptyTotal();
  }

  /** Lifetime totals for a key, split the way the keys list reports them. */
  keyAllTime(keyId: string): SpendWindowTotal {
    const entry = this.keys.get(keyId);
    return entry ? entry.allTime() : emptyTotal();
  }

  /** Lifetime totals for a provider. */
  providerAllTime(provider: string): SpendWindowTotal {
    const entry = this.providers.get(provider);
    return entry ? entry.allTime() : emptyTotal();
  }

  /** Hourly buckets for a provider inside the ring's horizon, oldest first. */
  providerBuckets(provider: string): SpendBucket[] {
    return this.providers.get(provider)?.buckets() ?? [];
  }

  /** Hourly buckets for a key inside the ring's horizon, oldest first. */
  keyBuckets(keyId: string): SpendBucket[] {
    return this.keys.get(keyId)?.buckets() ?? [];
  }

  /** How many costed records are reflected here. */
  get size(): number {
    return this.records;
  }

  /** The newest record timestamp seen, or 0 when nothing has been filed. */
  get lastAt(): number {
    return this.newestAt;
  }

  reset(): void {
    this.providers.clear();
    this.keys.clear();
    this.records = 0;
    this.newestAt = 0;
  }
}

/** The process-wide rollup every reader shares. */
export const ledgerSpendIndex = new LedgerSpendIndex();
