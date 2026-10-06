import type { LogFacets, LogFacetValue, LogFilterParams } from "@/lib/api";

/**
 * The active facet filter set. Keys map onto the repeatable query params;
 * values within one group are OR-ed, groups are AND-ed.
 */
export interface LogFilters {
  status: string[];
  phase: string[];
  provider: string[];
  model: string[];
  /** Free-text filter; kept here so every consumer reads the same set. */
  q: string;
}

export const EMPTY_FILTERS: LogFilters = {
  status: [],
  phase: [],
  provider: [],
  model: [],
  q: "",
};

/** Filter groups rendered in the rail, in display order. */
export const FILTER_GROUPS = [
  { key: "status", label: "Status" },
  { key: "phase", label: "Phase" },
  { key: "provider", label: "Provider" },
  { key: "model", label: "Model" },
] as const;

export type FilterGroupKey = (typeof FILTER_GROUPS)[number]["key"];

/** True when any checkbox filter or the search text is active. */
export function filtersActive(filters: LogFilters): boolean {
  return (
    filters.status.length > 0 ||
    filters.phase.length > 0 ||
    filters.provider.length > 0 ||
    filters.model.length > 0 ||
    filters.q.trim() !== ""
  );
}

/** Count of checked values across the checkbox groups; drives the rail badge. */
export function activeFilterCount(filters: LogFilters): number {
  return (
    filters.status.length + filters.phase.length + filters.provider.length + filters.model.length
  );
}

/** Toggle `value` inside `list`: add when absent, remove when present. */
export function toggleValue(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter((item) => item !== value) : [...list, value];
}

/**
 * Project the UI filter set onto the wire params. Arrays go straight through;
 * the API layer drops empty and `"all"` entries. `q` is trimmed here so the
 * list, the stream, and the facets all send the identical substring.
 */
export function filtersToParams(filters: LogFilters): LogFilterParams {
  return {
    phase: filters.phase,
    model: filters.model,
    provider: filters.provider,
    status: filters.status,
    q: filters.q.trim() || undefined,
  };
}

/**
 * Serialize the filter set the way `api.logs` does, so the live stream honors
 * exactly the same filters as the list. `session` is a list-only param and is
 * never sent to the stream.
 */
export function filtersToQuery(filters: LogFilters): string {
  const query = new URLSearchParams();
  for (const name of ["phase", "model", "provider", "status"] as const) {
    for (const raw of filters[name]) {
      const value = raw.trim();
      if (!value || value === "all") continue;
      query.append(name, value);
    }
  }
  const q = filters.q.trim();
  if (q) query.set("q", q);
  return query.toString();
}

/** Pull one group's facet rows out of a possibly-partial facets response. */
export function facetValues(facets: LogFacets | null, group: FilterGroupKey): LogFacetValue[] {
  return facets?.groups?.[group] ?? [];
}

/** Human label for a facet value; the phase `"-"` means "no phase recorded". */
export function facetLabel(group: FilterGroupKey, value: string): string {
  if (group === "phase" && value === "-") return "none";
  return value;
}
