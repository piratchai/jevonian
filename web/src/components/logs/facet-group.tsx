import { CaretDown } from "@phosphor-icons/react";
import { useId, useState } from "react";

import { facetLabel, type FilterGroupKey } from "@/components/logs/filter-types";
import { cn } from "@/lib/utils";

export interface FacetRow {
  /** The wire value sent on filter params. */
  value: string;
  /** Shown label; defaults to the value, with `"-"` phase rendered as "none". */
  label?: string;
  /** Matching records under the sibling filters; undefined while loading. */
  count?: number;
}

/**
 * A small styled checkbox. The visual box is a span; the input is visually
 * hidden but focusable and toggled through the label.
 */
function Checkbox({ checked }: { checked: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        "flex size-3.5 shrink-0 items-center justify-center rounded-[4px] border transition-colors",
        checked
          ? "border-kumo-brand bg-kumo-brand text-white"
          : "border-kumo-hairline bg-kumo-control",
      )}
    >
      {checked ? (
        <svg viewBox="0 0 10 8" className="size-2.5 fill-none stroke-current stroke-[1.8]">
          <path d="M1 4.2 3.6 6.6 9 1.2" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      ) : null}
    </span>
  );
}

/** One selectable facet row: checkbox, label, right-aligned count. */
export function FacetRowButton({
  group,
  row,
  checked,
  dimmed,
  onToggle,
}: {
  group: FilterGroupKey;
  row: FacetRow;
  checked: boolean;
  /** True while counts are loading or failed; keeps rows usable but quiet. */
  dimmed?: boolean;
  onToggle: (value: string) => void;
}) {
  return (
    <label
      className={cn(
        "flex min-w-0 cursor-pointer items-center gap-2 rounded-sm px-1.5 py-1 text-xs transition-colors hover:bg-kumo-tint/60",
        checked ? "text-kumo-default" : "text-kumo-subtle",
        dimmed ? "opacity-60" : "",
      )}
      title={`${checked ? "Remove" : "Add"} the ${row.label ?? facetLabel(group, row.value)} filter`}
    >
      <input
        type="checkbox"
        className="sr-only"
        checked={checked}
        onChange={() => onToggle(row.value)}
      />
      <Checkbox checked={checked} />
      {group === "status" ? (
        <span
          aria-hidden
          className={cn(
            "size-1.5 shrink-0 rounded-full",
            row.value === "error" ? "bg-kumo-danger" : "bg-kumo-success",
          )}
        />
      ) : null}
      <span className="min-w-0 flex-1 truncate">{row.label ?? facetLabel(group, row.value)}</span>
      <span className="shrink-0 font-mono text-[10px] text-kumo-subtle/80 tabular-nums">
        {row.count ?? 0}
      </span>
    </label>
  );
}

/**
 * A collapsible rail section: header with the group name and selected count,
 * then the rows. Rows can come from facets or from caller-added selections.
 */
export function FacetGroup({
  title,
  group,
  rows,
  selected,
  dimmed,
  onToggle,
  footer,
}: {
  title: string;
  group: FilterGroupKey;
  rows: FacetRow[];
  selected: string[];
  dimmed?: boolean;
  onToggle: (value: string) => void;
  /** Slot rendered under the row list, e.g. the model free-text input. */
  footer?: React.ReactNode;
}) {
  const [open, setOpen] = useState(true);
  const listId = useId();

  // Checked values that facets did not list still render, so a filter the
  // ledger cannot currently match stays visible and removable.
  const listed = new Set(rows.map((row) => row.value));
  const extras = selected.filter((value) => !listed.has(value));
  const allRows: FacetRow[] = [...rows, ...extras.map((value) => ({ value }))];

  return (
    <section className="flex min-h-0 shrink-0 flex-col border-b border-kumo-hairline last:border-b-0">
      <button
        type="button"
        className="flex w-full items-center gap-1.5 px-2.5 py-2 text-left text-[11px] font-semibold tracking-wider text-kumo-subtle uppercase hover:text-kumo-default"
        aria-expanded={open}
        aria-controls={listId}
        onClick={() => setOpen((current) => !current)}
      >
        <CaretDown
          size={12}
          aria-hidden
          className={cn("shrink-0 transition-transform", open ? "" : "-rotate-90")}
        />
        <span className="min-w-0 flex-1 truncate">{title}</span>
        {selected.length > 0 ? (
          <span className="shrink-0 rounded-full bg-kumo-brand/10 px-1.5 py-px font-mono text-[10px] font-medium text-kumo-brand normal-case">
            {selected.length}
          </span>
        ) : null}
      </button>
      {open ? (
        <div id={listId} className="max-h-48 overflow-y-auto px-1.5 pb-2">
          {allRows.length === 0 ? (
            <p className="px-1.5 py-1 text-[11px] text-kumo-subtle/70">No values yet.</p>
          ) : (
            allRows.map((row) => (
              <FacetRowButton
                key={row.value}
                group={group}
                row={row}
                checked={selected.includes(row.value)}
                dimmed={dimmed}
                onToggle={onToggle}
              />
            ))
          )}
          {footer}
        </div>
      ) : null}
    </section>
  );
}
