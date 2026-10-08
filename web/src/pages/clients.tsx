import { Badge, Banner, Button, LayerCard, Loader, Text } from "@cloudflare/kumo";
import { ArrowsClockwise, Laptop, Warning } from "@phosphor-icons/react";
import { useCallback, useEffect, useState } from "react";

import { ClientsSkeleton } from "@/components/page-skeletons";
import { ProviderLogo } from "@/components/provider-logo";
import {
  connectClient,
  disconnectClient,
  isRestartRequired,
  type ClientIdView,
  type ClientSurfaceView,
  type ClientTargetView,
  type ClientsResponse,
} from "@/lib/api";

interface PendingRestart {
  id: ClientIdView;
  action: "connect" | "disconnect";
  message: string;
}

const STATUS_LABEL: Record<ClientTargetView["status"], string> = {
  connected: "Connected",
  disconnected: "Not connected",
  unavailable: "Unavailable",
};

/** Status pill for a client or one of its surfaces. */
function StatusBadge({ status }: { status: ClientTargetView["status"] }) {
  if (status === "connected") {
    return (
      <Badge variant="success" appearance="dot" className="shrink-0">
        {STATUS_LABEL.connected}
      </Badge>
    );
  }
  if (status === "disconnected") {
    return (
      <Badge variant="neutral" appearance="dot" className="shrink-0">
        {STATUS_LABEL.disconnected}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="shrink-0">
      {STATUS_LABEL.unavailable}
    </Badge>
  );
}

function surfaceLine(surfaces: ClientSurfaceView[] | undefined): string | null {
  if (!surfaces || surfaces.length === 0) return null;
  return surfaces.map((surface) => `${surface.label}: ${STATUS_LABEL[surface.status]}`).join(" · ");
}

export function ClientsPage() {
  const [data, setData] = useState<ClientsResponse | null>(null);
  const [busy, setBusy] = useState<ClientIdView | null>(null);
  const [pending, setPending] = useState<PendingRestart | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setData((await (await fetch("/api/clients")).json()) as ClientsResponse);
      setError(null);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const run = useCallback(
    async (id: ClientIdView, action: "connect" | "disconnect", restart: boolean) => {
      setBusy(id);
      setError(null);
      setNotice(null);
      try {
        const result =
          action === "connect"
            ? await connectClient(id, restart)
            : await disconnectClient(id, restart);

        if (isRestartRequired(result)) {
          setPending({ id, action, message: result.message });
          return;
        }

        setPending(null);

        if (action === "connect" && "result" in result) {
          if (id === "claude") {
            setNotice(
              result.result.restarted
                ? "Claude Desktop and Claude Code are connected. Desktop was restarted; open a new `claude` session for the CLI."
                : "Claude Desktop and Claude Code are connected. Reopen Desktop and start a new `claude` session for the CLI.",
            );
          } else {
            setNotice(
              result.result.restarted
                ? "Profile applied and ChatGPT was restarted."
                : "Profile applied. It takes effect the next time you open the app.",
            );
          }
        } else {
          setNotice("Restored the client's normal profile.");
        }
        await refresh();
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        setBusy(null);
      }
    },
    [refresh],
  );

  if (error && !data) {
    return (
      <div className="flex flex-col gap-6">
        <Text variant="heading" size="lg" as="h1">
          Clients
        </Text>
        <Banner variant="error" size="sm" icon={<Warning size={16} />} description={error} />
      </div>
    );
  }
  if (!data) return <ClientsSkeleton />;

  const clients = data.clients;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <Text variant="heading" size="lg" as="h1">
            Clients
          </Text>
          <Text variant="secondary" size="sm">
            Point coding agents on this machine at Jevonian. Claude Connect covers Desktop and the
            CLI together; ChatGPT covers the Codex desktop app. Changes can be reverted at any time.
          </Text>
        </div>
        <Button
          variant="outline"
          size="sm"
          icon={<ArrowsClockwise size={16} />}
          onClick={() => void refresh()}
          disabled={busy !== null}
        >
          Refresh
        </Button>
      </div>

      <div className="flex items-center gap-2 rounded-lg border border-kumo-hairline bg-kumo-tint/40 px-3 py-2 text-xs text-kumo-subtle">
        <Laptop size={14} className="shrink-0" />
        <span>
          Config files are written on{" "}
          <span className="font-medium text-kumo-default">{data.hostname}</span> ({data.platform}).
          Run the dashboard on the machine whose apps you want to connect.
        </span>
      </div>

      {error ? (
        <Banner variant="error" size="sm" icon={<Warning size={16} />} description={error} />
      ) : null}

      {notice ? <Banner variant="secondary" size="sm" description={notice} /> : null}

      {pending ? (
        <LayerCard className="ring-kumo-brand/40">
          <LayerCard.Secondary className="block">
            <span className="flex flex-col gap-1">
              <Text variant="heading" as="h2">
                Restart {clients.find((client) => client.id === pending.id)?.label ?? "the app"}?
              </Text>
              <Text variant="secondary" size="sm">
                {pending.message} Restarting closes the app now — any running task will stop.
              </Text>
            </span>
          </LayerCard.Secondary>
          <LayerCard.Primary className="flex gap-2">
            <Button
              size="sm"
              icon={busy ? <Loader size="sm" /> : undefined}
              disabled={busy !== null}
              onClick={() => void run(pending.id, pending.action, true)}
            >
              Restart and apply
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setPending(null)}
              disabled={busy !== null}
            >
              Cancel
            </Button>
          </LayerCard.Primary>
        </LayerCard>
      ) : null}

      <div className="flex flex-col gap-4">
        {clients.map((client) => {
          const surfaces = surfaceLine(client.surfaces);
          return (
            <LayerCard key={client.id}>
              <LayerCard.Secondary className="justify-between">
                <div className="flex min-w-0 items-center gap-3">
                  <ProviderLogo id={client.logo} className="size-6 shrink-0" />
                  <div className="min-w-0">
                    <Text variant="heading" truncate>
                      {client.label}
                    </Text>
                    {surfaces ? (
                      <Text variant="secondary" size="xs" truncate>
                        {surfaces}
                      </Text>
                    ) : client.baseUrl ? (
                      <span className="block truncate font-mono text-xs text-kumo-subtle">
                        {client.baseUrl}
                      </span>
                    ) : client.configPath ? (
                      <span className="block truncate font-mono text-xs text-kumo-subtle">
                        {client.configPath}
                      </span>
                    ) : null}
                  </div>
                </div>
                <StatusBadge status={client.status} />
              </LayerCard.Secondary>
              <LayerCard.Primary className="flex flex-col gap-3">
                {client.reason ? (
                  <Text variant="secondary" size="sm">
                    {client.reason}
                  </Text>
                ) : null}
                {client.id === "claude" && client.status !== "unavailable" ? (
                  <Text variant="secondary" size="sm">
                    Connect rewrites Claude Desktop&apos;s gateway profile and Claude Code&apos;s{" "}
                    <span className="font-mono text-xs">~/.claude/settings.json</span>. You can also
                    run <span className="font-mono text-xs">jevonian launch claude</span> for a
                    one-shot CLI session.
                  </Text>
                ) : null}
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    icon={busy === client.id ? <Loader size="sm" /> : undefined}
                    disabled={
                      client.status === "unavailable" ||
                      client.status === "connected" ||
                      busy !== null
                    }
                    onClick={() => void run(client.id, "connect", false)}
                  >
                    Connect
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={
                      busy !== null ||
                      !(
                        client.status === "connected" ||
                        client.surfaces?.some((surface) => surface.status === "connected")
                      )
                    }
                    onClick={() => void run(client.id, "disconnect", false)}
                  >
                    Restore
                  </Button>
                </div>
              </LayerCard.Primary>
            </LayerCard>
          );
        })}
      </div>
    </div>
  );
}
