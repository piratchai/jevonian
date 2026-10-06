import { XIcon } from "lucide-react";
import { useCallback, useEffect, useState, type JSX } from "react";
import { Link } from "react-router";

import { AttemptTimeline } from "@/components/log-detail/attempt-timeline";
import { Exchange } from "@/components/log-detail/exchange";
import { buildBundle, requestCapture } from "@/components/log-detail/helpers";
import { OutcomeBanner } from "@/components/log-detail/outcome-banner";
import { Json, RawBlock } from "@/components/log-detail/raw";
import { RightRail } from "@/components/log-detail/right-rail";
import { RoutingTable } from "@/components/log-detail/routing-table";
import { LogDetailSkeleton } from "@/components/page-skeletons";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type LogDetailResponse, type LogRecord } from "@/lib/api";
import { cn, formatTime } from "@/lib/utils";

function RawSection({ detail }: { detail: LogDetailResponse }) {
  const { record } = detail;
  const capture = requestCapture(detail);
  const isBrain = record.kind === "brain";
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <h2 className="text-sm font-semibold tracking-tight">Raw</h2>
      <RawBlock summary="Raw record fields" className="min-w-0">
        <Json value={record} />
      </RawBlock>
      {isBrain ? null : (
        <>
          {/* The raw request JSON lives beside the exchange it describes. */}
          <RawBlock summary="Raw response capture" className="min-w-0">
            {capture.response ? (
              <Json value={capture.response} />
            ) : (
              <p className="text-xs text-muted-foreground">
                Response not captured for this record (recorded before response capture, or
                JEVONIAN_CAPTURE_BODIES=0).
              </p>
            )}
          </RawBlock>
        </>
      )}
    </div>
  );
}

/**
 * Fetch one log record and keep it in sync with `id`. A cancel flag makes a response for
 * an earlier id a no-op, so fast clicking between rows never shows the wrong record.
 */
export function useLogDetail(id: string): { detail: LogDetailResponse | null; error: string } {
  const [detail, setDetail] = useState<LogDetailResponse | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError("");
    api
      .logDetail(id)
      .then((next) => {
        if (cancelled) return;
        setDetail(next);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setError(String(cause));
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  return { detail, error };
}

/** The copy-context state, shared by the page and panel layouts. */
function useCopyContext(detail: LogDetailResponse | null): {
  copied: boolean;
  copyError: string;
  copyContext: () => Promise<void>;
} {
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState("");

  const copyContext = useCallback(async (): Promise<void> => {
    if (!detail) return;
    try {
      await navigator.clipboard.writeText(buildBundle(detail));
      setCopied(true);
      setCopyError("");
      setTimeout(() => setCopied(false), 1_500);
    } catch (cause) {
      setCopyError(String(cause));
    }
  }, [detail]);

  return { copied, copyError, copyContext };
}

/** The panel's sticky top row: status dot, model, time, and the two header actions. */
function PanelHeader({
  record,
  id,
  onClose,
}: {
  record: LogRecord;
  id: string;
  onClose?: () => void;
}) {
  const failed = record.status >= 400 || Boolean(record.error);
  return (
    <div className="sticky top-0 z-10 flex min-w-0 shrink-0 items-center gap-3 border-b bg-background px-4 py-3">
      <span
        className={cn(
          "size-2.5 shrink-0 rounded-full",
          failed ? "bg-destructive" : "bg-emerald-500",
        )}
        aria-hidden
      />
      <div className="flex min-w-0 flex-col">
        <span className="min-w-0 truncate text-sm font-medium">{record.model}</span>
        <span className="text-[11px] text-muted-foreground">{formatTime(record.ts)}</span>
      </div>
      <div className="ml-auto flex shrink-0 items-center gap-1">
        <Button
          variant="outline"
          size="xs"
          render={<Link to={`/logs/${id}`}>Open full page</Link>}
        />
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label="Close detail"
          onClick={onClose}
        >
          <XIcon />
        </Button>
      </div>
    </div>
  );
}

/** A compact skeleton sized for the narrow panel. */
function PanelSkeleton() {
  return (
    <div className="flex h-full min-h-0 flex-col" aria-busy="true" aria-label="Loading log detail">
      <div className="flex shrink-0 items-center gap-3 border-b px-4 py-3">
        <Skeleton className="size-2.5 rounded-full" />
        <div className="flex flex-col gap-1">
          <Skeleton className="h-4 w-32" />
          <Skeleton className="h-3 w-16" />
        </div>
        <Skeleton className="ml-auto h-8 w-28" />
      </div>
      <div className="flex flex-col gap-4 p-4">
        <Skeleton className="h-24 w-full rounded-xl" />
        <Skeleton className="h-48 w-full rounded-xl" />
        <Skeleton className="h-32 w-full rounded-xl" />
      </div>
    </div>
  );
}

/**
 * The log detail content, shared by the full `/logs/:id` route and the inline list panel.
 *
 * `variant="page"` keeps the two-column page layout. `variant="panel"` is a single narrow
 * column with a sticky header and its own scroll area, so the parent can give it a fixed
 * height. It owns fetching, loading, error, and the copy-context state in both variants.
 */
export function LogDetailView({
  id,
  variant = "page",
  onClose,
}: {
  id: string;
  /** "page" = full route layout (header + two columns). "panel" = narrow side panel. */
  variant?: "page" | "panel";
  /** Panel only: called by the panel's close button. */
  onClose?: () => void;
}): JSX.Element {
  const { detail, error } = useLogDetail(id);
  const { copied, copyError, copyContext } = useCopyContext(detail);

  if (error) {
    if (variant === "panel") {
      return (
        <div className="flex h-full min-h-0 flex-col items-center justify-center p-6 text-center">
          <p className="text-sm text-destructive">{error}</p>
        </div>
      );
    }
    return <p className="text-sm text-destructive">{error}</p>;
  }

  if (!detail) {
    return variant === "panel" ? <PanelSkeleton /> : <LogDetailSkeleton />;
  }

  const record = detail.record;
  const isBrain = record.kind === "brain";
  const capture = requestCapture(detail);

  if (variant === "panel") {
    return (
      <div className="flex h-full min-h-0 flex-col">
        <PanelHeader record={record} id={id} onClose={onClose} />
        <div className="min-h-0 flex-1 overflow-y-auto">
          <div className="flex min-w-0 flex-col gap-4 p-4">
            <OutcomeBanner record={record} layout="panel" />

            {isBrain ? null : (
              <Exchange
                detail={detail}
                clientRequest={capture.clientRequest}
                response={capture.response}
                layout="stacked"
              />
            )}

            {isBrain ? null : <AttemptTimeline record={record} />}

            <RoutingTable detail={detail} />

            <RightRail
              detail={detail}
              onCopyContext={() => void copyContext()}
              copied={copied}
              copyError={copyError}
              layout="panel"
            />

            <RawSection detail={detail} />
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="flex min-w-0 flex-col gap-6">
      <div className="flex min-w-0 flex-wrap items-center gap-3">
        <Link to="/logs" className="text-sm text-muted-foreground underline underline-offset-4">
          ← Logs
        </Link>
        <h1 className="min-w-0 text-lg font-semibold">
          {isBrain ? "Routing brain call" : "Request"} · {formatTime(record.ts)}
        </h1>
        {record.requestId ? (
          <Link
            to={`/logs/${record.requestId}`}
            className="text-xs text-muted-foreground underline underline-offset-4"
          >
            parent request
          </Link>
        ) : null}
        <span className="ml-auto text-xs text-muted-foreground">
          {record.provider} · {record.model}
        </span>
      </div>

      <div className="flex min-w-0 flex-col gap-6 lg:flex-row lg:items-start">
        <div className="flex min-w-0 flex-1 flex-col gap-6">
          <OutcomeBanner record={record} />

          {isBrain ? null : (
            <Exchange
              detail={detail}
              clientRequest={capture.clientRequest}
              response={capture.response}
            />
          )}

          {isBrain ? null : <AttemptTimeline record={record} />}

          <RoutingTable detail={detail} />

          <RawSection detail={detail} />
        </div>

        <RightRail
          detail={detail}
          onCopyContext={() => void copyContext()}
          copied={copied}
          copyError={copyError}
        />
      </div>
    </div>
  );
}
