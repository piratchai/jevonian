import type { RoutingEntryView, StateResponse } from "./api";

/** Append backup models without changing saved priorities or provider allow-lists. */
export function assignProviderModels(
  state: StateResponse & {
    canonicals?: Array<{ id: string; variants: Array<{ provider: string; model: string }> }>;
  },
  taskIds: string[],
  models: string[],
  provider: string,
  convertAuto: boolean,
): RoutingEntryView[] {
  const saved = state.config.routing.routings;
  const entries = saved.length > 0 ? saved : state.routings;
  return entries.map((entry) => {
    if (!taskIds.includes(entry.id)) return entry;
    const derived = state.routings.find((task) => task.id === entry.id)?.models ?? [];
    if (entry.models.length === 0 && derived.length > 0 && !convertAuto) {
      throw new Error(`Confirm conversion of auto-derived task "${entry.label}" to a fixed list.`);
    }
    const current = entry.models.length > 0 ? entry.models : derived;
    // An explicit empty list withholds a model. Never restore an excluded source implicitly.
    const allowed = models.filter((model) => {
      const matchingKeys = Object.keys(entry.providers ?? {}).filter((key) =>
        state.canonicals?.some(
          (canonical) =>
            canonical.id === model && canonical.variants.some((variant) => variant.model === key),
        ),
      );
      const keysToCheck = [...new Set([model, ...matchingKeys])];
      return keysToCheck.every((key) => {
        const allow = entry.providers?.[key];
        return allow === undefined || allow.includes(provider);
      });
    });
    return { ...entry, models: [...new Set([...current, ...allowed])] };
  });
}
