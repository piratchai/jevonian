import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
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
            attempt.fail ? "bg-destructive" : "bg-emerald-500"
          }`}
        />
      ))}
    </span>
  );
}

function AttemptLabel({ attempt, index }: { attempt: LogAttempt; index: number }) {
  return (
    <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
      <span className="font-mono text-muted-foreground">{index + 1}.</span>
      <span className="font-medium">{attempt.provider}</span>
      <span className="min-w-0 break-all text-muted-foreground">{attempt.model}</span>
      <Badge
        variant={attempt.cause === "failover" ? "default" : "outline"}
        className="px-1 py-0 text-[10px]"
      >
        {attempt.cause}
      </Badge>
      <span className="ml-auto shrink-0 font-mono text-muted-foreground">
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
      <div className="flex justify-between text-[10px] text-muted-foreground">
        <span>0</span>
        <span>relative duration</span>
      </div>
    );
  }
  return (
    <div className="flex justify-between text-[10px] text-muted-foreground">
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
      <Card className="min-w-0 overflow-hidden">
        <CardHeader>
          <CardTitle>Attempts</CardTitle>
          <CardDescription>
            {record.status >= 400
              ? "No attempt history was captured for this turn."
              : "This turn was served on its first attempt — nothing was retried or failed over."}
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }
  const { onAxis, total, bars } = timelineLayout(tries, record.latencyMs);
  const ttftPct =
    onAxis && record.ttftMs !== undefined
      ? Math.min(100, (record.ttftMs / total) * 100)
      : undefined;
  return (
    <Card className="min-w-0 overflow-hidden">
      <CardHeader>
        <CardTitle className="flex min-w-0 flex-wrap items-center gap-2">
          Attempts
          <AttemptDots tries={tries} />
        </CardTitle>
        <CardDescription>
          {tries.length} attempt{tries.length === 1 ? "" : "s"}
          {record.failovers
            ? ` · ${record.failovers} failover${record.failovers === 1 ? "" : "s"}`
            : ""}
          {record.retries ? ` · ${record.retries} retr${record.retries === 1 ? "y" : "ies"}` : ""}
          {record.ttftMs !== undefined ? ` · first token ${record.ttftMs}ms` : ""}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex min-w-0 flex-col gap-3">
        <div className="flex min-w-0 flex-col gap-3">
          {tries.map((attempt, index) => {
            const bar = bars[index] ?? { leftPct: 0, widthPct: 2 };
            const failed = Boolean(attempt.fail);
            return (
              <div key={`${attempt.provider}-${attempt.model}-${index}`} className="min-w-0">
                <AttemptLabel attempt={attempt} index={index} />
                <div className="mt-1 flex min-w-0 items-center gap-2">
                  <div className="relative h-2 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
                    <div
                      className={cn(
                        "absolute inset-y-0 rounded-full",
                        failed ? "bg-destructive" : "bg-emerald-500",
                      )}
                      style={{ left: `${bar.leftPct}%`, width: `${bar.widthPct}%` }}
                    />
                  </div>
                  <span
                    className={cn(
                      "shrink-0 text-[11px]",
                      failed ? "text-destructive" : "text-muted-foreground",
                    )}
                  >
                    {attempt.fail ? attemptFailLabel(attempt.fail) : "served this turn"}
                  </span>
                </div>
              </div>
            );
          })}
        </div>
        <div className="relative">
          <Axis total={total} onAxis={onAxis} />
          {ttftPct !== undefined ? (
            <span
              className="pointer-events-none absolute -top-3 h-3 w-px bg-foreground/50"
              style={{ left: `${ttftPct}%` }}
              title={`first token at ${record.ttftMs}ms`}
              aria-hidden
            />
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}
