import { PROVIDER_ICONS, PROVIDER_IMAGES } from "@/lib/logos";
import { cn } from "@/lib/utils";

function initials(value: string): string {
  return value
    .split(/[\s-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase() ?? "")
    .join("");
}

/** Pull a pixel size from Tailwind `size-*` classes so Lobe SVGs match the slot. */
function sizeFromClassName(className: string | undefined): number {
  if (!className) return 20;
  const match = /\bsize-(?:\[(\d+)px\]|(\d+(?:\.\d+)?))\b/.exec(className);
  if (!match) return 20;
  if (match[1]) return Number(match[1]);
  const rem = Number(match[2]);
  return Number.isFinite(rem) ? Math.round(rem * 4) : 20;
}

export function ProviderLogo({ id, className }: { id: string; className?: string }) {
  const Icon = PROVIDER_ICONS[id];
  if (Icon) {
    const size = sizeFromClassName(className);
    return (
      <span className={cn("inline-flex size-5 shrink-0 items-center justify-center", className)}>
        <Icon size={size} aria-hidden />
      </span>
    );
  }
  const image = PROVIDER_IMAGES[id];
  if (image) {
    return (
      <span className={cn("inline-flex size-5 shrink-0 items-center justify-center", className)}>
        <img src={image} alt="" className="size-full object-contain" />
      </span>
    );
  }
  return (
    <span
      className={cn(
        "inline-flex size-5 shrink-0 items-center justify-center rounded-sm border border-kumo-hairline bg-kumo-tint text-[9px] font-semibold text-kumo-subtle",
        className,
      )}
    >
      {initials(id)}
    </span>
  );
}
