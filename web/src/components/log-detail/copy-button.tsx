import { Button } from "@cloudflare/kumo";
import { Check, Copy } from "@phosphor-icons/react";
import { useState } from "react";

import { cn } from "@/lib/utils";

/** A small icon button that copies `text` and briefly confirms. */
export function CopyButton({
  text,
  label = "Copy",
  className,
}: {
  text: string;
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setError("");
      setTimeout(() => setCopied(false), 1_500);
    } catch (cause) {
      setError(String(cause));
    }
  }

  return (
    <Button
      type="button"
      variant="ghost"
      shape="square"
      size="sm"
      aria-label={label}
      title={error || label}
      onClick={() => void copy()}
      className={cn("text-kumo-subtle", className)}
      icon={
        copied ? (
          <Check size={14} className="text-kumo-success" aria-hidden />
        ) : (
          <Copy size={14} aria-hidden />
        )
      }
    />
  );
}
