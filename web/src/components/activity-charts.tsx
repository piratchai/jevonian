import { LayerCard, Text } from "@cloudflare/kumo";
import { useState } from "react";

import type { ActivitySeriesPointView } from "@/lib/api";
import { cn, money } from "@/lib/utils";

function formatCompactNumber(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`;
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return n.toLocaleString();
}

interface ChartBaseProps {
  series: ActivitySeriesPointView[];
  loading?: boolean;
}

/**
 * Spend Chart:
 * Visualizes pay-as-you-go API spend and subscription value over time.
 */
export function SpendChart({ series }: ChartBaseProps) {
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);

  const height = 180;
  const width = 800;
  const padBottom = 26;
  const padTop = 16;
  const chartHeight = height - padBottom - padTop;

  const maxSpend = Math.max(...series.map((pt) => pt.spendUsd + pt.subscriptionUsd), 0.001);

  const barWidth = series.length > 0 ? Math.max(2, (width / series.length) * 0.68) : 8;
  const barGap = series.length > 0 ? (width / series.length) * 0.32 : 2;

  const hovered = hoverIndex !== null ? series[hoverIndex] : null;

  return (
    <LayerCard className="overflow-hidden p-4">
      <div className="flex flex-row items-center justify-between gap-3 pb-2">
        <div>
          <Text variant="heading" as="h3">
            Spend over time
          </Text>
          <Text variant="secondary" size="xs">
            Pay-as-you-go API costs and subscription equivalent value
          </Text>
        </div>
        {hovered ? (
          <div className="flex items-center gap-3 rounded-md border border-kumo-hairline bg-kumo-tint px-2.5 py-1 font-mono text-xs">
            <span className="font-sans font-medium text-kumo-default">{hovered.label}:</span>
            <span className="font-medium text-kumo-brand">{money(hovered.spendUsd)} API</span>
            {hovered.subscriptionUsd > 0 ? (
              <span className="text-kumo-subtle">· {money(hovered.subscriptionUsd)} sub</span>
            ) : null}
            <span className="font-sans text-kumo-subtle">({hovered.requests} reqs)</span>
          </div>
        ) : (
          <div className="flex items-center gap-4 text-xs text-kumo-subtle">
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-sm bg-kumo-brand" /> API spend
            </span>
            <span className="flex items-center gap-1.5">
              <span
                className="size-2 rounded-sm"
                style={{ backgroundColor: "var(--text-color-kumo-subtle)", opacity: 0.4 }}
              />{" "}
              Subscription value
            </span>
          </div>
        )}
      </div>
      <div className="pt-2">
        <div className="relative w-full">
          <svg
            viewBox={`0 0 ${width} ${height}`}
            className="w-full overflow-visible"
            style={{ maxHeight: 180 }}
            preserveAspectRatio="none"
            onMouseLeave={() => setHoverIndex(null)}
          >
            <defs>
              <linearGradient id="spend-api-gradient" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="var(--color-kumo-brand)" stopOpacity="0.9" />
                <stop offset="100%" stopColor="var(--color-kumo-brand)" stopOpacity="0.4" />
              </linearGradient>
            </defs>

            {/* Grid lines */}
            <line
              x1="0"
              y1={padTop + chartHeight}
              x2={width}
              y2={padTop + chartHeight}
              stroke="var(--color-kumo-hairline)"
              strokeWidth="1"
            />
            <line
              x1="0"
              y1={padTop + chartHeight / 2}
              x2={width}
              y2={padTop + chartHeight / 2}
              stroke="var(--color-kumo-hairline)"
              strokeWidth="0.5"
              strokeDasharray="4 4"
            />

            {/* Bars */}
            {series.map((pt, idx) => {
              const x = idx * (barWidth + barGap);
              const totalVal = pt.spendUsd + pt.subscriptionUsd;
              const totalH = totalVal > 0 ? Math.max(3, (totalVal / maxSpend) * chartHeight) : 0;
              const apiH = totalVal > 0 ? (pt.spendUsd / totalVal) * totalH : 0;
              const subH = totalH - apiH;

              const yTotal = padTop + chartHeight - totalH;
              const yApi = padTop + chartHeight - apiH;
              const isHovered = hoverIndex === idx;

              // Step labels to avoid crowding
              const showLabel =
                series.length <= 14 ||
                idx % Math.ceil(series.length / 10) === 0 ||
                idx === series.length - 1;

              return (
                <g
                  key={pt.timestamp}
                  className="cursor-pointer"
                  onMouseEnter={() => setHoverIndex(idx)}
                >
                  <rect
                    x={x - barGap / 2}
                    y={0}
                    width={barWidth + barGap}
                    height={height}
                    fill="transparent"
                  />
                  {/* Hover background column */}
                  {isHovered ? (
                    <rect
                      x={x - 2}
                      y={padTop}
                      width={barWidth + 4}
                      height={chartHeight}
                      fill="var(--color-kumo-tint)"
                      opacity="0.5"
                      rx="2"
                    />
                  ) : null}

                  {subH > 0 ? (
                    <rect
                      x={x}
                      y={yTotal}
                      width={barWidth}
                      height={subH}
                      rx="1"
                      fill="var(--text-color-kumo-subtle)"
                      opacity={isHovered ? "0.6" : "0.35"}
                    />
                  ) : null}
                  {apiH > 0 ? (
                    <rect
                      x={x}
                      y={yApi}
                      width={barWidth}
                      height={apiH}
                      rx="1"
                      fill="url(#spend-api-gradient)"
                      className={isHovered ? "brightness-125" : ""}
                    />
                  ) : null}
                  {totalVal === 0 ? (
                    <circle
                      cx={x + barWidth / 2}
                      cy={padTop + chartHeight - 1}
                      r="1"
                      fill="var(--text-color-kumo-subtle)"
                      opacity="0.3"
                    />
                  ) : null}

                  {showLabel ? (
                    <text
                      x={x + barWidth / 2}
                      y={height - 6}
                      textAnchor="middle"
                      fill="var(--text-color-kumo-subtle)"
                      fontSize="10"
                      className="select-none"
                    >
                      {pt.label}
                    </text>
                  ) : null}
                </g>
              );
            })}
          </svg>
        </div>
      </div>
    </LayerCard>
  );
}

/**
 * Tokens Chart:
 * Stacked breakdown of Prompt, Completion, and Cache Read tokens.
 */
export function TokensChart({ series }: ChartBaseProps) {
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);

  const height = 180;
  const width = 800;
  const padBottom = 26;
  const padTop = 16;
  const chartHeight = height - padBottom - padTop;

  const maxTokens = Math.max(...series.map((pt) => pt.totalTokens), 1);
  const barWidth = series.length > 0 ? Math.max(2, (width / series.length) * 0.68) : 8;
  const barGap = series.length > 0 ? (width / series.length) * 0.32 : 2;

  const hovered = hoverIndex !== null ? series[hoverIndex] : null;

  return (
    <LayerCard className="overflow-hidden p-4">
      <div className="flex flex-row items-center justify-between gap-3 pb-2">
        <div>
          <Text variant="heading" as="h3">
            Tokens over time
          </Text>
          <Text variant="secondary" size="xs">
            Prompt, completion, and cache read volume breakdown
          </Text>
        </div>
        {hovered ? (
          <div className="flex items-center gap-2 rounded-md border border-kumo-hairline bg-kumo-tint px-2.5 py-1 font-mono text-xs">
            <span className="font-sans font-medium text-kumo-default">{hovered.label}:</span>
            <span className="font-medium text-kumo-default">
              {formatCompactNumber(hovered.totalTokens)} total
            </span>
            <span className="text-kumo-subtle">
              ({formatCompactNumber(hovered.promptTokens)} prompt ·{" "}
              {formatCompactNumber(hovered.completionTokens)} out ·{" "}
              {formatCompactNumber(hovered.cacheReadTokens)} cache)
            </span>
          </div>
        ) : (
          <div className="flex items-center gap-4 text-xs text-kumo-subtle">
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-sm bg-kumo-brand" /> Prompt
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-sm bg-kumo-success" /> Completion
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-sm bg-kumo-info" /> Cache Read
            </span>
          </div>
        )}
      </div>
      <div className="pt-2">
        <div className="relative w-full">
          <svg
            viewBox={`0 0 ${width} ${height}`}
            className="w-full overflow-visible"
            style={{ maxHeight: 180 }}
            preserveAspectRatio="none"
            onMouseLeave={() => setHoverIndex(null)}
          >
            <line
              x1="0"
              y1={padTop + chartHeight}
              x2={width}
              y2={padTop + chartHeight}
              stroke="var(--color-kumo-hairline)"
              strokeWidth="1"
            />
            <line
              x1="0"
              y1={padTop + chartHeight / 2}
              x2={width}
              y2={padTop + chartHeight / 2}
              stroke="var(--color-kumo-hairline)"
              strokeWidth="0.5"
              strokeDasharray="4 4"
            />

            {series.map((pt, idx) => {
              const x = idx * (barWidth + barGap);
              const totalH =
                pt.totalTokens > 0 ? Math.max(3, (pt.totalTokens / maxTokens) * chartHeight) : 0;
              const promptH = pt.totalTokens > 0 ? (pt.promptTokens / pt.totalTokens) * totalH : 0;
              const compH =
                pt.totalTokens > 0 ? (pt.completionTokens / pt.totalTokens) * totalH : 0;
              const cacheH = totalH - promptH - compH;

              const yCache = padTop + chartHeight - totalH;
              const yComp = yCache + cacheH;
              const yPrompt = yComp + compH;

              const isHovered = hoverIndex === idx;
              const showLabel =
                series.length <= 14 ||
                idx % Math.ceil(series.length / 10) === 0 ||
                idx === series.length - 1;

              return (
                <g
                  key={pt.timestamp}
                  className="cursor-pointer"
                  onMouseEnter={() => setHoverIndex(idx)}
                >
                  <rect
                    x={x - barGap / 2}
                    y={0}
                    width={barWidth + barGap}
                    height={height}
                    fill="transparent"
                  />
                  {isHovered ? (
                    <rect
                      x={x - 2}
                      y={padTop}
                      width={barWidth + 4}
                      height={chartHeight}
                      fill="var(--color-kumo-tint)"
                      opacity="0.5"
                      rx="2"
                    />
                  ) : null}

                  {cacheH > 0 ? (
                    <rect
                      x={x}
                      y={yCache}
                      width={barWidth}
                      height={cacheH}
                      fill="var(--color-kumo-info)"
                      opacity={isHovered ? "0.9" : "0.7"}
                    />
                  ) : null}
                  {compH > 0 ? (
                    <rect
                      x={x}
                      y={yComp}
                      width={barWidth}
                      height={compH}
                      fill="var(--color-kumo-success)"
                      opacity={isHovered ? "0.95" : "0.8"}
                    />
                  ) : null}
                  {promptH > 0 ? (
                    <rect
                      x={x}
                      y={yPrompt}
                      width={barWidth}
                      height={promptH}
                      rx="1"
                      fill="var(--color-kumo-brand)"
                      className={isHovered ? "brightness-125" : ""}
                    />
                  ) : null}

                  {pt.totalTokens === 0 ? (
                    <circle
                      cx={x + barWidth / 2}
                      cy={padTop + chartHeight - 1}
                      r="1"
                      fill="var(--text-color-kumo-subtle)"
                      opacity="0.3"
                    />
                  ) : null}

                  {showLabel ? (
                    <text
                      x={x + barWidth / 2}
                      y={height - 6}
                      textAnchor="middle"
                      fill="var(--text-color-kumo-subtle)"
                      fontSize="10"
                      className="select-none"
                    >
                      {pt.label}
                    </text>
                  ) : null}
                </g>
              );
            })}
          </svg>
        </div>
      </div>
    </LayerCard>
  );
}

/**
 * Requests Chart:
 * Volume of requests per time interval with error highlight.
 */
export function RequestsChart({
  series,
  title = "Requests over time",
  description = "Volume and error count across all models",
  compact = false,
  coverage,
}: ChartBaseProps & {
  title?: string;
  description?: string;
  compact?: boolean;
  /** Whole-window cache coverage, or null when the window has no accounting. */
  coverage?: number | null;
}) {
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);

  const height = compact ? 112 : 180;
  const width = 800;
  const padBottom = compact ? 20 : 26;
  const padTop = compact ? 6 : 16;
  const chartHeight = height - padBottom - padTop;

  const maxReq = Math.max(...series.map((pt) => pt.requests), 1);
  const barWidth = series.length > 0 ? Math.max(2, (width / series.length) * 0.68) : 8;
  const barGap = series.length > 0 ? (width / series.length) * 0.32 : 2;

  const hovered = hoverIndex !== null ? series[hoverIndex] : null;

  return (
    <LayerCard className="overflow-hidden p-4">
      <div
        className={cn(
          "flex flex-row items-center justify-between gap-3",
          compact ? "pb-1" : "pb-2",
        )}
      >
        <div className="min-w-0">
          <Text variant="heading" as="h3">
            {title}
          </Text>
          <Text variant="secondary" size="xs" truncate={compact}>
            {description}
          </Text>
        </div>
        {hovered ? (
          <div className="flex shrink-0 items-center gap-2 rounded-md border border-kumo-hairline bg-kumo-tint px-2.5 py-1 font-mono text-xs">
            <span className="font-sans font-medium text-kumo-default">{hovered.label}:</span>
            <span className="font-medium text-kumo-default">{hovered.requests} requests</span>
            {hovered.errorRequests > 0 ? (
              <span className="font-semibold text-kumo-danger">
                ({hovered.errorRequests} errors)
              </span>
            ) : null}
          </div>
        ) : (
          <div className="flex shrink-0 items-center gap-4 text-xs text-kumo-subtle">
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-sm bg-kumo-brand" /> Requests
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-sm bg-kumo-danger" /> Errors
            </span>
            {coverage !== null && coverage !== undefined ? (
              <span
                className="flex items-center gap-1.5 rounded-md border border-kumo-hairline bg-kumo-tint px-2 py-0.5 font-mono"
                title="Share of input tokens served from the prompt cache in this window"
              >
                <span className="font-sans">Cache</span>
                <span className="font-medium text-kumo-default">{Math.round(coverage * 100)}%</span>
              </span>
            ) : null}
          </div>
        )}
      </div>
      <div className={compact ? "pt-1 pb-1" : "pt-2"}>
        <div className="relative w-full">
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
              stroke="var(--color-kumo-hairline)"
              strokeWidth="1"
            />
            <line
              x1="0"
              y1={padTop + chartHeight / 2}
              x2={width}
              y2={padTop + chartHeight / 2}
              stroke="var(--color-kumo-hairline)"
              strokeWidth="0.5"
              strokeDasharray="4 4"
            />

            {series.map((pt, idx) => {
              const x = idx * (barWidth + barGap);
              const reqH = pt.requests > 0 ? Math.max(3, (pt.requests / maxReq) * chartHeight) : 0;
              const y = padTop + chartHeight - reqH;
              const hasErrors = pt.errorRequests > 0;
              const isHovered = hoverIndex === idx;

              const showLabel =
                series.length <= 14 ||
                idx % Math.ceil(series.length / 10) === 0 ||
                idx === series.length - 1;

              return (
                <g
                  key={pt.timestamp}
                  className="cursor-pointer"
                  onMouseEnter={() => setHoverIndex(idx)}
                >
                  <rect
                    x={x - barGap / 2}
                    y={0}
                    width={barWidth + barGap}
                    height={height}
                    fill="transparent"
                  />
                  {isHovered ? (
                    <rect
                      x={x - 2}
                      y={padTop}
                      width={barWidth + 4}
                      height={chartHeight}
                      fill="var(--color-kumo-tint)"
                      opacity="0.5"
                      rx="2"
                    />
                  ) : null}

                  {reqH > 0 ? (
                    <rect
                      x={x}
                      y={y}
                      width={barWidth}
                      height={reqH}
                      rx="1"
                      fill={hasErrors ? "var(--color-kumo-danger)" : "var(--color-kumo-brand)"}
                      opacity={isHovered ? "1" : "0.75"}
                    />
                  ) : (
                    <circle
                      cx={x + barWidth / 2}
                      cy={padTop + chartHeight - 1}
                      r="1"
                      fill="var(--text-color-kumo-subtle)"
                      opacity="0.3"
                    />
                  )}

                  {showLabel ? (
                    <text
                      x={x + barWidth / 2}
                      y={height - 4}
                      textAnchor="middle"
                      fill="var(--text-color-kumo-subtle)"
                      fontSize="10"
                      className="select-none"
                    >
                      {pt.label}
                    </text>
                  ) : null}
                </g>
              );
            })}
          </svg>
        </div>
      </div>
    </LayerCard>
  );
}
