import {
  Banner,
  Button,
  LayerCard,
  Select,
  SkeletonLine,
  Table,
  Tabs,
  Text,
} from "@cloudflare/kumo";
import { useCallback, useEffect, useState } from "react";
import { Link, useLocation } from "react-router";

import { RequestsChart, SpendChart, TokensChart } from "@/components/activity-charts";
import { ActivitySkeleton } from "@/components/page-skeletons";
import { api, type ActivityReportView, type ActivityTimeRangeView, type KeyView } from "@/lib/api";
import { money } from "@/lib/utils";

function formatNumber(n: number): string {
  return n.toLocaleString();
}

function formatCompactNumber(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`;
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return n.toLocaleString();
}

export function ActivitySection() {
  const { hash } = useLocation();
  const [range, setRange] = useState<ActivityTimeRangeView>("30d");
  const [keyId, setKeyId] = useState<string>("all");
  const [keysList, setKeysList] = useState<KeyView[]>([]);
  const [report, setReport] = useState<ActivityReportView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [chartTab, setChartTab] = useState<"spend" | "tokens" | "requests">("spend");

  const keyItems: Record<string, string> = {
    all: "All keys",
    ...Object.fromEntries(keysList.map((k) => [k.id, `${k.name} (${k.prefix}…)`])),
  };

  const loadData = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const [stateRes, reportRes] = await Promise.all([
        api.state(),
        api.activity({ range, keyId: keyId === "all" ? undefined : keyId }),
      ]);
      setKeysList(stateRes.keys);
      setReport(reportRes);
    } catch (cause) {
      setError(String(cause));
    } finally {
      setLoading(false);
    }
  }, [range, keyId]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  useEffect(() => {
    if (hash !== "#activity" || !report) return;
    // The section mounts after its first report is loaded on legacy /activity visits.
    document.getElementById("activity")?.scrollIntoView();
  }, [hash, Boolean(report)]);

  if (loading && !report) {
    return <ActivitySkeleton />;
  }

  return (
    <section
      id="activity"
      aria-labelledby="activity-heading"
      className="flex flex-col gap-6 scroll-mt-24"
    >
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex flex-col gap-1">
          <p className="text-xs font-semibold tracking-[0.16em] text-kumo-brand uppercase">
            Usage trends
          </p>
          <Text variant="heading" size="lg" as="h2" id="activity-heading">
            Activity
          </Text>
          <Text variant="secondary" size="sm">
            Spend, token usage, and request volume trends across your API keys.
          </Text>
        </div>

        {/* Filters */}
        <div className="flex flex-wrap items-center gap-3">
          <div className="w-48">
            <Select
              aria-label="API key"
              value={keyId}
              onValueChange={(val) => setKeyId(String(val))}
              items={keyItems}
            />
          </div>

          <div className="w-36">
            <Select
              aria-label="Time range"
              value={range}
              onValueChange={(val) => setRange(val as ActivityTimeRangeView)}
              items={{
                "24h": "Last 24 hours",
                "7d": "Last 7 days",
                "30d": "Last 30 days",
                all: "All time",
              }}
            />
          </div>

          <Button variant="outline" size="sm" onClick={() => void loadData()} disabled={loading}>
            Refresh
          </Button>
        </div>
      </div>

      {error ? (
        <Banner variant="error" title="Could not load activity" description={error} size="sm" />
      ) : null}

      {/* Totals for the selected time range and API key, distinct from the all-time overview. */}
      {report ? (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <LayerCard className="p-4">
            <p className="text-xs uppercase tracking-wider text-kumo-subtle">Total spend</p>
            <p className="mt-1 font-mono text-2xl font-semibold text-kumo-default">
              {money(report.summary.totalSpendUsd)}
            </p>
            <p className="mt-2 text-xs text-kumo-subtle">
              <span className="font-medium text-kumo-default">
                {money(report.summary.apiSpendUsd)}
              </span>{" "}
              API spend ·{" "}
              <span className="font-medium text-kumo-default">
                {money(report.summary.subscriptionValueUsd)}
              </span>{" "}
              sub value
            </p>
          </LayerCard>

          <LayerCard className="p-4">
            <p className="text-xs uppercase tracking-wider text-kumo-subtle">Total tokens</p>
            <p className="mt-1 font-mono text-2xl font-semibold text-kumo-default">
              {formatCompactNumber(report.summary.totalTokens)}
            </p>
            <p className="mt-2 text-xs text-kumo-subtle">
              {formatCompactNumber(report.summary.promptTokens)} prompt ·{" "}
              {formatCompactNumber(report.summary.completionTokens)} out ·{" "}
              {formatCompactNumber(report.summary.cacheReadTokens)} cache
            </p>
          </LayerCard>

          <LayerCard className="p-4">
            <p className="text-xs uppercase tracking-wider text-kumo-subtle">Total requests</p>
            <p className="mt-1 font-mono text-2xl font-semibold text-kumo-default">
              {formatNumber(report.summary.totalRequests)}
            </p>
            <p className="mt-2 text-xs text-kumo-subtle">
              {formatNumber(report.summary.successfulRequests)} ok ·{" "}
              <span
                className={
                  report.summary.errorRequests > 0 ? "font-medium text-kumo-danger" : undefined
                }
              >
                {formatNumber(report.summary.errorRequests)} err
              </span>{" "}
              · avg {report.summary.avgLatencyMs}ms
            </p>
          </LayerCard>
        </div>
      ) : null}

      {/* Top lists */}
      {report ? (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <LayerCard>
            <LayerCard.Secondary>
              <div className="flex flex-col gap-1">
                <Text variant="heading" as="h3">
                  Top models
                </Text>
                <Text variant="secondary" size="xs">
                  By estimated spend in this window
                </Text>
              </div>
            </LayerCard.Secondary>
            <LayerCard.Primary className="flex flex-col gap-2">
              {report.models.slice(0, 5).map((m, index) => (
                <div key={m.model} className="flex items-center justify-between gap-3 text-sm">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="w-4 shrink-0 text-xs text-kumo-subtle">{index + 1}</span>
                    <span
                      className="truncate font-medium text-kumo-default"
                      title={m.variants?.join(", ") ?? m.model}
                    >
                      {m.label ?? m.model}
                    </span>
                    {(m.variants?.length ?? 0) > 1 ? (
                      <span className="shrink-0 rounded-full bg-kumo-fill px-1.5 py-0.5 text-xs text-kumo-subtle">
                        ×{m.variants!.length}
                      </span>
                    ) : null}
                  </div>
                  <div className="shrink-0 text-right font-mono text-xs text-kumo-subtle">
                    {formatCompactNumber(m.totalTokens)} tok ·{" "}
                    {money(m.spendUsd + m.subscriptionUsd)}
                  </div>
                </div>
              ))}
              {report.models.length === 0 ? (
                <p className="py-4 text-center text-xs text-kumo-subtle">No model traffic yet.</p>
              ) : null}
            </LayerCard.Primary>
          </LayerCard>
          <LayerCard>
            <LayerCard.Secondary>
              <div className="flex flex-col gap-1">
                <Text variant="heading" as="h3">
                  Top API keys
                </Text>
                <Text variant="secondary" size="xs">
                  Attributed spend across keys
                </Text>
              </div>
            </LayerCard.Secondary>
            <LayerCard.Primary className="flex flex-col gap-2">
              {report.keys.slice(0, 5).map((k, index) => (
                <button
                  key={k.id}
                  type="button"
                  className="flex items-center justify-between gap-3 text-left text-sm hover:opacity-80"
                  onClick={() => setKeyId(k.id)}
                >
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="w-4 shrink-0 text-xs text-kumo-subtle">{index + 1}</span>
                    <span className="truncate font-medium text-kumo-default">{k.name}</span>
                  </div>
                  <div className="shrink-0 text-right font-mono text-xs text-kumo-subtle">
                    {formatNumber(k.requests)} req · {money(k.spendUsd)}
                  </div>
                </button>
              ))}
              {report.keys.length === 0 ? (
                <p className="py-4 text-center text-xs text-kumo-subtle">No key traffic yet.</p>
              ) : null}
            </LayerCard.Primary>
          </LayerCard>
        </div>
      ) : null}

      {/* Chart Section with Tabs */}
      <div className="flex flex-col gap-3">
        <div className="flex items-center justify-between">
          <Tabs
            variant="segmented"
            size="sm"
            tabs={[
              { value: "spend", label: "Spend" },
              { value: "tokens", label: "Tokens" },
              { value: "requests", label: "Requests" },
            ]}
            value={chartTab}
            onValueChange={(value) => setChartTab(value as "spend" | "tokens" | "requests")}
          />

          <Link
            to="/logs"
            className="text-xs text-kumo-subtle hover:text-kumo-default hover:underline"
          >
            Inspect generation logs →
          </Link>
        </div>

        {report ? (
          chartTab === "spend" ? (
            <SpendChart series={report.series} loading={loading} />
          ) : chartTab === "tokens" ? (
            <TokensChart series={report.series} loading={loading} />
          ) : (
            <RequestsChart series={report.series} loading={loading} />
          )
        ) : (
          <LayerCard className="p-4">
            <div className="flex flex-col gap-2">
              <SkeletonLine blockHeight={16} minWidth={30} maxWidth={40} />
              <SkeletonLine blockHeight={12} minWidth={45} maxWidth={60} />
              <SkeletonLine blockHeight={176} minWidth={100} maxWidth={100} className="mt-2" />
            </div>
          </LayerCard>
        )}
      </div>

      {/* Model Breakdown */}
      <LayerCard>
        <LayerCard.Secondary>
          <div className="flex flex-col gap-1">
            <Text variant="heading" as="h3">
              Model breakdown
            </Text>
            <Text variant="secondary" size="xs">
              Tokens and cost by model for the selected period
            </Text>
          </div>
        </LayerCard.Secondary>
        <LayerCard.Primary>
          {report && report.models.length > 0 ? (
            <div className="overflow-x-auto">
              <Table>
                <Table.Header>
                  <Table.Row>
                    <Table.Head>Model</Table.Head>
                    <Table.Head className="text-right">Requests</Table.Head>
                    <Table.Head className="text-right">Prompt</Table.Head>
                    <Table.Head className="text-right">Completion</Table.Head>
                    <Table.Head className="text-right">Cache read</Table.Head>
                    <Table.Head className="text-right">Total tokens</Table.Head>
                    <Table.Head className="text-right">API spend</Table.Head>
                    <Table.Head className="text-right">Sub value</Table.Head>
                    <Table.Head className="text-right w-28">Share</Table.Head>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {report.models.map((m) => (
                    <Table.Row key={m.model}>
                      <Table.Cell className="font-medium">
                        <div className="flex min-w-0 flex-col">
                          <span className="truncate" title={m.variants?.join(", ") ?? m.model}>
                            {m.label ?? m.model}
                          </span>
                          {m.label && m.label !== m.model ? (
                            <span
                              className="truncate font-mono text-xs text-kumo-subtle"
                              title={m.variants?.join(", ") ?? m.model}
                            >
                              {m.model}
                              {(m.variants?.length ?? 0) > 1 ? ` · ${m.variants!.length} ids` : ""}
                            </span>
                          ) : (m.variants?.length ?? 0) > 1 ? (
                            <span
                              className="truncate font-mono text-xs text-kumo-subtle"
                              title={m.variants?.join(", ")}
                            >
                              {m.variants!.length} ids
                            </span>
                          ) : null}
                        </div>
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs">
                        {formatNumber(m.requests)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs text-kumo-subtle">
                        {formatCompactNumber(m.promptTokens)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs text-kumo-subtle">
                        {formatCompactNumber(m.completionTokens)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs text-kumo-subtle">
                        {formatCompactNumber(m.cacheReadTokens)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs font-semibold">
                        {formatCompactNumber(m.totalTokens)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs font-medium">
                        {money(m.spendUsd)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs text-kumo-subtle">
                        {m.subscriptionUsd > 0 ? money(m.subscriptionUsd) : "—"}
                      </Table.Cell>
                      <Table.Cell className="text-right">
                        <div className="flex items-center justify-end gap-2">
                          <div className="h-1.5 w-14 rounded-full bg-kumo-fill overflow-hidden">
                            <div
                              className="h-full bg-kumo-brand rounded-full"
                              style={{ width: `${Math.min(100, m.percentSpend)}%` }}
                            />
                          </div>
                          <span className="font-mono text-xs w-10 text-right">
                            {m.percentSpend}%
                          </span>
                        </div>
                      </Table.Cell>
                    </Table.Row>
                  ))}
                </Table.Body>
              </Table>
            </div>
          ) : (
            <p className="text-sm text-kumo-subtle py-6 text-center">
              No model usage recorded in this time range.
            </p>
          )}
        </LayerCard.Primary>
      </LayerCard>

      {/* Key Breakdown (when All keys is selected) */}
      {keyId === "all" && report && report.keys.length > 0 ? (
        <LayerCard>
          <LayerCard.Secondary>
            <div className="flex flex-col gap-1">
              <Text variant="heading" as="h3">
                Key breakdown
              </Text>
              <Text variant="secondary" size="xs">
                Spend and activity attributed to each API key
              </Text>
            </div>
          </LayerCard.Secondary>
          <LayerCard.Primary>
            <div className="overflow-x-auto">
              <Table>
                <Table.Header>
                  <Table.Row>
                    <Table.Head>Key name</Table.Head>
                    <Table.Head className="text-right">Requests</Table.Head>
                    <Table.Head className="text-right">API spend</Table.Head>
                    <Table.Head className="text-right">Subscription value</Table.Head>
                    <Table.Head className="text-right">Filter</Table.Head>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {report.keys.map((k) => (
                    <Table.Row key={k.id}>
                      <Table.Cell className="font-medium">{k.name}</Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs">
                        {formatNumber(k.requests)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs font-semibold">
                        {money(k.spendUsd)}
                      </Table.Cell>
                      <Table.Cell className="text-right font-mono text-xs text-kumo-subtle">
                        {k.subscriptionUsd > 0 ? money(k.subscriptionUsd) : "—"}
                      </Table.Cell>
                      <Table.Cell className="text-right">
                        <Button variant="ghost" size="xs" onClick={() => setKeyId(k.id)}>
                          View Key →
                        </Button>
                      </Table.Cell>
                    </Table.Row>
                  ))}
                </Table.Body>
              </Table>
            </div>
          </LayerCard.Primary>
        </LayerCard>
      ) : null}
    </section>
  );
}
