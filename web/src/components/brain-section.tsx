import { Badge, Button, Input, LayerCard, Select, Table, Text } from "@cloudflare/kumo";
import { useCallback, useState } from "react";

import { KeysHelp } from "@/components/keys-help";
import { api, type BrainChannelView, type BrainView, type StateResponse } from "@/lib/api";

const ADD_TEMPLATE: BrainView = {
  channel: "typesafe",
  timeoutMs: 1_500,
  minConfidence: 0.6,
};

function channelOf(channels: BrainChannelView[], id: string): BrainChannelView | undefined {
  return channels.find((channel) => channel.id === id);
}

function keyLabel(view: { keySource?: string }): string {
  if (view.keySource && view.keySource !== "none") return view.keySource;
  return "none";
}

export function BrainSection({
  state,
  onSaved,
}: {
  state: StateResponse;
  onSaved: (state: StateResponse) => void;
}) {
  const brains = state.config.routing.brains ?? [];
  const channels = state.brainChannels;
  const [editing, setEditing] = useState<number | "new" | null>(null);
  const [draft, setDraft] = useState<BrainView>(ADD_TEMPLATE);
  const [key, setKey] = useState("");
  const [result, setResult] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  const saved = typeof editing === "number" ? brains[editing] : undefined;
  const active = channelOf(channels, draft.channel);
  const modelPresets = active?.models ?? [];
  const selectedModelHint = modelPresets.find((option) => option.id === draft.model)?.hint;
  // A channel with presets can still run an unlisted id typed into the advanced field; keep it
  // visible in the picker instead of showing an empty trigger.
  const customModel =
    (draft.model ?? "") !== "" && !modelPresets.some((option) => option.id === draft.model);
  const modelItems = [
    ...(customModel ? [{ value: draft.model ?? "", label: `${draft.model} (custom)` }] : []),
    ...modelPresets.map((option) => ({ value: option.id, label: option.label })),
  ];
  const dirty =
    editing === "new" ||
    key.length > 0 ||
    draft.channel !== saved?.channel ||
    (draft.baseUrl ?? "") !== (saved?.baseUrl ?? "") ||
    (draft.accountId ?? "") !== (saved?.accountId ?? "") ||
    (draft.model ?? "") !== (saved?.model ?? "") ||
    (draft.apiKeyEnv ?? "") !== (saved?.apiKeyEnv ?? "") ||
    draft.timeoutMs !== saved?.timeoutMs ||
    draft.minConfidence !== saved?.minConfidence ||
    Boolean(draft.fullPrompt) !== Boolean(saved?.fullPrompt);

  const applyChannels = useCallback(
    (next: BrainView[]) => {
      onSaved({
        ...state,
        config: { ...state.config, routing: { ...state.config.routing, brains: next } },
      });
    },
    [state, onSaved],
  );

  function beginAdd(): void {
    setDraft(ADD_TEMPLATE);
    setKey("");
    setResult("");
    setError("");
    setMessage("");
    setEditing("new");
  }

  function beginEdit(index: number): void {
    const current = brains[index];
    if (!current) return;
    setDraft({ ...current });
    setKey("");
    setResult("");
    setError("");
    setMessage("");
    setEditing(index);
  }

  const applyChannel = useCallback(
    (id: string) => {
      const preset = channelOf(channels, id);
      setDraft((current) => ({
        ...current,
        channel: id,
        baseUrl: preset?.requiresAccountId ? "" : (preset?.baseUrl ?? ""),
        accountId: preset?.requiresAccountId
          ? current.channel === id
            ? current.accountId
            : ""
          : "",
        model: preset?.model ?? "",
        apiKeyEnv: preset?.apiKeyEnv ?? "",
      }));
      setKey("");
      setResult("");
    },
    [channels],
  );

  async function save() {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const payload = { ...draft, ...(key ? { apiKey: key } : {}) };
      const response =
        editing === "new"
          ? await api.addBrain(payload)
          : await api.updateBrain(editing as number, payload);
      applyChannels(response.brains);
      setKey("");
      setResult("");
      setEditing(null);
      setMessage("Brain saved");
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function test() {
    setBusy(true);
    setResult("");
    setError("");
    try {
      const response = await api.testBrain({
        ...draft,
        ...(key ? { apiKey: key } : {}),
      });
      const latency =
        typeof response.latencyMs === "number" ? `${response.latencyMs}ms` : undefined;
      if (response.ok && response.verdict) {
        const ranking = Object.entries(response.verdict.probabilities ?? {})
          .sort(([, left], [, right]) => right - left)
          .map(([option, score]) => `${option} ${(score * 100).toFixed(0)}%`)
          .join(" · ");
        const detail = ranking
          ? ranking
          : `chose ${response.verdict.model} · confidence ${(response.verdict.confidence * 100).toFixed(0)}%`;
        setResult([response.channel ?? "brain", latency, detail].filter(Boolean).join(" · "));
      } else {
        setResult([latency, response.error ?? "no verdict"].filter(Boolean).join(" · "));
      }
    } catch (cause) {
      setResult(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function remove(index: number) {
    if (
      !window.confirm(
        "Remove this brain? Brains after it move up; the stored key is deleted when no other brain uses that channel.",
      )
    ) {
      return;
    }
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const response = await api.deleteBrain(index);
      applyChannels(response.brains);
      if (editing === index) setEditing(null);
      setMessage("Brain removed");
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function move(index: number, direction: "up" | "down") {
    setBusy(true);
    setError("");
    try {
      const response = await api.moveBrain(index, direction);
      applyChannels(response.brains);
      setMessage("Fallback order updated");
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  return (
    <LayerCard id="routing-brain">
      <LayerCard.Secondary className="flex-row items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <Text variant="heading" as="h2">
            Routing brain
          </Text>
          <Text variant="secondary" size="sm">
            Tried top to bottom on every routed turn; the first confident verdict wins. Adding one
            is required before <code>jevonian/auto</code> can route.
          </Text>
        </div>
        {editing === null ? (
          <Button variant="outline" size="sm" onClick={beginAdd} disabled={busy}>
            Add brain
          </Button>
        ) : null}
      </LayerCard.Secondary>
      <LayerCard.Primary className="flex flex-col gap-5">
        <div className="overflow-x-auto">
          <Table>
            <Table.Header>
              <Table.Row>
                <Table.Head>Order</Table.Head>
                <Table.Head>Channel</Table.Head>
                <Table.Head>Key</Table.Head>
                <Table.Head />
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {brains.map((brain, index) => (
                <Table.Row key={`${brain.channel}-${index}`}>
                  <Table.Cell className="text-xs text-kumo-subtle">
                    {index + 1}
                    {index === 0 ? " · primary" : ""}
                    {index > 0 ? " · fallback" : ""}
                  </Table.Cell>
                  <Table.Cell className="text-xs text-kumo-subtle">
                    {`${channelOf(channels, brain.channel)?.label ?? brain.channel}${brain.model ? ` · ${brain.model}` : ""}${brain.accountId ? ` · ${brain.accountId}` : ""}`}
                  </Table.Cell>
                  <Table.Cell>
                    <Badge
                      variant={keyLabel(brain) === "none" ? "error" : "secondary"}
                      className="text-xs"
                    >
                      {keyLabel(brain)}
                    </Badge>
                  </Table.Cell>
                  <Table.Cell className="whitespace-nowrap text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => void move(index, "up")}
                      disabled={busy || index === 0}
                    >
                      ↑
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => void move(index, "down")}
                      disabled={busy || index === brains.length - 1}
                    >
                      ↓
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => beginEdit(index)}
                      disabled={busy}
                    >
                      Edit
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-kumo-danger hover:text-kumo-danger"
                      onClick={() => void remove(index)}
                      disabled={busy}
                    >
                      Remove
                    </Button>
                  </Table.Cell>
                </Table.Row>
              ))}
              {brains.length === 0 ? (
                <Table.Row>
                  <Table.Cell colSpan={4} className="text-sm text-kumo-subtle">
                    No brain configured — jevonian/auto is disabled until you add one.
                  </Table.Cell>
                </Table.Row>
              ) : null}
            </Table.Body>
          </Table>
        </div>

        {editing === null ? (
          <div className="flex items-center gap-3">
            {brains.length > 0 ? (
              <span className="text-xs text-kumo-subtle">
                Tried in this order; add more for redundancy.
              </span>
            ) : null}
            {message ? <span className="text-xs text-kumo-subtle">{message}</span> : null}
            {error ? <span className="text-xs text-kumo-danger">{error}</span> : null}
          </div>
        ) : (
          <>
            <div className="flex flex-col gap-4">
              <div className="grid grid-cols-2 gap-4">
                <Select
                  id="brainChannel"
                  className="w-full"
                  label="Channel"
                  items={channels.map((channel) => ({
                    value: channel.id,
                    label: channel.label,
                  }))}
                  value={draft.channel}
                  onValueChange={(value) => applyChannel(String(value))}
                  description="Where the brain asks for a verdict; picking a channel loads its defaults."
                />
                <div className="flex flex-col gap-1.5">
                  <Input
                    id="brainKey"
                    label="API key"
                    type="password"
                    value={key}
                    placeholder={
                      saved?.keySource && saved.keySource !== "none"
                        ? `set (${saved.keySource}) — paste to replace`
                        : "stored per channel (0600)"
                    }
                    onValueChange={(value) => setKey(value)}
                  />
                  <KeysHelp keysUrl={active?.keysUrl} hint={active?.hint} />
                  <span className="text-xs text-kumo-subtle">
                    Stored encrypted on this machine; leave empty to keep the stored key.
                  </span>
                </div>
                {active?.requiresAccountId ? (
                  <div className="col-span-2 flex flex-col gap-1.5">
                    <Input
                      id="brainAccountId"
                      label="Account ID"
                      value={draft.accountId ?? ""}
                      placeholder="Cloudflare account id from the dashboard overview"
                      onValueChange={(value) => setDraft({ ...draft, accountId: value })}
                    />
                    <span className="text-xs text-kumo-subtle">
                      Used to call{" "}
                      <code className="text-xs">/client/v4/accounts/{"{id}"}/ai/run</code> with
                      model{" "}
                      <code className="text-xs">
                        {draft.model || active.model || "typesafe/jev"}
                      </code>
                      .
                    </span>
                  </div>
                ) : null}
                {modelPresets.length > 0 ? (
                  <div className="col-span-2 flex flex-col gap-1.5">
                    <Select
                      id="brainModelPreset"
                      className="w-full"
                      label="Model"
                      items={modelItems}
                      value={draft.model ?? ""}
                      onValueChange={(value) => setDraft({ ...draft, model: String(value) })}
                      placeholder="Pick a decision model"
                      renderValue={(value) =>
                        modelPresets.find((option) => option.id === value)?.label ??
                        (value ? `${value} (custom)` : undefined)
                      }
                    />
                    <span className="text-xs text-kumo-subtle">
                      {selectedModelHint ??
                        (customModel
                          ? "A model id not on this channel's preset list."
                          : `Served by ${active?.label ?? draft.channel}.`)}
                    </span>
                  </div>
                ) : null}
              </div>

              <details className="rounded-md border border-kumo-hairline">
                <summary className="cursor-pointer select-none px-4 py-2 text-sm font-medium">
                  Advanced settings
                </summary>
                <div className="flex flex-col gap-4 border-t border-kumo-hairline p-4">
                  <div className="grid grid-cols-2 gap-4">
                    {active?.requiresAccountId ? null : (
                      <div className="flex flex-col gap-1.5">
                        <Input
                          id="brainBaseUrl"
                          label="Endpoint"
                          value={draft.baseUrl ?? ""}
                          placeholder={active?.baseUrl || "https://…/v1/systemone"}
                          onValueChange={(value) => setDraft({ ...draft, baseUrl: value })}
                        />
                      </div>
                    )}
                    <div className="flex flex-col gap-1.5">
                      <Input
                        id="brainModel"
                        label={modelPresets.length > 0 ? "Model (override)" : "Model"}
                        value={draft.model ?? ""}
                        placeholder={active?.model ?? "jev-latest"}
                        onValueChange={(value) => setDraft({ ...draft, model: value })}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Input
                        id="brainEnv"
                        label="API key env var"
                        value={draft.apiKeyEnv ?? ""}
                        placeholder={active?.apiKeyEnv ?? "TYPESAFE_API_KEY"}
                        onValueChange={(value) => setDraft({ ...draft, apiKeyEnv: value })}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Input
                        id="brainTimeout"
                        label="Timeout (ms)"
                        type="number"
                        value={draft.timeoutMs}
                        onValueChange={(value) => setDraft({ ...draft, timeoutMs: Number(value) })}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Input
                        id="brainConfidence"
                        label="Min confidence"
                        type="number"
                        min={0}
                        max={1}
                        step={0.05}
                        value={draft.minConfidence}
                        onValueChange={(value) =>
                          setDraft({ ...draft, minConfidence: Number(value) })
                        }
                      />
                      <span className="text-xs text-kumo-subtle">
                        Below this the turn is marked low-confidence. Later brains are only tried
                        when this channel fails.
                      </span>
                    </div>
                    <div className="col-span-2 flex flex-col gap-1.5">
                      <Select
                        id="brainContext"
                        className="w-full"
                        label="Context sent to the brain"
                        items={{
                          compact: "compact — goal, recent turns, tool activity",
                          full: "full prompt — every message verbatim",
                        }}
                        value={draft.fullPrompt ? "full" : "compact"}
                        onValueChange={(value) =>
                          setDraft({ ...draft, fullPrompt: value === "full" })
                        }
                      />
                      <span className="text-xs text-kumo-subtle">
                        compact sends only the goal, recent turns, and tool activity. full prompt
                        sends every message verbatim, which is more private-data exposure but more
                        accurate. This content is sent to the channel above, not stored locally.
                      </span>
                    </div>
                  </div>
                </div>
              </details>
            </div>

            <div className="flex flex-wrap items-center gap-3">
              <Button variant="primary" onClick={() => void save()} disabled={busy || !dirty}>
                {editing === "new" ? "Add brain" : "Save brain"}
              </Button>
              <Button variant="outline" onClick={() => void test()} disabled={busy}>
                Test & measure
              </Button>
              <Button variant="ghost" onClick={() => setEditing(null)} disabled={busy}>
                Cancel
              </Button>
              {dirty ? (
                <span className="text-xs font-medium text-kumo-warning">unsaved changes</span>
              ) : null}
              {result ? <span className="text-xs text-kumo-subtle">{result}</span> : null}
              {error ? <span className="text-xs text-kumo-danger">{error}</span> : null}
            </div>
          </>
        )}
      </LayerCard.Primary>
    </LayerCard>
  );
}
