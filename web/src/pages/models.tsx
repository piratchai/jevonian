import { useCallback, useEffect, useState } from "react";
import { useLocation } from "react-router";

import { BrainSection } from "@/components/brain-section";
import { api, type StateResponse } from "@/lib/api";
import { ProviderSettings, ProvidersPage } from "@/pages/providers";
import { RoutingPage } from "@/pages/routing";

/** One workspace for task routes and the connections that supply their models. */
export function ModelsPage() {
  const { hash } = useLocation();
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<StateResponse | null>(null);
  const [error, setError] = useState("");
  const [brainOpen, setBrainOpen] = useState(hash === "#routing-brain");

  const reload = useCallback(async () => {
    try {
      setState(await api.state());
      setError("");
    } catch (cause) {
      setError(String(cause));
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload, revision]);

  useEffect(() => {
    if (hash === "#routing-brain") setBrainOpen(true);
    if (hash) {
      const frame = requestAnimationFrame(() => {
        document.getElementById(hash.slice(1))?.scrollIntoView({ block: "start" });
      });
      return () => cancelAnimationFrame(frame);
    }
  }, [hash, state]);

  const onChanged = useCallback(() => setRevision((value) => value + 1), []);

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-1">
        <h1 className="text-lg font-semibold">Models &amp; Routing</h1>
        <p className="text-sm text-muted-foreground">
          Choose models for each task. Connect the providers that supply them.
        </p>
      </header>
      <section id="task-routes" className="scroll-mt-20" aria-label="Task routes">
        <RoutingPage embedded refreshKey={revision} onChanged={onChanged} />
      </section>
      <section id="providers" className="scroll-mt-20" aria-label="Connected sources">
        <ProvidersPage embedded refreshKey={revision} onChanged={onChanged} />
      </section>
      <details
        id="routing-brain"
        className="scroll-mt-20 rounded-md border"
        open={brainOpen}
        onToggle={(event) => setBrainOpen(event.currentTarget.open)}
      >
        <summary className="cursor-pointer px-4 py-3 text-sm font-medium">
          Routing settings · auto selector, quota guard, and token saver
        </summary>
        <div className="space-y-4 border-t p-4">
          <p className="mb-4 text-sm text-muted-foreground">
            The routing brain chooses a task for auto requests. Explicit tasks and models do not use
            the brain.
          </p>
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
          <RoutingPage settingsOnly refreshKey={revision} onChanged={onChanged} />
          <ProviderSettings onChanged={onChanged} />
          {state ? (
            <BrainSection
              state={state}
              onSaved={(next) => {
                setState(next);
                onChanged();
              }}
            />
          ) : !error ? (
            <p className="text-sm text-muted-foreground">Loading auto selector settings…</p>
          ) : null}
        </div>
      </details>
    </div>
  );
}
