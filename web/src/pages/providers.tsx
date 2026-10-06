import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { BrainSection } from "@/components/brain-section";
import { KeysHelp } from "@/components/keys-help";
import { ProvidersSkeleton } from "@/components/page-skeletons";
import { ProviderLogo } from "@/components/provider-logo";
import { QuotaGrid } from "@/components/quota-card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  api,
  type ModelSyncResponse,
  type OAuthSourceView,
  type PresetView,
  type PriceInfo,
  type ProviderAuthView,
  type ProviderBillingView,
  type ProviderLoginView,
  type ProviderQuotaView,
  type ProviderTypeView,
  type ProviderView,
  type QuotaHealthView,
  type StateResponse,
} from "@/lib/api";
import { cn } from "@/lib/utils";

const CUSTOM_PRESET: PresetView = {
  id: "custom",
  name: "Custom",
  type: "openai",
  baseUrl: "",
  hint: "",
};

const AUTO_SELECT_LIMIT = 30;

/**
 * Display name and an example config directory for each OAuth sign-in source, so the
 * second-account fields describe the credential the provider actually reads.
 */
const LOGIN_SOURCE_HINTS: Record<string, { name: string; home: string; file?: string }> = {
  "claude-code": { name: "Claude Code", home: "~/.claude-work" },
  codex: { name: "Codex", home: "~/.codex-work" },
  devin: { name: "Devin", home: "~/.local/share/devin-work" },
  cursor: { name: "Cursor", home: "~/.cursor-work" },
  "workbuddy-ai": {
    name: "WorkBuddy AI",
    home: "",
    file: "~/.config/jevonian/workbuddy-ai-work.json",
  },
  // Antigravity redirects via a credential file or keychain entry, not a config directory.
  antigravity: { name: "Antigravity", home: "" },
};

function numberOrUndefined(value: string): number | undefined {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

export function ProvidersPage() {
  const [state, setState] = useState<StateResponse | null>(null);
  const [quotas, setQuotas] = useState<ProviderQuotaView[]>([]);
  const [health, setHealth] = useState<QuotaHealthView[]>([]);
  const [presetId, setPresetId] = useState("deepseek");
  const [name, setName] = useState("deepseek");
  const [type, setType] = useState("openai");
  const [auth, setAuth] = useState<ProviderAuthView>("api-key");
  const [oauthSource, setOauthSource] = useState<OAuthSourceView>("claude-code");
  const [billing, setBilling] = useState<ProviderBillingView>("api");
  const [baseUrl, setBaseUrl] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [apiKeyEnv, setApiKeyEnv] = useState("");
  const [loginLabel, setLoginLabel] = useState("");
  const [loginHome, setLoginHome] = useState("");
  const [loginFile, setLoginFile] = useState("");
  const [loginKeychain, setLoginKeychain] = useState("");
  const [quotaFiveHour, setQuotaFiveHour] = useState("");
  const [quotaWeekly, setQuotaWeekly] = useState("");
  const [quotaMonthly, setQuotaMonthly] = useState("");
  const [discovered, setDiscovered] = useState<string[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [prices, setPrices] = useState<Record<string, PriceInfo>>({});
  const [filter, setFilter] = useState("");
  const [customModel, setCustomModel] = useState("");
  // `null` = no override: follow the server's OAuth-source default. Only an explicit choice that
  // differs from that default is persisted, so saving a form never pins the default by accident.
  const [syncOverride, setSyncOverride] = useState<boolean | null>(null);
  const [modelSync, setModelSync] = useState<ModelSyncResponse | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const initialized = useRef(false);
  /** Stable card ref — never call scrollIntoView from an inline ref (that re-fires every render). */
  const formCardRef = useRef<HTMLDivElement | null>(null);
  /** Scroll the form into view once when Add/Edit opens; later clicks must not jump the page. */
  const scrollFormOnce = useRef(false);

  const allPresets = useMemo<PresetView[]>(
    () => [...(state?.presets ?? []), CUSTOM_PRESET],
    [state],
  );
  const presets = useMemo<PresetView[]>(
    () => allPresets.filter((preset) => preset.id !== "custom"),
    [allPresets],
  );
  const activePreset = useMemo(
    () => allPresets.find((preset) => preset.id === presetId),
    [allPresets, presetId],
  );
  /** Preset used for docs links — keeps working when editing (forced to custom). */
  const helpPreset = useMemo(() => {
    if (presetId !== "custom" && activePreset && activePreset.id !== "custom") {
      return activePreset;
    }
    const byId = presets.find((preset) => preset.id === name);
    if (byId) return byId;
    const normalized = baseUrl.replace(/\/+$/, "");
    return presets.find((preset) => preset.baseUrl.replace(/\/+$/, "") === normalized);
  }, [presetId, activePreset, presets, name, baseUrl]);

  const load = useCallback(async () => {
    try {
      const [nextState, nextQuotas, nextSync] = await Promise.all([
        api.state(),
        api.quota(),
        api.modelSync(),
      ]);
      setState(nextState);
      setQuotas(nextQuotas.quotas);
      setHealth(nextQuotas.health);
      setModelSync(nextSync);
    } catch (cause) {
      setError(String(cause));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!formOpen || !scrollFormOnce.current) return;
    scrollFormOnce.current = false;
    formCardRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
  }, [formOpen]);

  const applyPreset = useCallback((preset: PresetView | undefined) => {
    if (!preset) return;
    setPresetId(preset.id);
    setName(preset.id === "custom" ? "" : preset.id);
    setType(preset.type);
    setAuth(preset.auth ?? "api-key");
    setOauthSource(preset.oauthSource ?? "claude-code");
    setBilling(preset.billing ?? "api");
    setBaseUrl(preset.baseUrl);
    setApiKey("");
    setApiKeyEnv(preset.apiKeyEnv ?? "");
    setLoginLabel("");
    setLoginHome("");
    setLoginFile("");
    setLoginKeychain("");
    setQuotaFiveHour("");
    setQuotaWeekly("");
    setQuotaMonthly("");
    setDiscovered([]);
    setSelected([]);
    setFilter("");
    setCustomModel("");
    setSyncOverride(null);
    setEditing(null);
    setMessage("");
    setError("");
  }, []);

  useEffect(() => {
    if (initialized.current || !state) return;
    initialized.current = true;
    applyPreset(allPresets.find((preset) => preset.id === "deepseek") ?? allPresets[0]);
  }, [state, allPresets, applyPreset]);

  const loadPrices = useCallback(async (provider: string) => {
    if (!provider || provider === "custom") {
      setPrices({});
      return;
    }
    try {
      const result = await api.prices(provider);
      setPrices(result.prices);
    } catch {
      setPrices({});
    }
  }, []);

  useEffect(() => {
    void loadPrices(presetId);
  }, [presetId, loadPrices]);

  const visibleModels = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    if (!needle) return discovered;
    return discovered.filter((model) => model.toLowerCase().includes(needle));
  }, [discovered, filter]);

  const extraSelected = useMemo(
    () => selected.filter((model) => !discovered.includes(model)),
    [selected, discovered],
  );

  function resetForm(id = "deepseek") {
    applyPreset(allPresets.find((item) => item.id === id) ?? allPresets[0]);
  }

  function choosePreset(preset: PresetView) {
    if (preset.id === presetId) return;
    if (
      !window.confirm(
        "Switch provider preset? This replaces the current form, including credentials, advanced settings, and selected models. Saved providers are not changed.",
      )
    )
      return;
    applyPreset(preset);
  }

  const lockedType: ProviderTypeView | undefined =
    auth === "oauth" && oauthSource === "claude-code"
      ? "anthropic"
      : auth === "oauth" && oauthSource === "codex"
        ? "responses"
        : auth === "oauth" && oauthSource === "antigravity"
          ? "gemini"
          : auth === "oauth" && oauthSource === "devin"
            ? "devin"
            : auth === "oauth" && oauthSource === "cursor"
              ? "cursor"
              : auth === "oauth" && (oauthSource === "workbuddy-ai" || oauthSource === "freebuff")
                ? "openai"
                : undefined;
  const lockedBy =
    oauthSource === "codex"
      ? "Codex"
      : oauthSource === "antigravity"
        ? "Antigravity"
        : oauthSource === "devin"
          ? "Devin"
          : oauthSource === "cursor"
            ? "Cursor"
            : oauthSource === "workbuddy-ai"
              ? "WorkBuddy AI"
              : oauthSource === "freebuff"
                ? "Freebuff"
                : "Claude Code";
  const effectiveType = lockedType ?? type;
  const syncDefault =
    (auth === "oauth" && (state?.modelSyncDefaultSources ?? []).includes(oauthSource)) ||
    helpPreset?.syncModels === true;
  const syncModels = syncOverride ?? syncDefault;

  async function discover() {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const result = await api.discover({
        name,
        type: effectiveType,
        baseUrl,
        apiKey,
        auth,
        oauthSource,
        login: loginPayload(),
        ...(helpPreset?.noKey ? { noKey: true } : {}),
      });
      if (result.error) setError(result.error);
      setDiscovered(result.models);
      if (result.models.length > 0 && result.models.length <= AUTO_SELECT_LIMIT) {
        setSelected((current) => [...new Set([...current, ...result.models])]);
      }
      if (!prices || Object.keys(prices).length === 0) void loadPrices(presetId);
      setMessage(
        result.signedInAs
          ? `Signed in as ${result.signedInAs} · discovered ${result.models.length} models`
          : `Discovered ${result.models.length} models`,
      );
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function signInWorkbuddy() {
    // Discover already opens browser sign-in when no session exists; reuse that path.
    await discover();
  }

  function toggleModel(model: string) {
    setSelected((current) =>
      current.includes(model) ? current.filter((item) => item !== model) : [...current, model],
    );
  }

  function addCustomModel() {
    const value = customModel.trim();
    if (!value) return;
    setSelected((current) => (current.includes(value) ? current : [...current, value]));
    setCustomModel("");
  }

  async function save() {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const quota = {
        ...(numberOrUndefined(quotaFiveHour)
          ? { fiveHourUsd: numberOrUndefined(quotaFiveHour) }
          : {}),
        ...(numberOrUndefined(quotaWeekly) ? { weeklyUsd: numberOrUndefined(quotaWeekly) } : {}),
        ...(numberOrUndefined(quotaMonthly) ? { monthlyUsd: numberOrUndefined(quotaMonthly) } : {}),
      };
      const result = await api.addProvider({
        name,
        type: effectiveType,
        baseUrl,
        apiKey: apiKey || undefined,
        apiKeyEnv: apiKeyEnv || undefined,
        auth,
        oauthSource: auth === "oauth" ? oauthSource : undefined,
        login: loginPayload(),
        billing,
        quota: Object.keys(quota).length > 0 ? quota : undefined,
        models: selected,
        syncModels: syncOverride,
        ...(helpPreset?.noKey ? { noKey: true } : {}),
      });
      setMessage(
        result.signedInAs
          ? `Saved provider "${name}" · signed in as ${result.signedInAs}`
          : `Saved provider "${name}"`,
      );
      resetForm(presetId);
      await load();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function remove(providerName: string) {
    if (
      !window.confirm(
        `Remove provider "${providerName}"? Stored credentials for it are deleted. This cannot be undone.`,
      )
    )
      return;
    setBusy(true);
    try {
      await api.deleteProvider(providerName);
      if (editing === providerName) resetForm();
      await load();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function refreshQuota() {
    setBusy(true);
    try {
      const result = await api.quota(true);
      setQuotas(result.quotas);
      setHealth(result.health);
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function toggleModelSync(enabled: boolean) {
    setBusy(true);
    setError("");
    try {
      setModelSync(await api.saveModelSync({ enabled }));
      setMessage(enabled ? "Model auto-sync enabled" : "Model auto-sync disabled");
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function runModelSyncNow() {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const result = await api.runModelSync();
      setModelSync(result);
      const added = result.result?.added ?? 0;
      setMessage(
        added > 0
          ? `Synced models: +${added} appended to provider lists`
          : "Model lists are already up to date",
      );
      await load();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  function edit(provider: ProviderView) {
    setPresetId("custom");
    setName(provider.name);
    setType(provider.type);
    setAuth(provider.auth ?? "api-key");
    setOauthSource(provider.oauthSource ?? "claude-code");
    setBilling(provider.billing ?? "api");
    setBaseUrl(provider.baseUrl);
    setApiKey("");
    setApiKeyEnv(provider.apiKeyEnv ?? "");
    setLoginLabel(provider.login?.label ?? "");
    setLoginHome(provider.login?.home ?? "");
    setLoginFile(provider.login?.credentialsPath ?? "");
    setLoginKeychain(
      provider.login?.keychainService
        ? provider.login.keychainAccount
          ? `${provider.login.keychainService}:${provider.login.keychainAccount}`
          : provider.login.keychainService
        : "",
    );
    setQuotaFiveHour(provider.quota?.fiveHourUsd ? String(provider.quota.fiveHourUsd) : "");
    setQuotaWeekly(provider.quota?.weeklyUsd ? String(provider.quota.weeklyUsd) : "");
    setQuotaMonthly(provider.quota?.monthlyUsd ? String(provider.quota.monthlyUsd) : "");
    setDiscovered([]);
    setSelected(provider.models);
    setSyncOverride(typeof provider.syncModels === "boolean" ? provider.syncModels : null);
    setEditing(provider.name);
    scrollFormOnce.current = true;
    setFormOpen(true);
    setMessage("");
    setError("");
  }

  function beginAdd() {
    resetForm();
    scrollFormOnce.current = true;
    setFormOpen(true);
  }

  function closeForm() {
    setFormOpen(false);
    resetForm();
  }

  const priceLabel = (model: string) => {
    const price = prices[model];
    return price ? `$${price.input}/$${price.output} per M` : "";
  };

  const needsApiKey =
    !(helpPreset?.noKey === true) && (auth === "api-key" || oauthSource === "static");
  // Save signs in then discovers when the list is empty — don't force a Discover-first deadlock.
  const canSaveWithoutModels = auth === "oauth" && oauthSource === "workbuddy-ai";

  /**
   * Whether this credential source keeps its sign-in somewhere a `login` can point at. A stored
   * token (`static`) has no local sign-in to redirect, so the account fields stay hidden.
   */
  const loginSource = auth === "oauth" && oauthSource !== "static";
  /** Local sign-in the second-account fields point at: its display name and an example home. */
  const loginHint =
    LOGIN_SOURCE_HINTS[oauthSource] ?? ({ name: "Claude Code", home: "~/.claude-work" } as const);
  const loginSourceName = loginHint.name;

  /** Assemble the `login` payload; `null` clears an existing one, `undefined` sends nothing. */
  function loginPayload(): ProviderLoginView | null | undefined {
    const label = loginLabel.trim();
    const home = loginHome.trim();
    const credentialsPath = loginFile.trim();
    const keychain = loginKeychain.trim();
    const colon = keychain.indexOf(":");
    const keychainService = keychain
      ? colon >= 0
        ? keychain.slice(0, colon).trim()
        : keychain
      : "";
    const keychainAccount = colon >= 0 ? keychain.slice(colon + 1).trim() : "";
    const hasAny = Boolean(label || home || credentialsPath || keychainService || keychainAccount);
    if (!loginSource) return editing ? null : undefined;
    if (!hasAny) return editing ? null : undefined;
    return {
      ...(label ? { label } : {}),
      ...(home ? { home } : {}),
      ...(credentialsPath ? { credentialsPath } : {}),
      ...(keychainService ? { keychainService } : {}),
      ...(keychainAccount ? { keychainAccount } : {}),
    };
  }

  if (error && !state) return <p className="text-sm text-destructive">{error}</p>;
  if (!state) return <ProvidersSkeleton />;

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-lg font-semibold">Providers</h1>
        <p className="text-sm text-muted-foreground">
          Connect API providers, subscription endpoints, and local agent credentials. Secrets stay
          on this machine.
        </p>
      </div>

      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4 space-y-0">
          <div className="flex flex-col gap-1">
            <CardTitle>Model auto-sync</CardTitle>
            <CardDescription>
              While <code>serve</code> is running, Jevonian periodically discovers each
              provider&apos;s model list and appends new ids. Removals stick; fixed routings are
              never rewritten.
            </CardDescription>
          </div>
          <Badge variant={modelSync?.config.enabled === false ? "outline" : "secondary"}>
            {modelSync?.config.enabled === false ? "off" : "on"}
          </Badge>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <p className="text-xs text-muted-foreground">
            {modelSync?.lastCheckedAt
              ? `Last check ${new Date(modelSync.lastCheckedAt).toLocaleString()} · +${modelSync.lastAdded} last pass`
              : "No sync has run yet — start serve or Sync now."}
            {modelSync && modelSync.providersSkipped.length > 0
              ? ` · not syncing: ${modelSync.providersSkipped.join(", ")}`
              : ""}
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => void toggleModelSync(!(modelSync?.config.enabled !== false))}
              disabled={busy}
            >
              {modelSync?.config.enabled === false ? "Enable" : "Disable"}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void runModelSyncNow()}
              disabled={busy}
            >
              Sync now
            </Button>
          </div>
        </CardContent>
      </Card>

      {!formOpen && message ? <p className="text-sm text-muted-foreground">{message}</p> : null}
      {!formOpen && error ? <p className="text-sm text-destructive">{error}</p> : null}

      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4">
          <div className="flex flex-col gap-1">
            <CardTitle>Configured providers</CardTitle>
            <CardDescription>{state?.config.providers.length ?? 0} providers</CardDescription>
          </div>
          <Button variant="outline" size="sm" onClick={beginAdd} disabled={busy}>
            Add provider
          </Button>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>provider</TableHead>
                <TableHead>protocol</TableHead>
                <TableHead>billing</TableHead>
                <TableHead>credential</TableHead>
                <TableHead>models</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(state?.config.providers ?? []).map((provider) => (
                <TableRow key={provider.name}>
                  <TableCell>
                    <span className="flex items-center gap-2 font-medium">
                      <ProviderLogo id={provider.name} />
                      <span className="flex flex-col">
                        <span>{provider.name}</span>
                        <span className="max-w-[280px] truncate text-[11px] font-normal text-muted-foreground">
                          {provider.baseUrl}
                        </span>
                      </span>
                    </span>
                  </TableCell>
                  <TableCell>{provider.type}</TableCell>
                  <TableCell>
                    <Badge variant={provider.billing === "subscription" ? "default" : "secondary"}>
                      {provider.billing ?? "api"}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <Badge variant={provider.keySource === "none" ? "destructive" : "secondary"}>
                      {provider.keySource}
                    </Badge>
                    {provider.login ? (
                      <span className="ml-1.5 text-[11px] text-muted-foreground">
                        {provider.login.label ??
                          provider.login.home ??
                          provider.login.credentialsPath ??
                          provider.login.keychainService ??
                          "account"}
                      </span>
                    ) : null}
                  </TableCell>
                  <TableCell>{provider.models.length}</TableCell>
                  <TableCell className="whitespace-nowrap text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => edit(provider)}
                      disabled={busy}
                    >
                      Edit
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-destructive hover:text-destructive"
                      onClick={() => void remove(provider.name)}
                      disabled={busy}
                    >
                      Remove
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
              {(state?.config.providers.length ?? 0) === 0 ? (
                <TableRow>
                  <TableCell colSpan={6} className="text-sm text-muted-foreground">
                    No providers yet — choose Add provider to connect one.
                  </TableCell>
                </TableRow>
              ) : null}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <section className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <h2 className="text-sm font-semibold">Usage &amp; limits</h2>
          <p className="text-sm text-muted-foreground">
            Subscription windows, reset times, and local spend per provider. Estimates use
            models.dev rates.
          </p>
        </div>
        <QuotaGrid quotas={quotas} health={health} bare />
        <div>
          <Button variant="outline" size="sm" onClick={() => void refreshQuota()} disabled={busy}>
            Refresh quota
          </Button>
        </div>
      </section>

      {state ? <BrainSection state={state} onSaved={setState} /> : null}

      {formOpen ? (
        <Card id="provider-form" ref={formCardRef}>
          <CardHeader className="flex-row items-start justify-between gap-4">
            <div className="flex flex-col gap-1">
              <CardTitle>{editing ? `Edit provider "${editing}"` : "Add provider"}</CardTitle>
              <CardDescription>
                {presetId === "custom"
                  ? "Choose the endpoint and protocol first, then authenticate and pick models."
                  : "Pick a preset, choose how it authenticates, then select models."}
              </CardDescription>
            </div>
            <Button variant="ghost" size="sm" onClick={closeForm} disabled={busy}>
              Close
            </Button>
          </CardHeader>
          <CardContent className="flex flex-col gap-5">
            <div className="grid grid-cols-2 gap-2 md:grid-cols-3 xl:grid-cols-4">
              {presets.map((preset) => (
                <button
                  key={preset.id}
                  type="button"
                  onClick={() => choosePreset(preset)}
                  className={cn(
                    "flex items-center gap-2.5 rounded-md border px-3 py-2 text-left text-sm hover:bg-muted",
                    presetId === preset.id && "border-primary bg-muted",
                  )}
                >
                  <ProviderLogo id={preset.id} />
                  <span className="flex min-w-0 flex-col">
                    <span className="leading-tight">{preset.name}</span>
                    {preset.billing === "subscription" ? (
                      <span className="text-[10px] text-muted-foreground">subscription</span>
                    ) : null}
                  </span>
                </button>
              ))}
            </div>

            <button
              type="button"
              onClick={() => choosePreset(CUSTOM_PRESET)}
              className="self-start text-xs text-muted-foreground underline underline-offset-4 hover:text-foreground"
            >
              Add a custom provider
            </button>

            {presetId === "custom" ? (
              <div className="grid grid-cols-2 gap-4 xl:grid-cols-3">
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="name">Name</Label>
                  <Input
                    id="name"
                    value={name}
                    placeholder="my-provider"
                    onChange={(event) => setName(event.target.value)}
                  />
                  <span className="text-[11px] text-muted-foreground">
                    Used in routing rules and the provider list.
                  </span>
                </div>
                <div className="col-span-2 flex flex-col gap-1.5 xl:col-span-1">
                  <Label htmlFor="baseUrl">Base URL</Label>
                  <Input
                    id="baseUrl"
                    value={baseUrl}
                    placeholder="https://api.example.com/v1"
                    onChange={(event) => setBaseUrl(event.target.value)}
                  />
                  <span className="text-[11px] text-muted-foreground">
                    Where requests are sent from this machine.
                  </span>
                </div>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="type">Protocol</Label>
                  <Select
                    value={effectiveType}
                    onValueChange={(value) => setType(String(value))}
                    disabled={lockedType !== undefined}
                  >
                    <SelectTrigger id="type" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="openai">openai (chat/completions)</SelectItem>
                      <SelectItem value="anthropic">anthropic (messages)</SelectItem>
                      <SelectItem value="both">both — openai + anthropic</SelectItem>
                      <SelectItem value="responses">responses</SelectItem>
                      <SelectItem value="gemini">gemini — cloud code (Antigravity)</SelectItem>
                      <SelectItem value="devin">devin — Connect-RPC (Devin CLI)</SelectItem>
                      <SelectItem value="cursor">cursor — Connect-RPC (Cursor agent)</SelectItem>
                    </SelectContent>
                  </Select>
                  <span className="text-[11px] text-muted-foreground">
                    {lockedType
                      ? `Set by the ${lockedBy} credential source.`
                      : "Wire format the endpoint expects."}
                  </span>
                </div>
              </div>
            ) : null}

            <div className="flex flex-col gap-2 rounded-md border p-3">
              <p className="text-sm font-medium">Credential</p>
              <div className="grid grid-cols-2 gap-4 xl:grid-cols-3">
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="auth">Auth</Label>
                  <Select
                    value={auth}
                    onValueChange={(value) => setAuth(value as ProviderAuthView)}
                  >
                    <SelectTrigger id="auth" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="api-key">api key</SelectItem>
                      <SelectItem value="oauth">oauth (subscription)</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                {auth === "oauth" ? (
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="oauthSource">Credential source</Label>
                    <Select
                      value={oauthSource}
                      onValueChange={(value) => setOauthSource(String(value) as OAuthSourceView)}
                    >
                      <SelectTrigger id="oauthSource" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="claude-code">Claude Code (~/.claude)</SelectItem>
                        <SelectItem value="codex">Codex (~/.codex)</SelectItem>
                        <SelectItem value="antigravity">Antigravity (~/.gemini)</SelectItem>
                        <SelectItem value="devin">Devin (~/.local/share/devin)</SelectItem>
                        <SelectItem value="cursor">Cursor (cursor-agent)</SelectItem>
                        <SelectItem value="workbuddy-ai">WorkBuddy AI</SelectItem>
                        <SelectItem value="freebuff">Freebuff (free tier)</SelectItem>
                        <SelectItem value="static">stored token</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                ) : null}
                {needsApiKey ? (
                  <>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="apiKey">API key</Label>
                      <Input
                        id="apiKey"
                        type="password"
                        placeholder={
                          editing
                            ? "leave empty to keep the stored key"
                            : "stored in credentials.json (0600)"
                        }
                        value={apiKey}
                        onChange={(event) => setApiKey(event.target.value)}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="apiKeyEnv">or env var</Label>
                      <Input
                        id="apiKeyEnv"
                        placeholder="DEEPSEEK_API_KEY"
                        value={apiKeyEnv}
                        onChange={(event) => setApiKeyEnv(event.target.value)}
                      />
                    </div>
                  </>
                ) : helpPreset?.noKey ? (
                  <p className="col-span-2 self-end text-xs text-muted-foreground xl:col-span-1">
                    Local server — no API key required. Make sure it is running at the base URL.
                  </p>
                ) : (
                  <div className="col-span-2 flex flex-col gap-2 self-end xl:col-span-1">
                    <p className="text-xs text-muted-foreground">
                      {oauthSource === "claude-code"
                        ? "Uses the OAuth token from Claude Code; run `claude` to sign in or refresh."
                        : oauthSource === "codex"
                          ? "Uses the OAuth token from Codex; run `codex` to sign in or refresh."
                          : oauthSource === "devin"
                            ? "Uses the session token from `devin auth login`; run it again if the token is rejected."
                            : oauthSource === "cursor"
                              ? "Uses Cursor's CLI sign-in; run `cursor-agent login` if the token is rejected."
                              : oauthSource === "workbuddy-ai"
                                ? "Discover or Save opens WorkBuddy AI sign-in in your browser; or use a plaintext desktop session."
                                : oauthSource === "freebuff"
                                  ? "Save opens Freebuff sign-in in your browser. Or set FREEBUFF_AUTH_TOKEN. Free tier only: daily quota, one session per account."
                                  : "Uses the Antigravity token from `agy` / the IDE; run it to sign in or refresh."}
                    </p>
                    {oauthSource === "workbuddy-ai" ? (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="self-start"
                        onClick={() => void signInWorkbuddy()}
                        disabled={busy || !baseUrl}
                      >
                        Sign in & discover
                      </Button>
                    ) : null}
                  </div>
                )}
              </div>
              <KeysHelp
                keysUrl={helpPreset?.keysUrl}
                hint={helpPreset?.hint}
                linkLabel={
                  helpPreset?.noKey
                    ? "Local server docs"
                    : auth === "oauth"
                      ? "How to sign in"
                      : "Get an API key"
                }
              />
              {loginSource ? (
                <div className="flex flex-col gap-3 rounded-md border border-dashed p-3">
                  <div>
                    <p className="text-xs font-medium">Second account (optional)</p>
                    <p className="text-[11px] text-muted-foreground">
                      Point this provider at another local {loginSourceName} sign-in — a work and a
                      home account can each have their own quota, fallback place, and ledger. Blank
                      reads the agent&apos;s own sign-in.
                    </p>
                  </div>
                  <div className="grid grid-cols-2 gap-4 xl:grid-cols-4">
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="loginLabel">Label</Label>
                      <Input
                        id="loginLabel"
                        placeholder="work"
                        value={loginLabel}
                        onChange={(event) => setLoginLabel(event.target.value)}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="loginHome">Config dir</Label>
                      <Input
                        id="loginHome"
                        placeholder={loginHint.home}
                        value={loginHome}
                        onChange={(event) => setLoginHome(event.target.value)}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="loginFile">Credential file</Label>
                      <Input
                        id="loginFile"
                        placeholder={loginHint.file ?? "/path/to/credentials.json"}
                        value={loginFile}
                        onChange={(event) => setLoginFile(event.target.value)}
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="loginKeychain">Keychain</Label>
                      <Input
                        id="loginKeychain"
                        placeholder="service[:account]"
                        value={loginKeychain}
                        onChange={(event) => setLoginKeychain(event.target.value)}
                      />
                    </div>
                  </div>
                </div>
              ) : null}
              <span className="text-[11px] text-muted-foreground">
                Secrets stay on this machine; a blank API key keeps the stored value when editing.
              </span>
            </div>

            <div className="flex flex-col gap-3 rounded-md border p-4">
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-sm font-medium">Models</p>
                  <p className="text-xs text-muted-foreground">
                    {selected.length} selected
                    {discovered.length > 0 ? ` · ${discovered.length} discovered` : ""}
                  </p>
                </div>
                <div className="flex items-center gap-2">
                  {discovered.length > 0 ? (
                    <>
                      <Button size="sm" variant="ghost" onClick={() => setSelected(discovered)}>
                        Select all
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => setSelected([])}>
                        Clear
                      </Button>
                    </>
                  ) : null}
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void discover()}
                    disabled={busy || !baseUrl}
                  >
                    Discover models
                  </Button>
                </div>
              </div>

              {extraSelected.length > 0 ? (
                <div className="flex flex-wrap gap-2">
                  {extraSelected.map((model) => (
                    <Badge key={model} variant="outline" className="gap-1">
                      {model}
                      <button
                        type="button"
                        className="text-muted-foreground hover:text-foreground"
                        onClick={() => toggleModel(model)}
                      >
                        ×
                      </button>
                    </Badge>
                  ))}
                </div>
              ) : null}

              {discovered.length > 12 ? (
                <Input
                  placeholder="Filter models…"
                  value={filter}
                  onChange={(event) => setFilter(event.target.value)}
                />
              ) : null}

              {visibleModels.length > 0 ? (
                <div className="max-h-64 overflow-auto rounded-md border">
                  {visibleModels.map((model) => (
                    <label
                      key={model}
                      className="flex cursor-pointer items-center gap-3 border-b px-3 py-1.5 text-sm last:border-b-0 hover:bg-muted"
                    >
                      <input
                        type="checkbox"
                        checked={selected.includes(model)}
                        onChange={() => toggleModel(model)}
                      />
                      <span className="flex-1 truncate">{model}</span>
                      <span className="text-xs text-muted-foreground">{priceLabel(model)}</span>
                    </label>
                  ))}
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">
                  {discovered.length > 0
                    ? "No models match the filter."
                    : "No models yet — run Discover, or add a model id below."}
                </p>
              )}

              <div className="flex items-center gap-2">
                <Input
                  placeholder="add model id manually"
                  value={customModel}
                  onChange={(event) => setCustomModel(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter") addCustomModel();
                  }}
                />
                <Button variant="outline" onClick={addCustomModel} disabled={!customModel.trim()}>
                  Add
                </Button>
              </div>

              <label className="flex cursor-pointer items-start gap-3 text-sm">
                <input
                  type="checkbox"
                  className="mt-1"
                  checked={syncModels}
                  onChange={(event) =>
                    setSyncOverride(
                      event.target.checked === syncDefault ? null : event.target.checked,
                    )
                  }
                />
                <span>
                  <span className="font-medium">Auto-sync new models</span>
                  <span className="block text-xs text-muted-foreground">
                    Append ids this provider newly lists. On by default for Codex, Claude Code,
                    Antigravity, Devin, Cursor, WorkBuddy AI, and presets that discover live
                    (Mistral, Groq, Ollama, LM Studio, OpenCode, Command Code); off for other
                    API/reseller catalogs until you enable it. Unchecking a model remembers the
                    removal so sync does not bring it back.
                  </span>
                </span>
              </label>
            </div>

            {billing === "subscription" ? (
              <div className="flex flex-col gap-3 rounded-md border p-4">
                <div>
                  <p className="text-sm font-medium">Quota caps</p>
                  <p className="text-xs text-muted-foreground">
                    Optional local spend caps for this provider; refresh usage from the quota
                    section above.
                  </p>
                </div>
                <div className="grid grid-cols-2 gap-4 xl:grid-cols-3">
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="quotaFiveHour">5h cap (USD, optional)</Label>
                    <Input
                      id="quotaFiveHour"
                      value={quotaFiveHour}
                      onChange={(event) => setQuotaFiveHour(event.target.value)}
                      placeholder="12"
                    />
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="quotaWeekly">weekly cap (USD, optional)</Label>
                    <Input
                      id="quotaWeekly"
                      value={quotaWeekly}
                      onChange={(event) => setQuotaWeekly(event.target.value)}
                      placeholder="30"
                    />
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="quotaMonthly">monthly cap (USD, optional)</Label>
                    <Input
                      id="quotaMonthly"
                      value={quotaMonthly}
                      onChange={(event) => setQuotaMonthly(event.target.value)}
                      placeholder="60"
                    />
                  </div>
                </div>
              </div>
            ) : null}

            <details className="rounded-md border">
              <summary className="cursor-pointer select-none px-4 py-2 text-sm font-medium">
                Advanced settings
              </summary>
              <div className="flex flex-col gap-4 border-t p-4">
                <p className="text-xs text-muted-foreground">
                  {presetId === "custom"
                    ? "Provider name, endpoint protocol, billing mode, and extra capability flags."
                    : "Fine-tune how this provider is addressed; the preset already fills sensible defaults."}
                </p>
                <div className="grid grid-cols-2 gap-4 xl:grid-cols-3">
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="name">Provider name</Label>
                    <Input
                      id="name"
                      value={name}
                      onChange={(event) => setName(event.target.value)}
                    />
                    <span className="text-[11px] text-muted-foreground">
                      Key used in routing rules and the provider list.
                    </span>
                  </div>
                  <div className="col-span-2 flex flex-col gap-1.5 xl:col-span-1">
                    <Label htmlFor="baseUrl">Base URL</Label>
                    <Input
                      id="baseUrl"
                      value={baseUrl}
                      onChange={(event) => setBaseUrl(event.target.value)}
                    />
                    <span className="text-[11px] text-muted-foreground">
                      Preset default — change only for proxies or self-hosted endpoints.
                    </span>
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="type">Protocol</Label>
                    <Select
                      value={effectiveType}
                      onValueChange={(value) => setType(String(value))}
                      disabled={lockedType !== undefined}
                    >
                      <SelectTrigger id="type" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="openai">openai (chat/completions)</SelectItem>
                        <SelectItem value="anthropic">anthropic (messages)</SelectItem>
                        <SelectItem value="both">both — openai + anthropic</SelectItem>
                        <SelectItem value="responses">responses</SelectItem>
                        <SelectItem value="gemini">gemini — cloud code (Antigravity)</SelectItem>
                        <SelectItem value="devin">devin — Connect-RPC (Devin CLI)</SelectItem>
                        <SelectItem value="cursor">cursor — Connect-RPC (Cursor agent)</SelectItem>
                      </SelectContent>
                    </Select>
                    {lockedType ? (
                      <span className="text-[11px] text-muted-foreground">
                        Set by the {lockedBy} credential source.
                      </span>
                    ) : null}
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="billing">Billing</Label>
                    <Select
                      value={billing}
                      onValueChange={(value) => setBilling(value as ProviderBillingView)}
                    >
                      <SelectTrigger id="billing" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="api">api (pay per token)</SelectItem>
                        <SelectItem value="subscription">subscription</SelectItem>
                      </SelectContent>
                    </Select>
                    <span className="text-[11px] text-muted-foreground">
                      Subscription unlocks optional quota caps below.
                    </span>
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="authAdvanced">Auth</Label>
                    <Select
                      value={auth}
                      onValueChange={(value) => setAuth(value as ProviderAuthView)}
                    >
                      <SelectTrigger id="authAdvanced" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="api-key">api key</SelectItem>
                        <SelectItem value="oauth">oauth (subscription)</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  {auth === "oauth" ? (
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="oauthSourceAdvanced">Credential source</Label>
                      <Select
                        value={oauthSource}
                        onValueChange={(value) => setOauthSource(String(value) as OAuthSourceView)}
                      >
                        <SelectTrigger id="oauthSourceAdvanced" className="w-full">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="claude-code">Claude Code (~/.claude)</SelectItem>
                          <SelectItem value="codex">Codex (~/.codex)</SelectItem>
                          <SelectItem value="antigravity">Antigravity (~/.gemini)</SelectItem>
                          <SelectItem value="devin">Devin (~/.local/share/devin)</SelectItem>
                          <SelectItem value="cursor">Cursor (cursor-agent)</SelectItem>
                          <SelectItem value="workbuddy-ai">WorkBuddy AI</SelectItem>
                          <SelectItem value="freebuff">Freebuff (free tier)</SelectItem>
                          <SelectItem value="static">stored token</SelectItem>
                        </SelectContent>
                      </Select>
                    </div>
                  ) : null}
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="apiKeyEnvAdvanced">API key env var</Label>
                    <Input
                      id="apiKeyEnvAdvanced"
                      placeholder="DEEPSEEK_API_KEY"
                      value={apiKeyEnv}
                      onChange={(event) => setApiKeyEnv(event.target.value)}
                    />
                    <span className="text-[11px] text-muted-foreground">
                      Read from this machine&apos;s environment when set.
                    </span>
                  </div>
                </div>
              </div>
            </details>

            <div className="flex items-center gap-3">
              <Button
                onClick={() => void save()}
                disabled={
                  busy || !name || !baseUrl || (selected.length === 0 && !canSaveWithoutModels)
                }
              >
                {editing ? "Update provider" : "Save provider"}
              </Button>
              {editing ? (
                <Button variant="ghost" onClick={closeForm} disabled={busy}>
                  Cancel
                </Button>
              ) : null}
              {message ? <span className="text-xs text-muted-foreground">{message}</span> : null}
              {error ? <span className="text-xs text-destructive">{error}</span> : null}
            </div>
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}
