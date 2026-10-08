import { Text } from "@cloudflare/kumo";
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
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-1">
        <Text variant="heading" size="lg" as="h1">
          Models &amp; Routing
        </Text>
        <Text variant="secondary" size="sm">
          Choose models for each task. Connect the providers that supply them.
        </Text>
      </header>
      <section id="task-routes" className="scroll-mt-24" aria-label="Task routes">
        <RoutingPage embedded refreshKey={revision} onChanged={onChanged} />
      </section>
      <section id="providers" className="scroll-mt-24" aria-label="Connected sources">
        <ProvidersPage embedded refreshKey={revision} onChanged={onChanged} />
      </section>
      <details
        id="routing-brain"
        className="scroll-mt-24 rounded-lg border border-kumo-hairline"
        open={brainOpen}
        onToggle={(event) => setBrainOpen(event.currentTarget.open)}
      >
        <summary className="cursor-pointer px-4 py-3 text-sm font-medium text-kumo-default">
          Routing settings · auto selector, quota guard, and token saver
        </summary>
        <div className="flex flex-col gap-4 border-t border-kumo-hairline p-4">
          <Text variant="secondary" size="sm">
            The routing brain chooses a task for auto requests. Explicit tasks and models do not use
            the brain.
          </Text>
          {error ? (
            <Text variant="error" size="sm" as="p" role="alert">
              {error}
            </Text>
          ) : null}
          <RoutingPage settingsOnly embedded refreshKey={revision} onChanged={onChanged} />
          <ProviderSettings embedded onChanged={onChanged} />
          {state ? (
            <BrainSection
              state={state}
              onSaved={(next) => {
                setState(next);
                onChanged();
              }}
            />
          ) : !error ? (
            <Text variant="secondary" size="sm">
              Loading auto selector settings…
            </Text>
          ) : null}
        </div>
      </details>
    </div>
  );
}
