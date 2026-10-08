import { Sidebar } from "@cloudflare/kumo";
import {
  DeviceMobile,
  FlowArrow,
  GithubLogo,
  House,
  Key,
  Scroll,
} from "@phosphor-icons/react";
import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router";

import { ErrorBoundary } from "@/components/error-boundary";
import { ModeToggle } from "@/components/mode-toggle";
import { api } from "@/lib/api";

const GITHUB_REPO = "https://github.com/xinyao27/jevonian";

const linkGroups = [
  {
    label: "Workspace",
    links: [
      { to: "/", label: "Overview", icon: House, end: true },
      { to: "/logs", label: "Logs", icon: Scroll },
    ],
  },
  {
    label: "Configuration",
    links: [
      { to: "/models", label: "Models & Routing", icon: FlowArrow },
      { to: "/clients", label: "Clients", icon: DeviceMobile },
      { to: "/keys", label: "API keys", icon: Key },
    ],
  },
];

const pageNames: Record<string, string> = {
  "/": "Overview",
  "/models": "Models & Routing",
  "/keys": "API keys",
  "/clients": "Clients",
  "/logs": "Logs",
};

export function Layout() {
  const { pathname } = useLocation();
  const pageName = pathname.startsWith("/logs/")
    ? "Log detail"
    : (pageNames[pathname] ?? "Overview");
  const [version, setVersion] = useState(__JEVONIAN_VERSION__);

  useEffect(() => {
    void api
      .update()
      .then((response) => setVersion(response.update.current))
      .catch(() => {
        /* keep build-time fallback */
      });
  }, []);

  return (
    <Sidebar.Provider defaultOpen resizable>
      <div className="flex min-h-svh w-full bg-kumo-canvas text-kumo-default">
        <Sidebar className="border-r border-kumo-hairline bg-kumo-base">
          <Sidebar.Header className="px-3 py-3">
            <NavLink
              to="/"
              className="flex items-center gap-3 rounded-lg px-2 py-1.5 transition-colors hover:bg-kumo-tint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-kumo-line"
            >
              <span className="relative flex size-8 shrink-0 overflow-hidden rounded-lg border border-kumo-hairline bg-kumo-base">
                <img src="/jevonian-logo.png" alt="" className="size-full object-cover" />
              </span>
              <span className="grid min-w-0 flex-1 text-left leading-tight">
                <span className="truncate text-sm font-semibold">jevonian</span>
                <span className="truncate text-[11px] text-kumo-subtle">Local model router</span>
              </span>
            </NavLink>
          </Sidebar.Header>

          <Sidebar.Content className="px-2 py-4">
            {linkGroups.map((group) => (
              <Sidebar.Group key={group.label}>
                <Sidebar.GroupLabel>{group.label}</Sidebar.GroupLabel>
                <Sidebar.Menu>
                  {group.links.map((link) => {
                    const Icon = link.icon;
                    const isActive = link.end
                      ? pathname === link.to
                      : pathname.startsWith(link.to);
                    return (
                      <Sidebar.MenuButton
                        key={link.to}
                        icon={<Icon size={18} />}
                        active={isActive}
                        href={link.to}
                        tooltip={link.label}
                      >
                        {link.label}
                      </Sidebar.MenuButton>
                    );
                  })}
                </Sidebar.Menu>
              </Sidebar.Group>
            ))}
          </Sidebar.Content>

          <Sidebar.Footer className="border-t border-kumo-hairline px-3 py-2.5">
            <div className="flex items-center justify-between gap-2">
              <a
                href={GITHUB_REPO}
                target="_blank"
                rel="noreferrer"
                className="flex items-center gap-2 text-xs text-kumo-subtle transition-colors hover:text-kumo-default"
                title="Star on GitHub"
              >
                <GithubLogo size={16} />
                <span>GitHub</span>
              </a>
              <ModeToggle />
            </div>
            <div className="mt-2 text-[11px] text-kumo-subtle">
              v{version} · running locally
            </div>
          </Sidebar.Footer>
          <Sidebar.Rail />
        </Sidebar>

        <div className="flex min-w-0 flex-1 flex-col">
          <header className="sticky top-0 z-10 flex h-14 shrink-0 items-center justify-between gap-3 border-b border-kumo-hairline bg-kumo-base/90 px-5 backdrop-blur-sm md:px-10">
            <div className="flex min-w-0 items-center gap-3">
              <Sidebar.Trigger className="md:hidden" />
              <span className="text-[11px] font-semibold tracking-wider text-kumo-subtle uppercase">
                Workspace
              </span>
              <span className="text-kumo-subtle/50">/</span>
              <span className="truncate text-xs font-semibold text-kumo-default">{pageName}</span>
            </div>
            <span className="hidden items-center gap-2 text-[11px] text-kumo-subtle sm:flex">
              <span className="size-1.5 rounded-full bg-kumo-success" />
              Local dashboard
            </span>
          </header>

          <main className="min-w-0 flex-1 px-5 py-8 md:px-10 md:py-10">
            <div className="mx-auto max-w-6xl">
              <ErrorBoundary>
                <Outlet />
              </ErrorBoundary>
            </div>
          </main>
        </div>
      </div>
    </Sidebar.Provider>
  );
}
