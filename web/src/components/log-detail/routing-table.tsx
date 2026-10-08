import { Badge, LayerCard, Text } from "@cloudflare/kumo";
import { Link } from "react-router";

import { RawBlock, Json } from "@/components/log-detail/raw";
import type { LogDetailResponse, LogRecord } from "@/lib/api";
import { cn } from "@/lib/utils";

import {
  asRecord,
  brainVerdictState,
  formatPercent,
  probabilityRanking,
  routingRows,
  stateSummary,
  textOf,
  verdictSummary,
  type RoutingRow,
} from "./helpers";

function KeyValues({ record }: { record: LogRecord }) {
  const rows: Array<{ label: string; value: string; note?: string }> = [
    { label: "phase", value: record.phase ?? "-" },
    {
      label: "thinking effort",
      value: record.effort ?? "default",
      note: record.effortNote,
    },
    { label: "reason", value: record.reason ?? "-" },
    {
      label: "brain",
      value: [record.brain ?? "-", record.brainChannel ?? ""].filter(Boolean).join(" · "),
    },
  ];
  if (record.retries) {
    rows.push({ label: "network retries", value: `${record.retries} (transient, recovered)` });
  }
  if (record.failovers) {
    rows.push({ label: "failovers", value: `${record.failovers} (provider refused, moved on)` });
  }
  return (
    <div className="flex min-w-0 flex-col">
      {rows.map((row) => (
        <div
          key={row.label}
          className="flex min-w-0 items-start justify-between gap-4 border-b border-kumo-hairline py-1.5 text-xs last:border-b-0"
        >
          <span className="shrink-0 text-kumo-subtle">{row.label}</span>
          <span className="min-w-0 flex-1 text-right break-words">
            {row.value}
            {row.note ? <span className="text-kumo-subtle"> · {row.note}</span> : null}
          </span>
        </div>
      ))}
    </div>
  );
}

function ScoreBar({ score }: { score: number }) {
  return (
    <div className="h-1 w-full min-w-8 overflow-hidden rounded-full bg-kumo-fill">
      <div
        className="h-full rounded-full bg-kumo-contrast/40"
        style={{ width: `${Math.min(100, Math.max(0, score * 100))}%` }}
      />
    </div>
  );
}

function RoutingTableRow({ row, max }: { row: RoutingRow; max: number }) {
  const score = row.score;
  const barWidth = score !== undefined && max > 0 ? (score / max) * 100 : 0;
  return (
    <div
      className={cn(
        "grid min-w-0 grid-cols-[auto_1fr_auto] items-center gap-x-3 gap-y-1 border-b border-kumo-hairline py-2 text-xs last:border-b-0",
        row.chosen ? "bg-kumo-success-tint" : "",
      )}
    >
      <span className="flex min-w-0 items-center gap-2">
        {row.chosen ? (
          <Badge variant="secondary" className="px-1 py-0 text-[10px]">
            chosen
          </Badge>
        ) : row.withheld ? (
          <Badge variant="error" className="px-1 py-0 text-[10px]">
            withheld
          </Badge>
        ) : (
          <span className="size-1.5 shrink-0 rounded-full bg-kumo-interact/60" />
        )}
      </span>
      <span className="min-w-0">
        <span className="block break-all font-medium">{row.model}</span>
        {row.provider ? (
          <span className="block break-all text-[11px] text-kumo-subtle">{row.provider}</span>
        ) : null}
        {row.withheld ? (
          <span className="block text-[11px] text-kumo-subtle">
            {row.withheld.reason}: {row.withheld.detail}
          </span>
        ) : null}
      </span>
      <span className="flex w-24 shrink-0 items-center gap-2">
        {row.withheld || score === undefined ? null : (
          <>
            <ScoreBar score={barWidth / 100} />
            <span className="w-10 shrink-0 text-right font-mono text-kumo-subtle">
              {formatPercent(score)}
            </span>
          </>
        )}
      </span>
    </div>
  );
}

function BrainCallDetails({ detail }: { detail: LogDetailResponse }) {
  if (detail.brainCalls.length === 0) return null;
  return (
    <RawBlock
      summary={`Routing brain calls (${detail.brainCalls.length}, fallback order)`}
      className="min-w-0"
    >
      <div className="flex min-w-0 flex-col gap-3">
        {detail.brainCalls.map(({ record: call, body }) => {
          const data = asRecord(body);
          const verdict = asRecord(data?.verdict);
          const state = asRecord(data?.state);
          return (
            <div
              key={call.id ?? call.ts}
              className="flex min-w-0 flex-col gap-2 rounded-md border border-kumo-hairline p-3"
            >
              <div className="flex min-w-0 flex-wrap items-center gap-2 text-xs">
                <Badge
                  variant={call.status === 200 ? "secondary" : "error"}
                  className="text-[10px]"
                >
                  {call.status}
                </Badge>
                <span className="font-medium">{call.provider}</span>
                <span className="min-w-0 break-all text-kumo-subtle">{call.model}</span>
                <span className="ml-auto text-[11px] text-kumo-subtle">
                  {call.latencyMs}ms · {call.promptTokens} in / {call.completionTokens} out
                </span>
              </div>
              <p className="break-words text-xs">
                <span className="text-kumo-subtle">Verdict: </span>
                {verdict ? verdictSummary(verdict) : "No verdict was captured for this call."}
              </p>
              <p className="break-words text-xs text-kumo-subtle">
                {state ? stateSummary(state) : "No brain state was captured for this call."}
              </p>
              <RawBlock summary="Raw state and verdict for this call">
                <div className="flex flex-col gap-3">
                  {data?.state ? <Json value={data.state} /> : null}
                  {data?.verdict ? <Json value={data.verdict} /> : null}
                </div>
              </RawBlock>
              {call.id ? (
                <Link
                  to={`/logs/${call.id}`}
                  className="self-start text-xs text-kumo-subtle underline underline-offset-4"
                >
                  Open this brain call
                </Link>
              ) : null}
            </div>
          );
        })}
      </div>
    </RawBlock>
  );
}

/**
 * The routing decision: a compact table of the chosen model, the brain's ranked
 * alternatives, and every withheld model with its reason.
 */
export function RoutingTable({ detail }: { detail: LogDetailResponse }) {
  const { record } = detail;
  const isBrain = record.kind === "brain";
  const { verdict: rawVerdict, state: rawState } = brainVerdictState(detail);
  const verdict = asRecord(rawVerdict);
  const state = asRecord(rawState);
  const ranking = verdict ? probabilityRanking(verdict.probabilities) : [];
  const brainModel = textOf(verdict?.model);
  const rows = isBrain ? [] : routingRows(record, ranking, brainModel);
  const max = ranking.length > 0 ? ranking[0].score : 1;
  const effortRanking = verdict ? probabilityRanking(verdict.effortProbabilities) : [];
  const noBrain = !isBrain && detail.brainCalls.length === 0;

  return (
    <LayerCard className="min-w-0">
      <LayerCard.Secondary>
        <Text variant="heading">Routing decision</Text>
      </LayerCard.Secondary>
      <LayerCard.Primary className="gap-4">
        <Text variant="secondary" size="sm">
          {isBrain
            ? "Why the router asked the brain, and what it answered."
            : "Why this model, and what the router considered instead."}
        </Text>
        <KeyValues record={record} />

        {isBrain ? (
          <div className="flex min-w-0 flex-col gap-2 text-xs">
            <p className="break-words">
              <span className="text-kumo-subtle">Verdict: </span>
              {verdict ? verdictSummary(verdict) : "No verdict was captured for this call."}
            </p>
            <p className="break-words text-kumo-subtle">
              {state ? stateSummary(state) : "No brain state was captured for this call."}
            </p>
          </div>
        ) : (
          <div className="flex min-w-0 flex-col">
            {rows.map((row, index) => (
              <RoutingTableRow key={`${row.model}-${index}`} row={row} max={max} />
            ))}
          </div>
        )}

        {effortRanking.length > 0 ? (
          <div className="flex min-w-0 flex-col gap-1">
            <span className="text-[11px] tracking-[0.12em] text-kumo-subtle uppercase">
              Effort options
            </span>
            <ul className="list-none space-y-0.5 font-mono text-xs">
              {effortRanking.map(({ option, score }, index) => (
                <li key={option} className={cn("break-all", index === 0 ? "font-medium" : "")}>
                  {formatPercent(score).padStart(4)} {option}
                </li>
              ))}
            </ul>
          </div>
        ) : null}

        {textOf(verdict?.model) && ranking.length === 0 ? (
          <p className="text-xs text-kumo-subtle">
            The brain named a model but returned no probability ranking.
          </p>
        ) : null}

        {noBrain ? (
          <p className="text-xs text-kumo-subtle">
            The router decided without asking the brain for this turn, so there is no ranked
            alternative list.
          </p>
        ) : null}

        <BrainCallDetails detail={detail} />
      </LayerCard.Primary>
    </LayerCard>
  );
}
