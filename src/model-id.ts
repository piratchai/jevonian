/**
 * Model id spelling, the one place it is parsed.
 *
 * Catalogs and resellers prefix a model with its vendor (`openai/gpt-6`, `deepseek/deepseek-v4`),
 * some nest more than one segment (`accounts/fireworks/models/kimi-k3`), and a client may send
 * either form. Every lookup that compares models — pricing, capabilities, identity, the
 * leaderboard — wants the bare model segment, so it is computed here once rather than as a
 * hand-copied `lastIndexOf("/")` in each module. A leaf with no imports, so any module can use it
 * without a dependency cycle.
 */

/** The model segment after the last `/`; the id unchanged when it has no prefix. */
export function bareModelId(id: string): string {
  const slash = id.lastIndexOf("/");
  if (slash < 0) return id;
  return id.slice(slash + 1) || id;
}

/** The vendor segment before the first `/`, or "" when the id has no prefix. */
export function modelVendor(id: string): string {
  const slash = id.indexOf("/");
  return slash > 0 ? id.slice(0, slash) : "";
}
