import { Plus, X } from "lucide-react";
import { useMemo, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { RoutingEntryView, ScheduleStatusView, ScheduleView } from "@/lib/api";
import {
  browserZone,
  knownZones,
  MAX_SCHEDULE_WINDOWS,
  runsPastMidnight,
  scheduleError,
  scheduleSummary,
  windowRange,
  windowSlug,
} from "@/lib/schedule";

export interface ScheduleSectionProps {
  routes: RoutingEntryView[];
  schedule: ScheduleView | undefined;
  status: ScheduleStatusView | undefined;
  /** Models an automatic routing derives, shown when it lists none of its own. */
  derived: Map<string, string[]>;
  /**
   * What each task runs on right now, with the active window applied. An automatic task can pick
   * different models while a window changes an earlier task's list, which `derived` does not show.
   */
  effective?: Record<string, string[]>;
  names: Map<string, string | undefined>;
  disabled: boolean;
  /** Saves the schedule (null removes it). Returns an error message, or null on success. */
  onSave: (next: ScheduleView | null) => Promise<string | null>;
}

const EMPTY: ScheduleView = { timezone: "", windows: [] };

/** First two model names, then a count. */
function chainText(models: string[], names: Map<string, string | undefined>): string {
  if (models.length === 0) return "No models available";
  const shown = models
    .slice(0, 2)
    .map((id) => names.get(id) || id)
    .join(" → ");
  return models.length > 2 ? `${shown} → +${models.length - 2} more` : shown;
}

/**
 * Which models each task uses at which time of day. The table answers "what runs when"; the
 * sheet edits the time zone and the windows. Models for a window are set per task, in
 * Customize.
 */
export function ScheduleSection({
  routes,
  schedule,
  status,
  derived,
  effective,
  names,
  disabled,
  onSave,
}: ScheduleSectionProps) {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<ScheduleView>(EMPTY);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const zones = useMemo(() => knownZones(), []);
  const windows = schedule?.windows ?? [];

  function openEditor() {
    setDraft(
      schedule
        ? {
            timezone: schedule.timezone,
            windows: schedule.windows.map((window) => ({ ...window })),
          }
        : { timezone: browserZone(), windows: [] },
    );
    setError("");
    setConfirmRemove(false);
    setOpen(true);
  }
  function closeEditor() {
    if (!busy) setOpen(false);
  }
  function updateWindow(index: number, patch: Partial<ScheduleView["windows"][number]>) {
    setDraft((current) => ({
      ...current,
      windows: current.windows.map((window, i) => (i === index ? { ...window, ...patch } : window)),
    }));
  }
  function addWindow() {
    setDraft((current) => {
      const first = current.windows.length === 0;
      const label = first ? "Off-nights" : `Window ${current.windows.length + 1}`;
      return {
        ...current,
        windows: [
          ...current.windows,
          {
            id: windowSlug(
              label,
              current.windows.map((window) => window.id),
            ),
            label,
            start: first ? "22:00" : "09:00",
            end: first ? "08:00" : "17:00",
          },
        ],
      };
    });
  }
  async function save(next: ScheduleView | null) {
    if (next) {
      const message = scheduleError(next);
      if (message) {
        setError(message);
        return;
      }
    }
    setBusy(true);
    setError("");
    const message = await onSave(next);
    setBusy(false);
    if (message) setError(message);
    else setOpen(false);
  }
  function commit() {
    const timezone = draft.timezone.trim();
    // A zone alone does nothing, so an empty draft means "no schedule".
    if (draft.windows.length === 0 && !timezone) return void save(null);
    void save({
      timezone,
      windows: draft.windows.map((window) => ({ ...window, label: window.label.trim() })),
    });
  }

  return (
    <>
      <Card aria-label="Schedule">
        <CardHeader>
          <div className="flex items-start justify-between gap-3">
            <div className="space-y-1.5">
              <CardTitle>Schedule</CardTitle>
              <CardDescription>
                {schedule && status
                  ? scheduleSummary(status, windows)
                  : "Use different models at different times of day, for example cheaper models during an off-peak discount."}
              </CardDescription>
            </div>
            <Button variant="outline" size="sm" disabled={disabled} onClick={openEditor}>
              {schedule ? "Edit schedule" : "Set up a schedule"}
            </Button>
          </div>
        </CardHeader>
        {windows.length > 0 ? (
          <CardContent>
            <Table aria-label="Models by time">
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Task</TableHead>
                  {windows.map((window) => (
                    <TableHead key={window.id}>
                      <span className="text-foreground">{window.label}</span>{" "}
                      {status?.active === window.id ? <Badge variant="default">now</Badge> : null}
                      <br />
                      <span className="font-normal">{windowRange(window)}</span>
                      {runsPastMidnight(window) ? (
                        <span className="font-normal"> · past midnight</span>
                      ) : null}
                    </TableHead>
                  ))}
                  <TableHead>
                    <span className="text-foreground">Other times</span>{" "}
                    {status && !status.active ? <Badge variant="default">now</Badge> : null}
                    <br />
                    <span className="font-normal">Default models</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {routes.map((route) => {
                  const fallback = route.models.length
                    ? route.models
                    : (derived.get(route.id) ?? []);
                  // In the active window, a task with no list of its own can still run on other
                  // models than at other times, when it picks automatically.
                  const now = effective?.[route.id];
                  const shifted = Boolean(now && now.join("\n") !== fallback.join("\n"));
                  return (
                    <TableRow key={route.id}>
                      <TableCell className="whitespace-nowrap font-medium">{route.label}</TableCell>
                      {windows.map((window) => {
                        const own = route.windows?.[window.id];
                        return (
                          <TableCell
                            key={window.id}
                            className={status?.active === window.id ? "bg-muted/60" : undefined}
                          >
                            {own?.length ? (
                              chainText(own, names)
                            ) : status?.active === window.id && shifted && now ? (
                              chainText(now, names)
                            ) : (
                              <span className="text-muted-foreground">Same as other times</span>
                            )}
                          </TableCell>
                        );
                      })}
                      <TableCell className={status && !status.active ? "bg-muted/60" : undefined}>
                        {chainText(fallback, names)}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
            <p className="mt-3 text-xs text-muted-foreground">
              Windows repeat every day
              {schedule?.timezone ? ` in ${schedule.timezone}` : " in this machine's time zone"}.
              The first window that contains the time wins. Choose Customize on a task to set its
              models for each window.
            </p>
          </CardContent>
        ) : null}
      </Card>
      <Sheet
        open={open}
        onOpenChange={(next) => {
          if (!next) closeEditor();
        }}
      >
        <SheetContent className="w-full sm:w-full sm:max-w-xl" showCloseButton={!busy}>
          <SheetHeader>
            <SheetTitle>Schedule</SheetTitle>
            <SheetDescription>
              Name the time ranges. Then set each task's models for a range with Customize.
            </SheetDescription>
          </SheetHeader>
          <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-4 pb-4">
            <div className="space-y-2">
              <Label htmlFor="schedule-timezone">Time zone</Label>
              <Input
                id="schedule-timezone"
                list="schedule-zones"
                value={draft.timezone}
                disabled={busy}
                placeholder="Asia/Singapore"
                onChange={(event) => setDraft({ ...draft, timezone: event.target.value })}
              />
              <datalist id="schedule-zones">
                {zones.map((zone) => (
                  <option key={zone} value={zone} />
                ))}
              </datalist>
              <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <span>Leave empty to use the time zone of the machine that runs Jevonian.</span>
                {browserZone() && draft.timezone !== browserZone() ? (
                  <Button
                    size="xs"
                    variant="outline"
                    disabled={busy}
                    onClick={() => setDraft({ ...draft, timezone: browserZone() })}
                  >
                    Use {browserZone()}
                  </Button>
                ) : null}
              </div>
            </div>
            <fieldset disabled={busy} className="space-y-3">
              <legend className="mb-2 text-sm font-medium">Time windows</legend>
              {draft.windows.length === 0 ? (
                <p className="rounded-lg bg-muted p-3 text-sm text-muted-foreground">
                  No windows yet. A window is a daily time range, such as 22:00 to 08:00 for an
                  off-peak discount.
                </p>
              ) : null}
              {draft.windows.map((window, index) => (
                <div key={window.id} className="space-y-3 rounded-lg border p-3">
                  <div className="flex items-end gap-2">
                    <div className="min-w-0 flex-1 space-y-2">
                      <Label htmlFor={`schedule-label-${window.id}`}>Name</Label>
                      <Input
                        id={`schedule-label-${window.id}`}
                        value={window.label}
                        onChange={(event) => updateWindow(index, { label: event.target.value })}
                      />
                    </div>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={`Remove ${window.label || "window"}`}
                      onClick={() =>
                        setDraft({
                          ...draft,
                          windows: draft.windows.filter((_, i) => i !== index),
                        })
                      }
                    >
                      <X />
                    </Button>
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-2">
                      <Label htmlFor={`schedule-start-${window.id}`}>From</Label>
                      <Input
                        id={`schedule-start-${window.id}`}
                        type="time"
                        value={window.start}
                        onChange={(event) => updateWindow(index, { start: event.target.value })}
                      />
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor={`schedule-end-${window.id}`}>Until</Label>
                      <Input
                        id={`schedule-end-${window.id}`}
                        type="time"
                        value={window.end}
                        onChange={(event) => updateWindow(index, { end: event.target.value })}
                      />
                    </div>
                  </div>
                  {runsPastMidnight(window) ? (
                    <p className="text-xs text-muted-foreground">
                      This window runs past midnight: it ends the next day.
                    </p>
                  ) : null}
                </div>
              ))}
              <Button
                variant="outline"
                size="sm"
                disabled={draft.windows.length >= MAX_SCHEDULE_WINDOWS}
                onClick={addWindow}
              >
                <Plus /> Add window
              </Button>
            </fieldset>
            {error ? (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            ) : null}
            {confirmRemove ? (
              <div role="alert" className="space-y-3 rounded-lg border p-3 text-sm">
                <p>
                  Remove the schedule? Every task goes back to its default models. The models you
                  set for each window are deleted.
                </p>
                <div className="flex gap-2">
                  <Button variant="destructive" disabled={busy} onClick={() => void save(null)}>
                    Remove schedule
                  </Button>
                  <Button variant="outline" disabled={busy} onClick={() => setConfirmRemove(false)}>
                    Keep schedule
                  </Button>
                </div>
              </div>
            ) : null}
          </div>
          <SheetFooter className="border-t">
            <div className="flex flex-wrap items-center justify-between gap-2">
              {schedule ? (
                <Button variant="ghost" disabled={busy} onClick={() => setConfirmRemove(true)}>
                  Remove schedule
                </Button>
              ) : (
                <span className="text-xs text-muted-foreground">New schedule</span>
              )}
              <div className="flex gap-2">
                <Button variant="outline" disabled={busy} onClick={closeEditor}>
                  Cancel
                </Button>
                <Button disabled={busy} onClick={commit}>
                  {busy ? "Saving…" : "Save schedule"}
                </Button>
              </div>
            </div>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </>
  );
}
