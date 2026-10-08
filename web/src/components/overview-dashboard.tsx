import { Check, Copy } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Link } from "react-router";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import type { ActivityModelStatView, ActivityReportView, ActivitySeriesPointView } from "@/lib/api";
import { cn } from "@/lib/utils";

export function formatCompact(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`;
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return n.toLocaleString();
}

function usd(value: number): string {
  if (value >= 100) return `$${value.toFixed(0)}`;
  if (value >= 10) return `$${value.toFixed(1)}`;
  return `$${value.toFixed(2)}`;
}

function IconCopyButton({
  text,
  copiedId,
  activeId,
  onCopy,
}: {
  text: string;
  copiedId: string;
  activeId: string;
  onCopy: (value: string, id: string) => void;
}) {
  const done = copiedId === activeId;
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      aria-label={done ? "Copied" : "Copy"}
      title={done ? "Copied" : "Copy"}
      className="shrink-0 text-muted-foreground"
      onClick={() => onCopy(text, activeId)}
    >
      {done ? <Check className="size-3.5 text-emerald-500" /> : <Copy className="size-3.5" />}
    </Button>
  );
}

/** Compact single-metric bar chart used inside dashboard cards. */
function MiniBars({
  series,
  valueOf,
  height = 72,
  className,
  emptyLabel = "No activity in this window",
  formatValue = formatCompact,
}: {
  series: ActivitySeriesPointView[];
  valueOf: (pt: ActivitySeriesPointView) => number;
  height?: number;
  className?: string;
  emptyLabel?: string;
  formatValue?: (n: number) => string;
}) {
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);
  const width = 640;
  const padTop = 6;
  const padBottom = 16;
  const chartHeight = height - padTop - padBottom;
  const values = series.map(valueOf);
  const max = Math.max(...values, 0);
  const hasSignal = max > 0;
  const barWidth = series.length > 0 ? Math.max(2.5, (width / series.length) * 0.55) : 8;
  const gap = series.length > 0 ? (width / series.length) * 0.45 : 2;
  const hovered = hoverIndex !== null ? series[hoverIndex] : null;

  if (!hasSignal) {
    return (
      <div
        className={cn(
          "flex items-center justify-center rounded-lg border border-dashed bg-muted/30 text-[11px] text-muted-foreground",
          className,
        )}
        style={{ height }}
      >
        {emptyLabel}
      </div>
    );
  }

  return (
    <div className={cn("relative w-full", className)}>
      {hovered ? (
        <div className="pointer-events-none absolute -top-0.5 right-0 z-10 rounded-md border bg-background/95 px-2 py-0.5 font-mono text-[10px] shadow-xs">
          <span className="font-sans text-muted-foreground">{hovered.label}</span>{" "}
          {formatValue(valueOf(hovered))}
        </div>
      ) : null}
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="w-full overflow-visible"
        style={{ maxHeight: height }}
        preserveAspectRatio="none"
        onMouseLeave={() => setHoverIndex(null)}
      >
        <line
          x1="0"
          y1={padTop + chartHeight}
          x2={width}
          y2={padTop + chartHeight}
          stroke="var(--border)"
          strokeWidth="1"
        />
        {series.map((pt, idx) => {
          const value = values[idx];
          const h = value > 0 ? Math.max(3, (value / max) * chartHeight) : 0;
          const x = idx * (barWidth + gap);
          const y = padTop + chartHeight - h;
          const active = hoverIndex === idx;
          const showLabel =
            series.length <= 10 ||
            idx % Math.ceil(series.length / 7) === 0 ||
            idx === series.length - 1;
          return (
            <g key={pt.timestamp} onMouseEnter={() => setHoverIndex(idx)}>
              <rect
                x={x}
                y={padTop}
                width={barWidth + gap}
                height={chartHeight}
                fill="transparent"
              />
              {value > 0 ? (
                <rect
                  x={x}
                  y={y}
                  width={barWidth}
                  height={h}
                  rx={2}
                  fill="var(--primary)"
                  opacity={active ? 1 : 0.35 + 0.65 * (value / max)}
                />
              ) : null}
              {showLabel ? (
                <text
                  x={x + barWidth / 2}
                  y={height - 2}
                  textAnchor="middle"
                  fill="var(--muted-foreground)"
                  fontSize="9"
                >
                  {pt.label}
                </text>
              ) : null}
            </g>
          );
        })}
      </svg>
    </div>
  );
}

export function TodayTokensCard({
  today,
  week,
  month,
}: {
  today: ActivityReportView;
  week: ActivityReportView;
  month: ActivityReportView;
}) {
  const todayTokens = today.summary.totalTokens;
  const weekTokens = week.summary.totalTokens;
  const weekShare = weekTokens > 0 ? Math.round((todayTokens / weekTokens) * 100) : 0;
  const todaySpend = today.summary.totalSpendUsd;
  const monthSpend = month.summary.totalSpendUsd;
  const dateLabel = new Date(today.endTime || Date.now()).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });

  return (
    <Card className="flex min-h-0 flex-col overflow-hidden shadow-none">
      <CardHeader className="flex-row items-start justify-between gap-3 space-y-0 px-5 pt-5 pb-2">
        <div>
          <CardDescription className="text-[11px] tracking-wide">Today tokens</CardDescription>
          <CardTitle className="mt-1 text-[2rem] font-semibold tracking-[-0.04em] tabular-nums sm:text-[2.35rem]">
            {formatCompact(todayTokens)}
          </CardTitle>
        </div>
        <span className="rounded-md bg-muted px-2 py-0.5 text-[11px] text-muted-foreground tabular-nums">
          {dateLabel}
        </span>
      </CardHeader>
      <CardContent className="flex flex-1 flex-col justify-between gap-4 px-5 pb-5">
        <MiniBars
          series={today.series}
          valueOf={(pt) => pt.totalTokens}
          height={128}
          className="min-h-[8rem]"
          emptyLabel="No tokens yet today"
        />
        <div className="grid grid-cols-3 gap-0 border-t pt-3 text-xs">
          <div className="pr-3">
            <p className="font-medium tabular-nums text-foreground">{weekShare}% of week</p>
            <p className="mt-0.5 text-[11px] text-muted-foreground">vs last 7 days</p>
          </div>
          <div className="border-l px-3">
            <p className="font-medium tabular-nums text-foreground">{usd(todaySpend)} today</p>
            <p className="mt-0.5 text-[11px] text-muted-foreground">estimated spend</p>
          </div>
          <div className="border-l pl-3">
            <p className="font-medium tabular-nums text-foreground">{usd(monthSpend)} month</p>
            <p className="mt-0.5 text-[11px] text-muted-foreground">last 30 days</p>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

/** Tiny relative bars stand in for per-model sparklines (API has no model series). */
function ShareSpark({ ratio }: { ratio: number }) {
  const steps = 7;
  const filled = Math.max(1, Math.round(ratio * steps));
  return (
    <div className="flex h-5 items-end gap-0.5" aria-hidden>
      {Array.from({ length: steps }, (_, i) => {
        const t = (i + 1) / steps;
        const on = i < filled;
        const h = 30 + t * 70;
        return (
          <span
            key={i}
            className={cn("w-1 rounded-[1px]", on ? "bg-primary/80" : "bg-muted")}
            style={{ height: `${h}%` }}
          />
        );
      })}
    </div>
  );
}

export function ModelsCard({ models }: { models: ActivityModelStatView[] }) {
  const top = models.slice(0, 8);
  const maxTokens = Math.max(...top.map((m) => m.totalTokens), 1);

  return (
    <Card className="flex min-h-0 flex-col overflow-hidden shadow-none">
      <CardHeader className="flex-row items-center justify-between space-y-0 px-5 pt-5 pb-2">
        <CardTitle className="text-sm font-semibold">Models</CardTitle>
        <Link
          to="/models"
          className="text-xs text-muted-foreground transition-colors hover:text-foreground"
        >
          All models →
        </Link>
      </CardHeader>
      <CardContent className="flex flex-1 flex-col gap-0.5 px-3 pb-4">
        {top.length === 0 ? (
          <p className="px-2 py-8 text-center text-xs text-muted-foreground">
            No model traffic yet.
          </p>
        ) : (
          top.map((m) => {
            const ratio = m.totalTokens / maxTokens;
            const cost = m.spendUsd + m.subscriptionUsd;
            return (
              <div
                key={m.model}
                className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-3 rounded-lg px-2 py-2.5 transition-colors hover:bg-muted/50"
              >
                <div className="min-w-0">
                  <p
                    className="truncate text-sm font-medium"
                    title={m.variants?.join(", ") ?? m.model}
                  >
                    {m.label ?? m.model}
                  </p>
                  <p className="mt-0.5 text-[11px] text-muted-foreground tabular-nums">
                    {m.requests.toLocaleString()} req
                    {m.percentSpend > 0 ? ` · ${m.percentSpend.toFixed(0)}% spend` : ""}
                  </p>
                </div>
                <ShareSpark ratio={ratio} />
                <div className="min-w-[5.5rem] shrink-0 text-right font-mono text-[11px] text-muted-foreground tabular-nums">
                  <span className="text-foreground">{formatCompact(m.totalTokens)}</span>
                  <span className="text-muted-foreground"> · {usd(cost)}</span>
                </div>
              </div>
            );
          })
        )}
      </CardContent>
    </Card>
  );
}

const CELL = 11;
const CELL_GAP = 3;

/** GitHub-style contribution grid from daily activity buckets. */
export function UsageHeatmapCard({
  series,
  weekTokens,
  monthTokens,
}: {
  series: ActivitySeriesPointView[];
  weekTokens: number;
  monthTokens: number;
}) {
  const [hover, setHover] = useState<{ label: string; tokens: number } | null>(null);

  const cells = series.map((pt) => ({
    date: new Date(pt.timestamp),
    label: pt.label,
    tokens: pt.totalTokens,
  }));
  const max = Math.max(...cells.map((c) => c.tokens), 1);

  const weeks: Array<Array<{ label: string; tokens: number; empty?: boolean } | null>> = [];
  if (cells.length > 0) {
    const first = cells[0].date;
    const pad = first.getDay();
    let week: Array<{ label: string; tokens: number; empty?: boolean } | null> = Array.from(
      { length: pad },
      () => ({ label: "", tokens: 0, empty: true }),
    );
    for (const cell of cells) {
      week.push({ label: cell.label, tokens: cell.tokens });
      if (week.length === 7) {
        weeks.push(week);
        week = [];
      }
    }
    if (week.length > 0) {
      while (week.length < 7) week.push(null);
      weeks.push(week);
    }
  }

  const monthLabels: Array<{ text: string; col: number }> = [];
  let lastMonth = -1;
  weeks.forEach((week, col) => {
    const firstReal = week.find((c) => c && !c.empty);
    if (!firstReal) return;
    const match = cells.find((c) => c.label === firstReal.label);
    if (!match) return;
    const month = match.date.getMonth();
    if (month !== lastMonth) {
      monthLabels.push({
        text: match.date.toLocaleDateString(undefined, { month: "short" }),
        col,
      });
      lastMonth = month;
    }
  });

  function level(tokens: number): string {
    if (tokens <= 0) return "bg-muted";
    const ratio = tokens / max;
    if (ratio < 0.2) return "bg-primary/25";
    if (ratio < 0.45) return "bg-primary/45";
    if (ratio < 0.7) return "bg-primary/70";
    return "bg-primary";
  }

  const gridWidth = weeks.length * CELL + Math.max(0, weeks.length - 1) * CELL_GAP;

  return (
    <Card className="overflow-hidden shadow-none">
      <CardHeader className="flex-row items-start justify-between gap-3 space-y-0 px-5 pt-5 pb-2">
        <div>
          <CardTitle className="text-sm font-semibold">Usage</CardTitle>
          <CardDescription className="text-[11px]">Daily token activity</CardDescription>
        </div>
        <div className="space-y-1 text-right text-[11px] text-muted-foreground">
          <p>
            This week{" "}
            <span className="font-medium text-foreground tabular-nums">
              {formatCompact(weekTokens)}
            </span>
          </p>
          <p>
            This month{" "}
            <span className="font-medium text-foreground tabular-nums">
              {formatCompact(monthTokens)}
            </span>
          </p>
        </div>
      </CardHeader>
      <CardContent className="px-5 pb-5">
        {weeks.length === 0 ? (
          <p className="py-6 text-center text-xs text-muted-foreground">No usage yet.</p>
        ) : (
          <div className="overflow-x-auto">
            <div style={{ width: gridWidth, minWidth: gridWidth }}>
              <div
                className="mb-1.5 grid text-[10px] text-muted-foreground"
                style={{
                  gridTemplateColumns: `repeat(${weeks.length}, ${CELL}px)`,
                  columnGap: CELL_GAP,
                }}
              >
                {weeks.map((_, col) => {
                  const label = monthLabels.find((m) => m.col === col);
                  return (
                    <span key={col} className="truncate leading-none">
                      {label?.text ?? ""}
                    </span>
                  );
                })}
              </div>
              <div
                className="grid"
                style={{
                  gridTemplateRows: `repeat(7, ${CELL}px)`,
                  gridAutoFlow: "column",
                  gridTemplateColumns: `repeat(${weeks.length}, ${CELL}px)`,
                  gap: CELL_GAP,
                  width: gridWidth,
                }}
                onMouseLeave={() => setHover(null)}
              >
                {weeks.flatMap((week, col) =>
                  week.map((cell, row) => {
                    if (!cell) {
                      return <span key={`${col}-${row}`} style={{ width: CELL, height: CELL }} />;
                    }
                    if (cell.empty) {
                      return (
                        <span
                          key={`${col}-${row}`}
                          className="rounded-[3px] bg-transparent"
                          style={{ width: CELL, height: CELL }}
                        />
                      );
                    }
                    return (
                      <button
                        key={`${col}-${row}`}
                        type="button"
                        title={`${cell.label}: ${formatCompact(cell.tokens)} tokens`}
                        className={cn(
                          "rounded-[3px] outline-none transition-[transform,opacity] duration-150 ease-out",
                          "hover:scale-110 hover:opacity-100 focus-visible:ring-2 focus-visible:ring-ring",
                          level(cell.tokens),
                        )}
                        style={{ width: CELL, height: CELL }}
                        onMouseEnter={() => setHover({ label: cell.label, tokens: cell.tokens })}
                      />
                    );
                  }),
                )}
              </div>
            </div>
            <div className="mt-3 flex items-center justify-between gap-3 text-[11px] text-muted-foreground">
              <p className="min-h-[1rem] tabular-nums">
                {hover ? (
                  <>
                    {hover.label}:{" "}
                    <span className="font-medium text-foreground">
                      {formatCompact(hover.tokens)} tokens
                    </span>
                  </>
                ) : (
                  <span className="opacity-70">Hover a day for detail</span>
                )}
              </p>
              <div className="flex items-center gap-1">
                <span>Less</span>
                {[0, 0.15, 0.35, 0.55, 0.85].map((r, i) => (
                  <span
                    key={i}
                    className={cn("size-2.5 rounded-[2px]", level(r === 0 ? 0 : r * max))}
                  />
                ))}
                <span>More</span>
              </div>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

export function SpendCard({
  summary,
  series,
}: {
  summary: ActivityReportView["summary"];
  series: ActivitySeriesPointView[];
}) {
  return (
    <Card className="overflow-hidden shadow-none">
      <CardHeader className="space-y-1 px-5 pt-5 pb-2">
        <CardDescription className="text-[11px] tracking-wide">Cost · 30 days</CardDescription>
        <CardTitle className="text-[1.75rem] font-semibold tracking-[-0.03em] tabular-nums">
          {usd(summary.totalSpendUsd)}
        </CardTitle>
        <p className="text-[11px] text-muted-foreground">
          api {usd(summary.apiSpendUsd)} · sub {usd(summary.subscriptionValueUsd)}
        </p>
      </CardHeader>
      <CardContent className="px-5 pb-5">
        <MiniBars
          series={series}
          valueOf={(pt) => pt.spendUsd + pt.subscriptionUsd}
          height={56}
          emptyLabel="No spend in this window"
          formatValue={usd}
        />
      </CardContent>
    </Card>
  );
}

export function TokensCard({
  summary,
  series,
  cacheHitRate,
}: {
  summary: ActivityReportView["summary"];
  series: ActivitySeriesPointView[];
  cacheHitRate: number;
}) {
  return (
    <Card className="overflow-hidden shadow-none">
      <CardHeader className="space-y-1 px-5 pt-5 pb-2">
        <CardDescription className="text-[11px] tracking-wide">Tokens · 30 days</CardDescription>
        <CardTitle className="text-[1.75rem] font-semibold tracking-[-0.03em] tabular-nums">
          {formatCompact(summary.totalTokens)}
        </CardTitle>
        <p className="text-[11px] text-muted-foreground">
          prompt {formatCompact(summary.promptTokens)} · out{" "}
          {formatCompact(summary.completionTokens)} · cache {formatCompact(summary.cacheReadTokens)}{" "}
          · hit {(cacheHitRate * 100).toFixed(0)}%
        </p>
      </CardHeader>
      <CardContent className="px-5 pb-5">
        <MiniBars
          series={series}
          valueOf={(pt) => pt.totalTokens}
          height={56}
          emptyLabel="No tokens in this window"
        />
      </CardContent>
    </Card>
  );
}

function StripChip({
  label,
  children,
  className,
}: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5",
        className,
      )}
    >
      <span className="text-[11px] text-muted-foreground">{label}</span>
      {children}
    </div>
  );
}

export function OverviewStatusStrip({
  running,
  routingMode,
  localUrl,
  apiKeyHint,
  onCopyUrl,
  copied,
}: {
  running: boolean;
  routingMode: string;
  localUrl: string;
  apiKeyHint: string;
  onCopyUrl: (value: string, id: string) => void;
  copied: string;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <StripChip label="Status">
        <Badge
          variant="secondary"
          className={cn(
            "h-5 px-1.5 text-[11px] font-medium",
            running && "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400",
          )}
        >
          <span
            className={cn(
              "mr-1 inline-block size-1.5 rounded-full",
              running ? "bg-emerald-500" : "bg-muted-foreground",
            )}
          />
          {running ? "Running" : "Offline"}
        </Badge>
      </StripChip>

      <StripChip label="Mode">
        <Badge variant="outline" className="h-5 px-1.5 text-[11px] font-medium capitalize">
          {routingMode || "auto"}
        </Badge>
      </StripChip>

      <StripChip label="Local URL" className="min-w-0 max-w-full">
        <code className="max-w-[14rem] truncate font-mono text-[11px] sm:max-w-[18rem]">
          {localUrl}
        </code>
        <IconCopyButton text={localUrl} copiedId={copied} activeId="local" onCopy={onCopyUrl} />
      </StripChip>

      {apiKeyHint ? (
        <StripChip label="API key">
          <code className="font-mono text-[11px] tracking-wide">{apiKeyHint}</code>
        </StripChip>
      ) : null}
    </div>
  );
}

export function OverviewDashboardGrid({
  today,
  week,
  month,
  history,
  cacheHitRate,
}: {
  today: ActivityReportView;
  week: ActivityReportView;
  month: ActivityReportView;
  history: ActivityReportView;
  cacheHitRate: number;
}) {
  const heatmapSeries =
    history.series.length >= 14
      ? history.series
      : month.series.length > 0
        ? month.series
        : week.series;

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-12 lg:gap-4">
      <div className="flex flex-col gap-4 lg:col-span-7">
        <TodayTokensCard today={today} week={week} month={month} />
        <ModelsCard models={week.models.length > 0 ? week.models : month.models} />
      </div>
      <div className="flex flex-col gap-4 lg:col-span-5">
        <UsageHeatmapCard
          series={heatmapSeries}
          weekTokens={week.summary.totalTokens}
          monthTokens={month.summary.totalTokens}
        />
        <SpendCard summary={month.summary} series={month.series} />
        <TokensCard summary={month.summary} series={month.series} cacheHitRate={cacheHitRate} />
      </div>
    </div>
  );
}
