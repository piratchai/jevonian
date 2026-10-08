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
    <details
      className={
        className ? `rounded-md border border-kumo-hairline ${className}` : "rounded-md border border-kumo-hairline"
      }
    >
      <summary className="cursor-pointer px-3 py-2 text-xs text-kumo-subtle">
        {summary}
      </summary>
      <div className="border-t border-kumo-hairline p-3">{children}</div>
    </details>
  );
}

/** Pretty-printed JSON in a scrollable block. */
export function Json({ value }: { value: unknown }) {
  return (
    <pre className="max-h-96 overflow-auto rounded-md border border-kumo-hairline bg-kumo-tint/40 p-3 text-xs break-words whitespace-pre-wrap">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}
