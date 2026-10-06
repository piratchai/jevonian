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
      "rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors",
      active
        ? "border-primary/40 bg-primary/10 text-primary"
        : "border-transparent text-muted-foreground hover:bg-muted hover:text-foreground",
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
        <span aria-hidden className="size-1.5 rounded-full bg-destructive" />
        Errors
      </button>
    </div>
  );
}
