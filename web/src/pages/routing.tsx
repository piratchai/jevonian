import {
  closestCenter,
  DndContext,
  KeyboardSensor,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import {
  arrayMove,
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { ArrowDown, ArrowUp, GripVertical, X } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";

import { RoutingSkeleton } from "@/components/page-skeletons";
import { ProviderLogo } from "@/components/provider-logo";
import { ScheduleSection } from "@/components/schedule-section";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Combobox } from "@/components/ui/combobox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  api,
  type QuotaGuardView,
  type RoutingEntryView,
  type StateResponse,
  type CanonicalModelView,
  type ModelView,
  type QuotaHealthView,
  type ScheduleView,
  type TokenSaverConfigView,
} from "@/lib/api";
import { providerDisplayName } from "@/lib/provider-name";
import { offersTimeBasedModels, pruneWindowLists, windowRange } from "@/lib/schedule";

import {
  allowedProviders,
  BUILTIN_ROUTING_IDS,
  mergeRoutingDrafts,
  routeSavePayload,
  validRoutingId,
} from "./routing-state";

const GUARD_FALLBACK: QuotaGuardView = { enabled: true, lowPercent: 10, resetAware: true };
const NEW_ROUTE = "__new_route__";

export interface RoutingPageProps {
  embedded?: boolean;
  refreshKey?: number;
  onChanged?: () => void;
  onEditingChange?: (editing: boolean) => void;
  settingsOnly?: boolean;
}

function OrderedRow({
  id,
  index,
  count,
  onMove,
  children,
}: {
  id: string;
  index: number;
  count: number;
  onMove: (from: number, to: number) => void;
  children: ReactNode;
}) {
  const { attributes, listeners, setNodeRef, setActivatorNodeRef, transform, transition } =
    useSortable({ id });
  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className="flex items-start gap-2 rounded-lg border bg-background p-3"
    >
      <div className="flex shrink-0 flex-col gap-1">
        <button
          ref={setActivatorNodeRef}
          type="button"
          {...attributes}
          {...listeners}
          aria-label={`Drag ${id}`}
          className="touch-none rounded p-1 hover:bg-muted"
        >
          <GripVertical className="size-4" />
        </button>
        <button
          type="button"
          aria-label={`Move ${id} up`}
          disabled={index === 0}
          onClick={() => onMove(index, index - 1)}
          className="rounded p-1 hover:bg-muted disabled:opacity-30"
        >
          <ArrowUp className="size-4" />
        </button>
        <button
          type="button"
          aria-label={`Move ${id} down`}
          disabled={index === count - 1}
          onClick={() => onMove(index, index + 1)}
          className="rounded p-1 hover:bg-muted disabled:opacity-30"
        >
          <ArrowDown className="size-4" />
        </button>
      </div>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  );
}

function OrderedList({
  items,
  onChange,
  children,
}: {
  items: string[];
  onChange: (items: string[]) => void;
  children: (id: string, index: number) => ReactNode;
}) {
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  function move(from: number, to: number) {
    onChange(arrayMove(items, from, to));
  }
  function drop({ active, over }: DragEndEvent) {
    if (!over || active.id === over.id) return;
    const from = items.indexOf(String(active.id));
    const to = items.indexOf(String(over.id));
    if (from >= 0 && to >= 0) move(from, to);
  }
  return (
    <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={drop}>
      <SortableContext items={items} strategy={verticalListSortingStrategy}>
        <div className="flex flex-col gap-2">
          {items.map((id, index) => (
            <OrderedRow key={id} id={id} index={index} count={items.length} onMove={move}>
              {children(id, index)}
            </OrderedRow>
          ))}
        </div>
      </SortableContext>
    </DndContext>
  );
}

export function RoutingPage({
  embedded = false,
  refreshKey = 0,
  onChanged,
  onEditingChange,
  settingsOnly = false,
}: RoutingPageProps = {}) {
  const [state, setState] = useState<StateResponse | null>(null);
  const [models, setModels] = useState<ModelView[]>([]);
  const [canonicals, setCanonicals] = useState<CanonicalModelView[]>([]);
  const [health, setHealth] = useState<QuotaHealthView[]>([]);
  const [drafts, setDrafts] = useState<RoutingEntryView[]>([]);
  const [guard, setGuard] = useState<QuotaGuardView>(GUARD_FALLBACK);
  const [saver, setSaver] = useState<TokenSaverConfigView | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [newRoute, setNewRoute] = useState<RoutingEntryView>({
    id: "",
    label: "",
    description: "",
    models: [],
  });
  const [fixedNew, setFixedNew] = useState(false);
  const [fixedEmpty, setFixedEmpty] = useState<string | null>(null);
  const [confirmClose, setConfirmClose] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saverBusy, setSaverBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const savedRef = useRef<RoutingEntryView[]>([]);
  const guardRef = useRef<QuotaGuardView>(GUARD_FALLBACK);
  const loadVersion = useRef(0);

  const load = useCallback(async () => {
    const version = ++loadVersion.current;
    try {
      const [next, catalog, quota] = await Promise.all([api.state(), api.models(), api.quota()]);
      if (version !== loadVersion.current) return;
      // Config routes retain empty model pools. State routes contain the derived pools.
      const fresh = next.config.routing.routings.length
        ? next.config.routing.routings
        : next.routings;
      const previous = savedRef.current;
      setDrafts((current) => mergeRoutingDrafts(fresh, current, previous));
      savedRef.current = fresh;
      const nextGuard = next.config.routing.quotaGuard ?? GUARD_FALLBACK;
      const previousGuard = guardRef.current;
      setGuard((current) =>
        JSON.stringify(current) === JSON.stringify(previousGuard) ? nextGuard : current,
      );
      guardRef.current = nextGuard;
      setState(next);
      setModels(catalog.models);
      setCanonicals(catalog.canonicals ?? []);
      setHealth(quota.health);
      setSaver(next.config.tokenSaver ?? null);
    } catch (cause) {
      if (version === loadVersion.current) setError(String(cause));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load, refreshKey]);
  useEffect(() => {
    onEditingChange?.(editingId !== null);
  }, [editingId, onEditingChange]);
  useEffect(
    () => () => {
      onEditingChange?.(false);
    },
    [onEditingChange],
  );

  const providersByModel = useMemo(() => {
    const map = new Map<string, string[]>();
    const add = (id: string, provider: string) => {
      const list = map.get(id) ?? [];
      if (!list.includes(provider)) list.push(provider);
      map.set(id, list);
    };
    for (const model of models) add(model.id, model.provider);
    for (const model of canonicals)
      for (const variant of model.variants) add(model.id, variant.provider);
    return map;
  }, [models, canonicals]);
  const names = useMemo(
    () => new Map(canonicals.map((entry) => [entry.id, entry.name])),
    [canonicals],
  );
  const statuses = useMemo(
    () => new Map(health.map((entry) => [entry.provider, entry.status])),
    [health],
  );
  const derived = useMemo(
    () => new Map((state?.routings ?? []).map((entry) => [entry.id, entry.models])),
    [state],
  );
  const schedule = state?.config.routing.schedule;
  const scheduleStatus = state?.schedule;
  // The active window changes with the clock, not with the config, so keep it fresh.
  const hasSchedule = Boolean(schedule);
  useEffect(() => {
    if (!hasSchedule) return;
    const timer = window.setInterval(() => {
      api
        .routingNow()
        .then((now) =>
          setState((current) =>
            current ? { ...current, schedule: now.schedule, effective: now.effective } : current,
          ),
        )
        .catch(() => {});
    }, 60_000);
    return () => window.clearInterval(timer);
  }, [hasSchedule]);
  const route = editingId === NEW_ROUTE ? newRoute : drafts.find((entry) => entry.id === editingId);
  const fixed =
    editingId === NEW_ROUTE ? fixedNew : Boolean(route?.models.length || fixedEmpty === editingId);
  const routeDirty = Boolean(
    route &&
    (editingId === NEW_ROUTE
      ? route.id || route.label || route.description || route.models.length || fixedNew
      : JSON.stringify(route) !==
          JSON.stringify(savedRef.current.find((entry) => entry.id === editingId)) ||
        fixedEmpty === editingId),
  );
  const guardDirty = JSON.stringify(guard) !== JSON.stringify(guardRef.current);
  const dirty = routeDirty || guardDirty;
  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  function updateRoute(patch: Partial<RoutingEntryView>) {
    if (editingId === NEW_ROUTE) setNewRoute((current) => ({ ...current, ...patch }));
    else
      setDrafts((current) =>
        current.map((entry) => (entry.id === editingId ? { ...entry, ...patch } : entry)),
      );
  }
  function closeEditor(discard = false) {
    if (busy) return;
    if (routeDirty && !discard) {
      setConfirmClose(true);
      return;
    }
    if (editingId !== NEW_ROUTE) {
      const saved = savedRef.current.find((entry) => entry.id === editingId);
      setDrafts((current) =>
        current.flatMap((entry) => (entry.id !== editingId ? [entry] : saved ? [saved] : [])),
      );
    }
    setEditingId(null);
    setConfirmClose(false);
    setConfirmDelete(false);
    setFixedEmpty(null);
  }
  function setProviders(model: string, preferred: string[] | undefined) {
    if (!route) return;
    const providers = { ...route.providers };
    if (preferred === undefined) delete providers[model];
    else providers[model] = preferred;
    updateRoute({ providers: Object.keys(providers).length ? providers : undefined });
  }
  /** Sets the models a task uses during one schedule window. Undefined means "same as the default". */
  function setWindowModels(windowId: string, list: string[] | undefined) {
    if (!route) return;
    const windows = { ...route.windows };
    if (list === undefined) delete windows[windowId];
    else windows[windowId] = list;
    updateRoute({ windows: Object.keys(windows).length ? windows : undefined });
  }
  function removeModel(model: string) {
    if (!route) return;
    const providers = { ...route.providers };
    delete providers[model];
    updateRoute({
      models: route.models.filter((id) => id !== model),
      providers: Object.keys(providers).length ? providers : undefined,
    });
    setFixedEmpty(editingId);
  }
  /** Sends one routing save and applies the result to local state. Throws when the server refuses. */
  async function submitRouting(payload: Parameters<typeof api.saveRouting>[0]) {
    const result = await api.saveRouting(payload);
    const previous = savedRef.current;
    const fresh = result.routing.routings;
    savedRef.current = fresh;
    setDrafts((current) => mergeRoutingDrafts(fresh, current, previous));
    if (payload.quotaGuard) {
      guardRef.current = result.routing.quotaGuard ?? (payload.quotaGuard as QuotaGuardView);
      setGuard(guardRef.current);
    }
    setState((current) =>
      current
        ? {
            ...current,
            config: { ...current.config, routing: result.routing },
            routings: result.routings,
            schedule: result.schedule,
            effective: result.effective,
          }
        : current,
    );
    onChanged?.();
  }
  async function persist(routes: RoutingEntryView[], quotaGuard?: QuotaGuardView) {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await submitRouting({ routings: routes, ...(quotaGuard ? { quotaGuard } : {}) });
      setMessage("Routing saved");
      return true;
    } catch (cause) {
      setError(String(cause));
      return false;
    } finally {
      setBusy(false);
    }
  }
  /** Saves the schedule (null removes it). Returns an error message, or null on success. */
  async function saveSchedule(next: ScheduleView | null): Promise<string | null> {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      // Lists for windows that no longer exist go with them.
      await submitRouting({
        routings: pruneWindowLists(savedRef.current, next),
        schedule: next,
      });
      setMessage(next ? "Schedule saved" : "Schedule removed");
      return null;
    } catch (cause) {
      return cause instanceof Error ? cause.message : String(cause);
    } finally {
      setBusy(false);
    }
  }
  async function saveRoute() {
    if (!route) return;
    if (!validRoutingId(route.id)) {
      setError(
        "Use a lowercase task id with letters, digits, or hyphens. Start with a letter. Do not use auto.",
      );
      return;
    }
    if (editingId === NEW_ROUTE && drafts.some((entry) => entry.id === route.id)) {
      setError("This task id already exists.");
      return;
    }
    if (fixed && route.models.length === 0) {
      setError("Choose at least one model for a fixed fallback chain, or choose automatic mode.");
      return;
    }
    const committed = {
      ...route,
      label: route.label.trim() || route.id,
      description: route.description.trim(),
    };
    if (await persist(routeSavePayload(savedRef.current, committed))) {
      setDrafts((current) =>
        current.map((entry) => (entry.id === committed.id ? committed : entry)),
      );
      setEditingId(null);
      setFixedEmpty(null);
      setConfirmClose(false);
      setConfirmDelete(false);
    }
  }
  async function deleteRoute() {
    if (!route || BUILTIN_ROUTING_IDS.has(route.id) || editingId === NEW_ROUTE) return;
    const id = route.id;
    if (await persist(savedRef.current.filter((entry) => entry.id !== id))) {
      setDrafts((current) => current.filter((entry) => entry.id !== id));
      setEditingId(null);
      setConfirmDelete(false);
    }
  }
  async function saveSaver(patch: Partial<TokenSaverConfigView>) {
    if (!saver) return;
    const previous = state?.config.tokenSaver ?? saver;
    setSaverBusy(true);
    setError("");
    try {
      await api.saveTokenSaver(patch);
      const next = { ...saver, ...patch };
      setSaver(next);
      setState((current) =>
        current ? { ...current, config: { ...current.config, tokenSaver: next } } : current,
      );
    } catch (cause) {
      setSaver(previous);
      setError(String(cause));
    } finally {
      setSaverBusy(false);
    }
  }
  const modelOptions = useMemo(() => {
    const entries = new Map<
      string,
      { value: string; label: string; hint?: string; keywords: string }
    >();
    for (const model of models)
      entries.set(model.id, {
        value: model.id,
        label: model.id,
        keywords: providerDisplayName(model.provider),
      });
    for (const model of canonicals)
      entries.set(model.id, {
        value: model.id,
        label: model.id,
        hint: model.name,
        keywords: (providersByModel.get(model.id) ?? []).map(providerDisplayName).join(" "),
      });
    return [...entries.values()];
  }, [models, canonicals, providersByModel]);
  const options = useMemo(
    () => modelOptions.filter((entry) => !route?.models.includes(entry.value)),
    [modelOptions, route],
  );

  if (!state)
    return (
      <div>
        {error ? (
          <p role="alert" className="text-sm text-destructive">
            {error}{" "}
            <Button variant="outline" onClick={() => void load()}>
              Retry
            </Button>
          </p>
        ) : (
          <RoutingSkeleton />
        )}
      </div>
    );

  const settings = (
    <div className="grid gap-4 md:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle>Quota guard</CardTitle>
          <CardDescription>
            Skip exhausted providers when an alternative exists. Keep context checks active.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={guard.enabled}
              onChange={(event) => setGuard({ ...guard, enabled: event.target.checked })}
            />
            Enable quota guard
          </label>
          <div className="flex flex-col gap-2">
            <Label htmlFor="routing-guard-low">Low quota threshold (%)</Label>
            <Input
              id="routing-guard-low"
              type="number"
              min={0}
              max={100}
              value={guard.lowPercent}
              onChange={(event) => setGuard({ ...guard, lowPercent: Number(event.target.value) })}
            />
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={guard.resetAware}
              onChange={(event) => setGuard({ ...guard, resetAware: event.target.checked })}
            />
            Prefer the allowance that resets first
          </label>
          <div className="space-y-1 text-xs text-muted-foreground">
            {health
              .filter((entry) => entry.billing !== "api")
              .map((entry) => (
                <p key={entry.provider}>
                  {providerDisplayName(entry.provider)}: {entry.status}
                  {entry.note ? ` · ${entry.note}` : ""}
                  {entry.resetsAt ? ` · resets ${new Date(entry.resetsAt).toLocaleString()}` : ""}
                </p>
              ))}
          </div>
          <div className="flex gap-2">
            <Button
              disabled={
                busy ||
                !guardDirty ||
                !Number.isFinite(guard.lowPercent) ||
                guard.lowPercent < 0 ||
                guard.lowPercent > 100
              }
              onClick={() => void persist(savedRef.current, guard)}
            >
              Save guard
            </Button>
            <Button
              variant="ghost"
              disabled={busy || !guardDirty}
              onClick={() => setGuard(guardRef.current)}
            >
              Cancel
            </Button>
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Token saver</CardTitle>
          <CardDescription>
            Compress tool results with rtk before requests leave. Install with brew install rtk.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {saver ? (
            <>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={saver.enabled}
                  disabled={saverBusy}
                  onChange={(event) => void saveSaver({ enabled: event.target.checked })}
                />
                Enable token saver
              </label>
              <div className="flex flex-col gap-2">
                <Label htmlFor="routing-saver-command">rtk command</Label>
                <Input
                  id="routing-saver-command"
                  value={saver.command}
                  disabled={saverBusy}
                  onChange={(event) => setSaver({ ...saver, command: event.target.value })}
                  onBlur={() => void saveSaver({ command: saver.command })}
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="routing-saver-timeout">Timeout (ms)</Label>
                <Input
                  id="routing-saver-timeout"
                  type="number"
                  min={0}
                  value={saver.timeoutMs}
                  disabled={saverBusy}
                  onChange={(event) =>
                    setSaver({ ...saver, timeoutMs: Number(event.target.value) })
                  }
                  onBlur={() => {
                    if (Number.isFinite(saver.timeoutMs) && saver.timeoutMs >= 0)
                      void saveSaver({ timeoutMs: saver.timeoutMs });
                  }}
                />
              </div>
            </>
          ) : (
            <p className="text-sm text-muted-foreground">Token saver settings are unavailable.</p>
          )}
        </CardContent>
      </Card>
    </div>
  );

  return (
    <section
      className="flex flex-col gap-4"
      aria-label={settingsOnly ? "Routing settings" : "Task routing"}
    >
      {!settingsOnly ? (
        <>
          <div className="flex items-start justify-between gap-3">
            <div>
              {embedded ? (
                <h2 className="font-semibold">Task routing</h2>
              ) : (
                <h1 className="text-lg font-semibold">Routing</h1>
              )}
              <p className="text-sm text-muted-foreground">
                Describe the task. Set a short model fallback chain.
              </p>
            </div>
            <Button
              variant="outline"
              disabled={busy || editingId !== null}
              onClick={() => {
                setNewRoute({ id: "", label: "", description: "", models: [] });
                setFixedNew(false);
                setEditingId(NEW_ROUTE);
              }}
            >
              Add task
            </Button>
          </div>
          {schedule || offersTimeBasedModels(state?.config.providers ?? []) ? (
            <ScheduleSection
              routes={drafts}
              schedule={schedule}
              status={scheduleStatus}
              derived={derived}
              names={names}
              disabled={busy || editingId !== null}
              onSave={saveSchedule}
            />
          ) : null}
          <div className="divide-y rounded-lg border">
            {drafts.map((entry) => {
              const automatic = entry.models.length === 0;
              // While a window is active and lists models for this task, those models run now.
              const timed = scheduleStatus?.active
                ? entry.windows?.[scheduleStatus.active]
                : undefined;
              const chain = timed?.length
                ? timed
                : automatic
                  ? (derived.get(entry.id) ?? [])
                  : entry.models;
              const changesByTime = Object.keys(entry.windows ?? {}).length > 0;
              return (
                <div
                  key={entry.id}
                  className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:justify-between"
                >
                  <div className="min-w-0 space-y-1">
                    <h3 className="text-sm font-medium">
                      {entry.label}{" "}
                      <span className="font-normal text-muted-foreground">· {entry.id}</span>
                    </h3>
                    <p className="text-sm text-muted-foreground">
                      {entry.description || "No task description"}
                    </p>
                    <p className="break-words text-xs">
                      <span className="text-muted-foreground">
                        {timed?.length
                          ? `Now · ${scheduleStatus?.activeLabel ?? scheduleStatus?.active}`
                          : automatic
                            ? "Automatic"
                            : "Fixed"}{" "}
                        ·{" "}
                      </span>
                      {chain
                        .slice(0, 3)
                        .map((id) => names.get(id) || id)
                        .join(" → ") || "No models available"}
                      {chain.length > 3 ? ` → +${chain.length - 3} more` : ""}
                      {changesByTime ? (
                        <span className="text-muted-foreground"> · changes by time</span>
                      ) : null}
                    </p>
                  </div>
                  <Button
                    className="self-start shrink-0"
                    variant="outline"
                    size="sm"
                    disabled={busy || editingId !== null}
                    onClick={() => {
                      setEditingId(entry.id);
                      setConfirmClose(false);
                      setConfirmDelete(false);
                    }}
                  >
                    Customize
                  </Button>
                </div>
              );
            })}
          </div>
          <details className="rounded-lg border">
            <summary className="cursor-pointer p-3 text-sm text-muted-foreground">
              How routing works
            </summary>
            <div className="space-y-2 border-t p-3 text-sm text-muted-foreground">
              <p>
                The brain selects a task and thinking level. An explicit task or model skips the
                brain.
              </p>
              <p>
                The first healthy model in the fallback chain serves the request. Context checks
                withhold models that cannot fit the conversation. Skipped models appear in
                x-jevonian-skipped.
              </p>
              <p>
                If no model fits, routing compacts the history and tries again. A mid-turn quota
                error can route to another provider once.
              </p>
            </div>
          </details>
        </>
      ) : null}
      {settingsOnly ? (
        settings
      ) : embedded ? null : (
        <details className="rounded-lg border">
          <summary className="cursor-pointer p-3 text-sm text-muted-foreground">
            Routing settings · quota guard and token saver
          </summary>
          <div className="border-t p-3">{settings}</div>
        </details>
      )}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      {message ? (
        <p role="status" className="text-sm text-muted-foreground">
          {message}
        </p>
      ) : null}
      <Sheet
        open={editingId !== null}
        onOpenChange={(open) => {
          if (!open) closeEditor();
        }}
      >
        <SheetContent className="w-full sm:w-full sm:max-w-xl" showCloseButton={!busy}>
          <SheetHeader>
            <SheetTitle>
              {editingId === NEW_ROUTE ? "Add task" : `Customize ${route?.label ?? "task"}`}
            </SheetTitle>
            <SheetDescription>Changes apply only when you select Save route.</SheetDescription>
          </SheetHeader>
          {route ? (
            <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-4 pb-4">
              <div className="space-y-2">
                <Label htmlFor="routing-task-id">Task id (jevonian/…)</Label>
                <Input
                  id="routing-task-id"
                  value={route.id}
                  disabled={editingId !== NEW_ROUTE || busy}
                  placeholder="frontend"
                  onChange={(event) => updateRoute({ id: event.target.value })}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="routing-task-name">Task name</Label>
                <Input
                  id="routing-task-name"
                  value={route.label}
                  disabled={busy}
                  onChange={(event) => updateRoute({ label: event.target.value })}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="routing-task-description">When to use this task</Label>
                <Input
                  id="routing-task-description"
                  value={route.description}
                  disabled={busy}
                  placeholder="React, CSS, and UI changes"
                  onChange={(event) => updateRoute({ description: event.target.value })}
                />
              </div>
              <fieldset disabled={busy} className="space-y-3">
                <legend className="mb-2 text-sm font-medium">Model selection</legend>
                <label className="flex items-start gap-2 text-sm">
                  <input
                    type="radio"
                    name="routing-model-mode"
                    checked={!fixed}
                    onChange={() => {
                      updateRoute({ models: [] });
                      if (editingId === NEW_ROUTE) setFixedNew(false);
                      setFixedEmpty(null);
                    }}
                  />
                  Automatic · derive models from the price table
                </label>
                <label className="flex items-start gap-2 text-sm">
                  <input
                    type="radio"
                    name="routing-model-mode"
                    checked={fixed}
                    onChange={() => {
                      updateRoute({
                        models: route.models.length
                          ? route.models
                          : [...(derived.get(route.id) ?? [])],
                      });
                      if (editingId === NEW_ROUTE) setFixedNew(true);
                      else setFixedEmpty(editingId);
                    }}
                  />
                  Fixed · choose and order models
                </label>
                {!fixed ? (
                  <div className="rounded-lg bg-muted p-3 text-sm">
                    <p className="mb-2 text-muted-foreground">
                      These models are derived. Select Fixed to copy and change this chain.
                    </p>
                    <p className="break-words">
                      {(derived.get(route.id) ?? []).join(" → ") ||
                        "No derived models are available for this task yet."}
                    </p>
                  </div>
                ) : (
                  <>
                    <p className="text-xs text-muted-foreground">
                      Models run from top to bottom. Expand sources to set provider order.
                    </p>
                    <OrderedList
                      items={route.models}
                      onChange={(next) => updateRoute({ models: next })}
                    >
                      {(model, index) => {
                        const discovered = providersByModel.get(model) ?? [];
                        const preferred = route.providers?.[model];
                        const sources = allowedProviders(discovered, preferred);
                        const stale = (preferred ?? []).filter(
                          (provider) => !discovered.includes(provider),
                        );
                        return (
                          <div className="space-y-2">
                            <div className="flex items-start justify-between gap-2">
                              <div className="min-w-0">
                                <p className="break-words text-sm font-medium">
                                  {index + 1}. {names.get(model) || model}
                                </p>
                                {names.get(model) ? (
                                  <p className="break-words text-xs text-muted-foreground">
                                    {model}
                                  </p>
                                ) : null}
                              </div>
                              <button
                                type="button"
                                aria-label={`Remove ${model}`}
                                className="rounded p-1 hover:bg-muted"
                                onClick={() => removeModel(model)}
                              >
                                <X className="size-4" />
                              </button>
                            </div>
                            <details>
                              <summary className="cursor-pointer text-xs text-muted-foreground">
                                Sources ·{" "}
                                {preferred === undefined
                                  ? "All providers (automatic)"
                                  : `${sources.length} allowed (explicit)`}
                              </summary>
                              <div className="mt-3 space-y-3">
                                <p className="text-xs text-muted-foreground">
                                  An explicit empty list blocks this model. All providers includes
                                  new providers automatically.
                                </p>
                                <Button
                                  size="sm"
                                  variant="outline"
                                  onClick={() =>
                                    setProviders(
                                      model,
                                      preferred === undefined ? [...discovered] : undefined,
                                    )
                                  }
                                >
                                  {preferred === undefined
                                    ? "Choose providers explicitly"
                                    : "Use all providers automatically"}
                                </Button>
                                {preferred === undefined ? (
                                  <div className="text-xs text-muted-foreground">
                                    {discovered.map(providerDisplayName).join(" · ") ||
                                      "No provider serves this model."}
                                  </div>
                                ) : (
                                  <>
                                    <OrderedList
                                      items={preferred}
                                      onChange={(next) => setProviders(model, next)}
                                    >
                                      {(provider) => (
                                        <div className="flex items-center justify-between gap-2 text-xs">
                                          <span className="flex items-center gap-2">
                                            <ProviderLogo id={provider} />
                                            {providerDisplayName(provider)} ·{" "}
                                            {stale.includes(provider)
                                              ? "not available"
                                              : (statuses.get(provider) ?? "quota unknown")}
                                          </span>
                                          <button
                                            type="button"
                                            aria-label={`Remove ${provider} from ${model}`}
                                            className="rounded p-1 hover:bg-muted"
                                            onClick={() =>
                                              setProviders(
                                                model,
                                                preferred.filter((id) => id !== provider),
                                              )
                                            }
                                          >
                                            <X className="size-4" />
                                          </button>
                                        </div>
                                      )}
                                    </OrderedList>
                                    {sources.length === 0 ? (
                                      <p className="text-xs text-destructive">
                                        No available provider. Routing will not use this model.
                                      </p>
                                    ) : null}
                                    <div className="flex flex-wrap gap-2">
                                      {discovered
                                        .filter((provider) => !preferred.includes(provider))
                                        .map((provider) => (
                                          <Button
                                            key={provider}
                                            size="sm"
                                            variant="outline"
                                            onClick={() =>
                                              setProviders(model, [...preferred, provider])
                                            }
                                          >
                                            Add {providerDisplayName(provider)}
                                          </Button>
                                        ))}
                                    </div>
                                  </>
                                )}
                              </div>
                            </details>
                          </div>
                        );
                      }}
                    </OrderedList>
                    <div className="space-y-2">
                      <Label>Add model</Label>
                      <Combobox
                        value=""
                        onChange={(model) => {
                          if (model) updateRoute({ models: [...route.models, model] });
                        }}
                        options={options}
                        placeholder="Search model id, name, or provider…"
                        emptyText="No matching model is available."
                      />
                    </div>
                  </>
                )}
              </fieldset>
              {schedule?.windows.length ? (
                <fieldset disabled={busy} className="space-y-3">
                  <legend className="mb-2 text-sm font-medium">Models by time</legend>
                  <p className="text-xs text-muted-foreground">
                    Use other models while a time window is active. Outside every window, and in
                    windows left off, the models above run.
                  </p>
                  {schedule.windows.map((slot) => {
                    const list = route.windows?.[slot.id];
                    return (
                      <div key={slot.id} className="space-y-3 rounded-lg border p-3">
                        <label className="flex items-start gap-2 text-sm">
                          <input
                            type="checkbox"
                            checked={list !== undefined}
                            onChange={(event) =>
                              setWindowModels(
                                slot.id,
                                event.target.checked
                                  ? [
                                      ...(route.models.length
                                        ? route.models
                                        : (derived.get(route.id) ?? [])),
                                    ]
                                  : undefined,
                              )
                            }
                          />
                          <span>
                            {slot.label} · {windowRange(slot)}
                            {scheduleStatus?.active === slot.id ? " · now" : ""}
                            <br />
                            <span className="text-xs text-muted-foreground">
                              Use different models in this window
                            </span>
                          </span>
                        </label>
                        {list !== undefined ? (
                          <>
                            <OrderedList
                              items={list}
                              onChange={(next) => setWindowModels(slot.id, next)}
                            >
                              {(model, index) => (
                                <div className="flex items-start justify-between gap-2">
                                  <div className="min-w-0">
                                    <p className="break-words text-sm font-medium">
                                      {index + 1}. {names.get(model) || model}
                                    </p>
                                    {names.get(model) ? (
                                      <p className="break-words text-xs text-muted-foreground">
                                        {model}
                                      </p>
                                    ) : null}
                                  </div>
                                  <button
                                    type="button"
                                    aria-label={`Remove ${model} from ${slot.label}`}
                                    className="rounded p-1 hover:bg-muted"
                                    onClick={() =>
                                      setWindowModels(
                                        slot.id,
                                        list.filter((id) => id !== model),
                                      )
                                    }
                                  >
                                    <X className="size-4" />
                                  </button>
                                </div>
                              )}
                            </OrderedList>
                            <div className="space-y-2">
                              <Label>Add model for {slot.label}</Label>
                              <Combobox
                                value=""
                                onChange={(model) => {
                                  if (model && !list.includes(model))
                                    setWindowModels(slot.id, [...list, model]);
                                }}
                                options={modelOptions.filter(
                                  (entry) => !list.includes(entry.value),
                                )}
                                placeholder="Search model id, name, or provider…"
                                emptyText="No matching model is available."
                              />
                            </div>
                            {list.length === 0 ? (
                              <p className="text-xs text-destructive">
                                Add a model, or turn this off. An empty list is ignored.
                              </p>
                            ) : null}
                          </>
                        ) : null}
                      </div>
                    );
                  })}
                </fieldset>
              ) : null}
              {error ? (
                <p role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              ) : null}
              {confirmClose ? (
                <div role="alert" className="space-y-3 rounded-lg border p-3 text-sm">
                  <p>Discard unsaved route changes?</p>
                  <div className="flex gap-2">
                    <Button variant="destructive" onClick={() => closeEditor(true)}>
                      Discard changes
                    </Button>
                    <Button variant="outline" onClick={() => setConfirmClose(false)}>
                      Keep editing
                    </Button>
                  </div>
                </div>
              ) : null}
              {confirmDelete ? (
                <div role="alert" className="space-y-3 rounded-lg border p-3 text-sm">
                  <p>
                    Delete this task? Clients that use jevonian/{route.id} will need another route.
                  </p>
                  <div className="flex gap-2">
                    <Button
                      variant="destructive"
                      disabled={busy}
                      onClick={() => void deleteRoute()}
                    >
                      Confirm delete
                    </Button>
                    <Button
                      variant="outline"
                      disabled={busy}
                      onClick={() => setConfirmDelete(false)}
                    >
                      Keep task
                    </Button>
                  </div>
                </div>
              ) : null}
            </div>
          ) : null}
          <SheetFooter className="border-t">
            <div className="flex flex-wrap items-center justify-between gap-2">
              {route && editingId !== NEW_ROUTE && !BUILTIN_ROUTING_IDS.has(route.id) ? (
                <Button variant="ghost" disabled={busy} onClick={() => setConfirmDelete(true)}>
                  Delete task
                </Button>
              ) : (
                <span className="text-xs text-muted-foreground">
                  {editingId !== NEW_ROUTE ? "Built-in task" : "New task"}
                </span>
              )}
              <div className="flex gap-2">
                <Button variant="outline" disabled={busy} onClick={() => closeEditor()}>
                  Cancel
                </Button>
                <Button disabled={busy || !route} onClick={() => void saveRoute()}>
                  {busy ? "Saving…" : "Save route"}
                </Button>
              </div>
            </div>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </section>
  );
}
