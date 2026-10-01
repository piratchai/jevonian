import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { collapseCursorModels, cursorContext, splitCursorId, type CursorModel } from "./cursor";
import { dataDir } from "./paths";

/**
 * Cursor's own model ids, kept on disk. Cursor lists one entry per effort and speed
 * ("grok-4.7-low", "grok-4.7-low-fast", …), so the raw list is what maps a chosen effort onto
 * the id Cursor expects; the collapsed list is what the picker shows.
 */
export interface CursorCatalogFile {
  fetchedAt: string;
  /** Every id Cursor listed, in order, with its name and context. */
  raw: Array<{ id: string; name: string; context: number }>;
  /** The families the picker offers. */
  models: Array<{ id: string; name: string; context: number }>;
}

export function cursorCatalogPath(): string {
  return join(dataDir(), "cursor-models.json");
}

let cache: { path: string; file: CursorCatalogFile } | undefined;

function modelOf(raw: unknown): CursorModel | undefined {
  if (typeof raw !== "object" || raw === null) return undefined;
  const value = raw as Record<string, unknown>;
  const id = typeof value.id === "string" ? value.id : "";
  if (id.length === 0) return undefined;
  const name = typeof value.name === "string" ? value.name : id;
  const context =
    typeof value.context === "number" && value.context > 0 ? value.context : cursorContext(id, name);
  return { id, name, context };
}

function parseFile(raw: unknown): CursorCatalogFile | undefined {
  if (typeof raw !== "object" || raw === null) return undefined;
  const value = raw as Record<string, unknown>;
  const rawModels = (Array.isArray(value.raw) ? value.raw : []).flatMap((entry) => {
    const model = modelOf(entry);
    return model ? [model] : [];
  });
  const models = (Array.isArray(value.models) ? value.models : []).flatMap((entry) => {
    const model = modelOf(entry);
    return model ? [model] : [];
  });
  if (rawModels.length === 0) return undefined;
  return {
    fetchedAt: typeof value.fetchedAt === "string" ? value.fetchedAt : "",
    raw: rawModels,
    models: models.length > 0 ? models : collapseCursorModels(rawModels),
  };
}

export function loadCursorCatalog(): CursorCatalogFile | undefined {
  const path = cursorCatalogPath();
  if (cache?.path === path) return cache.file;
  if (!existsSync(path)) return undefined;
  try {
    const file = parseFile(JSON.parse(readFileSync(path, "utf8")));
    if (!file) return undefined;
    cache = { path, file };
    return file;
  } catch {
    return undefined;
  }
}

/** Persist what `cursor-agent models` listed, both as-is and collapsed for the picker. */
export function saveCursorCatalog(raw: CursorModel[]): CursorCatalogFile {
  const file: CursorCatalogFile = {
    fetchedAt: new Date().toISOString(),
    raw,
    models: collapseCursorModels(raw),
  };
  const path = cursorCatalogPath();
  try {
    mkdirSync(dirname(path), { recursive: true });
    const temporary = `${path}.${process.pid}.tmp`;
    writeFileSync(temporary, `${JSON.stringify(file, null, 2)}\n`);
    renameSync(temporary, path);
  } catch {
    // The catalog is a picker/pricing hint; failing to cache it must not fail discovery.
  }
  cache = { path, file };
  return file;
}

/** Drop the in-memory copy so the next lookup re-reads disk. For tests. */
export function resetCursorCatalog(): void {
  cache = undefined;
}

/**
 * Cursor's id for a model the picker offers, at the effort asked for: the family's variant at
 * it; else, for an effort between the ones it has, the one Cursor picks by default; else the
 * nearest. Fast, when asked, comes from the family's fast variant. An id Cursor itself listed
 * (an agent set to it before) is kept, its effort and fast taken as it has them.
 */
export function cursorModelId(
  model: string,
  effort: string | undefined,
  fast: boolean,
): string {
  const file = loadCursorCatalog();
  if (!file) return model;
  const families = new Map<string, Map<string, string>>();
  for (const entry of file.raw) {
    const { family, effort: level } = splitCursorId(entry.id);
    let by = families.get(family);
    if (!by) {
      by = new Map();
      families.set(family, by);
    }
    if (!by.has(level)) by.set(level, entry.id);
  }
  const at = (family: string, level: string): string | undefined => families.get(family)?.get(level);

  // An id Cursor listed is used as it is, but its effort and speed follow the request.
  const own = splitCursorId(model);
  if (own.family !== model) {
    let family = own.family;
    if (fast && !family.endsWith("-fast")) {
      const candidate = `${family}-fast`;
      if (families.has(candidate)) family = candidate;
    }
    const level = effort || own.effort;
    return (level && at(family, level)) || at(family, "") || model;
  }

  let family = model;
  if (fast && !family.endsWith("-fast")) {
    const candidate = `${family}-fast`;
    if (families.has(candidate)) family = candidate;
  }
  const by = families.get(family);
  if (!by) return model;
  if (!effort) return by.get("") ?? model;
  const exact = by.get(effort);
  if (exact) return exact;
  // Nearest level that exists, preferring the model's own default.
  const levels = ["none", "minimal", "low", "medium", "high", "xhigh", "max"].filter((level) =>
    by.has(level),
  );
  if (levels.length === 0) return by.get("") ?? model;
  const target = ["none", "minimal", "low", "medium", "high", "xhigh", "max"].indexOf(effort);
  let best = levels[0] as string;
  let bestDistance = Number.POSITIVE_INFINITY;
  for (const level of levels) {
    const distance = Math.abs(
      ["none", "minimal", "low", "medium", "high", "xhigh", "max"].indexOf(level) - target,
    );
    if (distance < bestDistance) {
      bestDistance = distance;
      best = level;
    }
  }
  return by.get(best) ?? by.get("") ?? model;
}
