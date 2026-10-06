import { ListFilterIcon, PanelLeftCloseIcon } from "lucide-react";
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
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
 * The left rail: one collapsible facet group per filter kind, fed by
 * /api/logs/facets. Checking a row toggles that value in the filter set; the
 * server applies every other group's filter to each group's counts.
 */
export function FilterRail({
  filters,
  facets,
  facetsStale,
  onToggle,
  onClear,
  onCollapse,
  className,
}: {
  filters: LogFilters;
  /** Latest facet response; null while loading or after an error. */
  facets: LogFacets | null;
  /** True while a facet request is in flight or failed; dims counts. */
  facetsStale?: boolean;
  onToggle: (group: FilterGroupKey, value: string) => void;
  onClear: () => void;
  onCollapse?: () => void;
  className?: string;
}) {
  const rowsByGroup = (group: FilterGroupKey): FacetRow[] =>
    facetValues(facets, group).map((entry) => ({ value: entry.value, count: entry.count }));

  return (
    <div className={cn("flex min-h-0 flex-col", className)}>
      <div className="flex shrink-0 items-center gap-1 border-b border-border/60 px-2.5 py-2">
        <span className="min-w-0 flex-1 text-[11px] font-semibold tracking-wider text-muted-foreground uppercase">
          Filters
        </span>
        {filtersActive(filters) ? (
          <Button variant="ghost" size="xs" className="text-muted-foreground" onClick={onClear}>
            Clear all
          </Button>
        ) : null}
        {onCollapse ? (
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label="Collapse filters"
            title="Collapse filters"
            onClick={onCollapse}
          >
            <PanelLeftCloseIcon />
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
 * The collapsed rail strip: a single button with the active-filter count.
 * Clicking it reopens the rail.
 */
export function CollapsedRail({
  filters,
  onExpand,
  className,
}: {
  filters: LogFilters;
  onExpand: () => void;
  className?: string;
}) {
  const count = activeFilterCount(filters);
  return (
    <div className={cn("flex shrink-0 flex-col items-center py-2", className)}>
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label={`Open filters${count > 0 ? `, ${count} active` : ""}`}
        title={count > 0 ? `Filters (${count} active)` : "Filters"}
        onClick={onExpand}
        className="relative"
      >
        <ListFilterIcon />
        {count > 0 ? (
          <span className="absolute -top-0.5 -right-0.5 flex min-w-3.5 items-center justify-center rounded-full bg-primary px-0.5 font-mono text-[9px] font-semibold text-primary-foreground">
            {count}
          </span>
        ) : null}
      </Button>
    </div>
  );
}
