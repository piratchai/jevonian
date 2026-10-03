import { identityFrom, type ModelIdentity } from "./identity";
import { bareModelId, modelVendor } from "./model-id";
import { loadIdentities, OFFICIAL_PROVIDERS, type CatalogIdentity } from "./modelsdev";

/**
 * The model catalog indexed once for identity lookup, rebuilt only when the catalog snapshot
 * changes. One index, one cache: every lookup that asks "what model is this id" — the label,
 * the vendor that owns it, the canonical fallback — reads the same structure.
 */
export interface IdentityIndex {
  /** The catalog entry as written: `deepseek/deepseek-v4.1-flash`. */
  byKey: Map<string, CatalogIdentity>;
  /** The bare model segment: `deepseek-v4.1-flash`. */
  byBare: Map<string, CatalogIdentity>;
  /** The canonical id, so a tier written `deepseek-v4-1-flash` still finds its label. */
  byCanonical: Map<string, CatalogIdentity>;
  /** Vendor ids that publish a model under a given comparable label. */
  officialByLabel: Map<string, Set<string>>;
}

const indexes = new WeakMap<object, IdentityIndex>();

/**
 * The index for the current catalog snapshot. `canonical` is passed in rather than imported
 * because the canonical-id rule lives with the model grouping that owns it, and importing it
 * here would make the two modules depend on each other.
 */
export function identityIndex(canonical: (model: string) => string): IdentityIndex {
  const raw = loadIdentities();
  const cached = indexes.get(raw);
  if (cached) return cached;

  const byKey = new Map<string, CatalogIdentity>();
  const byBare = new Map<string, CatalogIdentity>();
  const byCanonical = new Map<string, CatalogIdentity>();
  const officialByLabel = new Map<string, Set<string>>();

  // Prefer an entry that actually names the model, and among those an official vendor's —
  // resellers often ship the same id as the vendor with a reseller-flavoured label.
  const claim = (
    map: Map<string, CatalogIdentity>,
    mapKey: string,
    identity: CatalogIdentity,
    official: boolean,
  ): void => {
    const existing = map.get(mapKey);
    const better =
      !existing ||
      (!existing.name && Boolean(identity.name)) ||
      (Boolean(existing.name) && Boolean(identity.name) && official);
    if (better) map.set(mapKey, identity);
  };

  for (const [key, entry] of Object.entries(raw)) {
    const model = bareModelId(key);
    const resolved: ModelIdentity = identityFrom(model, entry as CatalogIdentity | undefined);
    const vendor = modelVendor(key);
    const official = Boolean(vendor) && OFFICIAL_PROVIDERS.has(vendor);
    const identity = toCatalog(resolved);
    // The official preference reads the map key's own vendor prefix, which only the full
    // catalog key carries; the bare and canonical maps keep first-named-wins.
    claim(byKey, key, identity, official);
    claim(byBare, model, identity, false);
    const canonicalId = canonical(model);
    if (canonicalId.length > 0) claim(byCanonical, canonicalId, identity, false);
    if (official && resolved.label) {
      const set = officialByLabel.get(resolved.label) ?? new Set<string>();
      set.add(vendor);
      officialByLabel.set(resolved.label, set);
    }
  }

  const index: IdentityIndex = { byKey, byBare, byCanonical, officialByLabel };
  indexes.set(raw, index);
  return index;
}

function toCatalog(identity: ModelIdentity): CatalogIdentity {
  return {
    ...(identity.displayName ? { name: identity.displayName } : {}),
    ...(identity.family ? { family: identity.family } : {}),
    ...(identity.releaseDate ? { releaseDate: identity.releaseDate } : {}),
  };
}
