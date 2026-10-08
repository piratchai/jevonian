import { Button, Input } from "@cloudflare/kumo";
import { FunnelSimple } from "@phosphor-icons/react";
import { useState } from "react";

import { FacetGroup, type FacetRow } from "@/components/logs/facet-group";
import {
  activeFilterCount,
  facetValues,
  FILTER_GROUPS,
  filtersActive,
  type FilterGroupKey,
  type LogFilters,
} from "@/components/logs/filter-types";
import type { LogFacets } from "@/lib/api";
import { cn } from "@/lib/utils";

/**
 * Free-text model entry: the facet list is capped at 50, so an operator can
 * still pin any model id. Enter adds it as a checked row; it renders with no
 * count when the facets do not list it.
 */
function ModelAddInput({ onAdd }: { onAdd: (value: string) => void }) {
  const [draft, setDraft] = useState("");
  const submit = () => {
    const value = draft.trim();
    if (!value) return;
    onAdd(value);
    setDraft("");
  };
  return (
    <Input
      size="sm"
      value={draft}
      onChange={(event) => setDraft(event.target.value)}
      onKeyDown={(event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          submit();
        }
      }}
      placeholder="Add model id…"
      autoComplete="off"
      aria-label="Add a model filter"
      className="mt-1 h-7 px-1.5 text-[11px]"
    />
  );
}

/**
 * The filter panel: one collapsible facet group per filter kind, fed by
 * /api/logs/facets. Checking a row toggles that value in the filter set; the
 * server applies every other group's filter to each group's counts. Rendered
 * inside a dropdown so it never takes layout space away from the log table.
 */
export function FilterRail({
  filters,
  facets,
  facetsStale,
  onToggle,
  onClear,
  className,
}: {
  filters: LogFilters;
  /** Latest facet response; null while loading or after an error. */
  facets: LogFacets | null;
  /** True while a facet request is in flight or failed; dims counts. */
  facetsStale?: boolean;
  onToggle: (group: FilterGroupKey, value: string) => void;
  onClear: () => void;
  className?: string;
}) {
  const rowsByGroup = (group: FilterGroupKey): FacetRow[] =>
    facetValues(facets, group).map((entry) => ({ value: entry.value, count: entry.count }));

  return (
    <div className={cn("flex min-h-0 flex-col", className)}>
      <div className="flex shrink-0 items-center gap-1 border-b border-kumo-hairline px-2.5 py-2">
        <span className="min-w-0 flex-1 text-[11px] font-semibold tracking-wider text-kumo-subtle uppercase">
          Filters
        </span>
        {filtersActive(filters) ? (
          <Button variant="ghost" size="xs" className="text-kumo-subtle" onClick={onClear}>
            Clear all
          </Button>
        ) : null}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {FILTER_GROUPS.map((group) => (
          <FacetGroup
            key={group.key}
            title={group.label}
            group={group.key}
            rows={rowsByGroup(group.key)}
            selected={filters[group.key]}
            dimmed={facetsStale}
            onToggle={(value) => onToggle(group.key, value)}
            footer={
              group.key === "model" ? (
                <ModelAddInput onAdd={(value) => onToggle("model", value)} />
              ) : undefined
            }
          />
        ))}
      </div>
    </div>
  );
}

/**
 * The filter trigger: a funnel button that carries the active-filter count as a
 * badge, so the operator sees that filters are on even while the panel is shut.
 * Used as the popover trigger next to the search input.
 */
export function FilterTriggerFace({ filters }: { filters: LogFilters }) {
  const count = activeFilterCount(filters);
  return (
    <span className="relative inline-flex">
      <FunnelSimple size={16} aria-hidden />
      {count > 0 ? (
        <span className="absolute -top-1.5 -right-1.5 flex min-w-3.5 items-center justify-center rounded-full bg-kumo-brand px-0.5 font-mono text-[9px] font-semibold text-white">
          {count}
        </span>
      ) : null}
    </span>
  );
}
