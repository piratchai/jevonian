import type { ReactNode } from "react";

/** A collapsible panel, used for raw JSON and secondary detail. */
export function RawBlock({
  summary,
  children,
  className,
}: {
  summary: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <details className={className ? `rounded-md border ${className}` : "rounded-md border"}>
      <summary className="cursor-pointer px-3 py-2 text-xs text-muted-foreground">
        {summary}
      </summary>
      <div className="border-t p-3">{children}</div>
    </details>
  );
}

/** Pretty-printed JSON in a scrollable block. */
export function Json({ value }: { value: unknown }) {
  return (
    <pre className="max-h-96 overflow-auto rounded-md border bg-muted/40 p-3 text-xs break-words whitespace-pre-wrap">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}
