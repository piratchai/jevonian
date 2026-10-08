import { Badge } from "@cloudflare/kumo";
import { ProviderIdentity } from "@/components/provider-identity";
import type { LogRecord } from "@/lib/api";
import { cn, money } from "@/lib/utils";

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <span className="text-[10px] tracking-[0.12em] text-kumo-subtle uppercase">{label}</span>
      <span className="min-w-0 font-mono text-xs break-words">{value}</span>
    </div>
  );
}

/**
 * One-glance answer to "did this turn work?". Green when the turn succeeded, red when it
 * failed, with the provider, model, latency, cost, and streaming mode that produced it.
 *
 * `layout` controls the fact grid. The panel is narrow even on wide screens, so its
 * viewport breakpoints would wrongly spread the facts into four columns; `"panel"` keeps
 * a fixed two-column grid instead.
 */
export function OutcomeBanner({
  record,
  layout = "page",
}: {
  record: LogRecord;
  layout?: "page" | "panel";
}) {
  const failed = record.status >= 400 || Boolean(record.error);
  return (
    <div
      className={cn(
        "flex min-w-0 flex-col gap-3 rounded-xl border p-4",
        failed
          ? "border-kumo-danger/40 bg-kumo-danger-tint"
          : "border-kumo-success/30 bg-kumo-success-tint",
      )}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2">
        <span
          className={cn(
            "size-2.5 shrink-0 rounded-full",
            failed ? "bg-kumo-danger" : "bg-kumo-success",
          )}
          aria-hidden
        />
        <span className={cn("text-sm font-semibold", failed ? "text-kumo-danger" : "")}>
          {failed ? "Failed" : "Succeeded"}
        </span>
        <Badge variant="outline" className="font-mono text-[10px]">
          {record.status}
        </Badge>
        <span className="flex min-w-0 items-center gap-1.5">
          <ProviderIdentity provider={record.provider} size="size-4" />
        </span>
        <span className="min-w-0 text-sm break-all text-kumo-subtle">· {record.model}</span>
        {record.requestedModel && record.requestedModel !== record.model ? (
          <Badge variant="outline" className="text-[10px]">
            requested {record.requestedModel}
          </Badge>
        ) : null}
        <Badge variant="outline" className="ml-auto text-[10px]">
          {record.stream ? "streamed" : "buffered"}
        </Badge>
      </div>

      <div className={cn("grid grid-cols-2 gap-3", layout === "panel" ? "" : "sm:grid-cols-4")}>
        <Fact label="latency" value={`${record.latencyMs}ms`} />
        <Fact
          label="first token"
          value={record.ttftMs !== undefined ? `${record.ttftMs}ms` : "—"}
        />
        <Fact label="cost" value={record.costUsd === null ? "—" : money(record.costUsd)} />
        <Fact label="tokens" value={`${record.promptTokens} in / ${record.completionTokens} out`} />
      </div>

      {record.error ? (
        <p className="rounded-md border border-kumo-danger/30 bg-kumo-danger-tint p-3 font-mono text-xs break-words text-kumo-danger">
          {record.error}
        </p>
      ) : null}
    </div>
  );
}
