import type { CanonicalModelView, ModelView, RoutingEntryView } from "../lib/api.ts";

export const BUILTIN_ROUTING_IDS = new Set(["plan", "execute", "utility", "chat"]);

/**
 * Collect all providers that serve a model, indexed by both canonical and raw variant spellings.
 * This ensures that a model like `deepseek-v4.1-flash` also sees providers configured with
 * `deepseek/deepseek-v4.1-flash`.
 */
export function collectProvidersByModel(
  models: ModelView[],
  canonicals: CanonicalModelView[],
): Map<string, string[]> {
  const map = new Map<string, string[]>();
  const add = (id: string, provider: string) => {
    const list = map.get(id) ?? [];
    if (!list.includes(provider)) list.push(provider);
    map.set(id, list);
  };
  for (const model of models) add(model.id, model.provider);
  for (const canonical of canonicals) {
    const providers = canonical.variants.map((v) => v.provider);
    // Associate all providers serving this canonical model with the canonical ID
    // as well as every variant spelling.
    for (const p of providers) {
      add(canonical.id, p);
      for (const variant of canonical.variants) {
        add(variant.model, p);
      }
    }
  }
  return map;
}

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
