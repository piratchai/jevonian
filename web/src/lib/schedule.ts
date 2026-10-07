import type {
  RoutingEntryView,
  ScheduleStatusView,
  ScheduleView,
  ScheduleWindowView,
} from "./api.ts";

/** The server accepts at most this many windows. */
export const MAX_SCHEDULE_WINDOWS = 12;

const CLOCK = /^([01]\d|2[0-3]):[0-5]\d$/;

/** HH:MM on a 24-hour clock. */
export function validClock(value: string): boolean {
  return CLOCK.test(value);
}

/** A window whose end is before its start covers midnight, such as 22:00 to 08:00. */
export function runsPastMidnight(window: Pick<ScheduleWindowView, "start" | "end">): boolean {
  return validClock(window.start) && validClock(window.end) && window.end < window.start;
}

export function windowRange(window: Pick<ScheduleWindowView, "start" | "end">): string {
  return `${window.start}–${window.end}`;
}

/** A unique window id (lowercase letters, digits, hyphens; starts with a letter) from a label. */
export function windowSlug(label: string, taken: string[]): string {
  let base =
    label
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^[^a-z]+/, "")
      .replace(/-+$/, "")
      .slice(0, 40) || "window";
  if (base === "auto") base = "auto-window";
  let id = base;
  for (let n = 2; taken.includes(id); n += 1) id = `${base}-${n}`;
  return id;
}

export function validZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat("en", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

/** The zone this browser runs in, a good default for a new schedule. */
export function browserZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
}

/** IANA zone names this browser knows, for the zone picker. May be empty on old browsers. */
export function knownZones(): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: (key: string) => string[] };
  try {
    return intl.supportedValuesOf?.("timeZone") ?? [];
  } catch {
    return [];
  }
}

/** Why a schedule cannot be saved, or null. The server checks again. */
export function scheduleError(schedule: ScheduleView): string | null {
  if (schedule.timezone && !validZone(schedule.timezone)) {
    return `"${schedule.timezone}" is not a time zone name. Use a name such as Asia/Singapore.`;
  }
  if (schedule.windows.length > MAX_SCHEDULE_WINDOWS) {
    return `Use at most ${MAX_SCHEDULE_WINDOWS} windows.`;
  }
  for (const [index, window] of schedule.windows.entries()) {
    if (!window.label.trim()) return `Name window ${index + 1}.`;
    if (!validClock(window.start) || !validClock(window.end)) {
      return `${window.label.trim()}: choose a start and an end time.`;
    }
    if (window.start === window.end) {
      return `${window.label.trim()}: the start and the end must differ.`;
    }
  }
  return null;
}

/** Drop per-window model lists whose window is gone, and empty lists. */
export function pruneWindowLists(
  routes: RoutingEntryView[],
  schedule: ScheduleView | null,
): RoutingEntryView[] {
  const known = new Set(schedule?.windows.map((window) => window.id) ?? []);
  return routes.map((route) => {
    if (!route.windows) return route;
    const kept = Object.fromEntries(
      Object.entries(route.windows).filter(([id, models]) => known.has(id) && models.length > 0),
    );
    if (Object.keys(kept).length > 0) return { ...route, windows: kept };
    const copy = { ...route };
    delete copy.windows;
    return copy;
  });
}

/** HH:MM of an RFC 3339 time, read in the zone the server wrote it in. */
export function clockOf(iso: string | undefined): string {
  return iso && iso.length >= 16 ? iso.slice(11, 16) : "";
}

/** "today", "tomorrow", or the date, for `iso` as seen from `nowIso` (both in the schedule zone). */
export function dayWord(iso: string | undefined, nowIso: string | undefined): string {
  if (!iso || !nowIso) return "";
  const day = iso.slice(0, 10);
  const today = nowIso.slice(0, 10);
  if (day === today) return "today";
  const next = new Date(`${today}T00:00:00Z`);
  next.setUTCDate(next.getUTCDate() + 1);
  return day === next.toISOString().slice(0, 10) ? "tomorrow" : day;
}

/** One sentence on what is active now and when that changes. */
export function scheduleSummary(status: ScheduleStatusView, windows: ScheduleWindowView[]): string {
  const label = (id: string | undefined) => windows.find((window) => window.id === id)?.label ?? id;
  const now = `It is ${clockOf(status.now)} in ${status.timezone}.`;
  const active = status.active
    ? `${status.activeLabel ?? label(status.active)} is active.`
    : "No window is active, so every task uses its default models.";
  if (!status.nextChange) return `${now} ${active}`;
  const when = `${clockOf(status.nextChange)} ${dayWord(status.nextChange, status.now)}`;
  const next = status.nextActive
    ? `${label(status.nextActive)} starts at ${when}.`
    : `${status.activeLabel ?? "The window"} ends at ${when}.`;
  return `${now} ${active} ${next}`;
}
