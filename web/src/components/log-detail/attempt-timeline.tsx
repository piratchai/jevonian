import { Badge, LayerCard, Text } from "@cloudflare/kumo";
import type { LogAttempt, LogRecord } from "@/lib/api";
import { cn } from "@/lib/utils";

import { attemptFailLabel, timelineLayout } from "./helpers";

/** A single dot per attempt: red for a failure, green for the one that served the turn. */
function AttemptDots({ tries }: { tries: LogAttempt[] }) {
  return (
    <span className="inline-flex items-center gap-1">
      {tries.map((attempt, index) => (
        <span
          key={`${attempt.provider}-${index}`}
          title={`${attempt.provider}/${attempt.model} · ${attempt.cause}${
            attempt.fail ? ` · ${attemptFailLabel(attempt.fail)}` : ""
          }${attempt.status !== undefined ? ` · ${attempt.status}` : ""}`}
          className={`size-1.5 shrink-0 rounded-full ${
            attempt.fail ? "bg-kumo-danger" : "bg-kumo-success"
          }`}
        />
      ))}
    </span>
  );
}

function AttemptLabel({ attempt, index }: { attempt: LogAttempt; index: number }) {
  return (
    <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
      <span className="font-mono text-kumo-subtle">{index + 1}.</span>
      <span className="font-medium">{attempt.provider}</span>
      <span className="min-w-0 break-all text-kumo-subtle">{attempt.model}</span>
      <Badge
        variant={attempt.cause === "failover" ? "primary" : "outline"}
        className="px-1 py-0 text-xs"
      >
        {attempt.cause}
      </Badge>
      <span className="ml-auto shrink-0 font-mono text-kumo-subtle">
        {attempt.status !== undefined ? `${attempt.status} · ` : ""}
        {attempt.ms !== undefined ? `${attempt.ms}ms` : "in flight"}
        {attempt.ttftMs !== undefined ? ` · first byte ${attempt.ttftMs}ms` : ""}
      </span>
    </div>
  );
}

function Axis({ total, onAxis }: { total: number; onAxis: boolean }) {
  if (!onAxis) {
    return (
      <div className="flex justify-between text-xs text-kumo-subtle">
        <span>0</span>
        <span>relative duration</span>
      </div>
    );
  }
  return (
    <div className="flex justify-between text-xs text-kumo-subtle">
      <span>0</span>
      <span>{Math.round(total / 2)}ms</span>
      <span>{total}ms</span>
    </div>
  );
}

/**
 * The attempts this turn made, on a shared millisecond axis when `startedAt` is present, so
 * a slow first try and a quick retry read differently at a glance. Falls back to
 * proportional-width bars for older records.
 */
export function AttemptTimeline({ record }: { record: LogRecord }) {
  const tries = record.tries ?? [];
  if (tries.length === 0) {
    return (
      <LayerCard className="min-w-0">
        <LayerCard.Secondary className="flex-col items-start gap-1">
          <Text variant="heading">Attempts</Text>
          <Text variant="secondary" size="sm">
            {record.status >= 400
              ? "No attempt history was captured for this turn."
              : "This turn was served on its first attempt — nothing was retried or failed over."}
          </Text>
        </LayerCard.Secondary>
      </LayerCard>
    );
  }
  const { onAxis, total, bars } = timelineLayout(tries, record.latencyMs);
  const ttftPct =
    onAxis && record.ttftMs !== undefined
      ? Math.min(100, (record.ttftMs / total) * 100)
      : undefined;
  return (
    <LayerCard className="min-w-0">
      <LayerCard.Secondary>
        <Text variant="heading">Attempts</Text>
        <AttemptDots tries={tries} />
      </LayerCard.Secondary>
      <LayerCard.Primary className="min-w-0 gap-3">
        <Text variant="secondary" size="sm">
          {tries.length} attempt{tries.length === 1 ? "" : "s"}
          {record.failovers
            ? ` · ${record.failovers} failover${record.failovers === 1 ? "" : "s"}`
            : ""}
          {record.retries ? ` · ${record.retries} retr${record.retries === 1 ? "y" : "ies"}` : ""}
          {record.ttftMs !== undefined ? ` · first token ${record.ttftMs}ms` : ""}
        </Text>
        {tries.map((attempt, index) => {
          const bar = bars[index] ?? { leftPct: 0, widthPct: 2 };
          const failed = Boolean(attempt.fail);
          return (
            <div key={`${attempt.provider}-${attempt.model}-${index}`} className="min-w-0">
              <AttemptLabel attempt={attempt} index={index} />
              <div className="mt-1 flex min-w-0 items-center gap-2">
                <div className="relative h-2 min-w-0 flex-1 overflow-hidden rounded-full bg-kumo-fill">
                  <div
                    className={cn(
                      "absolute inset-y-0 rounded-full",
                      failed ? "bg-kumo-danger" : "bg-kumo-success",
                    )}
                    style={{ left: `${bar.leftPct}%`, width: `${bar.widthPct}%` }}
                  />
                </div>
                <span
                  className={cn(
                    "shrink-0 text-xs",
                    failed ? "text-kumo-danger" : "text-kumo-subtle",
                  )}
                >
                  {attempt.fail ? attemptFailLabel(attempt.fail) : "served this turn"}
                </span>
              </div>
            </div>
          );
        })}
        <div className="relative">
          <Axis total={total} onAxis={onAxis} />
          {ttftPct !== undefined ? (
            <span
              className="pointer-events-none absolute -top-3 h-3 w-px bg-kumo-contrast/50"
              style={{ left: `${ttftPct}%` }}
              title={`first token at ${record.ttftMs}ms`}
              aria-hidden
            />
          ) : null}
        </div>
      </LayerCard.Primary>
    </LayerCard>
  );
}
