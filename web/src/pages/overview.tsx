import { Gauge, ShieldCheck } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useLocation } from "react-router";

import { OverviewDashboardGrid, OverviewStatusStrip } from "@/components/overview-dashboard";
import { OverviewSkeleton } from "@/components/page-skeletons";
import { QuotaGrid } from "@/components/quota-card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  api,
  type ActivityReportView,
  type ProviderQuotaView,
  type QuotaHealthView,
  type StateResponse,
  type StatsResponse,
  type TunnelProviderView,
  type TunnelStatusView,
  type LanResponse,
  type UpdateResponse,
} from "@/lib/api";
import { money } from "@/lib/utils";
import { ActivitySection } from "@/pages/activity";

function Panel({
  title,
  summary,
  children,
  defaultOpen = false,
}: {
  title: string;
  summary: string;
  children: ReactNode;
  defaultOpen?: boolean;
}) {
  return (
    <details className="rounded-xl border bg-card p-5" open={defaultOpen || undefined}>
      <summary className="cursor-pointer text-sm font-semibold tracking-tight">
        {title} <span className="font-normal text-muted-foreground">· {summary}</span>
      </summary>
      <div className="mt-3 flex flex-col gap-3">{children}</div>
    </details>
  );
}

type TunnelDraft = { provider: TunnelProviderView; command: string; url: string };

export function OverviewPage() {
  const { hash } = useLocation();
  const [state, setState] = useState<StateResponse | null>(null);
  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [today, setToday] = useState<ActivityReportView | null>(null);
  const [week, setWeek] = useState<ActivityReportView | null>(null);
  const [month, setMonth] = useState<ActivityReportView | null>(null);
  const [history, setHistory] = useState<ActivityReportView | null>(null);
  const [quotas, setQuotas] = useState<ProviderQuotaView[]>([]);
  const [health, setHealth] = useState<QuotaHealthView[]>([]);
  const [tunnel, setTunnel] = useState<TunnelStatusView | null>(null);
  const [tunnelProvider, setTunnelProvider] = useState<TunnelProviderView>("cloudflare");
  const [tunnelCommand, setTunnelCommand] = useState("");
  const [tunnelUrl, setTunnelUrl] = useState("");
  const [tunnelBusy, setTunnelBusy] = useState(false);
  const [tunnelError, setTunnelError] = useState("");
  const [lan, setLan] = useState<LanResponse | null>(null);
  const [lanBusy, setLanBusy] = useState(false);
  const [lanError, setLanError] = useState("");
  const [update, setUpdate] = useState<UpdateResponse | null>(null);
  const [updateBusy, setUpdateBusy] = useState(false);
  const [updateError, setUpdateError] = useState("");
  const [copied, setCopied] = useState("");
  const [error, setError] = useState("");
  // True while the local draft differs from what the server last reported. Polling
  // must not overwrite typed input, otherwise a 10s refresh destroys a draft.
  const draftDirty = useRef(false);
  const [draftUnsaved, setDraftUnsaved] = useState(false);

  const loadCore = useCallback(async () => {
    try {
      const [nextState, nextStats, nextQuotas, nextTunnel, nextUpdate, nextLan] = await Promise.all(
        [api.state(), api.stats(), api.quota(), api.tunnel(), api.update(), api.lan()],
      );
      setState(nextState);
      setStats(nextStats);
      setQuotas(nextQuotas.quotas);
      setHealth(nextQuotas.health);
      setTunnel(nextTunnel.tunnel);
      setUpdate(nextUpdate);
      setLan(nextLan);
      if (!draftDirty.current) {
        setTunnelProvider(nextTunnel.config.provider);
        setTunnelCommand(nextTunnel.config.command ?? "");
        setTunnelUrl(nextTunnel.config.url ?? "");
      }
    } catch (cause) {
      setError(String(cause));
    }
  }, []);

  const loadActivity = useCallback(async () => {
    try {
      const [nextToday, nextWeek, nextMonth, nextHistory] = await Promise.all([
        api.activity({ range: "today" }),
        api.activity({ range: "7d" }),
        api.activity({ range: "30d" }),
        api.activity({ range: "all" }),
      ]);
      setToday(nextToday);
      setWeek(nextWeek);
      setMonth(nextMonth);
      setHistory(nextHistory);
    } catch (cause) {
      setError(String(cause));
    }
  }, []);

  const load = useCallback(async () => {
    await Promise.all([loadCore(), loadActivity()]);
  }, [loadCore, loadActivity]);

  useEffect(() => {
    void load();
    const coreTimer = setInterval(() => void loadCore(), 10_000);
    const activityTimer = setInterval(() => void loadActivity(), 30_000);
    return () => {
      clearInterval(coreTimer);
      clearInterval(activityTimer);
    };
  }, [load, loadCore, loadActivity]);

  function currentDraft(): TunnelDraft {
    return { provider: tunnelProvider, command: tunnelCommand, url: tunnelUrl };
  }

  function editDraft(patch: Partial<TunnelDraft>): void {
    draftDirty.current = true;
    setDraftUnsaved(true);
    if (patch.provider !== undefined) setTunnelProvider(patch.provider);
    if (patch.command !== undefined) setTunnelCommand(patch.command);
    if (patch.url !== undefined) setTunnelUrl(patch.url);
  }

  async function toggleTunnel(): Promise<void> {
    setTunnelBusy(true);
    setTunnelError("");
    try {
      const draft = currentDraft();
      const response = await api.saveTunnel({
        enabled: tunnel?.status !== "on",
        provider: draft.provider,
        ...(draft.provider === "custom" ? { command: draft.command } : {}),
        // ngrok: optional reserved domain; custom: stable URL; cloudflare: clear any leftover
        url: draft.provider === "ngrok" || draft.provider === "custom" ? draft.url.trim() : "",
      });
      setTunnel(response.tunnel);
      if (response.error) setTunnelError(response.error);
      draftDirty.current = false;
      setDraftUnsaved(false);
      void load();
    } catch (cause) {
      setTunnelError(String(cause));
    } finally {
      setTunnelBusy(false);
    }
  }

  async function installUpdate(): Promise<void> {
    setUpdateBusy(true);
    setUpdateError("");
    try {
      setUpdate(await api.installUpdate());
    } catch (cause) {
      setUpdateError(String(cause));
    } finally {
      setUpdateBusy(false);
    }
  }

  async function checkUpdate(): Promise<void> {
    setUpdateBusy(true);
    setUpdateError("");
    try {
      setUpdate(await api.checkUpdate());
    } catch (cause) {
      setUpdateError(String(cause));
    } finally {
      setUpdateBusy(false);
    }
  }

  async function toggleLan(): Promise<void> {
    setLanBusy(true);
    setLanError("");
    try {
      const response = await api.saveLan({ enabled: !(lan?.config.enabled ?? false) });
      setLan(response);
      if (response.error) setLanError(response.error);
    } catch (cause) {
      setLanError(String(cause));
    } finally {
      setLanBusy(false);
    }
  }

  async function copy(value: string, id: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(id);
      setTimeout(() => setCopied((current) => (current === id ? "" : current)), 1_500);
    } catch {
      setTunnelError("Clipboard is unavailable.");
    }
  }

  if (error) return <p className="text-sm text-destructive">{error}</p>;
  if (!state || !stats || !today || !week || !month || !history) return <OverviewSkeleton />;

  const localUrl = `http://${state.config.listen.host}:${state.config.listen.port}/v1`;
  const publicUrl = tunnel?.status === "on" && tunnel.url ? `${tunnel.url}/v1` : undefined;
  const authed = state.keys.length > 0;
  const firstKey = state.keys[0];
  const apiKeyHint = firstKey ? `${firstKey.prefix}••••` : "";
  const curl = (base: string) =>
    `curl ${base}/chat/completions \\
  -H "authorization: Bearer <key>" \\
  -H "content-type: application/json" \\
  -d '{"model":"jevonian/auto","messages":[{"role":"user","content":"hi"}]}'`;

  const nowLabel = new Date().toLocaleString(undefined, {
    weekday: "long",
    month: "long",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-[-0.03em] sm:text-[1.75rem]">
            Welcome back
          </h1>
          <p className="mt-1 text-[13px] text-muted-foreground">{nowLabel}</p>
        </div>
        <OverviewStatusStrip
          running
          routingMode={state.config.routing.mode}
          localUrl={localUrl}
          apiKeyHint={apiKeyHint}
          copied={copied}
          onCopyUrl={(value, id) => void copy(value, id)}
        />
      </header>

      <OverviewDashboardGrid
        today={today}
        week={week}
        month={month}
        history={history}
        cacheHitRate={stats.cacheHitRate}
      />

      <details className="rounded-xl border bg-card p-5" open={hash === "#activity" || undefined}>
        <summary className="cursor-pointer text-sm font-semibold tracking-tight">
          Activity detail{" "}
          <span className="font-normal text-muted-foreground">
            · filters, tables, and full charts
          </span>
        </summary>
        <div className="mt-4">
          <ActivitySection />
        </div>
      </details>

      <div className="flex items-center gap-2 pt-2">
        <ShieldCheck className="size-4 text-primary" />
        <h2 className="text-lg font-semibold tracking-tight">Connect &amp; maintain</h2>
      </div>
      <p className="-mt-3 text-sm text-muted-foreground">
        Endpoints, provider limits, tunnel, LAN, and updates. Open a section when you need it.
      </p>

      <Panel title="Agent endpoints" summary={publicUrl ? "local + public" : "local only"}>
        <p className="text-xs text-muted-foreground">
          {authed
            ? "Both endpoints speak OpenAI and Anthropic protocols; every request needs a Jevonian key."
            : "OpenAI- and Anthropic-compatible. No keys exist yet, so requests are accepted without authentication — create one on the Keys page."}
        </p>
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="secondary">local</Badge>
            <code className="rounded-md bg-muted px-3 py-2 text-sm">{localUrl}</code>
            <Button variant="outline" size="sm" onClick={() => void copy(localUrl, "local-ops")}>
              {copied === "local-ops" ? "Copied" : "Copy"}
            </Button>
          </div>
          <details>
            <summary className="cursor-pointer text-xs text-muted-foreground">curl example</summary>
            <pre className="mt-2 overflow-auto rounded-md bg-muted p-3 text-xs">
              {curl(localUrl)}
            </pre>
          </details>
        </div>
        <div className="flex flex-col gap-2 border-t pt-3">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={publicUrl ? "default" : "outline"}>
              public{publicUrl ? "" : " · off"}
            </Badge>
            {publicUrl ? (
              <>
                <code className="rounded-md bg-muted px-3 py-2 text-sm">{publicUrl}</code>
                <Button variant="outline" size="sm" onClick={() => void copy(publicUrl, "public")}>
                  {copied === "public" ? "Copied" : "Copy"}
                </Button>
              </>
            ) : (
              <span className="text-xs text-muted-foreground">
                Start a tunnel under “Public tunnel” below to publish this machine.
              </span>
            )}
          </div>
        </div>
        <p className="text-xs text-muted-foreground">
          Prefer <code>jevonian/auto</code>. Tier setup lives on{" "}
          <Link to="/models#task-routes" className="underline hover:text-foreground">
            Models &amp; Routing
          </Link>
          .
        </p>
      </Panel>

      <Panel
        title="Provider availability"
        summary={`${quotas.length} provider${quotas.length === 1 ? "" : "s"}`}
      >
        <QuotaGrid quotas={quotas} health={health} bare />
        <details className="border-t pt-3">
          <summary className="cursor-pointer text-xs text-muted-foreground">
            Savings and baseline
          </summary>
          <div className="mt-2 flex flex-col gap-1 text-xs text-muted-foreground">
            {stats.apiBaselineUsd > 0 ? (
              <>
                <p className="text-foreground">
                  {stats.savingsPct.toFixed(1)}% lower on pay-per-token traffic (
                  {money(stats.savingsUsd)} saved) against baseline model{" "}
                  <code>{stats.baselineModel ?? "unknown"}</code>.
                </p>
                <p>
                  Baseline: the same tokens priced at{" "}
                  <code>{stats.baselineModel ?? "the configured baseline"}</code> (
                  {money(stats.apiBaselineUsd)} estimated) instead of what was actually paid (
                  {money(stats.apiUsd)}).
                </p>
              </>
            ) : (
              <p>
                No pay-per-token traffic yet
                {stats.subscriptionBaselineUsd > 0
                  ? `; ${money(stats.subscriptionBaselineUsd)} of baseline usage ran on subscription plans.`
                  : "."}
              </p>
            )}
          </div>
        </details>
      </Panel>

      <Panel
        title="Jevonian updates"
        summary={
          update?.update?.updateAvailable
            ? `v${update.update.latest} available`
            : `v${update?.update?.current ?? "—"}`
        }
      >
        <div className="flex flex-wrap items-center gap-3">
          {(() => {
            const status = update?.update;
            const restartOnly = Boolean(
              status?.restartRequired &&
              (!status.updateAvailable || status.installed === status.latest),
            );
            const needsAction = Boolean(status?.updateAvailable || status?.restartRequired);
            return (
              <>
                <Badge variant={needsAction ? "default" : "outline"}>
                  {update?.active
                    ? `restarting · ${update.activeRequests ?? 0} active`
                    : restartOnly
                      ? `v${status!.installed} ready · restart`
                      : status?.updateAvailable
                        ? `v${status.latest} available`
                        : `v${status?.current ?? "—"} · current`}
                </Badge>
                {status?.channel !== "source" && status?.channel !== "unknown" ? (
                  needsAction ? (
                    <Button
                      size="sm"
                      onClick={() => void installUpdate()}
                      disabled={updateBusy || Boolean(update?.active)}
                    >
                      {updateBusy
                        ? restartOnly
                          ? "Restarting…"
                          : "Installing…"
                        : restartOnly
                          ? "Restart"
                          : "Update and restart"}
                    </Button>
                  ) : (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => void checkUpdate()}
                      disabled={updateBusy}
                    >
                      Check now
                    </Button>
                  )
                ) : (
                  <span className="text-xs text-muted-foreground">
                    Source checkouts update with Git and are never self-updated.
                  </span>
                )}
                {updateError || update?.error ? (
                  <span className="text-xs text-destructive">{updateError || update?.error}</span>
                ) : null}
              </>
            );
          })()}
        </div>
      </Panel>

      <Panel
        title="Public tunnel"
        summary={
          tunnel?.status === "on"
            ? `${tunnel.provider} · on`
            : tunnel?.status === "error"
              ? "error"
              : `${tunnelProvider} · off`
        }
      >
        <p className="text-xs text-muted-foreground">
          Explicit opt-in. Jevonian never publishes this machine on its own.
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <Badge
            variant={
              tunnel?.status === "on"
                ? "default"
                : tunnel?.status === "error"
                  ? "destructive"
                  : "secondary"
            }
          >
            {tunnel?.status ?? "off"}
          </Badge>
          <Select
            value={tunnelProvider}
            onValueChange={(value) => editDraft({ provider: value as TunnelProviderView })}
          >
            <SelectTrigger className="w-56">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="cloudflare">cloudflare (quick tunnel)</SelectItem>
              <SelectItem value="ngrok">ngrok</SelectItem>
              <SelectItem value="custom">custom command</SelectItem>
            </SelectContent>
          </Select>
          {tunnelProvider === "ngrok" ? (
            <Input
              className="max-w-md"
              placeholder="casqued-….ngrok-free.dev (optional static domain)"
              value={tunnelUrl}
              onChange={(event) => editDraft({ url: event.target.value })}
            />
          ) : null}
          {tunnelProvider === "custom" ? (
            <>
              <Input
                className="max-w-md"
                placeholder="cloudflared tunnel run my-named-tunnel"
                value={tunnelCommand}
                onChange={(event) => editDraft({ command: event.target.value })}
              />
              <Input
                className="max-w-xs"
                placeholder="https://ai.example.com (stable URL, optional)"
                value={tunnelUrl}
                onChange={(event) => editDraft({ url: event.target.value })}
              />
            </>
          ) : null}
          <Button onClick={() => void toggleTunnel()} disabled={tunnelBusy}>
            {tunnel?.status === "on" ? "Stop tunnel" : "Start tunnel"}
          </Button>
          {draftUnsaved ? (
            <span className="text-[11px] font-medium text-amber-600">unsaved draft</span>
          ) : null}
        </div>
        {tunnel?.url ? (
          <div className="flex items-center gap-2">
            <code className="rounded-md bg-muted px-3 py-2 text-sm">{tunnel.url}/v1</code>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void copy(`${tunnel.url}/v1`, "tunnel")}
            >
              {copied === "tunnel" ? "Copied" : "Copy"}
            </Button>
          </div>
        ) : null}
        {tunnelError || tunnel?.error ? (
          <p className="text-xs text-destructive">{tunnelError || tunnel?.error}</p>
        ) : null}
        <p className="text-[11px] text-amber-600">
          Security: a tunnel exposes this proxy to the internet. Keep at least one API key active
          and stop the tunnel when you are done.
        </p>
      </Panel>

      <Panel title="LAN access" summary={lan?.config.enabled ? "on" : "off"}>
        <p className="text-xs text-muted-foreground">
          Let another machine on this network use this instance. Only <code>/v1</code> is served.
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => void toggleLan()} disabled={lanBusy}>
            {lan?.config.enabled ? "Disable" : "Enable"}
          </Button>
          <span className="text-[11px] text-muted-foreground">
            {lan?.bindHost ?? "0.0.0.0"}:{lan?.port ?? "—"}
            {lan?.restartRequired ? " · restart Jevonian to apply" : ""}
          </span>
        </div>
        {(lan?.urls.length ?? 0) > 0 ? (
          <div className="flex flex-col gap-2">
            {lan?.urls.map((url) => (
              <div key={url} className="flex items-center gap-2">
                <code className="rounded-md bg-muted px-3 py-2 text-sm">{url}</code>
                <Button variant="outline" size="sm" onClick={() => void copy(url, url)}>
                  {copied === url ? "Copied" : "Copy"}
                </Button>
              </div>
            ))}
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">
            No non-loopback IPv4 address was found on this machine.
          </p>
        )}
        {lanError ? <p className="text-xs text-destructive">{lanError}</p> : null}
      </Panel>

      <Panel
        title="Model routing"
        summary={`mode ${state.config.routing.mode} · pricing ${state.pricing.source}`}
      >
        <div className="flex flex-col gap-2 text-sm">
          {(state.routings ?? state.config.routing.routings ?? []).map((entry) => (
            <div key={entry.id} className="flex justify-between gap-4">
              <span className="text-muted-foreground">{entry.label || entry.id}</span>
              <span className="text-right">{entry.models.join(", ") || "—"}</span>
            </div>
          ))}
        </div>
      </Panel>

      <Panel title="Spend by routing" summary={`${stats.byPhase.length} routings with traffic`}>
        <div className="flex flex-col gap-2 text-sm">
          {stats.byPhase.length === 0 ? (
            <p className="text-muted-foreground">No traffic yet.</p>
          ) : (
            stats.byPhase.map((phase) => (
              <div key={phase.phase} className="flex justify-between">
                <span className="text-muted-foreground">
                  {phase.phase} · {phase.requests} reqs
                </span>
                <span>{money(phase.costUsd)}</span>
              </div>
            ))
          )}
        </div>
      </Panel>

      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <Gauge className="size-3.5" />
        <span>
          Dashboard uses today / 7d / 30d activity windows. All-time ledger totals stay in the
          sections above when opened.
        </span>
      </div>
    </div>
  );
}
