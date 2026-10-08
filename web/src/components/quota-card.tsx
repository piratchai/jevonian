import { Badge, Button, LayerCard, Text } from "@cloudflare/kumo";
import { useEffect, useState } from "react";

import { ProviderIdentity } from "@/components/provider-identity";
import type {
  ModelQuotaHealthView,
  ProviderQuotaView,
  ProviderView,
  QuotaHealthView,
  QuotaWindow,
} from "@/lib/api";
import { cn, formatTime, money } from "@/lib/utils";

function usedLabel(usage: QuotaWindow): string {
  if (usage.usedPercent !== undefined) return `${usage.usedPercent.toFixed(1)}% used`;
  if (usage.usedUsd !== undefined) return `${money(usage.usedUsd)} used`;
  return "—";
}

function remainingLabel(usage: QuotaWindow): string | undefined {
  if (usage.usedPercent !== undefined) {
    return `${Math.max(0, 100 - usage.usedPercent).toFixed(1)}% left`;
  }
  if (usage.usedUsd !== undefined && usage.limitUsd !== undefined) {
    return `${money(Math.max(0, usage.limitUsd - usage.usedUsd))} left`;
  }
  return undefined;
}

function resetLabel(iso: string | undefined, now: number): string | undefined {
  if (!iso) return undefined;
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return undefined;
  const diff = at.getTime() - now;
  if (diff <= 0) return "reset time passed · refresh status";
  const seconds = Math.ceil(diff / 1_000);
  if (seconds < 60) return `resets in ${seconds}s`;
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 60) return `resets in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours >= 24) return `resets in ${Math.floor(hours / 24)}d ${hours % 24}h`;
  return `resets in ${hours}h ${minutes % 60}m`;
}

function useResetClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}

export function balanceLabel(balance: { amount: number; currency: string }): string {
  const symbol =
    balance.currency === "USD" ? "$" : balance.currency === "CNY" ? "¥" : `${balance.currency} `;
  return `${symbol}${balance.amount.toFixed(2)}`;
}

const SOURCE_LABEL: Record<ProviderQuotaView["source"], string> = {
  live: "live",
  headers: "from responses",
  ledger: "local ledger",
  none: "unavailable",
};

export function QuotaWindowRow({ window }: { window: QuotaWindow }) {
  const now = useResetClock();
  const used = window.usedPercent ?? 0;
  const width = Math.min(100, Math.max(0, used));
  const tone = used >= 90 ? "bg-kumo-danger" : used >= 70 ? "bg-kumo-warning" : "bg-kumo-success";
  const reset = resetLabel(window.resetsAt, now);
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline justify-between text-xs">
        <span className="flex items-baseline gap-1.5">
          <span className="font-medium">{window.label}</span>
          {window.model ? (
            <span className="text-[10px] tracking-wide text-kumo-subtle uppercase">model</span>
          ) : null}
        </span>
        <span className="text-kumo-subtle">
          {usedLabel(window)}
          {remainingLabel(window) ? ` · ${remainingLabel(window)}` : ""}
        </span>
      </div>
      <div className="h-2 w-full overflow-hidden rounded-full bg-kumo-fill">
        <div
          className={cn("h-full rounded-full transition-all", tone)}
          style={{ width: `${width}%` }}
        />
      </div>
      {reset || window.status ? (
        <span className="text-[11px] text-kumo-subtle">
          {[reset, window.status && window.status !== "ok" ? window.status : undefined]
            .filter(Boolean)
            .join(" · ")}
        </span>
      ) : null}
    </div>
  );
}

function ModelCooldownRow({ model, now }: { model: ModelQuotaHealthView; now: number }) {
  const reset = resetLabel(model.resetsAt, now);
  const status = model.status.toLowerCase();
  const exhausted =
    status.includes("exhaust") || status.includes("cooldown") || status.includes("rate");
  return (
    <div className="flex flex-col gap-1 rounded-md border border-kumo-warning/40 bg-kumo-warning-tint px-3 py-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <code className="break-all text-xs font-medium">{model.model}</code>
        <Badge variant={exhausted ? "error" : "secondary"}>{model.status}</Badge>
      </div>
      {model.reason ? <p className="text-xs text-kumo-subtle">{model.reason}</p> : null}
      {reset ? <p className="text-[11px] text-kumo-subtle">{reset}</p> : null}
    </div>
  );
}

export function ProviderQuotaCard({
  quota,
  provider,
  health,
  onReset,
  resetDisabled,
  onEdit,
  onRemove,
}: {
  /** Live quota for this provider. Absent when the provider has no quota source. */
  quota?: ProviderQuotaView;
  /** Saved provider record — enables the account row and Edit / Remove. */
  provider?: ProviderView;
  health?: QuotaHealthView;
  /** Clears Jevonian's local quota state for this provider. Omit to hide the control. */
  onReset?: (provider: string) => void;
  resetDisabled?: boolean;
  onEdit?: (provider: ProviderView) => void;
  onRemove?: (provider: string) => void;
}) {
  const name = provider?.name ?? quota?.provider ?? "";
  const billing = provider?.billing ?? quota?.billing ?? "api";
  const spend = quota?.spend;
  const now = useResetClock();
  const windows = quota?.windows ?? [];
  const payPerToken = billing === "api" && windows.length === 0;
  return (
    <LayerCard className="flex flex-col gap-4 p-4">
      <div className="flex items-start justify-between gap-2">
        <div>
          <p className="flex items-center gap-2 text-sm font-medium">
            <ProviderIdentity provider={provider ?? name} />
          </p>
          <p className="text-xs text-kumo-subtle">
            {billing === "subscription" ? "subscription" : "pay per token"}
            {quota?.plan ? ` · ${quota.plan}` : ""}
            {quota?.note ? ` · ${quota.note}` : ""}
          </p>
        </div>
        {!quota ? (
          <Badge variant="outline">no quota</Badge>
        ) : payPerToken ? (
          <Badge variant="outline">no quota windows</Badge>
        ) : (
          <Badge variant={quota.source === "live" ? "primary" : "secondary"}>
            {SOURCE_LABEL[quota.source]}
          </Badge>
        )}
      </div>

      {windows.length > 0 ? (
        <div className="flex flex-col gap-3">
          {windows.map((window) => (
            <QuotaWindowRow key={window.id} window={window} />
          ))}
        </div>
      ) : quota?.balance ? (
        <p className="text-sm">
          <span className="text-kumo-subtle">remaining balance </span>
          <span className="font-medium">{balanceLabel(quota.balance)}</span>
        </p>
      ) : (
        <p className="text-xs text-kumo-subtle">
          {!quota
            ? "No quota source for this provider."
            : billing === "api"
              ? "Pay per token — no quota window."
              : (quota.error ?? "No quota source for this provider.")}
        </p>
      )}

      {windows.length > 0 && quota?.error ? (
        <p className="text-[11px] text-kumo-subtle">{quota.error}</p>
      ) : null}

      {quota?.resets && quota.resets.count > 0 ? (
        <section
          className="flex flex-col gap-1 border-t border-kumo-hairline pt-3"
          aria-label="Available resets"
        >
          <div className="flex items-center justify-between gap-2">
            <p className="text-xs font-medium">
              {quota.resets.count} reset{quota.resets.count === 1 ? "" : "s"} available
            </p>
            {onReset ? (
              <Button
                variant="outline"
                size="xs"
                onClick={() => onReset(name)}
                disabled={resetDisabled}
                title="Clears Jevonian's local cooldown and cached response quota only."
              >
                Reset local state
              </Button>
            ) : null}
          </div>
          {quota.resets.each?.map((reset, index) => (
            <p
              key={`${reset.expiresAt ?? "never"}-${index}`}
              className="text-[11px] text-kumo-subtle"
            >
              Reset {index + 1}:{" "}
              {reset.expiresAt ? resetLabel(reset.expiresAt, now) : "no expiry reported"}
            </p>
          ))}
          {!quota.resets.each?.length && quota.resets.until ? (
            <p className="text-[11px] text-kumo-subtle">
              Next expiry: {resetLabel(quota.resets.until, now)}
            </p>
          ) : null}
        </section>
      ) : null}

      {health?.modelHealth && health.modelHealth.length > 0 ? (
        <section
          className="flex flex-col gap-2 border-t border-kumo-hairline pt-3"
          aria-label="Model cooldowns"
        >
          <div>
            <p className="text-xs font-medium">Model-specific limits</p>
            <p className="text-[11px] text-kumo-subtle">
              Account quota can be OK while an individual model is rate-limited.
            </p>
          </div>
          {health.modelHealth.map((model) => (
            <ModelCooldownRow key={model.model} model={model} now={now} />
          ))}
        </section>
      ) : null}

      {health?.remainingUsd !== undefined ? (
        <p className="text-[11px] text-kumo-subtle">
          {money(health.remainingUsd)} left in the {health.window ?? "current"} window
          {health.avgRequestUsd === undefined
            ? ""
            : ` · ~${money(health.avgRequestUsd)} per request`}
        </p>
      ) : null}

      {quota && spend ? (
        <details className="border-t border-kumo-hairline pt-2">
          <summary className="cursor-pointer text-[11px] text-kumo-subtle">
            Periods &amp; source
          </summary>
          <div className="mt-2 flex flex-col gap-2">
            <p className="text-[11px] text-kumo-subtle">
              Read {SOURCE_LABEL[quota.source]} · fetched {formatTime(quota.fetchedAt)}
              {payPerToken ? " · pay per token, no window" : ""}
            </p>
            <div className="flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-kumo-subtle">
              <span>5h {money(spend.fiveHourUsd)}</span>
              <span>24h {money(spend.dayUsd)}</span>
              <span>7d {money(spend.weekUsd)}</span>
              <span>
                30d {money(spend.monthUsd)} · {spend.monthRequests} reqs
              </span>
            </div>
          </div>
        </details>
      ) : null}

      {provider ? (
        <div className="flex items-center justify-between gap-2 border-t border-kumo-hairline pt-3">
          <span className="truncate font-mono text-[11px] text-kumo-subtle">
            {provider.name}
            {" · "}
            {provider.models.length} model{provider.models.length === 1 ? "" : "s"}
            {provider.keySource === "none" ? " · no key" : ""}
          </span>
          <span className="flex shrink-0 items-center gap-0.5">
            {onEdit ? (
              <Button
                variant="ghost"
                size="sm"
                className="h-7 px-2 text-xs"
                onClick={() => onEdit(provider)}
                disabled={resetDisabled}
              >
                Edit
              </Button>
            ) : null}
            {onRemove ? (
              <Button
                variant="ghost"
                size="sm"
                className="h-7 px-2 text-xs text-kumo-danger hover:text-kumo-danger"
                onClick={() => onRemove(provider.name)}
                disabled={resetDisabled}
              >
                Remove
              </Button>
            ) : null}
          </span>
        </div>
      ) : null}
    </LayerCard>
  );
}

export function QuotaGrid({
  quotas,
  health = [],
  title = "Usage & limits",
  description,
  bare = false,
  onReset,
  resetDisabled,
}: {
  quotas: ProviderQuotaView[];
  health?: QuotaHealthView[];
  title?: string;
  description?: string;
  /** Render just the provider grid, for callers that supply their own card. */
  bare?: boolean;
  /** Clears Jevonian's local quota state for one provider. Omit to hide the control. */
  onReset?: (provider: string) => void;
  resetDisabled?: boolean;
}) {
  const byProvider = new Map(health.map((item) => [item.provider, item]));
  const grid =
    quotas.length === 0 ? (
      <p className="text-sm text-kumo-subtle">No providers configured.</p>
    ) : (
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3">
        {quotas.map((quota) => (
          <ProviderQuotaCard
            key={quota.provider}
            quota={quota}
            health={byProvider.get(quota.provider)}
            onReset={onReset}
            resetDisabled={resetDisabled}
          />
        ))}
      </div>
    );
  if (bare) return grid;
  return (
    <LayerCard>
      <LayerCard.Secondary className="block">
        <span className="flex flex-col gap-1">
          <Text variant="heading" as="h2">
            {title}
          </Text>
          <Text variant="secondary" size="sm">
            {description ??
              "Remaining quota, reset times, and per-provider spend. Estimates use models.dev rates."}
          </Text>
        </span>
      </LayerCard.Secondary>
      <LayerCard.Primary>{grid}</LayerCard.Primary>
    </LayerCard>
  );
}
