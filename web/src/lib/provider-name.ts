/**
 * Provider identity: the brand a provider belongs to, and which account of it this is.
 *
 * A provider's `name` is user-chosen, so a second account of one agent (a work and a home
 * Claude Code, two Codex sign-ins, two OpenCode keys) reads as a different string. Deriving
 * the logo and the label from the name alone loses the brand — `claude-work` fell back to a
 * "CW" initials box — and shows the account twice. This module splits the two: a stable
 * `brand` for the logo and product name, and an `account` for the second sign-in.
 *
 * Brand resolution order, most reliable first:
 *   1. `oauthSource` — a provider that reads a named local sign-in names its brand outright.
 *   2. the provider `name` — the longest known preset id or brand token that prefixes it.
 *   3. `type` — a last resort for well-known wires.
 */

/** The provider fields brand resolution reads. Every field is optional. */
export interface ProviderIdentityInput {
  /** Provider name, the routing key (`claude-work`). */
  name?: string;
  /** Local sign-in source, when the provider is OAuth (`claude-code`, `codex`, …). */
  oauthSource?: string;
  /** Wire the provider speaks (`anthropic`, `responses`, …). */
  type?: string;
  /** Endpoint, shown as the row subtitle. */
  baseUrl?: string;
  /** The alternate local sign-in, when this is a second account. */
  login?: {
    label?: string;
    home?: string;
    credentialsPath?: string;
    keychainService?: string;
    keychainAccount?: string;
  };
}

export interface ProviderIdentity {
  /** Logo + product-name key (`claude-subscription`, `opencode-go`). */
  brand: string;
  /** Product display name (`Claude`, `OpenCode Go`). */
  name: string;
  /** Which account of the brand this is (`work`), or undefined for the agent's own. */
  account?: string;
  /** Where the credential comes from, for a tooltip (`~/.claude-work`). */
  accountDetail?: string;
  /** True when a second-account sign-in was named. */
  isSecondAccount: boolean;
}

interface Brand {
  /** Logo and name lookup key. */
  id: string;
  /** Product display name. */
  name: string;
  /** Preset ids and bare brand tokens, longest first at match time. */
  aliases: string[];
  /** OAuth sources that read this brand's local sign-in. */
  sources?: string[];
}

/**
 * Known brands. `aliases` cover both the preset id and the plain token a user is likely to
 * type in a provider name; `sources` maps a local sign-in onto the brand it belongs to.
 */
const BRANDS: Brand[] = [
  {
    id: "claude-subscription",
    name: "Claude",
    aliases: ["claude-subscription", "claude"],
    sources: ["claude-code"],
  },
  {
    id: "chatgpt-subscription",
    name: "ChatGPT (Codex)",
    aliases: ["chatgpt-subscription", "codex", "chatgpt"],
    sources: ["codex"],
  },
  {
    id: "chatgpt-web",
    name: "ChatGPT Web",
    aliases: ["chatgpt-web"],
  },
  {
    id: "antigravity",
    name: "Antigravity",
    aliases: ["antigravity"],
    sources: ["antigravity"],
  },
  {
    id: "devin-subscription",
    name: "Devin",
    aliases: ["devin-subscription", "devin"],
    sources: ["devin"],
  },
  {
    id: "cursor-subscription",
    name: "Cursor",
    aliases: ["cursor-subscription", "cursor"],
    sources: ["cursor"],
  },
  {
    id: "workbuddy-ai",
    name: "WorkBuddy AI",
    aliases: ["workbuddy-ai-subscription", "workbuddy-ai", "workbuddy"],
    sources: ["workbuddy-ai"],
  },
  {
    id: "freebuff",
    name: "Freebuff",
    aliases: ["freebuff-subscription", "freebuff"],
    sources: ["freebuff"],
  },
  {
    id: "opencode-go",
    name: "OpenCode Go",
    aliases: ["opencode-go"],
  },
  {
    id: "opencode-zen",
    name: "OpenCode Zen",
    aliases: ["opencode-zen"],
  },
  // A bare `opencode` name maps to the shared mark; the account label keeps the variant.
  { id: "opencode", name: "OpenCode", aliases: ["opencode"] },
  {
    id: "commandcode",
    name: "Command Code",
    aliases: ["commandcode", "command-code"],
  },
  { id: "deepseek", name: "DeepSeek", aliases: ["deepseek"] },
  { id: "anthropic", name: "Anthropic", aliases: ["anthropic"] },
  { id: "openai", name: "OpenAI", aliases: ["openai"] },
  { id: "moonshotai", name: "Moonshot", aliases: ["moonshotai", "moonshot", "kimi"] },
  { id: "zai", name: "Z.ai", aliases: ["zai", "z-ai", "glm"] },
  { id: "minimax", name: "MiniMax", aliases: ["minimax"] },
  { id: "qwen", name: "Alibaba Qwen", aliases: ["qwen", "dashscope"] },
  { id: "xai", name: "xAI", aliases: ["xai", "grok"] },
  { id: "google", name: "Google Gemini", aliases: ["google", "gemini"] },
  { id: "openrouter", name: "OpenRouter", aliases: ["openrouter"] },
  { id: "orcarouter", name: "OrcaRouter", aliases: ["orcarouter"] },
  { id: "mistral", name: "Mistral", aliases: ["mistral"] },
  { id: "groq", name: "Groq", aliases: ["groq"] },
  { id: "ollama", name: "Ollama", aliases: ["ollama"] },
  { id: "lmstudio", name: "LM Studio", aliases: ["lmstudio", "lm-studio"] },
  { id: "jevonian", name: "Jevonian", aliases: ["jevonian-remote", "jevonian"] },
];

/** Wires with a single obvious brand, used only when the name and source say nothing. */
const TYPE_BRANDS: Record<string, string> = {
  devin: "devin-subscription",
  cursor: "cursor-subscription",
  "chatgpt-web": "chatgpt-web",
};

function tokenize(value: string): string[] {
  return value
    .toLowerCase()
    .split(/[\s._/-]+/)
    .filter(Boolean);
}

/** True when `prefix` tokens begin `name` tokens at a token boundary. */
function tokenPrefix(prefix: string[], name: string[]): boolean {
  if (prefix.length === 0 || prefix.length > name.length) return false;
  return prefix.every((token, index) => name[index] === token);
}

function matchBrandByName(name: string): { brand: Brand; consumed: number } | undefined {
  const tokens = tokenize(name);
  if (tokens.length === 0) return undefined;
  let best: { brand: Brand; consumed: number } | undefined;
  for (const brand of BRANDS) {
    for (const alias of brand.aliases) {
      const aliasTokens = tokenize(alias);
      if (!tokenPrefix(aliasTokens, tokens)) continue;
      if (!best || aliasTokens.length > best.consumed) {
        best = { brand, consumed: aliasTokens.length };
      }
    }
  }
  return best;
}

function brandBySource(source: string | undefined): Brand | undefined {
  if (!source) return undefined;
  return BRANDS.find((brand) => brand.sources?.includes(source));
}

function brandById(id: string): Brand | undefined {
  return BRANDS.find((brand) => brand.id === id);
}

/**
 * The last path segment of a sign-in location, without a credential-file extension. A leading
 * brand token is dropped, so `~/.codex-work` reads as `work` and not `codex-work`.
 */
function locationLabel(path: string, brand?: Brand): string | undefined {
  const trimmed = path.trim().replace(/[/\\]+$/, "");
  if (!trimmed) return undefined;
  const segment = trimmed.split(/[/\\]/).pop();
  if (!segment) return undefined;
  const withoutExtension = segment.replace(/\.(json|toml)$/i, "");
  // A dotfile directory (`~/.claude-work`) reads better without its leading dot.
  const cleaned = withoutExtension.replace(/^\.+/, "");
  if (!cleaned) return undefined;
  const tokens = tokenize(cleaned);
  const longestBrandPrefix = (brand?.aliases ?? [])
    .map(tokenize)
    .filter((aliasTokens) => tokenPrefix(aliasTokens, tokens))
    .sort((a, b) => b.length - a.length)[0];
  if (!longestBrandPrefix) return cleaned;
  return tokens.slice(longestBrandPrefix.length).join("-") || cleaned;
}

/** The account a login names: an explicit label, and the location it points at. */
function accountFromLogin(
  login: ProviderIdentityInput["login"],
  brand?: Brand,
): { label?: string; derived?: string; detail?: string } {
  if (!login) return {};
  const detail = login.home ?? login.credentialsPath ?? login.keychainService ?? undefined;
  const derived =
    (login.home ? locationLabel(login.home, brand) : undefined) ||
    (login.credentialsPath ? locationLabel(login.credentialsPath, brand) : undefined) ||
    login.keychainService?.trim() ||
    undefined;
  const label = login.label?.trim() || undefined;
  return { label, derived, detail };
}

/** Humanize an unknown provider name: `my-gateway` → `My Gateway`. */
function humanize(name: string): string {
  const base = name.endsWith("-subscription") ? name.slice(0, -"-subscription".length) : name;
  return base
    .split("-")
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

/**
 * Resolve a provider's brand and account. Accepts a provider record, or a bare name string
 * for call sites that only carry the routing key (logs, quota rows).
 */
export function resolveProviderIdentity(
  input: string | ProviderIdentityInput | undefined,
): ProviderIdentity {
  const value: ProviderIdentityInput = typeof input === "string" ? { name: input } : (input ?? {});
  const name = (value.name ?? "").trim();

  // 1. A named local sign-in decides the brand outright.
  let brand = brandBySource(value.oauthSource);

  // 2. Otherwise the name, longest alias first.
  if (!brand) {
    const match = matchBrandByName(name);
    if (match) brand = match.brand;
  }

  // 3. A well-known wire is better than nothing.
  if (!brand && value.type) {
    brand = brandById(TYPE_BRANDS[value.type]);
  }

  // What the name carries after its brand token (`codex-personal` → `personal`). Read even
  // when `oauthSource` named the brand, so a user-chosen account still wins over a directory.
  const nameMatch = matchBrandByName(name);
  const remainder =
    nameMatch && nameMatch.brand.id === brand?.id
      ? tokenize(name).slice(nameMatch.consumed).join("-")
      : "";
  const loginAccount = accountFromLogin(value.login, brand);

  if (!brand) {
    return {
      brand: name,
      name: name ? humanize(name) : "Unknown",
      account: loginAccount.label || loginAccount.derived,
      accountDetail: loginAccount.detail,
      isSecondAccount: Boolean(loginAccount.label || loginAccount.derived),
    };
  }

  // The account is what the user named, then what is left of the provider name after its brand
  // token, then the login location. So `codex-personal` reads `personal`, not `codex-work`.
  const account = loginAccount.label || remainder || loginAccount.derived || undefined;

  return {
    brand: brand.id,
    name: brand.name,
    account,
    accountDetail: loginAccount.detail,
    isSecondAccount: Boolean(account),
  };
}

/** The product name for a provider id (`claude-work` → `Claude`). */
export function providerDisplayName(name: string): string {
  return resolveProviderIdentity(name).name;
}

/** The logo / brand key for a provider id (`claude-work` → `claude-subscription`). */
export function providerBrandId(name: string): string {
  return resolveProviderIdentity(name).brand;
}

/** The account label for a provider id, or undefined for the agent's own sign-in. */
export function providerAccountLabel(name: string): string | undefined {
  return resolveProviderIdentity(name).account;
}
