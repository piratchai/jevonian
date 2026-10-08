import { ListFilterIcon } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";

import { RequestsChart } from "@/components/activity-charts";
import { LogDetailView } from "@/components/log-detail/log-detail-view";
import { LOG_COLUMNS } from "@/components/logs/columns";
import { CollapsedRail, FilterRail } from "@/components/logs/filter-rail";
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
import { useMediaQuery } from "@/components/logs/use-media-query";
import { LogsTableSkeleton } from "@/components/page-skeletons";
import { ProviderIdentity } from "@/components/provider-identity";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent } from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
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
  // The log table is the point of this page, so the filter rail starts
  // collapsed as a thin strip. Expanding it is one click.
  const [railOpen, setRailOpen] = useState(false);
  /** Mobile-only rail drawer; the md+ rail is controlled by `railOpen`. */
  const [railDrawerOpen, setRailDrawerOpen] = useState(false);
  /** Selected record id; the inline panel (xl) or drawer (below xl) reads it. */
  const [selectedId, setSelectedId] = useState<string | null>(null);
  /** Tailwind's xl breakpoint; the drawer only opens below it. */
  const isXl = useMediaQuery("(min-width: 80rem)");

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
    <div className="flex h-[calc(100svh-6rem)] gap-3 md:h-[calc(100svh-3rem)]">
      {railOpen ? (
        <FilterRail
          filters={filters}
          facets={facets}
          facetsStale={facetsStale}
          onToggle={toggleFilter}
          onClear={clearFilters}
          onCollapse={() => setRailOpen(false)}
          className="hidden w-60 shrink-0 rounded-xl border bg-card md:flex"
        />
      ) : (
        <CollapsedRail
          filters={filters}
          onExpand={() => setRailOpen(true)}
          className="hidden w-10 rounded-xl border bg-card md:flex"
        />
      )}

      {/* Below md the rail is a left drawer; from md up it is the strip above. */}
      <Sheet open={railDrawerOpen} onOpenChange={setRailDrawerOpen}>
        <SheetContent side="left" className="w-72 gap-0 p-0 md:hidden">
          <FilterRail
            filters={filters}
            facets={facets}
            facetsStale={facetsStale}
            onToggle={toggleFilter}
            onClear={clearFilters}
            className="h-full"
          />
        </SheetContent>
      </Sheet>

      <div className="flex min-w-0 flex-1 flex-col gap-3">
        <div className="flex shrink-0 items-center justify-between gap-4">
          <div className="flex min-w-0 items-center gap-2">
            <Button
              variant="outline"
              size="icon-sm"
              className="md:hidden"
              aria-label="Toggle filters"
              title="Toggle filters"
              onClick={() => setRailDrawerOpen(true)}
            >
              <ListFilterIcon />
            </Button>
            <h1 className="shrink-0 text-lg font-semibold tracking-tight">Logs</h1>
          </div>
          <div className="flex min-w-0 flex-nowrap items-center justify-end gap-2">
            <Input
              value={searchDraft}
              onChange={(event) => setSearchDraft(event.target.value)}
              placeholder="Search…"
              autoComplete="off"
              className="h-9 w-40 shrink-0"
              aria-label="Search logs"
            />

            <Button
              variant="outline"
              className="h-9 shrink-0 gap-2 px-3"
              aria-pressed={live}
              title={live ? "Pause live stream" : "Resume live stream"}
              onClick={() => setLive((current) => !current)}
            >
              <span
                className={cn(
                  "size-2 rounded-full",
                  streamStatus === "live"
                    ? "animate-pulse bg-emerald-500"
                    : streamStatus === "connecting"
                      ? "animate-pulse bg-amber-500"
                      : "bg-muted-foreground/50",
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

            <Button variant="outline" className="h-9 shrink-0" onClick={() => void loadInitial()}>
              Refresh
            </Button>
          </div>
        </div>

        {error ? <p className="shrink-0 text-xs text-destructive">{error}</p> : null}

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

        <Card className="flex min-h-0 flex-1 flex-col overflow-hidden border">
          <div
            className="grid shrink-0 items-center gap-3 border-b bg-muted/40 px-4 py-2.5 text-xs font-semibold tracking-wider text-muted-foreground uppercase"
            style={{ gridTemplateColumns: LOG_COLUMNS }}
          >
            <div className="truncate">Time</div>
            <div className="truncate">Model</div>
            <div className="truncate">Provider</div>
            <div className="truncate">Phase</div>
            <div className="truncate">Effort</div>
            <div className="truncate">Status</div>
            <div
              className="truncate"
              title="Share of input tokens served from the prompt cache"
            >
              Cache
            </div>
            <div className="truncate">Cost</div>
            <div className="truncate">Latency</div>
            <div className="truncate text-right">Details</div>
          </div>

          <div
            ref={scrollContainerRef}
            className="relative min-h-0 flex-1 divide-y divide-border/40 overflow-x-hidden overflow-y-auto"
          >
            {loadingInitial ? (
              <LogsTableSkeleton rows={12} />
            ) : logs.length === 0 ? (
              <div className="flex h-40 items-center justify-center text-sm text-muted-foreground">
                {filtered
                  ? "No requests match these filters."
                  : "No traffic yet — proxied requests will stream in here."}
              </div>
            ) : (
              <div style={{ height: `${totalHeight}px`, width: "100%", position: "relative" }}>
                {virtualItems.map((virtualRow) => {
                  const log = logs[virtualRow.index];
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
                        "grid items-center gap-3 px-4 py-2.5 text-xs transition-colors outline-none focus-visible:bg-muted/70 hover:bg-muted/60",
                        log.id ? "cursor-pointer" : "",
                        isNew ? "animate-flash-new" : "",
                        isSelected ? "bg-muted hover:bg-muted" : failed ? "bg-destructive/5" : "",
                      )}
                      title={log.id ? "Inspect this request" : "No record ID captured"}
                    >
                      <div className="flex min-w-0 items-center gap-1.5 font-mono whitespace-nowrap text-muted-foreground">
                        <span
                          aria-hidden
                          className={cn(
                            "size-1.5 shrink-0 rounded-full",
                            failed ? "bg-destructive" : "bg-emerald-500",
                          )}
                        />
                        {formatTime(log.ts)}
                      </div>
                      <div className="flex min-w-0 items-center gap-1.5">
                        <span className="truncate font-medium text-foreground">{log.model}</span>
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
                          nameClassName="text-muted-foreground"
                        />
                      </div>
                      <div className="min-w-0 truncate">
                        <Badge
                          variant={
                            log.phase === "plan"
                              ? "default"
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
                          <Badge
                            variant="outline"
                            title={log.effortNote ?? undefined}
                            className="text-[10px]"
                          >
                            {log.effort}
                          </Badge>
                        ) : (
                          <span className="text-muted-foreground">default</span>
                        )}
                      </div>
                      <div
                        className={cn(
                          "flex min-w-0 items-center gap-1.5 font-mono font-medium",
                          failed ? "text-destructive" : "text-muted-foreground",
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
                                  attempt.fail ? "bg-destructive" : "bg-emerald-500",
                                )}
                              />
                            ))}
                          </span>
                        ) : null}
                      </div>
                      <div className="flex min-w-0 items-center gap-1.5">
                        {coverage === null ? (
                          <span className="text-muted-foreground" title={cacheCoverageTitle(log)}>
                            —
                          </span>
                        ) : (
                          <>
                            <span className="h-1 w-6 shrink-0 overflow-hidden rounded-full bg-muted">
                              <span
                                className={cn(
                                  "block h-full rounded-full",
                                  coverage >= 0.5
                                    ? "bg-emerald-500"
                                    : coverage >= 0.1
                                      ? "bg-amber-500"
                                      : "bg-muted-foreground/40",
                                )}
                                style={{ width: `${Math.min(100, coverage * 100)}%` }}
                              />
                            </span>
                            <span
                              className={cn(
                                "font-mono tabular-nums",
                                cacheCoverageTone(coverage),
                              )}
                              title={cacheCoverageTitle(log)}
                            >
                              {formatCacheCoverage(coverage)}
                            </span>
                          </>
                        )}
                      </div>
                      <div className="truncate font-mono text-muted-foreground">
                        {log.costUsd === null ? "—" : money(log.costUsd)}
                      </div>
                      <div className="truncate font-mono text-muted-foreground">
                        {log.latencyMs}ms
                      </div>
                      <div className="truncate text-right text-muted-foreground">
                        {log.id ? (
                          <span className="inline-flex items-center gap-1.5">
                            <span>open →</span>
                            <Link
                              to={`/logs/${log.id}`}
                              className="rounded-sm underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-ring"
                              title="Open the full detail page"
                              onClick={(event) => event.stopPropagation()}
                            >
                              page
                            </Link>
                          </span>
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
              <div className="flex flex-col gap-0 border-t bg-muted/20 px-4 py-2">
                {Array.from({ length: 3 }, (_, index) => (
                  <div
                    key={index}
                    className="grid items-center gap-3 py-1.5"
                    style={{ gridTemplateColumns: LOG_COLUMNS }}
                  >
                    <Skeleton className="h-3 w-10" />
                    <Skeleton className="h-3 w-[80%]" />
                    <Skeleton className="h-3 w-16" />
                    <Skeleton className="h-3 w-full" style={{ gridColumn: "4 / -1" }} />
                  </div>
                ))}
              </div>
            ) : nextBefore === null && logs.length > 0 ? (
              <div className="border-t bg-muted/10 py-2.5 text-center text-xs text-muted-foreground">
                Beginning of ledger reached ({logs.length} records)
              </div>
            ) : null}
          </div>
        </Card>
      </div>

      {/* Inline panel on xl and up; below xl the same view lives in the Sheet. */}
      <aside className="hidden min-h-0 w-[30rem] shrink-0 overflow-hidden rounded-xl border bg-card xl:block">
        {selectedId ? (
          <LogDetailView id={selectedId} variant="panel" onClose={() => setSelectedId(null)} />
        ) : (
          <div className="flex h-full items-center justify-center p-6 text-center text-sm text-muted-foreground">
            Select a request to inspect it here.
          </div>
        )}
      </aside>

      <Sheet
        open={Boolean(selectedId) && !isXl}
        onOpenChange={(open) => {
          if (!open) setSelectedId(null);
        }}
      >
        <SheetContent side="right" showCloseButton={false} className="w-full gap-0 p-0 sm:max-w-lg">
          {selectedId ? (
            <LogDetailView id={selectedId} variant="panel" onClose={() => setSelectedId(null)} />
          ) : null}
        </SheetContent>
      </Sheet>
    </div>
  );
}
