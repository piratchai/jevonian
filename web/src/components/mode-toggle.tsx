import { DropdownMenu, Button } from "@cloudflare/kumo";
import { Check, Desktop, Moon, Sun } from "@phosphor-icons/react";
import type { ComponentType } from "react";

import { useTheme, type Theme } from "@/components/theme-provider";

type ThemeOption = {
  value: Theme;
  label: string;
  icon: ComponentType<{ size?: number; className?: string }>;
};

const OPTIONS: readonly ThemeOption[] = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "System", icon: Desktop },
];

/**
 * Compact theme switcher for the sidebar footer.
 * Built with Kumo DropdownMenu and Phosphor Icons.
 */
export function ModeToggle({ className }: { className?: string }) {
  const { theme, resolvedTheme, setTheme } = useTheme();
  const ActiveIcon = resolvedTheme === "dark" ? Moon : Sun;

  return (
    <DropdownMenu>
      <DropdownMenu.Trigger
        render={
          <Button
            variant="ghost"
            shape="square"
            size="sm"
            className={className}
            aria-label="Toggle theme"
            title="Toggle theme"
            icon={<ActiveIcon size={16} />}
          />
        }
      />
      <DropdownMenu.Content align="end">
        {OPTIONS.map((option) => {
          const Icon = option.icon;
          const selected = option.value === theme;
          return (
            <DropdownMenu.Item
              key={option.value}
              onClick={() => setTheme(option.value)}
              className="flex items-center justify-between gap-4"
            >
              <span className="flex items-center gap-2">
                <Icon size={16} />
                <span>{option.label}</span>
              </span>
              {selected ? <Check size={14} className="text-kumo-brand" /> : null}
            </DropdownMenu.Item>
          );
        })}
      </DropdownMenu.Content>
    </DropdownMenu>
  );
}
