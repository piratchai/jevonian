/**
 * One outcome for a relayed stream: what the ledger should record, and what a client
 * disconnect should be recorded as.
 *
 * A bridged stream decides "was anything delivered" from whatever its upstream has surfaced
 * so far — a usage event, a content delta, a finish — instead of relying on a completion
 * callback that a client cancel cuts short. Passthrough bodies that already track their own
 * delivery feed the same shape and share the same `cancelOutcome` decision.
 */

import { type Usage } from "./pricing";

/** An event a relayed stream has decided matters for the outcome. */
export type StreamEvent =
  /** A token the client can render: text, thinking, or a tool call. */
  | { kind: "content" }
  /** A usage report, from whatever framing the upstream wire uses. */
  | { kind: "usage"; usage: Usage }
  /** The turn finished cleanly (finish_reason, message_stop, response.completed, [DONE]). */
  | { kind: "finish" }
  /** The upstream folded an error into the stream instead of a status. */
  | { kind: "error"; message: string };

/**
 * Delivers events a relayed stream has surfaced so far. `delivered` is whether a cancel now
 * means "finished turn" rather than "abandoned request"; `usage` is the latest accounting.
 */
export interface StreamTracker {
  feed(event: StreamEvent): void;
  readonly delivered: boolean;
  readonly finished: boolean;
  readonly usage: Usage;
  readonly failure: string | undefined;
}

const zeroUsage = (): Usage => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });

/** A tracker driven by event kinds — for bridges whose transforms know what they emitted. */
export function trackEvents(): StreamTracker {
  const state = { delivered: false, finished: false, failure: undefined as string | undefined };
  const usage = zeroUsage();
  return {
    feed(event) {
      if (event.kind === "content") state.delivered = true;
      if (event.kind === "usage") Object.assign(usage, event.usage);
      if (event.kind === "finish") {
        state.delivered = true;
        state.finished = true;
      }
      if (event.kind === "error") state.failure = event.message;
    },
    get delivered() {
      return state.delivered;
    },
    get finished() {
      return state.finished;
    },
    get usage() {
      return usage;
    },
    get failure() {
      return state.failure;
    },
  };
}

/**
 * What the ledger row says when the client hung up: 200 when something was delivered — the
 * turn is already on its screen — and 499 only when the request truly went unanswered.
 */
export function cancelOutcome(tracker: Pick<StreamTracker, "delivered" | "usage">): {
  status: 200 | 499;
  usage: Usage;
  error?: string;
} {
  if (tracker.delivered) return { status: 200, usage: tracker.usage };
  return { status: 499, usage: zeroUsage(), error: "client canceled" };
}
