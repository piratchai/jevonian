import { Badge, Button, Input, LayerCard, Table, Text } from "@cloudflare/kumo";
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router";

import { KeysSkeleton } from "@/components/page-skeletons";
import { api, type KeyView } from "@/lib/api";
import { money } from "@/lib/utils";

/** Parses a limit field. Blank means "no limit"; anything unusable is rejected. */
function parseLimitInput(value: string): { ok: true; limit: number | null } | { ok: false } {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, limit: null };
  const parsed = Number.parseFloat(trimmed.replace(/^\$/, ""));
  if (!Number.isFinite(parsed) || parsed <= 0) return { ok: false };
  return { ok: true, limit: parsed };
}

function usagePercent(spend: number, limit: number | null | undefined): number {
  if (!limit || limit <= 0) return 0;
  return Math.min(100, (spend / limit) * 100);
}

export function KeysPage() {
  const [keys, setKeys] = useState<KeyView[]>([]);
  const [ready, setReady] = useState(false);
  const [name, setName] = useState("my-agent");
  const [newLimit, setNewLimit] = useState("");
  const [createdKey, setCreatedKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState("");
  const [message, setMessage] = useState("");
  const [confirmingId, setConfirmingId] = useState<string | null>(null);
  // Which key's limit is open for editing, and the in-progress value.
  const [editingLimitId, setEditingLimitId] = useState<string | null>(null);
  const [limitDraft, setLimitDraft] = useState("");
  const [limitError, setLimitError] = useState("");

  const load = useCallback(async () => {
    try {
      const state = await api.state();
      setKeys(state.keys);
    } catch (cause) {
      setError(String(cause));
    } finally {
      setReady(true);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function create() {
    setBusy(true);
    setError("");
    setMessage("");
    const parsed = parseLimitInput(newLimit);
    if (!parsed.ok) {
      setError("Credit limit must be a positive number of dollars, or left blank for unlimited.");
      setBusy(false);
      return;
    }
    try {
      const result = await api.createKey(name, parsed.limit);
      setCreatedKey(result.key);
      setCopied(false);
      setCopyError("");
      setNewLimit("");
      await load();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function saveLimit(key: KeyView) {
    const parsed = parseLimitInput(limitDraft);
    if (!parsed.ok) {
      setLimitError("Enter a positive dollar amount, or leave blank for unlimited.");
      return;
    }
    setBusy(true);
    setLimitError("");
    try {
      const result = await api.updateKey(key.id, { limitUsd: parsed.limit });
      setKeys(result.keys);
      setEditingLimitId(null);
      setLimitDraft("");
      setMessage(
        parsed.limit === null
          ? `Removed the credit limit on “${key.name}”.`
          : `Set a $${parsed.limit.toFixed(2)} credit limit on “${key.name}”.`,
      );
      await load();
    } catch (cause) {
      setLimitError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function copyCreatedKey() {
    try {
      await navigator.clipboard.writeText(createdKey);
      setCopyError("");
      setCopied(true);
      setTimeout(() => setCopied(false), 1_500);
    } catch (cause) {
      setCopyError(`Could not copy: ${String(cause)}`);
    }
  }

  async function revoke(key: KeyView) {
    // Capture whether this was the last key *before* awaiting, since the list reloads after.
    const wasLast = keys.length <= 1;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await api.revokeKey(key.id);
      setConfirmingId(null);
      setMessage(
        wasLast
          ? `Revoked the last Jevonian key “${key.name}”. Local /v1 requests are unauthenticated again until you create a new key.`
          : `Revoked key “${key.name}”. Requests using it now fail with 401.`,
      );
      setCreatedKey("");
      await load();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  if (!ready) return <KeysSkeleton />;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <Text variant="heading" size="lg" as="h1">
          Jevonian access keys
        </Text>
        <Text variant="secondary" size="sm">
          Keys Jevonian issues so agents can call this router. They are not upstream provider
          secrets — those live in Providers and stay on this machine.
        </Text>
        <Text variant="secondary" size="sm">
          While at least one key exists, every request to <code>/v1</code> must send it as{" "}
          <code>authorization: Bearer …</code> or <code>x-api-key</code>. With none, local{" "}
          <code>/v1</code> requests are accepted unauthenticated and the tunnel stays disabled.
        </Text>
      </div>

      <LayerCard>
        <LayerCard.Secondary className="block">
          <span className="flex flex-col gap-1">
            <Text variant="heading" as="h2">
              Create key
            </Text>
            <Text variant="secondary" size="sm">
              Give it a name so you can revoke it later. An optional credit limit stops requests
              once estimated pay-as-you-go spend reaches that amount.
            </Text>
          </span>
        </LayerCard.Secondary>
        <LayerCard.Primary className="flex flex-col gap-3">
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-64">
              <Input id="keyName" label="Name" value={name} onValueChange={setName} />
            </div>
            <div className="w-52 shrink-0">
              <Input
                id="keyLimit"
                label="Credit limit (USD)"
                inputMode="decimal"
                placeholder="unlimited"
                value={newLimit}
                onValueChange={setNewLimit}
              />
            </div>
            <Button variant="primary" onClick={() => void create()} disabled={busy || !name}>
              Generate key
            </Button>
          </div>
          {createdKey ? (
            <div className="rounded-lg border border-kumo-warning/40 bg-kumo-warning-tint p-3 text-sm">
              <p className="mb-2 text-xs text-kumo-subtle">Copy it now — it is shown only once.</p>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded-md bg-kumo-base px-2 py-1 text-xs">
                  {createdKey}
                </code>
                <Button size="sm" variant="outline" onClick={() => void copyCreatedKey()}>
                  {copied ? "Copied" : "Copy"}
                </Button>
              </div>
              {copied ? (
                <p className="mt-2 text-xs text-kumo-subtle">Copied to clipboard.</p>
              ) : null}
              {copyError ? <p className="mt-2 text-xs text-kumo-danger">{copyError}</p> : null}
            </div>
          ) : null}
          {message ? <span className="text-xs text-kumo-subtle">{message}</span> : null}
          {error ? <span className="text-xs text-kumo-danger">{error}</span> : null}
        </LayerCard.Primary>
      </LayerCard>

      <LayerCard>
        <LayerCard.Secondary className="block">
          <span className="flex flex-col gap-1">
            <Text variant="heading" as="h2">
              Active keys
            </Text>
            <Text variant="secondary" size="sm">
              {keys.length === 0
                ? "No keys yet"
                : `${keys.length} key${keys.length === 1 ? "" : "s"} · usage is estimated from the local ledger`}
            </Text>
          </span>
        </LayerCard.Secondary>
        <LayerCard.Primary className="overflow-x-auto">
          <Table>
            <Table.Header>
              <Table.Row>
                <Table.Head>Name</Table.Head>
                <Table.Head>Prefix</Table.Head>
                <Table.Head className="text-right">Key usage</Table.Head>
                <Table.Head>Key limit</Table.Head>
                <Table.Head>Last used</Table.Head>
                <Table.Head className="text-right">Requests</Table.Head>
                <Table.Head />
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {keys.map((key) => {
                const spend = key.spendUsd ?? 0;
                const subscription = key.subscriptionUsd ?? 0;
                const limit = key.limitUsd ?? null;
                const pct = usagePercent(spend, limit);
                const overLimit = limit !== null && spend >= limit;
                const nearLimit = limit !== null && !overLimit && pct >= 80;
                return (
                  <Table.Row key={key.id}>
                    <Table.Cell className="font-medium">
                      <div className="flex items-center gap-2">
                        <span>{key.name}</span>
                        {overLimit ? (
                          <Badge variant="error" className="text-xs">
                            limit reached
                          </Badge>
                        ) : null}
                      </div>
                    </Table.Cell>
                    <Table.Cell className="text-xs text-kumo-subtle">{key.prefix}…</Table.Cell>
                    <Table.Cell className="text-right">
                      <span className="font-mono text-xs font-medium">{money(spend)}</span>
                      {subscription > 0 ? (
                        <span className="ml-1 text-xs text-kumo-subtle">
                          +{money(subscription)} sub
                        </span>
                      ) : null}
                    </Table.Cell>
                    <Table.Cell className="min-w-[190px]">
                      {editingLimitId === key.id ? (
                        <div className="flex flex-col gap-1">
                          <div className="flex items-center gap-1.5">
                            <Input
                              autoFocus
                              aria-label="Credit limit (USD)"
                              inputMode="decimal"
                              placeholder="unlimited"
                              className="h-8 w-28 text-xs"
                              value={limitDraft}
                              onValueChange={setLimitDraft}
                              onKeyDown={(event) => {
                                if (event.key === "Enter") void saveLimit(key);
                                if (event.key === "Escape") {
                                  setEditingLimitId(null);
                                  setLimitDraft("");
                                  setLimitError("");
                                }
                              }}
                            />
                            <Button
                              size="sm"
                              className="h-8"
                              onClick={() => void saveLimit(key)}
                              disabled={busy}
                            >
                              Save
                            </Button>
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-8"
                              onClick={() => {
                                setEditingLimitId(null);
                                setLimitDraft("");
                                setLimitError("");
                              }}
                              disabled={busy}
                            >
                              Cancel
                            </Button>
                          </div>
                          {limitError ? (
                            <span className="text-xs text-kumo-danger">{limitError}</span>
                          ) : null}
                        </div>
                      ) : (
                        <button
                          type="button"
                          className="flex w-full flex-col items-start gap-1.5 text-left"
                          onClick={() => {
                            setEditingLimitId(key.id);
                            setLimitDraft(limit === null ? "" : String(limit));
                            setLimitError("");
                          }}
                          title="Click to edit the credit limit"
                        >
                          <span className="flex items-center gap-2">
                            <span className="border-b border-dotted border-kumo-subtle/60 font-mono text-xs">
                              {limit === null
                                ? "unlimited"
                                : `$${limit.toFixed(limit % 1 === 0 ? 0 : 2)}`}
                            </span>
                            <Badge variant="outline" className="h-5 px-1.5 text-xs tracking-wider">
                              TOTAL
                            </Badge>
                          </span>
                          <span className="h-1 w-full overflow-hidden rounded-full bg-kumo-tint">
                            <span
                              className={`block h-full rounded-full ${
                                overLimit
                                  ? "bg-kumo-danger"
                                  : nearLimit
                                    ? "bg-kumo-warning"
                                    : "bg-kumo-contrast"
                              }`}
                              style={{
                                width: `${limit === null ? 0 : Math.max(pct, spend > 0 ? 2 : 0)}%`,
                              }}
                            />
                          </span>
                        </button>
                      )}
                    </Table.Cell>
                    <Table.Cell className="text-xs">
                      {key.lastUsedAt ? new Date(key.lastUsedAt).toLocaleString() : "never"}
                    </Table.Cell>
                    <Table.Cell className="text-right text-xs">{key.requests}</Table.Cell>
                    <Table.Cell className="whitespace-nowrap text-right">
                      {confirmingId === key.id ? (
                        <span className="flex items-center justify-end gap-2 text-xs">
                          <span className="text-kumo-subtle">
                            {keys.length <= 1
                              ? "Revoke? Local /v1 requests become unauthenticated again."
                              : "Revoke this key? Requests using it start failing with 401."}
                          </span>
                          <Button
                            variant="destructive"
                            size="sm"
                            onClick={() => void revoke(key)}
                            disabled={busy}
                          >
                            Revoke
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => setConfirmingId(null)}
                            disabled={busy}
                          >
                            Cancel
                          </Button>
                        </span>
                      ) : (
                        <span className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => setConfirmingId(key.id)}
                            disabled={busy}
                          >
                            Revoke
                          </Button>
                        </span>
                      )}
                    </Table.Cell>
                  </Table.Row>
                );
              })}
              {keys.length === 0 ? (
                <Table.Row>
                  <Table.Cell colSpan={7} className="text-sm text-kumo-subtle">
                    No keys yet — requests are currently accepted without authentication.
                  </Table.Cell>
                </Table.Row>
              ) : null}
            </Table.Body>
          </Table>
          <p className="mt-3 text-xs text-kumo-subtle">
            Usage is estimated from ledger records attributed to each key. See{" "}
            <Link to="/#activity" className="underline hover:text-kumo-default">
              Activity
            </Link>{" "}
            for per-model and time-series breakdowns.
          </p>
        </LayerCard.Primary>
      </LayerCard>
    </div>
  );
}
