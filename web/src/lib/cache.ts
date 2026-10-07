import type { LogRecord } from "@/lib/api";

/**
 * How much of this turn's input the provider served from its prompt cache:
 * cached reads over cached reads plus uncached input.
 *
 * The ledger stores `promptTokens` in whichever convention the serving wire
 * used. OpenAI and Responses count cache reads *inside* `promptTokens`, so the
 * uncached part is `promptTokens - cacheReadTokens`. Anthropic and the
 * Connect-RPC hosts count cache reads *outside* `promptTokens`, so `promptTokens`
 * is already the uncached part. The recorded `exclusiveInput` flag — written at
 * request time — tells the two apart; rows written before it existed keep the
 * historical `promptTokens`-as-uncached reading so totals never go negative.
 *
 * A record with no input accounting returns `null`, so the row prints "—"
 * instead of a fake "0%".
 */
export function cacheCoverage(record: LogRecord): number | null {
  const cached = record.cacheReadTokens ?? 0;
  const uncached = uncachedInputTokens(record);
  if (uncached === null) return null;
  const total = cached + uncached;
  if (total <= 0) return null;
  return cached / total;
}

/**
 * The uncached half of a record's input, honoring its usage convention. Returns
 * `null` only when the row carries no input accounting at all.
 */
function uncachedInputTokens(record: LogRecord): number | null {
  const cached = record.cacheReadTokens ?? 0;
  const prompt = record.promptTokens ?? 0;
  if (cached <= 0 && prompt <= 0) return null;
  if (record.exclusiveInput === false) return Math.max(prompt - cached, 0);
  return prompt;
}

/** Compact label for a coverage ratio: 0.625 → "63%". */
export function formatCacheCoverage(coverage: number): string {
  return `${Math.round(coverage * 100)}%`;
}

/** Tone for a coverage ratio, so a scan of the column reads at a glance. */
export function cacheCoverageTone(coverage: number): string {
  if (coverage >= 0.5) return "text-emerald-600 dark:text-emerald-400";
  if (coverage >= 0.1) return "text-amber-600 dark:text-amber-400";
  return "text-muted-foreground";
}

/** The hover explanation for one row's cache cell. */
export function cacheCoverageTitle(record: LogRecord): string {
  const cached = record.cacheReadTokens ?? 0;
  const uncached = uncachedInputTokens(record);
  if (uncached === null || cached + uncached <= 0) {
    return "No token accounting was captured for this turn.";
  }
  return `${cached.toLocaleString()} cached of ${(cached + uncached).toLocaleString()} input tokens`;
}
