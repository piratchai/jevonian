import type { RoutingEntryView } from "../lib/api.ts";

export const BUILTIN_ROUTING_IDS = new Set(["plan", "execute", "utility", "chat"]);

/** Undefined allows discovery. An explicit empty list withholds the model. */
export function allowedProviders(discovered: string[], preferred: string[] | undefined): string[] {
  return preferred === undefined
    ? discovered
    : preferred.filter(
        (name, index) => discovered.includes(name) && preferred.indexOf(name) === index,
      );
}

/** Merge fresh server routes without replacing local edits or new routes. */
export function mergeRoutingDrafts(
  fresh: RoutingEntryView[],
  drafts: RoutingEntryView[],
  previous: RoutingEntryView[],
): RoutingEntryView[] {
  const changed = drafts.filter((draft) => {
    const saved = previous.find((entry) => entry.id === draft.id);
    return JSON.stringify(saved) !== JSON.stringify(draft);
  });
  return [
    ...fresh.map((entry) => changed.find((draft) => draft.id === entry.id) ?? entry),
    ...changed.filter((entry) => !fresh.some((saved) => saved.id === entry.id)),
  ];
}

/** Save one route, never the other route drafts. */
export function routeSavePayload(
  persisted: RoutingEntryView[],
  draft: RoutingEntryView,
): RoutingEntryView[] {
  return persisted.some((entry) => entry.id === draft.id)
    ? persisted.map((entry) => (entry.id === draft.id ? draft : entry))
    : [...persisted, draft];
}

export function validRoutingId(value: string): boolean {
  return /^[a-z][a-z0-9-]{0,63}$/.test(value) && value !== "auto";
}
