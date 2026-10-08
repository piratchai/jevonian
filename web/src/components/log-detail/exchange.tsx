import { Badge, Button, LayerCard, Text } from "@cloudflare/kumo";
import { useState, type ReactNode } from "react";

import { CopyButton } from "@/components/log-detail/copy-button";
import { RawBlock, Json } from "@/components/log-detail/raw";
import type { CapturedResponse, LogDetailResponse } from "@/lib/api";

import { asRecord, clientMessages, contentText, prettyJsonText } from "./helpers";

/** How many trailing messages stay expanded; the latest turn matters most. */
const EXPANDED_TAIL = 3;

function Panel({ title, meta, children }: { title: string; meta?: string; children: ReactNode }) {
  return (
    <LayerCard className="min-w-0">
      <LayerCard.Secondary>
        <Text variant="heading">{title}</Text>
        {meta ? <span className="min-w-0 truncate text-xs text-kumo-subtle">{meta}</span> : null}
      </LayerCard.Secondary>
      <LayerCard.Primary>{children}</LayerCard.Primary>
    </LayerCard>
  );
}

function MessageBlock({ role, content }: { role: string; content: unknown }) {
  const text = contentText(content);
  return (
    <div className="flex min-w-0 flex-col gap-1 rounded-md border border-kumo-hairline bg-kumo-base p-3">
      <div className="flex min-w-0 items-center gap-2">
        <span className="text-xs font-medium text-kumo-subtle">{role}</span>
        <CopyButton text={text} label={`Copy ${role} message`} className="ml-auto" />
      </div>
      <pre className="max-h-72 overflow-auto text-xs break-words whitespace-pre-wrap">{text}</pre>
    </div>
  );
}

function RequestSide({ clientRequest }: { clientRequest: unknown }) {
  const [showEarlier, setShowEarlier] = useState(false);
  const messages = clientMessages(clientRequest);
  if (!messages) {
    return (
      <div className="flex min-w-0 flex-col gap-2">
        <p className="text-xs text-kumo-subtle">
          No message list was captured; the raw request is below.
        </p>
        <Json value={clientRequest ?? null} />
      </div>
    );
  }
  const split = Math.max(0, messages.length - EXPANDED_TAIL);
  const recent = messages.slice(split);
  if (messages.length === 0) {
    return (
      <p className="text-xs text-kumo-subtle">The client sent no messages for this request.</p>
    );
  }
  return (
    <div className="flex min-w-0 flex-col gap-2">
      {split > 0 && !showEarlier ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="self-start text-xs text-kumo-subtle"
          onClick={() => setShowEarlier(true)}
        >
          Show {split} earlier message{split === 1 ? "" : "s"}
        </Button>
      ) : null}
      {(showEarlier ? messages : recent).map((message, index) => (
        <MessageBlock
          key={`${message.role}-${showEarlier ? index : index + split}`}
          role={message.role}
          content={message.content}
        />
      ))}
    </div>
  );
}

function ToolCall({ call }: { call: { id?: string; name: string; arguments: string } }) {
  const { pretty, parsed } = prettyJsonText(call.arguments);
  return (
    <div className="flex min-w-0 flex-col gap-1 rounded-md border border-kumo-hairline bg-kumo-base p-2">
      <div className="flex min-w-0 items-center gap-2">
        <span className="min-w-0 font-mono text-xs font-medium break-all">{call.name}</span>
        {call.id ? (
          <span className="min-w-0 truncate text-xs text-kumo-subtle">{call.id}</span>
        ) : null}
        {!parsed ? (
          <Badge variant="outline" className="text-xs">
            unparsed
          </Badge>
        ) : null}
        <CopyButton
          text={call.arguments}
          label={`Copy arguments for ${call.name}`}
          className="ml-auto"
        />
      </div>
      <pre className="max-h-48 overflow-auto text-xs break-words whitespace-pre-wrap">{pretty}</pre>
    </div>
  );
}

function ResponseSide({ response }: { response?: CapturedResponse }) {
  if (!response) {
    return (
      <p className="rounded-md border border-kumo-hairline bg-kumo-tint/40 p-3 text-xs text-kumo-subtle">
        Response not captured for this record (recorded before response capture, or
        JEVONIAN_CAPTURE_BODIES=0).
      </p>
    );
  }
  const text = response.text ?? "";
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <Badge variant="outline" className="font-mono text-xs">
          {response.wire}
        </Badge>
        <Badge
          variant={response.status >= 400 ? "error" : "secondary"}
          className="font-mono text-xs"
        >
          {response.status}
        </Badge>
        <Badge variant="outline" className="text-xs">
          {response.stream ? "streamed" : "buffered"}
        </Badge>
        {response.finishReason ? (
          <Badge variant="outline" className="text-xs">
            finish: {response.finishReason}
          </Badge>
        ) : null}
        <CopyButton text={text} label="Copy response text" className="ml-auto" />
      </div>

      {response.error ? (
        <p className="rounded-md border border-kumo-danger/40 bg-kumo-danger-tint p-3 font-mono text-xs break-words text-kumo-danger">
          {response.error}
        </p>
      ) : null}

      {response.truncated ? (
        <p className="text-xs text-kumo-subtle">
          The capture was cut at a size cap, so this response is incomplete.
        </p>
      ) : null}

      {response.reasoning ? (
        <RawBlock summary="Reasoning">
          <pre className="max-h-72 overflow-auto text-xs break-words whitespace-pre-wrap text-kumo-subtle">
            {response.reasoning}
          </pre>
        </RawBlock>
      ) : null}

      {text ? (
        <pre className="max-h-96 overflow-auto rounded-md border border-kumo-hairline bg-kumo-base p-3 text-xs break-words whitespace-pre-wrap">
          {text}
        </pre>
      ) : (
        <p className="text-xs text-kumo-subtle">
          The model returned no visible text for this turn.
        </p>
      )}

      {response.toolCalls && response.toolCalls.length > 0 ? (
        <div className="flex min-w-0 flex-col gap-2">
          <span className="text-xs tracking-[0.12em] text-kumo-subtle uppercase">
            Tool calls ({response.toolCalls.length})
          </span>
          {response.toolCalls.map((call, index) => (
            <ToolCall key={call.id ?? `${call.name}-${index}`} call={call} />
          ))}
        </div>
      ) : null}
    </div>
  );
}

/**
 * The exchange: what the client sent on the left, what came back on the right. `layout`
 * picks how they read: `"split"` places them side by side on wide screens, `"stacked"`
 * always puts Request above Response. The panel uses `"stacked"` because it stays narrow
 * even on wide screens, where a viewport breakpoint would wrongly create two columns.
 */
export function Exchange({
  detail,
  clientRequest,
  response,
  layout = "split",
}: {
  detail: LogDetailResponse;
  clientRequest: unknown;
  response?: CapturedResponse;
  layout?: "split" | "stacked";
}) {
  const body = asRecord(detail.body);
  const messages = clientMessages(clientRequest);
  return (
    <LayerCard className="min-w-0">
      <LayerCard.Secondary>
        <Text variant="heading">Request ↔ Response</Text>
        <span className="min-w-0 truncate text-xs text-kumo-subtle">
          {messages
            ? `${messages.length} message${messages.length === 1 ? "" : "s"} sent`
            : "request body"}
          {response ? ` · ${response.wire} reply` : " · no response captured"}
        </span>
      </LayerCard.Secondary>

      <LayerCard.Primary className="gap-4">
        <div
          className={
            layout === "stacked"
              ? "grid min-w-0 grid-cols-1 gap-4"
              : "grid min-w-0 gap-4 lg:grid-cols-2"
          }
        >
          <Panel title="Request" meta={messages ? undefined : "raw body"}>
            <RequestSide clientRequest={clientRequest} />
          </Panel>
          <Panel title="Response">
            <ResponseSide response={response} />
          </Panel>
        </div>

        <RawBlock summary="Raw request JSON" className="min-w-0">
          <Json value={body?.body ?? detail.body ?? null} />
        </RawBlock>
      </LayerCard.Primary>
    </LayerCard>
  );
}
