import { LayerCard, SkeletonLine, Text } from "@cloudflare/kumo";
import { useEffect, useState } from "react";
import { Link } from "react-router";

import { api, type LogRecord } from "@/lib/api";
import { cn, formatTime } from "@/lib/utils";

const SESSION_LIMIT = 50;

/**
 * The other turns in this session, newest first. Loads on its own so a slow or failing
 * session query never blocks the rest of the page.
 */
export function SessionStrip({ session, currentId }: { session: string; currentId?: string }) {
  const [logs, setLogs] = useState<LogRecord[] | null>(null);
  const [error, setError] = useState("");
  const [total, setTotal] = useState<number | null>(null);

  useEffect(() => {
    if (!session) return;
    let cancelled = false;
    setLogs(null);
    setError("");
    void api
      .logs({ session, limit: SESSION_LIMIT })
      .then((page) => {
        if (cancelled) return;
        setLogs(page.logs);
        setTotal(page.total);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setError(String(cause));
      });
    return () => {
      cancelled = true;
    };
  }, [session]);

  return (
    <LayerCard className="min-w-0">
      <LayerCard.Secondary>
        <Text variant="heading">Session</Text>
        {total !== null ? (
          <span className="ml-auto text-xs text-kumo-subtle">
            {total} turn{total === 1 ? "" : "s"}
          </span>
        ) : null}
      </LayerCard.Secondary>
      <LayerCard.Primary>
        {!session ? (
          <p className="text-xs text-kumo-subtle">No session id was recorded for this turn.</p>
        ) : error ? (
          <p className="text-xs text-kumo-subtle">Session turns are unavailable right now.</p>
        ) : logs === null ? (
          <div className="flex flex-col gap-1.5">
            {Array.from({ length: 4 }, (_, index) => (
              <SkeletonLine key={index} blockHeight={32} />
            ))}
          </div>
        ) : logs.length === 0 ? (
          <p className="text-xs text-kumo-subtle">No other turns in this session.</p>
        ) : (
          <ul className="flex min-w-0 flex-col divide-y divide-kumo-hairline">
            {logs.map((log) => {
              const current = Boolean(currentId) && log.id === currentId;
              const failed = log.status >= 400;
              const row = (
                <span className="flex min-w-0 items-center gap-2 py-1.5 text-xs">
                  <span
                    className={cn(
                      "size-1.5 shrink-0 rounded-full",
                      failed ? "bg-kumo-danger" : "bg-kumo-success",
                    )}
                    aria-hidden
                  />
                  <span className="shrink-0 font-mono text-kumo-subtle">{formatTime(log.ts)}</span>
                  <span className="min-w-0 flex-1 truncate">{log.model}</span>
                  <span className="shrink-0 font-mono text-kumo-subtle">{log.latencyMs}ms</span>
                </span>
              );
              return (
                <li key={log.id ?? `${log.ts}-${log.model}`} className="min-w-0">
                  {log.id ? (
                    <Link
                      to={`/logs/${log.id}`}
                      className={cn(
                        "block min-w-0 rounded-sm px-1 transition-colors hover:bg-kumo-tint/60",
                        current ? "bg-kumo-tint/60 font-medium" : "",
                      )}
                      title={current ? "This turn" : "Open this turn"}
                    >
                      {row}
                    </Link>
                  ) : (
                    <span className="block min-w-0 px-1">{row}</span>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </LayerCard.Primary>
    </LayerCard>
  );
}
