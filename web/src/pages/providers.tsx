import { Badge, Button, Input, LayerCard, LayerDialog, Select, Text } from "@cloudflare/kumo";
import { X } from "@phosphor-icons/react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { KeysHelp } from "@/components/keys-help";
import { ProvidersSkeleton } from "@/components/page-skeletons";
import { ProviderIdentity } from "@/components/provider-identity";
import { ProviderLogo } from "@/components/provider-logo";
import { ProviderQuotaCard } from "@/components/quota-card";
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
import { assignProviderModels } from "@/lib/provider-assignment";
import { resolveProviderIdentity } from "@/lib/provider-name";
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

export interface ProvidersPageProps {
  embedded?: boolean;
  refreshKey?: number;
  onChanged?: () => void;
  settingsOnly?: boolean;
}

export function ProviderSettings({
  embedded = false,
  onChanged,
}: Pick<ProvidersPageProps, "embedded" | "onChanged">) {
  return <ProvidersPage settingsOnly embedded={embedded} onChanged={onChanged} />;
}

export function ProvidersPage({
  embedded = false,
  refreshKey = 0,
  onChanged,
  settingsOnly = false,
}: ProvidersPageProps = {}) {
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
  const [step, setStep] = useState(1);
  const [tasks, setTasks] = useState<string[]>([]);
  const [convertAuto, setConvertAuto] = useState(false);
  const [savedProvider, setSavedProvider] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const initialDraft = useRef<string | null>(null);
  const draft = JSON.stringify({
    presetId,
    name,
    type,
    auth,
    oauthSource,
    billing,
    baseUrl,
    apiKey,
    apiKeyEnv,
    loginLabel,
    loginHome,
    loginFile,
    loginKeychain,
    quotaFiveHour,
    quotaWeekly,
    quotaMonthly,
    selected,
    syncOverride,
    tasks,
    convertAuto,
  });
  useEffect(() => {
    if (!formOpen) {
      initialDraft.current = null;
      return;
    }
    if (initialDraft.current === null) initialDraft.current = draft;
    setDirty(draft !== initialDraft.current);
  }, [formOpen, draft]);

  useEffect(() => {
    if (!formOpen || (!dirty && !savedProvider)) return;
    const warn = (event: BeforeUnloadEvent) => event.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [formOpen, dirty, savedProvider]);

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

  const load = useCallback(async (refreshQuota = false) => {
    try {
      const [nextState, nextQuotas, nextSync] = await Promise.all([
        api.state(),
        api.quota(refreshQuota),
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
  }, [load, refreshKey]);

  useEffect(() => {
    const timer = window.setInterval(() => void load(false), 5_000);
    return () => window.clearInterval(timer);
  }, [load]);

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

  /**
   * One card per connected source: quota rows plus saved providers that have no quota yet —
   * so a plain API key still shows with Edit / Remove. Cards sort alphabetically by the brand
   * label the card header renders, so order does not depend on fetch order.
   */
  const usageEntries = useMemo(() => {
    const providers = state?.config.providers ?? [];
    const providerByName = new Map(providers.map((provider) => [provider.name, provider]));
    const quotaByName = new Map(quotas.map((quota) => [quota.provider, quota]));
    const names: string[] = [];
    const seen = new Set<string>();
    for (const quota of quotas) {
      if (seen.has(quota.provider)) continue;
      seen.add(quota.provider);
      names.push(quota.provider);
    }
    for (const provider of providers) {
      if (seen.has(provider.name)) continue;
      seen.add(provider.name);
      names.push(provider.name);
    }
    // Deterministic order: sort by the same brand label the card header renders
    // (`ProviderIdentity`), then by provider name so two accounts of one brand stay stable.
    const label = (entry: { provider?: ProviderView; name: string }) =>
      resolveProviderIdentity(entry.provider ?? entry.name).name;
    names.sort((a, b) => {
      const byLabel = label({ name: a, provider: providerByName.get(a) }).localeCompare(
        label({ name: b, provider: providerByName.get(b) }),
      );
      return byLabel !== 0 ? byLabel : a.localeCompare(b);
    });
    return names.map((name) => ({
      name,
      quota: quotaByName.get(name),
      provider: providerByName.get(name),
    }));
  }, [quotas, state]);

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
    effectiveType === "chatgpt-web" ||
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
        ...(noKey ? { noKey: true } : {}),
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
      if (tasks.length > 0) {
        const fresh = await api.state();
        const catalog = await api.models();
        assignProviderModels(
          { ...fresh, canonicals: catalog.canonicals },
          tasks,
          selected,
          savedProvider ?? name,
          convertAuto,
        );
      }
      if (
        !savedProvider &&
        !editing &&
        state?.config.providers.some((provider) => provider.name === name)
      ) {
        throw new Error(
          "This source name already exists. Choose another name for a second account.",
        );
      }
      const result: { signedInAs?: string } = savedProvider
        ? {}
        : await api.addProvider({
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
            syncModels: syncOverride ?? syncDefault,
            ...(noKey ? { noKey: true } : {}),
          });
      const providerName = savedProvider ?? name;
      setSavedProvider(providerName);
      onChanged?.();
      if (tasks.length > 0) {
        // Read after save: browser sign-in may discover models at save time.
        const fresh = await api.state();
        const savedModels =
          fresh.config.providers.find((provider) => provider.name === providerName)?.models ??
          selected;
        const catalog = await api.models();
        const routeModels = savedModels.map(
          (model) =>
            catalog.models.find((entry) => entry.provider === providerName && entry.id === model)
              ?.canonical ??
            catalog.canonicals.find((entry) =>
              entry.variants.some(
                (variant) => variant.provider === providerName && variant.model === model,
              ),
            )?.id ??
            model,
        );
        await api.saveRouting({
          routings: assignProviderModels(
            { ...fresh, canonicals: catalog.canonicals },
            tasks,
            routeModels,
            providerName,
            convertAuto || Boolean(savedProvider),
          ),
        });
        onChanged?.();
      }
      setFormOpen(false);
      setSavedProvider(null);
      setDirty(false);
      resetForm(presetId);
      setMessage(
        result.signedInAs
          ? `Saved source "${providerName}" · signed in as ${result.signedInAs}`
          : `Saved source "${providerName}"`,
      );
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
        `Remove source "${providerName}"? Stored credentials for it are deleted. This cannot be undone. Tasks that use its models may lose a source: ${
          (state?.routings ?? [])
            .filter((task) =>
              task.models.some(
                (model) =>
                  state?.config.providers
                    .find((provider) => provider.name === providerName)
                    ?.models.includes(model) || task.providers?.[model]?.includes(providerName),
              ),
            )
            .map((task) => task.label)
            .join(", ") || "none detected"
        }.`,
      )
    )
      return;
    setBusy(true);
    try {
      await api.deleteProvider(providerName);
      onChanged?.();
      if (editing === providerName) resetForm();
      await load();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function resetLocalQuota(provider: string) {
    if (
      !window.confirm(
        `Clear Jevonian's local quota state for ${provider}? This does not reset the provider's remote quota.`,
      )
    )
      return;
    setBusy(true);
    setError("");
    try {
      await api.resetQuota(provider);
      await load(true);
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }

  async function refreshQuota() {
    setBusy(true);
    try {
      await load(true);
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
      onChanged?.();
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
      onChanged?.();
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
    // Open on the models step: the models section is the only editable list an existing
    // source has, and step 1 settings stay reachable via Back.
    setStep(2);
    setTasks([]);
    setConvertAuto(false);
    setSavedProvider(null);
    setDirty(false);
    setFormOpen(true);
    setMessage("");
    setError("");
  }

  function beginAdd() {
    resetForm();
    setStep(1);
    setTasks([]);
    setConvertAuto(false);
    setSavedProvider(null);
    setDirty(false);
    setFormOpen(true);
  }

  function closeForm() {
    if (busy) return;
    if (
      (dirty || savedProvider) &&
      !window.confirm(
        savedProvider
          ? "The source is saved, but task assignment is not complete. Close without assigning tasks?"
          : "Discard unsaved source changes?",
      )
    )
      return;
    setFormOpen(false);
    setSavedProvider(null);
    setDirty(false);
    resetForm();
  }

  async function nextStep() {
    if (step === 2) {
      setBusy(true);
      try {
        setState(await api.state());
        setStep(3);
      } catch (cause) {
        setError(String(cause));
      } finally {
        setBusy(false);
      }
    } else setStep(step + 1);
  }

  const priceLabel = (model: string) => {
    const price = prices[model];
    return price ? `$${price.input}/$${price.output} per M` : "";
  };

  const noKey =
    effectiveType === "chatgpt-web" ||
    helpPreset?.noKey === true ||
    Boolean(
      editing && state?.config.providers.find((provider) => provider.name === editing)?.noKey,
    );
  const needsApiKey = !noKey && (auth === "api-key" || oauthSource === "static");
  // Save signs in then discovers when the list is empty — don't force a Discover-first deadlock.
  const canSaveWithoutModels =
    auth === "oauth" && (oauthSource === "workbuddy-ai" || oauthSource === "freebuff");

  /**
   * Whether this credential source keeps its sign-in somewhere a `login` can point at. A stored
   * token (`static`) has no local sign-in to redirect, so the account fields stay hidden.
   */
  const loginSource = auth === "oauth" && oauthSource !== "static";
  /** Local sign-in the second-account fields point at: its display name and an example home. */
  const loginHint =
    LOGIN_SOURCE_HINTS[oauthSource] ?? ({ name: "Claude Code", home: "~/.claude-work" } as const);
  const loginSourceName = loginHint.name;

  /**
   * The identity the second-account fields currently describe, for the live preview. Only built
   * when a sign-in is actually named, so a blank fieldset stays quiet.
   */
  const loginIdentityPreview = useMemo(() => {
    if (!loginSource) return null;
    const hasAny = Boolean(
      loginLabel.trim() || loginHome.trim() || loginFile.trim() || loginKeychain.trim(),
    );
    if (!hasAny) return null;
    return {
      name: name || loginHint.home || "account",
      oauthSource,
      type: effectiveType,
      login: {
        label: loginLabel.trim() || undefined,
        home: loginHome.trim() || undefined,
        credentialsPath: loginFile.trim() || undefined,
        keychainService: loginKeychain.trim().split(":")[0]?.trim() || undefined,
      },
    };
  }, [
    loginSource,
    loginLabel,
    loginHome,
    loginFile,
    loginKeychain,
    name,
    oauthSource,
    effectiveType,
    loginHint.home,
  ]);

  /**
   * A provider name that will not collide, derived from the account label: `claude-work` when
   * the brand is Claude and the label is `work`. Falls back to appending a counter.
   */
  const suggestedSecondAccountName = useMemo(() => {
    const brand =
      resolveProviderIdentity({ name, oauthSource, type: effectiveType }).brand ||
      name ||
      "account";
    // `claude-subscription` reads better as `claude` in a provider name.
    const base = brand.replace(/-subscription$/, "");
    const slug = loginLabel
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "");
    const candidate = slug ? `${base}-${slug}` : `${base}-account`;
    const taken = new Set((state?.config.providers ?? []).map((provider) => provider.name));
    if (!taken.has(candidate)) return candidate;
    let index = 2;
    while (taken.has(`${candidate}-${index}`)) index += 1;
    return `${candidate}-${index}`;
  }, [name, oauthSource, effectiveType, loginLabel, state]);

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

  if (error && !state) return <p className="text-sm text-kumo-danger">{error}</p>;
  if (!state) return <ProvidersSkeleton />;

  return (
    <div className="flex flex-col gap-6">
      {!embedded && !settingsOnly ? (
        <div>
          <h1 className="text-lg font-semibold">Connected sources</h1>
          <p className="text-sm text-kumo-subtle">
            Connect API providers, subscription endpoints, and local agent credentials. Secrets stay
            on this machine.
          </p>
        </div>
      ) : null}

      {!embedded ? (
        <details className="order-2 rounded-lg border border-kumo-hairline" open={settingsOnly}>
          <summary className="cursor-pointer px-4 py-3 text-sm font-medium">
            Model auto-sync
          </summary>
          <div className="flex flex-col gap-3 border-t border-kumo-hairline px-4 py-3">
            <div className="flex items-start justify-between gap-4">
              <p className="text-xs text-kumo-subtle">
                While <code>serve</code> is running, Jevonian periodically discovers each
                provider&apos;s model list and appends new ids. Removals stick; fixed routings are
                never rewritten.
              </p>
              <Badge variant={modelSync?.config.enabled === false ? "outline" : "secondary"}>
                {modelSync?.config.enabled === false ? "off" : "on"}
              </Badge>
            </div>
            <p className="text-xs text-kumo-subtle">
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
          </div>
        </details>
      ) : settingsOnly ? (
        <div className="order-2 flex flex-col gap-3">
          <div className="flex items-start justify-between gap-4">
            <p className="text-xs text-kumo-subtle">
              While <code>serve</code> is running, Jevonian periodically discovers each
              provider&apos;s model list and appends new ids. Removals stick; fixed routings are
              never rewritten.
            </p>
            <Badge variant={modelSync?.config.enabled === false ? "outline" : "secondary"}>
              {modelSync?.config.enabled === false ? "off" : "on"}
            </Badge>
          </div>
          <p className="text-xs text-kumo-subtle">
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
        </div>
      ) : null}

      {!formOpen && message ? <p className="text-sm text-kumo-subtle">{message}</p> : null}
      {!formOpen && error ? <p className="text-sm text-kumo-danger">{error}</p> : null}

      {!settingsOnly ? (
        <LayerCard>
          <LayerCard.Secondary className="flex-row items-start justify-between gap-4">
            <div className="flex flex-col gap-1">
              <Text variant="heading" as="h2">Usage &amp; limits</Text>
              <Text variant="secondary" size="sm">
                {state.config.providers.length} sources · windows, reset times, and local spend.
                Estimates use models.dev rates.
              </Text>
            </div>
            <div className="flex items-center gap-2">
              <Button variant="outline" size="sm" onClick={beginAdd} disabled={busy}>
                Add provider
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => void refreshQuota()}
                disabled={busy}
              >
                Refresh quota
              </Button>
            </div>
          </LayerCard.Secondary>
          <LayerCard.Primary>
            {usageEntries.length === 0 ? (
              <p className="text-sm text-kumo-subtle">
                No providers yet — choose Add provider to connect one.
              </p>
            ) : (
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3">
                {usageEntries.map(({ name, quota, provider }) => (
                  <ProviderQuotaCard
                    key={name}
                    quota={quota}
                    provider={provider}
                    health={health.find((item) => item.provider === name)}
                    onReset={(target) => void resetLocalQuota(target)}
                    resetDisabled={busy}
                    onEdit={edit}
                    onRemove={(target) => void remove(target)}
                  />
                ))}
              </div>
            )}
          </LayerCard.Primary>
        </LayerCard>
      ) : null}

      <LayerDialog.Root
        open={formOpen}
        onOpenChange={(open) => {
          if (!open) closeForm();
        }}
      >
        <LayerDialog.Content size="lg" verticalAlign="top">
          <LayerDialog.Title>
            {editing ? `Edit source “${editing}”` : "Connect a source"}
          </LayerDialog.Title>
          <LayerDialog.Description>
            Step {step} of 3:{" "}
            {step === 1
              ? "Source and authentication"
              : step === 2
                ? "Check connection and choose models"
                : "Assign tasks and save"}
            {editing && step === 1 ? " — settings keep their values" : ""}
          </LayerDialog.Description>
          <LayerDialog.Body>
            <div id="provider-form" className="flex flex-col gap-5 pb-4">
              <fieldset
                disabled={busy || Boolean(savedProvider)}
                className={cn("contents", step !== 1 && "hidden")}
              >
                {!editing ? (
                  <div className="grid grid-cols-2 gap-2 md:grid-cols-3 xl:grid-cols-4">
                    {presets.map((preset) => (
                      <button
                        key={preset.id}
                        type="button"
                        onClick={() => choosePreset(preset)}
                        className={cn(
                          "flex items-center gap-2.5 rounded-lg border border-kumo-hairline px-3 py-2 text-left text-sm hover:bg-kumo-tint",
                          presetId === preset.id && "border-kumo-brand bg-kumo-tint",
                        )}
                      >
                        <ProviderLogo id={preset.id} />
                        <span className="flex min-w-0 flex-col">
                          <span className="leading-tight">{preset.name}</span>
                          {preset.billing === "subscription" ? (
                            <span className="text-[10px] text-kumo-subtle">subscription</span>
                          ) : null}
                        </span>
                      </button>
                    ))}
                  </div>
                ) : null}

                {!editing ? (
                  <button
                    type="button"
                    onClick={() => choosePreset(CUSTOM_PRESET)}
                    className="self-start text-xs text-kumo-subtle underline underline-offset-4 hover:text-kumo-default"
                  >
                    Add a custom provider
                  </button>
                ) : null}

                <div className="flex flex-col gap-3 rounded-lg border border-kumo-hairline bg-kumo-elevated p-4">
                  <p className="text-sm font-medium">Credential</p>
                  <div className="grid grid-cols-2 gap-4 xl:grid-cols-3">
                    <Select
                      label="Auth"
                      value={auth}
                      onValueChange={(value) => setAuth(value as ProviderAuthView)}
                      items={{
                        "api-key": "api key",
                        oauth: "oauth (subscription)",
                      }}
                    />
                    {auth === "oauth" ? (
                      <Select
                        label="Credential source"
                        value={oauthSource}
                        onValueChange={(value) => setOauthSource(String(value) as OAuthSourceView)}
                        items={{
                          "claude-code": "Claude Code (~/.claude)",
                          codex: "Codex (~/.codex)",
                          antigravity: "Antigravity (~/.gemini)",
                          devin: "Devin (~/.local/share/devin)",
                          cursor: "Cursor (cursor-agent)",
                          "workbuddy-ai": "WorkBuddy AI",
                          freebuff: "Freebuff (free tier)",
                          static: "stored token",
                        }}
                      />
                    ) : null}
                    {needsApiKey ? (
                      <>
                        <Input
                          id="apiKey"
                          label="API key"
                          type="password"
                          placeholder={
                            editing
                              ? "leave empty to keep the stored key"
                              : "stored in credentials.json (0600)"
                          }
                          value={apiKey}
                          onChange={(event) => setApiKey(event.target.value)}
                        />
                        <Input
                          id="apiKeyEnv"
                          label="or env var"
                          placeholder="DEEPSEEK_API_KEY"
                          value={apiKeyEnv}
                          onChange={(event) => setApiKeyEnv(event.target.value)}
                        />
                      </>
                    ) : helpPreset?.noKey ? (
                      <p className="col-span-2 self-end text-xs text-kumo-subtle xl:col-span-1">
                        Local server — no API key required. Make sure it is running at the base URL.
                      </p>
                    ) : (
                      <div className="col-span-2 flex flex-col gap-2 self-end xl:col-span-1">
                        <p className="text-xs text-kumo-subtle">
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
                    <details
                      className="rounded-lg border border-dashed border-kumo-line p-3"
                      open={Boolean(loginLabel || loginHome || loginFile || loginKeychain)}
                    >
                      <summary className="cursor-pointer text-xs font-medium">
                        Second account (optional)
                      </summary>
                      <div className="flex flex-col gap-1 pt-2">
                        <p className="text-xs font-medium">
                          Point this provider at another {loginSourceName} sign-in
                        </p>
                        <p className="text-[11px] text-kumo-subtle">
                          A work and a home account can each have their own quota, fallback place, and
                          ledger. Blank reads the agent&apos;s own sign-in. Sign in to that account
                          once with the {loginSourceName} CLI first, then point Jevonian at where it
                          stored the sign-in.
                        </p>
                      </div>
                      <div className="mt-3 grid grid-cols-2 gap-4 xl:grid-cols-4">
                        <Input
                          id="loginLabel"
                          label="Label"
                          description="Shown beside the provider name."
                          placeholder="work"
                          value={loginLabel}
                          onChange={(event) => setLoginLabel(event.target.value)}
                        />
                        <Input
                          id="loginHome"
                          label="Config dir"
                          description="Directory the agent keeps this sign-in in."
                          placeholder={loginHint.home}
                          value={loginHome}
                          onChange={(event) => setLoginHome(event.target.value)}
                        />
                        <Input
                          id="loginFile"
                          label="Credential file"
                          description="Wins over the config dir when both are set."
                          placeholder={loginHint.file ?? "/path/to/credentials.json"}
                          value={loginFile}
                          onChange={(event) => setLoginFile(event.target.value)}
                        />
                        <Input
                          id="loginKeychain"
                          label="Keychain"
                          description="macOS keychain item, as service[:account]."
                          placeholder="service[:account]"
                          value={loginKeychain}
                          onChange={(event) => setLoginKeychain(event.target.value)}
                        />
                      </div>
                      {loginIdentityPreview ? (
                        <div className="mt-3 flex flex-wrap items-center gap-2 rounded-lg bg-kumo-tint/50 px-3 py-2">
                          <span className="text-[11px] text-kumo-subtle">Reads as</span>
                          <ProviderIdentity provider={loginIdentityPreview} showAccount />
                          <span className="font-mono text-[11px] text-kumo-subtle">
                            {suggestedSecondAccountName}
                          </span>
                          {name !== suggestedSecondAccountName ? (
                            <Button
                              type="button"
                              variant="ghost"
                              size="sm"
                              className="ml-auto h-6 px-2 text-[11px]"
                              onClick={() => setName(suggestedSecondAccountName)}
                            >
                              Use this name
                            </Button>
                          ) : null}
                        </div>
                      ) : null}
                    </details>
                  ) : null}
                  <span className="text-[11px] text-kumo-subtle">
                    Secrets stay on this machine; a blank API key keeps the stored value when editing.
                  </span>
                </div>
              </fieldset>
            <fieldset
              disabled={busy || Boolean(savedProvider)}
              className={cn("contents", step !== 2 && "hidden")}
            >
              <div className="flex flex-col gap-3 rounded-lg border border-kumo-hairline bg-kumo-elevated p-4">
                <div className="flex items-center justify-between">
                  <div>
                    <p className="text-sm font-medium">Models</p>
                    <p className="text-xs text-kumo-subtle">
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
                          aria-label={`Remove ${model}`}
                          className="text-kumo-subtle hover:text-kumo-default"
                          onClick={() => toggleModel(model)}
                        >
                          <X size={12} />
                        </button>
                      </Badge>
                    ))}
                  </div>
                ) : null}

                {discovered.length > 12 ? (
                  <Input
                    aria-label="Filter models"
                    placeholder="Filter models…"
                    value={filter}
                    onChange={(event) => setFilter(event.target.value)}
                  />
                ) : null}

                {visibleModels.length > 0 ? (
                  <div className="max-h-64 overflow-auto rounded-lg border border-kumo-hairline">
                    {visibleModels.map((model) => (
                      <label
                        key={model}
                        className="flex cursor-pointer items-center gap-3 border-b border-kumo-hairline px-3 py-1.5 text-sm last:border-b-0 hover:bg-kumo-tint"
                      >
                        <input
                          type="checkbox"
                          checked={selected.includes(model)}
                          onChange={() => toggleModel(model)}
                        />
                        <span className="flex-1 truncate">{model}</span>
                        <span className="text-xs text-kumo-subtle">{priceLabel(model)}</span>
                      </label>
                    ))}
                  </div>
                ) : (
                  <p className="text-xs text-kumo-subtle">
                    {discovered.length > 0
                      ? "No models match the filter."
                      : "No models yet — run Discover, or add a model id below."}
                  </p>
                )}

                <div className="flex items-center gap-2">
                  <Input
                    aria-label="Add model id manually"
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
                    <span className="block text-xs text-kumo-subtle">
                      Append ids this provider newly lists. On by default for ChatGPT Web, Codex,
                      Claude Code, Antigravity, Devin, Cursor, WorkBuddy AI, and presets that
                      discover live (Mistral, Groq, Ollama, LM Studio, OpenCode, Command Code); off
                      for other API/reseller catalogs until you enable it. Unchecking a model
                      remembers the removal so sync does not bring it back.
                    </span>
                  </span>
                </label>
              </div>
            </fieldset>
            <fieldset
              disabled={busy || Boolean(savedProvider)}
              className={cn("contents", step !== 1 && "hidden")}
            >
              {billing === "subscription" ? (
                <details className="rounded-lg border border-kumo-hairline p-4">
                  <summary className="cursor-pointer text-sm font-medium">
                    Quota caps (optional)
                  </summary>
                  <div className="pt-2">
                    <p className="text-sm font-medium">Quota caps</p>
                    <p className="text-xs text-kumo-subtle">
                      Optional local spend caps for this provider; refresh usage from the quota
                      section above.
                    </p>
                  </div>
                  <div className="mt-3 grid grid-cols-2 gap-4 xl:grid-cols-3">
                    <Input
                      id="quotaFiveHour"
                      label="5h cap (USD, optional)"
                      value={quotaFiveHour}
                      onChange={(event) => setQuotaFiveHour(event.target.value)}
                      placeholder="12"
                    />
                    <Input
                      id="quotaWeekly"
                      label="weekly cap (USD, optional)"
                      value={quotaWeekly}
                      onChange={(event) => setQuotaWeekly(event.target.value)}
                      placeholder="30"
                    />
                    <Input
                      id="quotaMonthly"
                      label="monthly cap (USD, optional)"
                      value={quotaMonthly}
                      onChange={(event) => setQuotaMonthly(event.target.value)}
                      placeholder="60"
                    />
                  </div>
                </details>
              ) : null}

              <details className="rounded-lg border border-kumo-hairline" open={presetId === "custom"}>
                <summary className="cursor-pointer select-none px-4 py-2 text-sm font-medium">
                  Advanced settings
                </summary>
                <div className="flex flex-col gap-4 border-t border-kumo-hairline p-4">
                  <p className="text-xs text-kumo-subtle">
                    {presetId === "custom"
                      ? "Provider name, endpoint protocol, billing mode, and extra capability flags."
                      : "Fine-tune how this provider is addressed; the preset already fills sensible defaults."}
                  </p>
                  <div className="grid grid-cols-2 gap-4 xl:grid-cols-3">
                    <Input
                      id="name"
                      label="Provider name"
                      description="Key used in routing rules and the provider list."
                      disabled={Boolean(editing)}
                      value={name}
                      onChange={(event) => setName(event.target.value)}
                    />
                    <div className="col-span-2 xl:col-span-1">
                      <Input
                        id="baseUrl"
                        label="Base URL"
                        description="Preset default — change only for proxies or self-hosted endpoints."
                        value={baseUrl}
                        onChange={(event) => setBaseUrl(event.target.value)}
                      />
                    </div>
                    <div>
                      <Select
                        label="Protocol"
                        value={effectiveType}
                        onValueChange={(value) => setType(String(value))}
                        disabled={lockedType !== undefined}
                        items={{
                          openai: "openai (chat/completions)",
                          anthropic: "anthropic (messages)",
                          both: "both — openai + anthropic",
                          responses: "responses",
                          gemini: "gemini — cloud code (Antigravity)",
                          devin: "devin — Connect-RPC (Devin CLI)",
                          cursor: "cursor — Connect-RPC (Cursor agent)",
                          "chatgpt-web": "chatgpt-web — ChatGPT Web browser bridge",
                        }}
                      />
                      {lockedType ? (
                        <span className="mt-1 block text-[11px] text-kumo-subtle">
                          Set by the {lockedBy} credential source.
                        </span>
                      ) : null}
                    </div>
                    <div>
                      <Select
                        label="Billing"
                        value={billing}
                        onValueChange={(value) => setBilling(value as ProviderBillingView)}
                        description="Subscription unlocks optional quota caps below."
                        items={{
                          api: "api (pay per token)",
                          subscription: "subscription",
                        }}
                      />
                    </div>
                  </div>
                </div>
              </details>
            </fieldset>
            {step === 3 ? (
              <div className="flex flex-col gap-3">
                <p className="text-sm">
                  Assign selected models to tasks (optional). New models go last as backups.
                  Existing priorities and source allow-lists stay unchanged.
                </p>
                {(state.config.routing.routings.length > 0
                  ? state.config.routing.routings
                  : state.routings
                ).map((task) => (
                  <label key={task.id} className="flex items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      disabled={busy}
                      checked={tasks.includes(task.id)}
                      onChange={(event) => {
                        setDirty(true);
                        setTasks((current) =>
                          event.target.checked
                            ? [...current, task.id]
                            : current.filter((id) => id !== task.id),
                        );
                      }}
                    />
                    {task.label}
                    {task.models.length === 0 ? " (auto-derived)" : ""}
                  </label>
                ))}
                {tasks.length > 0 ? (
                  <label className="flex items-start gap-2 text-xs">
                    <input
                      type="checkbox"
                      disabled={busy}
                      checked={convertAuto}
                      onChange={(event) => {
                        setDirty(true);
                        setConvertAuto(event.target.checked);
                      }}
                    />
                    Convert selected auto-derived tasks to fixed lists. Keep their current models
                    first, then append these models. Models excluded by a source allow-list are not
                    added.
                  </label>
                ) : null}
                <p className="text-xs text-kumo-subtle">
                  {selected.length} models selected.{" "}
                  {canSaveWithoutModels && selected.length === 0
                    ? "Save will sign in and discover models."
                    : ""}
                </p>
                {savedProvider ? (
                  <p role="status" className="text-sm font-medium">
                    Source saved. Task assignment failed. Retry saves task assignment only.
                  </p>
                ) : null}
              </div>
            ) : null}
            <div className="flex flex-wrap items-center gap-3">
              {step > 1 && !savedProvider ? (
                <Button variant="outline" disabled={busy} onClick={() => setStep(step - 1)}>
                  Back
                </Button>
              ) : null}
              {step < 3 ? (
                <Button
                  variant="primary"
                  disabled={
                    busy ||
                    !name ||
                    !baseUrl ||
                    (step === 2 && selected.length === 0 && !canSaveWithoutModels)
                  }
                  onClick={() => void nextStep()}
                >
                  Continue
                </Button>
              ) : (
                <Button
                  variant="primary"
                  onClick={() => void save()}
                  disabled={
                    busy || !name || !baseUrl || (selected.length === 0 && !canSaveWithoutModels)
                  }
                >
                  {savedProvider
                    ? "Retry task assignment"
                    : editing
                      ? "Update source"
                      : "Save source"}
                </Button>
              )}
              {editing ? (
                <Button variant="ghost" onClick={closeForm} disabled={busy}>
                  Cancel
                </Button>
              ) : null}
              {message ? <span className="text-xs text-kumo-subtle">{message}</span> : null}
              {error ? <span className="text-xs text-kumo-danger">{error}</span> : null}
            </div>
          </div>
        </LayerDialog.Body>
      </LayerDialog.Content>
    </LayerDialog.Root>
    </div>
  );
}
