import { Badge, Button, Input, LayerCard, LayerDialog, Popover, SkeletonLine } from "@cloudflare/kumo";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { RequestsChart } from "@/components/activity-charts";
import { LogDetailView } from "@/components/log-detail/log-detail-view";
import { LOG_COLUMNS } from "@/components/logs/columns";
import { FilterRail, FilterTriggerFace } from "@/components/logs/filter-rail";
import {
  EMPTY_FILTERS,
  filtersActive,
  filtersToParams,
  filtersToQuery,
  toggleValue,
  type FilterGroupKey,
  type LogFilters,
} from "@/components/logs/filter-types";
import { StatusPills } from "@/components/logs/status-pills";
import { useLogFacets } from "@/components/logs/use-log-facets";
import { LogsTableSkeleton } from "@/components/page-skeletons";
import { ProviderIdentity } from "@/components/provider-identity";
import { useLogStream } from "@/hooks/use-log-stream";
import { useVirtualizer } from "@/hooks/use-virtualizer";
import {
  api,
  logKey,
  type ActivitySeriesPointView,
  type LogRecord,
  type LogSeries,
} from "@/lib/api";
import {
  cacheCoverage,
  cacheCoverageTitle,
  cacheCoverageTone,
  formatCacheCoverage,
} from "@/lib/cache";
import { cn, formatTime, money } from "@/lib/utils";

const ROW_ESTIMATE_HEIGHT = 44;
const SEARCH_DEBOUNCE_MS = 300;
const CHART_DEBOUNCE_MS = 800;

/** Map the logs series endpoint onto the shared Activity chart shape. */
function toRequestSeries(series: LogSeries | null): ActivitySeriesPointView[] {
  if (!series) return [];
  return series.buckets.map((bucket) => {
    const at = new Date(bucket.start);
    const label = Number.isNaN(at.getTime())
      ? bucket.start
      : at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
    return {
      timestamp: bucket.start,
      label,
      spendUsd: bucket.costUsd,
      subscriptionUsd: 0,
      promptTokens: 0,
      completionTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 0,
      requests: bucket.requests,
      errorRequests: bucket.errors,
    };
  });
}

export function LogsPage() {
  const [filters, setFilters] = useState<LogFilters>(EMPTY_FILTERS);
  const [searchDraft, setSearchDraft] = useState("");
  const [live, setLive] = useState(true);
  // The log table is the point of this page, so filters live in a dropdown
  // beside the search input instead of a rail that costs layout width.
  const [filtersOpen, setFiltersOpen] = useState(false);
  /** Selected record id; opens the detail dialog. */
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const [logs, setLogs] = useState<LogRecord[]>([]);
  const [total, setTotal] = useState<number | null>(null);
  const [nextBefore, setNextBefore] = useState<number | null>(null);
  const [loadingInitial, setLoadingInitial] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState("");
  const [newLogIds, setNewLogIds] = useState<Set<string>>(() => new Set());
  const [series, setSeries] = useState<LogSeries | null>(null);
  const scrollContainerRef = useRef<HTMLDivElement | null>(null);

  // One serialization feeds the list, the chart, the facets, and the stream,
  // so every surface sees the identical filtered ledger.
  const filterParams = useMemo(() => filtersToParams(filters), [filters]);
  const streamQuery = useMemo(() => filtersToQuery(filters), [filters]);
  const { facets, stale: facetsStale, refresh: refreshFacets } = useLogFacets(filters);

  useEffect(() => {
    const timer = setTimeout(
      () => setFilters((current) => ({ ...current, q: searchDraft })),
      SEARCH_DEBOUNCE_MS,
    );
    return () => clearTimeout(timer);
  }, [searchDraft]);

  const toggleFilter = useCallback((group: FilterGroupKey, value: string) => {
    setFilters((current) => ({ ...current, [group]: toggleValue(current[group], value) }));
  }, []);

  const clearFilters = useCallback(() => {
    setFilters(EMPTY_FILTERS);
    setSearchDraft("");
  }, []);

  const loadInitial = useCallback(async () => {
    setLoadingInitial(true);
    try {
      const [res, chartRes] = await Promise.all([
        api.logs({ limit: 100, ...filterParams }),
        api.logSeries({ minutes: 60, buckets: 40, ...filterParams }),
      ]);
      setLogs(res.logs);
      setTotal(res.total);
      setNextBefore(res.nextBefore);
      setSeries(chartRes);
      setError("");
    } catch (cause) {
      setError(String(cause));
    } finally {
      setLoadingInitial(false);
    }
  }, [filterParams]);

  const loadMore = useCallback(async () => {
    if (loadingMore || nextBefore === null) return;
    setLoadingMore(true);
    try {
      const res = await api.logs({ limit: 100, before: nextBefore, ...filterParams });
      setLogs((prev) => {
        const existing = new Set(prev.map(logKey));
        const append = res.logs.filter((item) => !existing.has(logKey(item)));
        return [...prev, ...append];
      });
      setNextBefore(res.nextBefore);
      setTotal(res.total);
    } catch (cause) {
      setError(String(cause));
    } finally {
      setLoadingMore(false);
    }
  }, [loadingMore, nextBefore, filterParams]);

  useEffect(() => {
    void loadInitial();
  }, [loadInitial]);

  const chartTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const refreshChart = useCallback(() => {
    if (chartTimer.current) clearTimeout(chartTimer.current);
    chartTimer.current = setTimeout(() => {
      void api
        .logSeries({ minutes: 60, buckets: 40, ...filterParams })
        .then(setSeries)
        .catch(() => {});
    }, CHART_DEBOUNCE_MS);
  }, [filterParams]);

  useEffect(() => {
    return () => {
      if (chartTimer.current) clearTimeout(chartTimer.current);
    };
  }, []);

  const handleLiveRecord = useCallback(
    (record: LogRecord) => {
      const key = logKey(record);
      setNewLogIds((prev) => {
        const next = new Set(prev);
        next.add(key);
        return next;
      });
      setTimeout(() => {
        setNewLogIds((prev) => {
          const next = new Set(prev);
          next.delete(key);
          return next;
        });
      }, 2400);
      setLogs((prev) => {
        if (prev.some((item) => logKey(item) === key)) return prev;
        return [record, ...prev];
      });
      setTotal((prev) => (prev !== null ? prev + 1 : 1));
      refreshChart();
      refreshFacets();
    },
    [refreshChart, refreshFacets],
  );

  const streamStatus = useLogStream({
    enabled: live,
    query: streamQuery,
    onRecord: handleLiveRecord,
    onReady: refreshChart,
  });

  const virtualizer = useVirtualizer({
    count: logs.length,
    estimateSize: () => ROW_ESTIMATE_HEIGHT,
    overscan: 10,
    getScrollElement: () => scrollContainerRef.current,
  });
  const virtualItems = virtualizer.getVirtualItems();
  const totalHeight = virtualizer.getTotalSize();

  useEffect(() => {
    const el = scrollContainerRef.current;
    if (!el) return;
    const onScroll = () => {
      const { scrollTop, scrollHeight, clientHeight } = el;
      if (scrollHeight - (scrollTop + clientHeight) < 250) void loadMore();
    };
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => el.removeEventListener("scroll", onScroll);
  }, [loadMore]);

  const filtered = filtersActive(filters);
  const requestSeries = useMemo(() => toRequestSeries(series), [series]);
  const chartDescription = useMemo(() => {
    const window = series?.minutes ?? 60;
    const count = total !== null ? total : logs.length;
    const scope = filtered ? "matching the current filters" : "across the ledger";
    const coverage =
      series?.cacheCoverage !== null && series?.cacheCoverage !== undefined
        ? ` Cache covers ${Math.round(series.cacheCoverage * 100)}% of input tokens in this window.`
        : "";
    return `Last ${window} minutes ${scope}. Primary bars are request volume; red marks intervals that include errors.${coverage} ${count} ${count === 1 ? "record" : "records"} loaded.`;
  }, [series?.minutes, series?.cacheCoverage, total, logs.length, filtered]);

  const selectRecord = useCallback((log: LogRecord) => {
    if (log.id) setSelectedId(log.id);
  }, []);

  return (
    <div className="flex h-[calc(100svh-58px-3rem)] gap-3 md:h-[calc(100svh-58px-4rem)]">
      <div className="flex min-w-0 flex-1 flex-col gap-3">
        <div className="flex shrink-0 items-center justify-between gap-4">
          <div className="flex min-w-0 items-center gap-2">
            <h1 className="shrink-0 text-lg font-semibold tracking-tight">Logs</h1>
          </div>
          <div className="flex min-w-0 flex-nowrap items-center justify-end gap-2">
            <Popover open={filtersOpen} onOpenChange={setFiltersOpen}>
              <Popover.Trigger
                render={
                  <Button
                    variant="outline"
                    shape="square"
                    className="shrink-0"
                    aria-label="Toggle filters"
                  />
                }
              >
                <FilterTriggerFace filters={filters} />
              </Popover.Trigger>
              <Popover.Content align="end" sideOffset={6} className="w-72 gap-0 p-0">
                <FilterRail
                  filters={filters}
                  facets={facets}
                  facetsStale={facetsStale}
                  onToggle={toggleFilter}
                  onClear={clearFilters}
                  className="max-h-[70vh]"
                />
              </Popover.Content>
            </Popover>
            <Input
              value={searchDraft}
              onValueChange={setSearchDraft}
              placeholder="Search…"
              autoComplete="off"
              className="w-40 shrink-0"
              aria-label="Search logs"
            />

            <Button
              variant="outline"
              className="shrink-0 gap-2"
              aria-pressed={live}
              title={live ? "Pause live stream" : "Resume live stream"}
              onClick={() => setLive((current) => !current)}
            >
              <span
                className={cn(
                  "size-2 rounded-full",
                  streamStatus === "live"
                    ? "animate-pulse bg-kumo-success"
                    : streamStatus === "connecting"
                      ? "animate-pulse bg-kumo-warning"
                      : "bg-kumo-interact/60",
                )}
              />
              <span className="capitalize">
                {streamStatus === "live"
                  ? "Live"
                  : streamStatus === "connecting"
                    ? "Connecting"
                    : "Paused"}
              </span>
            </Button>

            <Button variant="outline" className="shrink-0" onClick={() => void loadInitial()}>
              Refresh
            </Button>
          </div>
        </div>

        {error ? <p className="shrink-0 text-xs text-kumo-danger">{error}</p> : null}

        <div className="shrink-0">
          <RequestsChart
            series={requestSeries}
            title="Requests over time"
            description={chartDescription}
            coverage={series?.cacheCoverage}
            compact
          />
        </div>

        <StatusPills
          status={filters.status}
          onChange={(next) => setFilters((c) => ({ ...c, status: next }))}
        />

        <LayerCard className="flex min-h-0 flex-1 flex-col">
          <div
            className="grid shrink-0 items-center gap-3 border-b border-kumo-hairline bg-kumo-tint/40 px-4 py-2.5 text-xs font-semibold tracking-wider text-kumo-subtle uppercase"
            style={{ gridTemplateColumns: LOG_COLUMNS }}
          >
            <div className="truncate">Time</div>
            <div className="truncate">Model</div>
            <div className="truncate">Provider</div>
            <div className="truncate">Phase</div>
            <div className="truncate">Effort</div>
            <div className="truncate">Status</div>
            <div className="truncate" title="Share of input tokens served from the prompt cache">
              Cache
            </div>
            <div className="truncate">Cost</div>
            <div className="truncate">Latency</div>
            <div className="truncate text-right">Details</div>
          </div>

          <div
            ref={scrollContainerRef}
            className="relative min-h-0 flex-1 divide-y divide-kumo-hairline overflow-x-hidden overflow-y-auto"
          >
            {loadingInitial ? (
              <LogsTableSkeleton rows={12} />
            ) : logs.length === 0 ? (
              <div className="flex h-40 items-center justify-center text-sm text-kumo-subtle">
                {filtered
                  ? "No requests match these filters."
                  : "No traffic yet — proxied requests will stream in here."}
              </div>
            ) : (
              <div style={{ height: `${totalHeight}px`, width: "100%", position: "relative" }}>
                {virtualItems.map((virtualRow) => {                  const log = logs[virtualRow.index];
                  if (!log) return null;
                  const key = logKey(log);
                  const isNew = newLogIds.has(key);
                  const isSelected = Boolean(log.id) && log.id === selectedId;
                  const failed = log.status >= 400;
                  const coverage = cacheCoverage(log);

                  return (
                    <div
                      key={key}
                      data-index={virtualRow.index}
                      ref={virtualizer.measureElement}
                      role="button"
                      tabIndex={log.id ? 0 : -1}
                      aria-selected={isSelected}
                      style={{
                        position: "absolute",
                        top: 0,
                        left: 0,
                        width: "100%",
                        transform: `translateY(${virtualRow.start}px)`,
                        gridTemplateColumns: LOG_COLUMNS,
                      }}
                      onClick={() => selectRecord(log)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          selectRecord(log);
                        }
                      }}
                      className={cn(
                        "grid items-center gap-3 px-4 py-2.5 text-xs transition-colors outline-none focus-visible:bg-kumo-tint/70 hover:bg-kumo-tint/60",
                        log.id ? "cursor-pointer" : "",
                        isNew ? "animate-flash-new" : "",
                        isSelected
                          ? "bg-kumo-tint hover:bg-kumo-tint"
                          : failed
                            ? "bg-kumo-danger-tint"
                            : "",
                      )}
                      title={log.id ? "Inspect this request" : "No record ID captured"}
                    >
                      <div className="flex min-w-0 items-center gap-1.5 font-mono whitespace-nowrap text-kumo-subtle">
                        <span
                          aria-hidden
                          className={cn(
                            "size-1.5 shrink-0 rounded-full",
                            failed ? "bg-kumo-danger" : "bg-kumo-success",
                          )}
                        />
                        {formatTime(log.ts)}
                      </div>
                      <div className="flex min-w-0 items-center gap-1.5">
                        <span className="truncate font-medium text-kumo-default">{log.model}</span>
                        {log.billing === "subscription" ? (
                          <Badge variant="outline" className="shrink-0 px-1 py-0 text-[10px]">
                            sub
                          </Badge>
                        ) : null}
                      </div>
                      <div className="flex min-w-0 items-center gap-1.5">
                        <ProviderIdentity
                          provider={log.provider}
                          size="size-4"
                          nameClassName="text-kumo-subtle"
                        />
                      </div>
                      <div className="min-w-0 truncate">
                        <Badge
                          variant={
                            log.phase === "plan"
                              ? "primary"
                              : log.phase === "execute"
                                ? "secondary"
                                : "outline"
                          }
                          className="text-[10px]"
                        >
                          {log.phase ?? "-"}
                        </Badge>
                      </div>
                      <div className="min-w-0 truncate">
                        {log.effort ? (
                          <span title={log.effortNote ?? undefined}>
                            <Badge variant="outline" className="text-[10px]">
                              {log.effort}
                            </Badge>
                          </span>
                        ) : (
                          <span className="text-kumo-subtle">default</span>
                        )}
                      </div>
                      <div
                        className={cn(
                          "flex min-w-0 items-center gap-1.5 font-mono font-medium",
                          failed ? "text-kumo-danger" : "text-kumo-subtle",
                        )}
                      >
                        <span>{log.status}</span>
                        {log.tries && log.tries.length > 1 ? (
                          <span className="inline-flex shrink-0 items-center gap-0.5">
                            {log.tries.map((attempt, index) => (
                              <span
                                key={`${attempt.provider}-${index}`}
                                title={`${attempt.provider}/${attempt.model} · ${attempt.cause}${
                                  attempt.fail ? ` · ${attempt.fail}` : ""
                                }${attempt.status !== undefined ? ` · ${attempt.status}` : ""}`}
                                className={cn(
                                  "size-1.5 shrink-0 rounded-full",
                                  attempt.fail ? "bg-kumo-danger" : "bg-kumo-success",
                                )}
                              />
                            ))}
                          </span>
                        ) : null}
                      </div>
                      <div className="flex min-w-0 items-center gap-1.5">
                        {coverage === null ? (
                          <span className="text-kumo-subtle" title={cacheCoverageTitle(log)}>
                            —
                          </span>
                        ) : (
                          <>
                            <span className="h-1 w-6 shrink-0 overflow-hidden rounded-full bg-kumo-fill">
                              <span
                                className={cn(
                                  "block h-full rounded-full",
                                  coverage >= 0.5
                                    ? "bg-kumo-success"
                                    : coverage >= 0.1
                                      ? "bg-kumo-warning"
                                      : "bg-kumo-interact/60",
                                )}
                                style={{ width: `${Math.min(100, coverage * 100)}%` }}
                              />
                            </span>
                            <span
                              className={cn("font-mono tabular-nums", cacheCoverageTone(coverage))}
                              title={cacheCoverageTitle(log)}
                            >
                              {formatCacheCoverage(coverage)}
                            </span>
                          </>
                        )}
                      </div>
                      <div className="truncate font-mono text-kumo-subtle">
                        {log.costUsd === null ? "—" : money(log.costUsd)}
                      </div>
                      <div className="truncate font-mono text-kumo-subtle">
                        {log.latencyMs}ms
                      </div>
                      <div className="truncate text-right text-kumo-subtle">
                        {log.id ? (
                          <Button
                            type="button"
                            variant="ghost"
                            size="xs"
                            className="text-xs"
                            title="Inspect this request"
                            onClick={(event) => {
                              event.stopPropagation();
                              selectRecord(log);
                            }}
                          >
                            Details
                          </Button>
                        ) : (
                          "no id"
                        )}
                      </div>
                    </div>
                  );
                })}
              </div>
            )}

            {loadingMore ? (
              <div className="flex flex-col gap-0 border-t border-kumo-hairline bg-kumo-tint/20 px-4 py-2">
                {Array.from({ length: 3 }, (_, index) => (
                  <div
                    key={index}
                    className="grid items-center gap-3 py-1.5"
                    style={{ gridTemplateColumns: LOG_COLUMNS }}
                  >
                    <SkeletonLine blockHeight={12} className="w-10" />
                    <SkeletonLine blockHeight={12} className="w-[80%]" />
                    <SkeletonLine blockHeight={12} className="w-16" />
                    <SkeletonLine blockHeight={12} className="w-full [grid-column:4/-1]" />
                  </div>
                ))}
              </div>
            ) : nextBefore === null && logs.length > 0 ? (
              <div className="border-t border-kumo-hairline bg-kumo-tint/10 py-2.5 text-center text-xs text-kumo-subtle">
                Beginning of ledger reached ({logs.length} records)
              </div>
            ) : null}
          </div>
        </LayerCard>
      </div>

      <LayerDialog.Root
        open={Boolean(selectedId)}
        onOpenChange={(open) => {
          if (!open) setSelectedId(null);
        }}
      >
        <LayerDialog.Content size="base" verticalAlign="top">
          <LayerDialog.Title>Request detail</LayerDialog.Title>
          <LayerDialog.Body>
            {selectedId ? (
              <LogDetailView
                id={selectedId}
                variant="panel"
                onClose={() => setSelectedId(null)}
              />
            ) : null}
          </LayerDialog.Body>
        </LayerDialog.Content>
      </LayerDialog.Root>
    </div>
  );
}
