import { cn } from "@/lib/utils";

/**
 * Quick status shortcuts above the table. "Errors" maps to status=["error"];
 * "All" clears the status group (the server also accepts both values, but an
 * empty set is the same thing and keeps the rail badge quiet).
 */
export function StatusPills({
  status,
  onChange,
}: {
  status: string[];
  onChange: (next: string[]) => void;
}) {
  const errorsOnly = status.length === 1 && status[0] === "error";
  const pill = (active: boolean) =>
    cn(
      "rounded-full border px-2.5 py-1 text-xs font-medium transition-colors",
      active
        ? "border-kumo-brand/40 bg-kumo-brand/10 text-kumo-default"
        : "border-transparent text-kumo-subtle hover:bg-kumo-tint hover:text-kumo-default",
    );
  return (
    <div className="flex shrink-0 items-center gap-1" role="group" aria-label="Status filter">
      <button
        type="button"
        className={pill(status.length === 0)}
        aria-pressed={status.length === 0}
        onClick={() => onChange([])}
      >
        All
      </button>
      <button
        type="button"
        className={cn(pill(errorsOnly), "inline-flex items-center gap-1.5")}
        aria-pressed={errorsOnly}
        onClick={() => onChange(["error"])}
      >
        <span aria-hidden className="size-1.5 rounded-full bg-kumo-danger" />
        Errors
      </button>
    </div>
  );
}
