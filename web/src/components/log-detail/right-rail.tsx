import { useState } from "react";
import { Link } from "react-router";

import { CopyButton } from "@/components/log-detail/copy-button";
import { SessionStrip } from "@/components/log-detail/session-strip";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { LogDetailResponse, LogRecord } from "@/lib/api";
import { cn, money } from "@/lib/utils";

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex min-w-0 items-start justify-between gap-3 py-1 text-xs">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className="min-w-0 flex-1 break-words text-right">{children}</span>
    </div>
  );
}

function RailCard({
  title,
  children,
  action,
}: {
  title: string;
  children: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <Card className="min-w-0 overflow-hidden">
      <CardHeader className="flex-row items-center justify-between gap-2 p-4 pb-2">
        <CardTitle>{title}</CardTitle>
        {action}
      </CardHeader>
      <CardContent className="p-4 pt-0">{children}</CardContent>
    </Card>
  );
}

function Identity({ record }: { record: LogRecord }) {
  return (
    <div className="flex min-w-0 flex-col">
      <Row label="time">{new Date(record.ts).toLocaleString()}</Row>
      <Row label="record">
        {record.id ? (
          <span className="inline-flex min-w-0 items-center gap-1">
            <span className="min-w-0 truncate font-mono">{record.id}</span>
            <CopyButton text={record.id} label="Copy record id" />
          </span>
        ) : (
          "—"
        )}
      </Row>
      <Row label="session">
        {record.session ? (
          <span className="inline-flex min-w-0 items-center gap-1">
            <span className="min-w-0 truncate font-mono">{record.session}</span>
            <CopyButton text={record.session} label="Copy session id" />
          </span>
        ) : (
          "—"
        )}
      </Row>
      <Row label="key">{record.keyName ?? record.keyId ?? "—"}</Row>
      <Row label="path">{record.path || "—"}</Row>
      {record.requestId ? (
        <Row label="parent">
          <Link
            to={`/logs/${record.requestId}`}
            className="text-muted-foreground underline underline-offset-4"
          >
            open parent request
          </Link>
        </Row>
      ) : null}
    </div>
  );
}

function TokensCost({ record }: { record: LogRecord }) {
  return (
    <div className="flex min-w-0 flex-col">
      <Row label="in">{record.promptTokens}</Row>
      <Row label="out">{record.completionTokens}</Row>
      <Row label="cached read">{record.cacheReadTokens}</Row>
      <Row label="cached write">{record.cacheWriteTokens}</Row>
      {record.savedTokens ? <Row label="saved">~{record.savedTokens}</Row> : null}
      <Row label="cost">{record.costUsd === null ? "—" : money(record.costUsd)}</Row>
      <Row label="billing">{record.billing ?? "pay-as-you-go"}</Row>
    </div>
  );
}

/**
 * The right rail: identity, tokens and cost, the session's other turns, and the handoff
 * actions. Every section is independent so one slow load never blocks the page.
 *
 * `layout="panel"` drops the fixed `lg:w-72` width so the rail fills the narrow panel
 * column instead of forcing a viewport breakpoint that does not fit the panel.
 */
export function RightRail({
  detail,
  onCopyContext,
  copied,
  copyError,
  layout = "page",
}: {
  detail: LogDetailResponse;
  onCopyContext: () => void;
  copied: boolean;
  copyError: string;
  layout?: "page" | "panel";
}) {
  const { record } = detail;
  const [idCopied, setIdCopied] = useState(false);

  async function copyId(value: string): Promise<void> {
    if (!value) return;
    try {
      await navigator.clipboard.writeText(value);
      setIdCopied(true);
      setTimeout(() => setIdCopied(false), 1_500);
    } catch {
      // The identity row already offers the same copy affordance.
    }
  }

  return (
    <aside className={cn("flex min-w-0 flex-col gap-4", layout === "panel" ? "" : "lg:w-72")}>
      <RailCard title="Identity">
        <Identity record={record} />
      </RailCard>

      <RailCard title="Tokens & cost">
        <TokensCost record={record} />
      </RailCard>

      <SessionStrip session={record.session} currentId={record.id} />

      <RailCard title="Actions">
        <div className="flex min-w-0 flex-col gap-2">
          <Button size="sm" className="w-full" onClick={onCopyContext}>
            {copied ? "Copied" : "Copy context"}
          </Button>
          {copyError ? <span className="text-xs text-destructive">{copyError}</span> : null}
          {record.id ? (
            <Button
              variant="outline"
              size="sm"
              className="w-full"
              onClick={() => void copyId(record.id ?? "")}
            >
              {idCopied ? "Copied" : "Copy record id"}
            </Button>
          ) : null}
          {record.requestId ? (
            <Button
              variant="outline"
              size="sm"
              className="w-full"
              render={<Link to={`/logs/${record.requestId}`}>Open parent request</Link>}
            />
          ) : null}
        </div>
      </RailCard>
    </aside>
  );
}
