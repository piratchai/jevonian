# Providers

Everything — providers, keys, routing, logs — is configured in the browser, or from the CLI with `jevonian add`.

## Provider fields

| Field         | Values                                                                             | Notes                                                                                                                                   |
| ------------- | ---------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `type`        | `openai`, `anthropic`, `responses`, `both`, `gemini`, `devin`, `cursor`            | the wire protocol; `both` serves OpenAI and Anthropic from one entry; `gemini` is Cloud Code Assist; `devin` / `cursor` are Connect-RPC |
| `auth`        | `api-key` (default), `oauth`                                                       | `oauth` adds bearer/beta headers and reads the credential from `oauthSource`                                                            |
| `oauthSource` | `claude-code`, `codex`, `antigravity`, `devin`, `cursor`, `workbuddy-ai`, `freebuff`, `static` | `static` uses the stored key as a bearer token                                                                                          |
| `billing`     | `api` (default), `subscription`                                                    | subscription spend is recorded as quota value, not real money                                                                           |
| `quota`       | `{ fiveHourUsd, weeklyUsd, monthlyUsd }`                                           | optional caps for ledger-based quota meters                                                                                             |

A top-level `modelAliases` map pins irregular cross-provider names to a canonical id (see [routing.md](routing.md#canonical-models)).

## Presets

Built-in presets cover DeepSeek, Anthropic (Claude), OpenAI, Moonshot (Kimi), Z.ai (GLM), MiniMax, Alibaba Qwen, xAI (Grok), Google Gemini, OpenRouter, OrcaRouter, OpenCode Go, OpenCode Zen, Command Code, Mistral, Groq, Ollama, LM Studio, Claude Pro/Max, ChatGPT (Codex), Antigravity, Devin, Cursor, WorkBuddy AI, Freebuff, and another Jevonian instance. Each preset carries its base URL, protocol type, key variable, and a hint for where to create a key.

`jevonian init` (or `jevonian add`) walks through everything:

1. Pick a provider from the built-in list, or add a custom endpoint.
2. Paste the API key. Keys are stored in `~/.config/jevonian/credentials.json` with `0600` permissions; set `--env NAME` to reference an environment variable instead.
3. Models are discovered live from the provider's `/models` endpoint (falling back to the models.dev catalog), and `plan`/`execute`/`utility` tiers are derived automatically from the price table.

Non-interactive, for scripts and agents:

```bash
jevonian add deepseek --key sk-...
jevonian add moonshotai --env MOONSHOT_API_KEY --models kimi-k3,kimi-k2.7-code
jevonian add my-gateway --base-url https://gateway.internal/v1 --type openai --key ...
jevonian add opencode-go --key sk-... --models opencode-go/kimi-k3,opencode-go/deepseek-v4.1-flash
jevonian add claude-subscription            # reads your Claude Code login
jevonian add chatgpt-subscription --models gpt-5.6-codex   # adds a responses provider
jevonian add devin-subscription             # reads your `devin auth login` session
jevonian add workbuddy-ai-subscription      # browser sign-in to WorkBuddy AI
jevonian add freebuff-subscription          # browser sign-in to Freebuff (free, ad-supported tier)
```

## Subscriptions

Besides classic API providers, Jevonian speaks to the subscriptions you already pay for. Providers carry two flags:

- `auth: "api-key" | "oauth"` — how the credential is obtained
- `billing: "api" | "subscription"` — pay-per-token or flat-rate quota

Two families are supported:

**API-key subscriptions** — endpoints that issue a key and speak OpenAI/Anthropic protocols. Presets: `opencode-go`, `commandcode`. Both serve two wires from one base URL, so they use `type: "both"`: one provider entry answers OpenAI (`/chat/completions`) clients and Anthropic (`/messages`) clients, and the endpoint is picked from the incoming request. Everything else works like a normal provider; `billing: "subscription"` marks ledger entries as quota value rather than real spend.

**OAuth subscriptions** — the credential lives with the agent you already signed into:

| Provider         | Preset                      | Credential source                                                                                  | Wire                                                                                 |
| ---------------- | --------------------------- | -------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| Claude Pro/Max   | `claude-subscription`       | `~/.claude/.credentials.json`, or the macOS keychain item `Claude Code-credentials`                | Anthropic Messages (Bearer + `oauth-2025-04-20`, Claude Code system prompt injected) |
| ChatGPT Plus/Pro | `chatgpt-subscription`      | `~/.codex/auth.json`                                                                               | OpenAI Responses (`store: false`, account + originator headers)                      |
| Antigravity      | `antigravity`               | macOS keychain item `gemini`/`antigravity` and the local project id                                | Gemini / Cloud Code Assist                                                           |
| Devin            | `devin-subscription`        | `~/.local/share/devin/credentials.toml` (`$XDG_DATA_HOME/devin`, `%APPDATA%\devin`)                | Devin Connect-RPC (`GetChatMessage`)                                                 |
| Cursor           | `cursor-subscription`       | macOS keychain item `cursor-access-token`/`cursor-user`, or `~/.cursor/auth.json`                  | Cursor Connect-RPC (`AgentService/Run`, `cursor-agent` wire)                         |
| Freebuff         | `freebuff-subscription`     | `~/.config/jevonian/freebuff.json` (browser sign-in), `FREEBUFF_AUTH_TOKEN`, or `JEVONIAN_FREEBUFF_AUTH` | OpenAI Chat Completions at `www.codebuff.com/api/v1` (forced stream, session + agent run per turn) |
| WorkBuddy AI     | `workbuddy-ai-subscription` | `~/.config/jevonian/workbuddy-ai.json` (browser sign-in), or plaintext `workbuddy-desktop-ai.info` | OpenAI Chat Completions at `www.workbuddy.ai/v2` (forced stream, WorkBuddy headers)  |

- Tokens are read on demand, cached in memory, and refreshed with the vendor's refresh-token endpoint when they are about to expire; rotated tokens are written back to the source file so Claude Code / Codex keep working. Set `oauthSource: "static"` to use a stored long-lived token instead.
- Devin's session token (written by `devin auth login`) does not expire and has no refresh flow. When Devin rejects it, run `devin auth login` again; Jevonian re-reads the file on the next request. `JEVONIAN_DEVIN_CREDENTIALS` points at a different credentials file. The default upstream is `https://server.codeium.com`; set the provider's `baseUrl` explicitly for a different endpoint.
- Cursor's token comes from the `cursor-agent` CLI's own sign-in. A token about to expire is renewed by running `cursor-agent status`; if it is still rejected, run `cursor-agent login` again. `JEVONIAN_CURSOR_AUTH` points at a specific `auth.json`. Cursor has no plain REST endpoint — the conversation is sent as a bidirectional Connect stream — so the docs' `/v1` surface is the only way to reach it.
- WorkBuddy AI uses Magpie's browser sign-in (`jevonian add workbuddy-ai-subscription` opens the WorkBuddy login and stores tokens under `~/.config/jevonian/workbuddy-ai.json`). A plaintext desktop session at `…/CodeBuddyExtension/Data/Public/auth/workbuddy-desktop-ai.info` is used when credential protection is off; encrypted desktop tokens are not readable. `JEVONIAN_WORKBUDDY_AI_AUTH` points at a session file. Upstream chats must stream; non-stream clients get the SSE folded into one completion. Adding the preset from the dashboard also opens browser sign-in when no session is stored yet (`POST /api/oauth/workbuddy-ai/signin` re-runs it).
- Freebuff is Codebuff's free, ad-supported coding agent. It has no public API key, so Jevonian does what the official CLI does: sign in with a device code (`jevonian add freebuff-subscription` opens the browser and stores the token owner-only), then for each turn hold a free session and an agent run and send the request inside the CLI's envelope (`codebuff_metadata` with `cost_mode: "free"`, the "Buffy" system opening, `stop`, `provider.data_collection: "deny"`). Sessions last about an hour and are shared by every turn on the account; the quota resets daily (Pacific midnight), and a 429 benches the provider until the time the server states. Upstream chats must stream; non-stream clients get the SSE folded into one completion. Models are listed from a built-in table (the server cannot list models without taking a session, which would supersede a live chat); use `bare` ids like `deepseek-v4-flash` or `freebuff/deepseek-v4-flash`. Concurrency is capped at 2 per provider. Unlike the paid subscriptions this is a reverse-engineered, fingerprint-gated free tier: it can break whenever Freebuff changes its checks, and using it outside the official CLI may violate its terms. Use it at your own risk.
- Model discovery works for subscriptions too: Claude reads Anthropic's `/v1/models` with the OAuth token, ChatGPT reads the model list Codex caches at `~/.codex/models_cache.json` (run `codex` once if it is missing), Antigravity calls `v1internal:fetchAvailableModels` with the local token, Devin calls `GetCliModelConfigs`, Cursor reads `cursor-agent models` (cached to `cursor-models.json`), and WorkBuddy AI reads `GET /v3/config` (CLI agent models). Devin lists only the models your plan unlocks — the Free plan only offers `swe-1-6-slow`. The protocol field follows the credential source — Claude Code pins `anthropic`, Codex pins `responses`, Antigravity pins `gemini`, Devin and Cursor pin `devin` and `cursor`, WorkBuddy AI pins `openai` — so one entry gets the right wire automatically.
- Background auto-sync (see [configuration.md](configuration.md#model-auto-sync)) appends newly listed ids while `serve` runs. Removals are sticky via `excludeModels`. Fixed routings are never rewritten; only empty auto-derived routings can pick an unpriced new id as a last-resort candidate.
- `/v1/responses` is proxied for clients that speak the Responses API (Codex CLI). A chat-completions request that routes to a Responses provider is translated on the fly (streaming chunks included), so any OpenAI-compatible agent can use the ChatGPT subscription. The reverse also works: a Responses client that routes to an Anthropic-only host (Claude Pro/Max) folds through Chat Completions → Anthropic Messages and back.
- Subscription access through third-party clients is outside the vendors' official clients. Expect the usual caveats: it can break when upstream headers change, and use is at your own risk.

A subscription provider in `~/.config/jevonian/config.json`:

```json
{
  "name": "claude-subscription",
  "type": "anthropic",
  "baseUrl": "https://api.anthropic.com/v1",
  "auth": "oauth",
  "oauthSource": "claude-code",
  "billing": "subscription",
  "models": ["claude-sonnet-4-6", "claude-opus-4-6"]
}
```

## Usage and limits

The Overview and Providers pages show, per provider, the rolling windows, remaining quota, reset times, and local spend. Sources, in order:

1. **Live** — vendor usage endpoints: OpenCode Go (`GET {baseUrl}/usage`), Claude (`GET https://api.anthropic.com/api/oauth/usage`), Codex (`GET https://chatgpt.com/backend-api/wham/usage`), Devin (`GetUserStatus` daily and weekly windows), WorkBuddy AI (`POST /billing/meter/get-user-resource-summary`). Fetches are cached (Claude for 5 minutes, everything else for 1 minute) and refreshed with `jevonian quota --refresh` or the dashboard button. Claude reports its shared 5h/7d pools plus any model-scoped weekly limits (for example a Fable-only window); scoped windows are shown for visibility but never drive the quota guard, since a spent scoped pool says nothing about the rest of the account.
2. **Response headers** — `anthropic-ratelimit-unified-*` and `x-codex-*` headers captured passively from every proxied response, persisted at `~/.local/share/jevonian/quota.json`.
3. **Ledger** — dollar windows computed from the local ledger when a provider declares caps (`quota.fiveHourUsd` / `weeklyUsd` / `monthlyUsd`). Useful for Command Code and any subscription without a usage API.

## Another Jevonian as a provider

An instance can route through another instance, which is useful when the credentials live on one machine (a desktop with your Claude/Codex logins) and you want to spend from another (a laptop, a build box).

On the machine that holds the credentials, enable **LAN access** — Overview page, or:

```bash
jevonian serve --lan                 # bind every interface on listen.port + 2
jevonian serve --lan --lan-host 192.168.1.20 --lan-port 9500
jevonian serve --no-lan              # turn it back off
```

The flags persist to `config.json`, so the setting survives the macOS LaunchAgent restart. On startup the console prints the base URLs a peer should use:

```
lan: listening on 0.0.0.0:8789 (only /v1, key required)
lan: provider base URL http://192.168.1.20:8789/v1
```

On the other machine, add a provider pointing at that base URL with a Jevonian API key created on the first machine's **Keys** page (the `Jevonian (another machine)` preset, or `jevonian add` with `--base-url`). Requests then route: the second instance picks a model, and the first instance routes it again to its real provider.

The LAN listener serves **only `/v1`** — the same surface the public tunnel forwards to. The dashboard and `/api` are never bound there, so a neighbour on the network cannot edit providers or read keys. Every request needs a real Jevonian key, and the loopback-only desktop sentinel (`jevonian-local`) is rejected on the LAN address. Enabling LAN access requires at least one key to exist.

## Pricing

Prices come from [models.dev](https://models.dev) (`https://models.dev/api.json`), cached at `~/.local/share/jevonian/pricing.json`. Refresh with `jevonian pricing --refresh`; `serve`, `report`, and `doctor` load the snapshot automatically.

- Lookups prefer the provider-qualified rate (`deepseek/deepseek-v4-pro`) and fall back to the bare model id. Reseller-prefixed ids live under their own key (`reseller/vendor/model`), so they never shadow the owner's rate.
- `fallbackPrices` in `internal/cli/pricing.go` is an offline fallback for ids models.dev does not carry; it also holds the DeepSeek peak/off-peak rules, which models.dev does not express.
- models.dev rates win when both sources know a model. Note that models.dev's own `deepseek` entry differs from DeepSeek's pricing page, so estimates follow models.dev.
- Context-tiered rates (`cost.tiers`) are not modeled yet; the base rate is used.
