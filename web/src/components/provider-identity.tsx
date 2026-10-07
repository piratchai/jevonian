import { ProviderLogo } from "@/components/provider-logo";
import { Badge } from "@/components/ui/badge";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { resolveProviderIdentity, type ProviderIdentityInput } from "@/lib/provider-name";
import { cn } from "@/lib/utils";

/**
 * One provider, rendered the same way everywhere: its brand logo, product name, and — for a
 * second account — a small account chip so two sign-ins of one agent never look identical.
 *
 * Pass the whole provider record when you have it (the brand then comes from `oauthSource`,
 * the most reliable signal); a bare name still resolves, and keeps the logo.
 */
export function ProviderIdentity({
  provider,
  size,
  className,
  nameClassName,
  accountClassName,
  showAccount = true,
  logoClassName,
}: {
  provider: string | ProviderIdentityInput;
  /** Logo size class, forwarded to `ProviderLogo` (default `size-5`). */
  size?: string;
  className?: string;
  nameClassName?: string;
  accountClassName?: string;
  /** Hide the account chip (for tight rows where a tooltip carries it instead). */
  showAccount?: boolean;
  logoClassName?: string;
}) {
  const identity = resolveProviderIdentity(provider);
  return (
    <span className={cn("flex min-w-0 items-center gap-2", className)}>
      <ProviderLogo id={identity.brand} className={cn(size ?? "size-5", logoClassName)} />
      <span className={cn("flex min-w-0 items-center gap-1.5", nameClassName)}>
        <span className="truncate">{identity.name}</span>
        {showAccount && identity.isSecondAccount ? (
          <AccountChip
            label={identity.account}
            detail={identity.accountDetail}
            className={accountClassName}
          />
        ) : null}
      </span>
    </span>
  );
}

/**
 * The account chip. A second sign-in shows as `work` beside the brand; the tooltip names the
 * credential location, so `~/.claude-work` is discoverable without widening the row.
 */
export function AccountChip({
  label,
  detail,
  className,
}: {
  label?: string;
  detail?: string;
  className?: string;
}) {
  const text = label?.trim() || "account";
  const chip = (
    <Badge
      variant="outline"
      className={cn("shrink-0 px-1.5 py-0 text-[10px] font-normal text-muted-foreground", className)}
    >
      {text}
    </Badge>
  );
  if (!detail) return chip;
  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger render={chip} />
        <TooltipContent>
          <span className="font-mono">{detail}</span>
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
