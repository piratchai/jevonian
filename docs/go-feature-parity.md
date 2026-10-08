# Go rewrite — feature-parity checklist

Definition of **100% user-facing parity** with the TypeScript product (v0.5.4 surface, plus capabilities documented in README / CHANGELOG 0.4.x–0.5.4 and still present in `src/`).

**Status:** all 276 items verified against the Go binary. The TypeScript runtime is deleted; the verification method and per-section verdicts are in [go-parity-report.md](go-parity-report.md).

**Out of scope (explicit drop):**

- [x] ~~Vercel AI Gateway brain (`channel: "vercel"`, `@ai-sdk/gateway` / `experimental_evaluate`)~~ — **OUT OF SCOPE**; omitted from Go. Do not block 100% parity on this path.

Supported brain channels (TypeSafe / SystemOne HTTP, OpenRouter decisions, OpenCode Zen SystemOne, Cloudflare Workers AI including Clef / Clef-flash, and custom SystemOne URL) and heuristic fallback **must** remain.

**Format:** `- [ ] **Name** — \`ts hint\` — Acceptance: …`

---

## 1. CLI

### 1.1 Lifecycle / serve

- [x] **Default `jevonian` / `serve`** — `src/cli.ts` — Acceptance: bare invoke starts (or ensures) the proxy; prints listen URL, providers, routing mode, pricing source.
- [x] **`jevonian start`** — `src/cli.ts` — Acceptance: alias for serve / LaunchAgent start after `stop`; same outcome as bare `jevonian` on macOS.
- [x] **`jevonian stop`** — `src/cli.ts`, `src/service.ts` — Acceptance: stops background service; prints how to start again.
- [x] **`jevonian stop --uninstall`** — `src/cli.ts`, `src/service.ts` — Acceptance: stops and removes LaunchAgent plist.
- [x] **`jevonian restart`** — `src/cli.ts`, `src/service.ts` — Acceptance: kickstarts LaunchAgent (or starts if stopped); picks up newly installed binary.
- [x] **`jevonian status`** — `src/cli.ts`, `src/service.ts` — Acceptance: shows LaunchAgent state, pid, LAN state, recent serve log tail.
- [x] **`--foreground` / `--fg`** — `src/cli.ts` — Acceptance: attached process on macOS instead of LaunchAgent install-and-exit.
- [x] **`--no-open` / `JEVONIAN_NO_OPEN`** — `src/cli.ts`, `src/browser.ts` — Acceptance: dashboard browser not launched.
- [x] **Serve refuses occupied `listen.port`** — `src/cli.ts`, `src/ports.ts` — Acceptance: foreground serve exits with clear message when port already bound (skipped under launchd).
- [x] **System proxy install at CLI entry** — `src/cli.ts`, `src/proxy.ts` — Acceptance: machine proxy settings applied before outbound fetches unless `JEVONIAN_SYSTEM_PROXY=off`.
- [x] **Unhandled transient proxy errors logged compactly** — `src/cli.ts`, `src/proxy.ts` — Acceptance: HTTP/1.1 parse / terminated fetch does not crash process with TypeError dump.
- [x] **Usage dump on unknown command** — `src/cli.ts` — Acceptance: lists supported commands and key serve flags; exit 1.

### 1.2 Setup and providers

- [x] **`jevonian init`** — `src/cli.ts` — Acceptance: interactive first-provider wizard; non-interactive writes example config.
- [x] **`jevonian add <preset|custom>`** — `src/cli.ts`, `src/providers.ts` — Acceptance: add/update provider with interactive picker or flags; live model discovery; auto-fills empty routings / baseline.
- [x] **`add` flags: key / env / models / base-url / type / auth / oauth-source / billing / name** — `src/cli.ts` — Acceptance: documented flags produce correct `config.json` + credentials.
- [x] **`add` multi-account login flags** — `src/cli.ts`, `src/oauth.ts` — Acceptance: `--login-home`, `--login-file`, `--login-keychain`, `--login-label` create second-account providers; `jevonian providers` shows `account=`.
- [x] **OAuth wire pairing on add** — `src/cli.ts`, `src/oauth.ts` — Acceptance: Devin/Cursor require matching `--type` / `--oauth-source`; mismatch refused with clear error.
- [x] **`jevonian providers` (alias list)** — `src/cli.ts` — Acceptance: lists name, key source, model count, account label when set.
- [x] **`jevonian remove <provider> [--keep-key]`** — `src/cli.ts`, `src/credentials.ts` — Acceptance: removes provider; drops stored key unless `--keep-key`.

### 1.3 Observability CLI

- [x] **`jevonian report`** — `src/cli.ts`, `src/ledger.ts`, `src/pricing.ts` — Acceptance: spend, cache hit rate, brain-decided turns, savings vs baseline, token-saver `savedTokens` total.
- [x] **`jevonian doctor [--network]`** — `src/cli.ts` — Acceptance: config / providers / tiers / ledger / catalog / pricing health; optional live network probes.
- [x] **`jevonian models [--refresh]`** — `src/cli.ts`, `src/catalog.ts` — Acceptance: discovered models per provider; `--refresh` hits live `/models` (or OAuth discovery).
- [x] **`jevonian models --sync`** — `src/cli.ts`, `src/model-sync.ts` — Acceptance: appends newly discovered ids into `config.json` without removing/reordering.
- [x] **`jevonian pricing [--refresh]`** — `src/cli.ts`, `src/modelsdev.ts` — Acceptance: shows price table source/size; `--refresh` pulls models.dev.
- [x] **`jevonian refresh`** — `src/cli.ts`, `src/catalog-sync.ts` — Acceptance: refreshes catalog + pricing + leaderboard snapshots.
- [x] **`jevonian quota [--refresh]`** — `src/cli.ts`, `src/quota.ts` — Acceptance: per-provider windows, reset times, 30-day spend; `--refresh` blocks on live probes.

### 1.4 Update and launch

- [x] **`jevonian update [--check]`** — `src/cli.ts`, `src/updates.ts` — Acceptance: registry check with tarball HEAD probe; install via detected package manager; verify on-disk version.
- [x] **Update restarts macOS service** — `src/cli.ts`, `src/updates.ts`, `src/service.ts` — Acceptance: after install, LaunchAgent reloads onto new build when loaded.
- [x] **Cached update notice on short commands** — `src/cli.ts`, `src/updates.ts` — Acceptance: non-serve commands print prior cached “update available” notice.
- [x] **`jevonian launch claude [--model M] [--] args…`** — `src/cli.ts`, `src/claude-code.ts` — Acceptance: one-shot Claude Code with env remap; no disk rewrite; model override works.

### 1.5 Serve flags (persist / one-shot)

- [x] **`--tunnel` / `--no-tunnel`** — `src/cli.ts`, `src/tunnel.ts` — Acceptance: force tunnel on/off for one run; implies foreground on macOS.
- [x] **`--lan` / `--no-lan` / `--lan-host` / `--lan-port`** — `src/cli.ts`, `src/lan.ts`, `src/config.ts` — Acceptance: persists `lan` into config; survives LaunchAgent restart; prints provider base URL(s).

---

## 2. HTTP surface

### 2.1 Public /v1 (loopback, tunnel, LAN)

- [x] **`GET /healthz` (main)** — `src/server.ts` — Acceptance: `{ ok, sessions, routing }`.
- [x] **`GET /healthz` (public surface)** — `src/server.ts` — Acceptance: `{ ok, public: true }` on tunnel/LAN listener.
- [x] **`GET /v1/models`** — `src/server.ts`, `src/models.ts` — Acceptance: lists virtual models + provider/canonical ids; respects client filters.
- [x] **`POST /v1/chat/completions`** — `src/server.ts`, `src/upstream.ts`, `src/wire.ts` — Acceptance: OpenAI Chat Completions proxy with streaming and non-streaming.
- [x] **`POST /v1/messages`** — `src/server.ts`, `src/anthropic.ts` — Acceptance: Anthropic Messages proxy with streaming and non-streaming.
- [x] **`POST /v1/messages/count_tokens`** — `src/server.ts` — Acceptance: count_tokens handled (main app only).
- [x] **`POST /v1/responses`** — `src/server.ts`, `src/responses.ts` — Acceptance: OpenAI Responses proxy with streaming and non-streaming.
- [x] **WebSocket upgrade → 426** — `src/server.ts` — Acceptance: Codex WS upgrade gets 426 so client falls back to HTTP Responses.
- [x] **Account / session probe proxy** — `src/server.ts`, `src/relay.ts` — Acceptance: ChatGPT account probes reach real backend; desktop clients not stuck on sign-in.
- [x] **First-run open auth** — `src/server.ts`, `src/auth.ts`, `src/keys.ts` — Acceptance: while no Jevonian keys exist, `/v1` accepts traffic tagged `unauthenticated`.
- [x] **Bearer / `x-api-key` Jevonian key** — `src/server.ts`, `src/auth.ts`, `src/keys.ts` — Acceptance: valid `sk-jev-…` required once any key exists.
- [x] **Loopback desktop sentinel (`jevonian-local` / local token)** — `src/server.ts`, `src/local-client.ts` — Acceptance: accepted only from loopback without Jevonian key; never on tunnel/LAN.
- [x] **Credit limit 429** — `src/server.ts`, `src/keys.ts` — Acceptance: API spend ≥ `limitUsd` → `type: credit_limit_exceeded`; subscription spend excluded.
- [x] **Public listener: only `/v1` + `/healthz`** — `src/server.ts`, `src/tunnel.ts` — Acceptance: dashboard/`/api` unreachable on public/LAN ports.
- [x] **LAN rejects desktop sentinel** — `src/lan.ts`, `src/server.ts` — Acceptance: LAN peer must present real Jevonian key.
- [x] **Static dashboard from main listener** — `src/server.ts` — Acceptance: SPA assets served; `/api` and `/v1` not shadowed by SPA catch-all.
- [x] **Legacy `GET /stats` summary** — `src/server.ts` — Acceptance: request count, sessions, cost, cache/prompt tokens (compat endpoint).

### 2.2 Response decision headers

- [x] **`x-jevonian-model`** — `src/upstream.ts` — Acceptance: actual served model id.
- [x] **`x-jevonian-provider`** — `src/upstream.ts` — Acceptance: provider name that served.
- [x] **`x-jevonian-phase`** — `src/upstream.ts` — Acceptance: route / phase used.
- [x] **`x-jevonian-reason`** — `src/upstream.ts` — Acceptance: why chosen, including skips/clamps/failover.
- [x] **`x-jevonian-effort` / `x-jevonian-effort-note`** — `src/upstream.ts` — Acceptance: thinking level actually sent + clamp note when applicable.
- [x] **`x-jevonian-skipped`** — `src/upstream.ts` — Acceptance: withheld models with reasons.
- [x] **`x-jevonian-session`** — `src/upstream.ts` — Acceptance: session affinity id.
- [x] **`x-jevonian-request-id`** — `src/upstream.ts` — Acceptance: joins ledger row, body capture, and `/logs/:id`.
- [x] **`x-jevonian-retries`** — `src/upstream.ts` — Acceptance: present only when same-host retries > 0.
- [x] **`x-jevonian-brain` / `x-jevonian-brain-channel`** — `src/upstream.ts`, `src/brain.ts` — Acceptance: `jev` / `jev-low-confidence` + channel id.
- [x] **`x-jevonian-canonical`** — `src/upstream.ts` — Acceptance: when canonical expansion applied.
- [x] **`x-jevonian-cache-state` / `x-jevonian-cache-keep`** — `src/upstream.ts`, `src/routing.ts` — Acceptance: cache estimate + affinity stay/move reason.
- [x] **`x-jevonian-soft-error`** — `src/upstream.ts`, `src/soft-error.ts` — Acceptance: set when soft completion substituted for hard stream failure.
- [x] **Request header `x-jevonian-phase`** — `src/routing.ts` — Acceptance: forces route without brain.
- [x] **Request header `x-jevonian-effort`** — `src/routing.ts` — Acceptance: thinking floor; shallower models skipped.
- [x] **Request header `x-jevonian-affinity`** — `src/routing.ts` — Acceptance: modes `auto` / `session` / `turn` / `off`.
- [x] **Request header `x-jevonian-session`** — `src/upstream.ts`, `src/routing.ts` — Acceptance: client can pin session id for affinity TTL.

---

## 3. Routing

### 3.1 Precedence and virtual models

- [x] **`jevonian/auto` brain pick** — `src/routing.ts`, `src/brain.ts` — Acceptance: one brain call chooses route (+ effort when enabled) from candidates.
- [x] **`jevonian/<route-id>` explicit route** — `src/routing.ts`, `src/config.ts` — Acceptance: builtins + custom routing ids; brain not consulted.
- [x] **Virtual ids beat catalog name collisions** — `src/routing.ts`, CHANGELOG 0.4.1 — Acceptance: `jevonian/auto` never pins a provider model literally named `auto`; `provider/auto` still pins.
- [x] **Bare `auto`/`plan`/… when no collision** — `src/routing.ts` — Acceptance: bare names virtual unless a provider declares that exact id.
- [x] **Pinned real model id** — `src/routing.ts` — Acceptance: never brain-routed; still subject to provider selection / quota / failover among providers serving it.
- [x] **`routing.mode: "off"` pass-through** — `src/routing.ts` — Acceptance: virtual models rejected; real models forward.
- [x] **No brain configured → auto errors** — `src/routing.ts`, `src/brain.ts` — Acceptance: `jevonian/auto` fails clearly; does not silently guess.
- [x] **All brains unreachable → heuristic fallback** — `src/routing.ts`, `src/brain.ts`, CHANGELOG 0.1.7 — Acceptance: after retries/breaker, `classifyPhase` path with `brain-fallback:…` reason (not hard 502 that freezes agents).
- [x] **Builtin routings plan/execute/utility/chat** — `src/config.ts` — Acceptance: always present with default copy; editable model chains.
- [x] **Custom routing ids** — `src/config.ts` — Acceptance: slug `^[a-z][a-z0-9-]{0,63}$`, not `auto`.
- [x] **Per-routing model fallback chain** — `src/routing.ts` — Acceptance: first healthy model wins; later wait.
- [x] **Per-routing per-model provider allow-list** — `src/config.ts`, `src/routing.ts` — Acceptance: empty list withholds; missing key = all providers; `providerOrder` legacy loads.
- [x] **Per-routing pinned `effort`** — `src/config.ts`, `src/routing.ts` — Acceptance: applied on brain pick and explicit `jevonian/<id>`.
- [x] **Session route affinity + TTL** — `src/routing.ts`, `src/session.ts` — Acceptance: route sticky until brain moves it or `sessionTtlMinutes` expires.
- [x] **`tiers` mirrored from `routings`** — `src/config.ts` — Acceptance: older configs/API still see four builtins synced.

### 3.2 Candidate narrowing (pre-brain)

- [x] **Quota health filter** — `src/routing.ts`, `src/quota.ts` — Acceptance: exhausted removed before brain; unknown never blocks; low still offered.
- [x] **Reset-aware ordering** — `src/routing.ts`, `src/quota.ts`, CHANGELOG 0.4.0 — Acceptance: `quotaGuard.resetAware` spends soonest-renewing first; same-hour ties preserve cache warmth; off = config order.
- [x] **Context-window filter + headroom** — `src/routing.ts`, `src/compaction.ts` — Acceptance: too-small windows in `skipped`; unknown window never withheld.
- [x] **Effort floor filter** — `src/routing.ts`, `src/capabilities.ts` — Acceptance: models that cannot meet floor skipped with `effort-skip`.
- [x] **Effort clamp to model capabilities** — `src/routing.ts`, `src/anthropic-thinking.ts` — Acceptance: clamp recorded in `effortNote`; adaptive vs legacy thinking shapes.
- [x] **`rejectsDisabled` for Sonnet ≥5.5 / Opus** — `src/anthropic-thinking.ts`, CHANGELOG 0.4.0 — Acceptance: explicit `disabled` not sent to models that reject it.
- [x] **Official host preference over reseller** — `src/routing.ts`, CHANGELOG 0.1.5 — Acceptance: when same model on official + reseller, official wins.
- [x] **Canonical model identity** — `src/model-id.ts`, `src/identity.ts`, `src/identityIndex.ts` — Acceptance: cross-provider spelling normalization; catalog display merge for stats.
- [x] **`modelAliases` config** — `src/config.ts`, `src/routing.ts` — Acceptance: irregular ids map to provider/model spellings.
- [x] **`routing.capacities` overrides** — `src/config.ts` — Acceptance: contextWindow / maxOutput / efforts override catalog unknowns only.
- [x] **Cache affinity (measured)** — `src/routing.ts`, CHANGELOG 0.4.0 — Acceptance: stay on warm provider within turn / across turns when cache read ≥1024 and fresh; modes via header; reason in ledger + `x-jevonian-cache-keep`.
- [x] **Cache evidence in brain state** — `src/routing.ts`, `src/brain.ts` — Acceptance: hit ratio estimates, switch penalty USD in state; `prefixMatch` may stay `unknown`.
- [x] **Provider concurrency cap + circuit breaker** — `src/provider-guard.ts`, CHANGELOG 0.5.4 — Acceptance: saturated/failing hosts skipped; env-tunable.
- [x] **Transport failover walks candidate plan** — `src/upstream.ts`, `src/retry.ts`, CHANGELOG 0.5.4 — Acceptance: ECONNRESET/timeout cools host and moves to next plan entry; no mid-turn re-decide.
- [x] **Same-host retry budget** — `src/retry.ts` — Acceptance: `JEVONIAN_SAME_HOST_RETRIES` caps quick repeats; rest for cross-provider failover.
- [x] **Quota / refusal failover** — `src/upstream.ts`, `src/quota.ts` — Acceptance: 429/402/refusal walks next candidate; content_policy retry drops client system once on Devin.
- [x] **Model-scoped Devin free-rate cooldown** — `src/quota.ts`, CHANGELOG 0.3.4 — Acceptance: free-model limit cools model not whole provider.
- [x] **Jevonian-key + configured model routes via Jevonian** — `src/server.ts`, CHANGELOG 0.3.2 — Acceptance: not mis-forwarded to api.openai.com with `sk-jev-…`.

### 3.3 Compaction / overflow

- [x] **Proactive long-context compaction** — `src/compaction.ts`, `src/upstream.ts` — Acceptance: when no candidate fits, compact then re-route; tool schemas/instructions counted; safety margin for bridged wires.
- [x] **Brain-guided tool-result deletion compaction** — `src/compaction.ts` — Acceptance: Jev decides keep call/result; user/assistant prose verbatim; insufficient shrink fails with `context_length_exceeded` rather than lying.
- [x] **Hard context rejection retry-after-compact** — `src/upstream.ts`, CHANGELOG 0.3.0 — Acceptance: one retry after compact on provider context rejection.
- [x] **Codex remote compaction v2** — `src/responses.ts`, `src/upstream.ts` — Acceptance: `compaction_trigger` pinned to ChatGPT Responses provider; soft-error if unavailable; never failover onto Chat Completions bridge.

### 3.4 Heuristic phase classifier

- [x] **`classifyPhase` signals** — `src/routing.ts` — Acceptance: plan/execute/utility/chat from message/tool shapes for brain-fallback and diagnostics.
- [x] **Brain state context envelopes** — `src/brain.ts` — Acceptance: `<user_info>` / system-reminder style wrappers not mistaken for user intent; HTML tags exempt.

---

## 4. Providers

### 4.1 Presets (API key / keyless)

- [x] **DeepSeek** — `src/providers.ts` — Acceptance: preset add + OpenAI/Anthropic both wire.
- [x] **Anthropic (Claude) API** — `src/providers.ts` — Acceptance: Messages wire + key.
- [x] **OpenAI API** — `src/providers.ts` — Acceptance: Chat Completions wire + key.
- [x] **Moonshot (Kimi)** — `src/providers.ts` — Acceptance: preset works.
- [x] **Z.ai (GLM)** — `src/providers.ts` — Acceptance: preset works.
- [x] **MiniMax** — `src/providers.ts` — Acceptance: preset works.
- [x] **Alibaba Qwen** — `src/providers.ts` — Acceptance: preset works.
- [x] **xAI (Grok)** — `src/providers.ts` — Acceptance: preset works.
- [x] **Google Gemini (OpenAI-compat)** — `src/providers.ts` — Acceptance: preset works.
- [x] **OpenRouter** — `src/providers.ts` — Acceptance: both wires + attribution headers.
- [x] **OrcaRouter** — `src/providers.ts` — Acceptance: preset works.
- [x] **OpenCode Go** — `src/providers.ts`, `src/quota.ts` — Acceptance: subscription billing + quota windows + `syncModels`.
- [x] **OpenCode Zen** — `src/providers.ts` — Acceptance: full Zen endpoint preset + sync.
- [x] **Command Code** — `src/providers.ts`, `src/quota.ts` — Acceptance: subscription + live quota (`JEVONIAN_COMMANDCODE_BASE_URL`).
- [x] **Mistral** — `src/providers.ts`, CHANGELOG 0.5.1 — Acceptance: API key + `syncModels` live discovery.
- [x] **Groq** — `src/providers.ts` — Acceptance: API key + sync.
- [x] **Ollama (keyless local)** — `src/providers.ts` — Acceptance: `noKey`, default `127.0.0.1:11434`, discovery without Bearer.
- [x] **LM Studio (keyless local)** — `src/providers.ts` — Acceptance: `noKey`, default `127.0.0.1:1234`.
- [x] **Custom / arbitrary base URL** — `src/cli.ts`, `src/admin.ts` — Acceptance: `--base-url` + `--type` without preset id.
- [x] **Jevonian remote (another machine)** — `src/providers.ts`, `src/lan.ts` — Acceptance: preset points at LAN default port (`listen+2`), uses Jevonian key env.

### 4.2 Subscription / special wires

- [x] **Claude Pro/Max subscription** — `src/providers.ts`, `src/oauth.ts`, `src/anthropic.ts` — Acceptance: credentials file or macOS keychain; Anthropic Messages Bearer; live 5h/7d (+ scoped weekly visibility).
- [x] **ChatGPT / Codex subscription** — `src/providers.ts`, `src/oauth.ts`, `src/responses.ts` — Acceptance: `~/.codex/auth.json`; Responses wire; usage endpoint; token refresh write-back.
- [x] **Antigravity (Gemini / Cloud Code Assist)** — `src/providers.ts`, `src/oauth.ts`, `src/gemini.ts` — Acceptance: local IDE token + project id; Gemini wire.
- [x] **Devin subscription** — `src/providers.ts`, `src/devin.ts`, `src/devin-catalog.ts` — Acceptance: Connect-RPC; `GetCliModelConfigs` / `GetUserStatus`; strip tool schema `description` + inject to system; content_policy handling.
- [x] **Cursor subscription** — `src/providers.ts`, `src/cursor.ts`, `src/cursor-catalog.ts` — Acceptance: Connect `AgentService/Run` blob wire; keychain/`auth.json`; `cursor-agent models` / status renew; per-login renew lock.
- [x] **WorkBuddy AI subscription** — `src/providers.ts`, `src/workbuddy.ts`, CHANGELOG 0.5.0 — Acceptance: browser sign-in, session file, forced streaming + SSE fold, headers, system inject, `/v3/config` discovery, billing meter credits; encrypted `$wbEncrypted` not supported (must sign in via Jevonian).

### 4.3 Provider mechanics

- [x] **Provider types vocabulary** — `src/config.ts`, `src/admin-types.ts` — Acceptance: `openai|anthropic|responses|both|gemini|devin|cursor` (+ effective type from URL).
- [x] **Auth modes `api-key` / `oauth` / static** — `src/config.ts`, `src/oauth.ts` — Acceptance: credentials resolution order documented in `apiKeySource`.
- [x] **Billing `api` vs `subscription`** — `src/config.ts`, `src/pricing.ts`, `src/keys.ts` — Acceptance: credit limits only count API billing.
- [x] **`injectStreamUsage`** — `src/config.ts`, `src/upstream.ts` — Acceptance: providers that need stream usage injection get it.
- [x] **Custom provider headers** — `src/config.ts` — Acceptance: `headers` map forwarded upstream.
- [x] **Per-model wire pins in `models[]`** — `src/config.ts` — Acceptance: `{ id, wire? }` entries serialize/load.
- [x] **`syncModels` / `excludeModels` / `noKey`** — `src/config.ts`, `src/model-sync.ts` — Acceptance: background append-only discovery; removals stick; keyless locals work.
- [x] **Background model sync scheduler** — `src/model-sync.ts`, `src/cli.ts` — Acceptance: interval ≥15m; state in `model-sync.json`; empty plan may take unpriced flagship only.
- [x] **Live discover on empty model list** — `src/admin.ts`, `src/cli.ts`, CHANGELOG 0.5.1 — Acceptance: OAuth/keyless/keyed discovery; no stale hardcoded Magpie lists.
- [x] **models.dev catalog + pricing snapshots** — `src/modelsdev.ts`, `src/catalog-sync.ts` — Acceptance: 12h TTL background refresh; manual refresh; bundled fallback.
- [x] **Benchmark / leaderboard soft evidence** — `src/leaderboard.ts`, `src/brain.ts` — Acceptance: board scores only (compact); missing never biases.
- [x] **Provider quota specs on provider** — `src/config.ts`, `src/quota.ts` — Acceptance: optional `quota` override fields parse/apply.
- [x] **Open $0 balance = exhausted** — `src/quota.ts`, CHANGELOG 0.1.7 — Acceptance: OpenRouter/DeepSeek live $0 persisted as spent; brain 402 marks provider spent for turn.

---

## 5. Auth / OAuth

- [x] **Credentials file `credentials.json` 0600** — `src/credentials.ts`, `src/paths.ts` — Acceptance: provider + `brain:<channel>` keys; env override `JEVONIAN_CREDENTIALS`.
- [x] **OAuth sources registry** — `src/oauth.ts` — Acceptance: `claude-code|codex|antigravity|devin|cursor|workbuddy-ai|static`.
- [x] **On-demand token read + near-expiry refresh** — `src/oauth.ts` — Acceptance: refreshed tokens written back so Claude Code / Codex keep working.
- [x] **Per-account token cache keying** — `src/oauth.ts`, CHANGELOG 0.4.0 — Acceptance: two providers of same source never share cached token / Cursor renew promise.
- [x] **Claude credentials path / keychain** — `src/oauth.ts` — Acceptance: `JEVONIAN_CLAUDE_CREDENTIALS` disables keychain fallback when set.
- [x] **Codex auth path** — `src/oauth.ts` — Acceptance: `JEVONIAN_CODEX_AUTH` / `JEVONIAN_CODEX_USAGE_URL`.
- [x] **Antigravity token + project** — `src/oauth.ts` — Acceptance: `JEVONIAN_ANTIGRAVITY_TOKEN` / `JEVONIAN_ANTIGRAVITY_PROJECT`.
- [x] **Devin credentials path + client version** — `src/oauth.ts`, `src/devin.ts` — Acceptance: `JEVONIAN_DEVIN_CREDENTIALS` / `JEVONIAN_DEVIN_CLIENT_VERSION`.
- [x] **Cursor auth path** — `src/cursor.ts` — Acceptance: `JEVONIAN_CURSOR_AUTH`; keychain `cursor-access-token` or `~/.cursor/auth.json`.
- [x] **WorkBuddy session path** — `src/workbuddy.ts` — Acceptance: `JEVONIAN_WORKBUDDY_AI_AUTH`; browser plugin auth flow; plaintext desktop fallback.
- [x] **Admin WorkBuddy sign-in endpoint** — `src/admin.ts` — Acceptance: `POST /api/oauth/workbuddy-ai/signin` without requiring full provider save.
- [x] **Jevonian API keys hashed store** — `src/keys.ts` — Acceptance: `sk-jev-…` shown once; stored hashed in `keys.json`.
- [x] **Key CRUD + rename + credit limit** — `src/keys.ts`, `src/admin.ts`, `docs/keys.md` — Acceptance: create/list/update/delete; optional `limitUsd`; usage attribution by `keyId`.
- [x] **Key spend incremental index** — `src/ledger-index.ts`, CHANGELOG 0.5.4 — Acceptance: routing/key spend does not full-scan huge ledger each turn.

---

## 6. Quota / Ledger

### 6.1 Quota

- [x] **Live provider quota probes** — `src/quota.ts` — Acceptance: Claude / Codex / OpenCode / Command Code / Devin / WorkBuddy / header capture / etc.
- [x] **Quota snapshot cache + async refresh for dashboard** — `src/quota.ts`, `src/admin.ts`, CHANGELOG 0.3.4 — Acceptance: `GET /api/quota` serves last snapshot fast; background refresh; probe timeouts; Refresh button blocks.
- [x] **Boot-time spent-provider log** — `src/cli.ts` — Acceptance: serve logs providers already at 100% so routing skips early.
- [x] **Rejection snapshot clear on healthy probe** — `src/quota.ts`, CHANGELOG 0.1.2 — Acceptance: prior 429/402 rejection not sticky after reset.
- [x] **Claude rate_limit_error header failover** — `src/quota.ts`, CHANGELOG 0.1.8 — Acceptance: unified rate-limit headers trigger same-request failover; live cache cleared.
- [x] **Devin prose reset parsing** — `src/quota.ts`, CHANGELOG 0.3.4 — Acceptance: “reset in 2 hours 37 minutes” sets cooldown correctly.
- [x] **Claude utilization as percentage points** — `src/quota.ts`, CHANGELOG 0.1.9 — Acceptance: reported 1% ≠ 100% used.
- [x] **`quotaGuard.enabled` / `lowPercent` / `resetAware`** — `src/config.ts`, `src/routing.ts` — Acceptance: config honored end-to-end.

### 6.2 Ledger

- [x] **Append-only `ledger.jsonl`** — `src/ledger.ts`, `src/paths.ts` — Acceptance: request + brain rows; env `JEVONIAN_LEDGER` / `JEVONIAN_DATA_DIR`.
- [x] **Warm append cache (no full re-parse)** — `src/ledger.ts`, CHANGELOG 0.5.2 / 0.5.4 — Acceptance: concurrent sessions do not re-parse 100MB+ file each read.
- [x] **Request row fields** — `src/ledger.ts` — Acceptance: tokens, cache, cost, phase, reason, effort, skipped, cacheKeep, savedTokens, keyId/keyName, billing, canonical, brain/confidence.
- [x] **Attempt history `tries` / `failovers` / `ttftMs`** — `src/ledger.ts`, `src/trace.ts`, CHANGELOG 0.5.2 — Acceptance: multi-attempt turns carry waterfall data; clean first-try omits fields.
- [x] **Brain rows `kind: "brain"`** — `src/ledger.ts`, `src/brain.ts` — Acceptance: cost in spend; hidden from Logs list; visible on log detail.
- [x] **Client cancel → 499** — `src/upstream.ts`, CHANGELOG 0.1.8 / 0.3.3 — Acceptance: disconnect before delivered content is 499; after delivered stream abandon stays 200.
- [x] **Body capture store** — `src/bodies.ts` — Acceptance: `bodies/` 0600, newest 1000; `JEVONIAN_CAPTURE_BODIES=0` disables; async write on hot path.
- [x] **Pricing estimates + baseline savings** — `src/pricing.ts`, `src/cli.ts` — Acceptance: cost from price table; savings vs `baselineModel`; subscription equivalent separate from API spend.

---

## 7. Streaming / Wires

### 7.1 Wire bridging and normalization

- [x] **Chat Completions ↔ Anthropic bridge** — `src/chat-anthropic-stream.ts`, `src/wire.ts` — Acceptance: either client can hit Anthropic-only hosts (stream + non-stream).
- [x] **Responses ↔ Anthropic bridge** — `src/responses.ts`, CHANGELOG 0.1.5 — Acceptance: Codex/ChatGPT Desktop can use Claude subscription.
- [x] **Gemini bridge** — `src/gemini.ts` — Acceptance: Antigravity / Gemini upstream served from OpenAI-shaped clients as configured.
- [x] **Devin Connect-RPC wire** — `src/devin.ts` — Acceptance: tools + streaming path work without MCP schema 502.
- [x] **Cursor Connect agent wire** — `src/cursor.ts` — Acceptance: content-addressed blobs + tool round-trips.
- [x] **OpenCode message normalizer** — `src/wire.ts`, CHANGELOG 0.3.3 — Acceptance: multi-agent turns route; prune old screenshots; Anthropic URL images accepted.
- [x] **Tool message ordering** — `src/wire.ts`, CHANGELOG 0.3.3 — Acceptance: tool results immediately follow producing `tool_calls`.
- [x] **Empty tool_call id/name sanitize** — `src/wire.ts`, CHANGELOG 0.3.3 — Acceptance: strict backends no longer 400.
- [x] **Anthropic tool_use id charset rewrite** — `src/wire.ts`, CHANGELOG 0.1.5 — Acceptance: ≤64 `[a-zA-Z0-9_-]+` with stable hash suffix.
- [x] **Responses oversized `call_id` clamp** — `src/responses.ts`, CHANGELOG 0.1.4 — Acceptance: stable short ids; call/output pairs matched.
- [x] **Anthropic prompt-cache breakpoints on bridge** — `src/chat-anthropic-stream.ts`, CHANGELOG 0.1.8 — Acceptance: system / last tool / last message breakpoints set.
- [x] **Anthropic assistant prefill fold** — `src/prepare.ts`, CHANGELOG 0.5.2 — Acceptance: trailing assistant-only turns folded; in-flight `tool_use` preserved.
- [x] **Claude adaptive thinking + effort mapping** — `src/anthropic-thinking.ts`, CHANGELOG 0.1.5 — Acceptance: 4.6+ adaptive shape; `none`→`low` for always-thinking; client thinking translated.
- [x] **`max_tokens` vs thinking budget** — `src/anthropic-thinking.ts` — Acceptance: thinking enable does not 400.
- [x] **Client reasoning effort pass-through when bridged** — `src/effort-wire` / `src/routing.ts`, CHANGELOG 0.1.5 — Acceptance: Codex/Chat effort reaches Claude.
- [x] **Reasoning content passback (DeepSeek/Kimi)** — `src/reasoning-passback.ts`, CHANGELOG 0.1.1 — Acceptance: reinject `reasoning_content` when clients drop it after tools.
- [x] **Claude Code User-Agent on subscription** — `src/anthropic.ts`, CHANGELOG 0.1.5 — Acceptance: current `claude-cli/…` UA so newer models appear.
- [x] **WorkBuddy forced stream + SSE fold for non-stream clients** — `src/workbuddy.ts` — Acceptance: non-stream clients still get full JSON.
- [x] **Upstream body prep module** — `src/prepare.ts` — Acceptance: shared prep for all wires (prefill, tools, effort) before fetch.
- [x] **Request body encoding / compression handling** — `src/body-encoding.ts` — Acceptance: gzip/br client bodies decoded correctly.

### 7.2 Streaming reliability

- [x] **SSE keepalive every 15s idle** — `src/stream-keepalive.ts`, CHANGELOG 0.1.8 — Acceptance: long thinking turns not canceled by agents/tunnels ~2 min.
- [x] **Layered upstream timeouts** — `src/upstream-timeout.ts`, CHANGELOG 0.5.4 — Acceptance: connect / headers / total / idle / first-byte; env `JEVONIAN_UPSTREAM_*_TIMEOUT_MS`.
- [x] **Streaming first-byte deadline failover** — `src/upstream.ts` — Acceptance: no first byte → failover before client committed.
- [x] **Soft-error stream completion** — `src/soft-error.ts`, CHANGELOG 0.5.2 — Acceptance: mid-stream HTTP/SSE/Anthropic/Responses/socket failures finish as assistant message with proper stop events; ledger keeps real status; non-stream still JSON error.
- [x] **Soft-error secret redaction** — `src/soft-error.ts` — Acceptance: echoed credentials become `[REDACTED]`.
- [x] **Transient upstream retries with jitter** — `src/retry.ts`, CHANGELOG 0.1.6 — Acceptance: 5xx/network retried; 429 → quota failover not same-host repeat; `JEVONIAN_UPSTREAM_RETRIES`.
- [x] **HTTP/1.1 pinned dispatcher / keep-alive pool** — `src/proxy.ts`, CHANGELOG 0.0.2 / 0.1.2 — Acceptance: quota JSON not empty under HTTP/2 mismatch; idle sockets ~2 min.
- [x] **Lifecycle drain on update/restart** — `src/lifecycle.ts`, `src/server.ts` — Acceptance: in-flight responses finish (or timeout) before listeners close; 503 during drain as designed.

### 7.3 Egress hygiene

- [x] **`promptPolicy.builtins` rival-prompt rewrites** — `src/prompt-policy.ts`, CHANGELOG 0.3.2 — Acceptance: Cursor signatures rewritten before Devin (and all wires).
- [x] **`promptPolicy.rewrites` custom regex rules** — `src/prompt-policy.ts` — Acceptance: invalid pattern ignored; `$1` replace; system/developer only.
- [x] **`tokenSaver` via `rtk pipe`** — `src/saver.ts`, CHANGELOG 0.3.3 — Acceptance: tool results compressed on all three wires; missing binary = no-op; `savedTokens` in ledger/report/stats; `PUT /api/token-saver`.
- [x] **Shell-tool arg redaction in brain state** — `src/brain.ts`, CHANGELOG 0.1.7 — Acceptance: `; curl …` style snippets redacted so WAF does not 403 brain.

---

## 8. Platform (macOS)

- [x] **LaunchAgent install on bare `jevonian`** — `src/service.ts` — Acceptance: `~/Library/LaunchAgents/ai.jevonian.serve.plist`; KeepAlive; survives reboot.
- [x] **Refuse redirected `JEVONIAN_SERVICE_PLIST` for live control** — `src/service.ts`, CHANGELOG 0.4.0 — Acceptance: install/stop/start/restart refuse override that could hijack production.
- [x] **Refuse overwriting LaunchAgent pointing at different binary** — `src/service.ts` — Acceptance: checkout cannot steal global install; `JEVONIAN_SERVICE_TAKEOVER=1` override.
- [x] **Strip config/data/ledger overrides from service env** — `src/service.ts` — Acceptance: agent never inherits scratch `JEVONIAN_CONFIG` / `DATA_DIR` / `LEDGER` from installing shell.
- [x] **User PATH for tunnel binaries under launchd** — `src/user-path.ts`, `src/service.ts` — Acceptance: ngrok/cloudflared resolve without interactive shell profile.
- [x] **Keychain reads for Claude / Cursor** — `src/oauth.ts`, `src/cursor.ts` — Acceptance: macOS keychain items work when files absent.
- [x] **Darwin-only service control messaging** — `src/cli.ts` — Acceptance: stop/status/restart on non-macOS explain limitation or no-op clearly (TS: requireDarwinServiceControl).
- [x] **System proxy from macOS network settings** — `src/proxy.ts` — Acceptance: applied unless disabled.

---

## 9. Dashboard / Admin

### 9.1 Admin API (`/api/*` on loopback only)

- [x] **`GET /api/state`** — `src/admin.ts` — Acceptance: config snapshot + runtime fields dashboard needs.
- [x] **`GET|POST /api/update/check` + `POST /api/update/install`** — `src/admin.ts`, `src/updates.ts` — Acceptance: tarball-probed upgrade; install+restart; restart-only when disk newest/process stale.
- [x] **`GET|PUT /api/tunnel`** — `src/admin.ts`, `src/tunnel.ts` — Acceptance: status + start/stop/config; cannot start without a Jevonian key.
- [x] **`GET|PUT /api/lan`** — `src/admin.ts`, `src/lan.ts` — Acceptance: enable requires ≥1 key; persists; returns bind URLs.
- [x] **`GET /api/quota`** — `src/admin.ts` — Acceptance: snapshot-first; refresh query/button live.
- [x] **Brains CRUD + move + test** — `src/admin.ts` — Acceptance: `GET/POST /brains`, `PUT/DELETE /brains/:index`, `POST …/move`, `POST /brain/test`.
- [x] **`PUT /api/routing`** — `src/admin.ts` — Acceptance: mode, routings, quotaGuard, capacities, effort flags; does not wipe brains.
- [x] **`POST /api/providers` + `DELETE /api/providers/:name`** — `src/admin.ts` — Acceptance: upsert with discovery/oauth pairing; delete cleans credentials when unused.
- [x] **`POST /api/providers/discover`** — `src/admin.ts` — Acceptance: live model list for form without save.
- [x] **`GET /api/models` + pricing + catalog** — `src/admin.ts` — Acceptance: canonical picker data; `GET/POST /catalog/refresh`; pricing payload.
- [x] **Model-sync get/put/run** — `src/admin.ts` — Acceptance: `GET/PUT /model-sync`, `POST /model-sync/run`.
- [x] **`PUT /api/token-saver`** — `src/admin.ts` — Acceptance: partial update of enabled/command/timeout.
- [x] **Keys CRUD** — `src/admin.ts` — Acceptance: `GET/POST /keys`, `PUT/DELETE /keys/:id` with limits.
- [x] **`GET /api/activity`** — `src/admin.ts`, `src/activity.ts` — Acceptance: spend/token/request series + model/key breakdowns; range + key filters; stats by **canonical** model id (0.5.3).
- [x] **Logs list / stream / series / detail** — `src/admin.ts` — Acceptance: `GET /logs`, SSE `/logs/stream`, `/logs/series`, `/logs/:id` with bodies + brain calls + attempt waterfall; search `failover`/`retry`.
- [x] **`GET /api/stats`** — `src/admin.ts`, `src/stats.ts` — Acceptance: summary including tokens saved; canonical model grouping.
- [x] **`GET /api/tiers`** — `src/admin.ts` — Acceptance: derived builtin tiers for UI.
- [x] **Clients get / connect / restore** — `src/admin.ts`, `src/clients.ts` — Acceptance: `GET /clients`, `POST/DELETE /clients/:id` for `chatgpt` and `claude`.

### 9.2 Web UI pages

- [x] **Overview** — `web/src/pages/overview.tsx` — Acceptance: endpoint copy, savings, cache, tiers, quota cards, update/tunnel/LAN controls, Activity section with charts.
- [x] **Providers (+ brains section)** — `web/src/pages/providers.tsx`, `web/src/components/brain-section.tsx` — Acceptance: add/edit/remove, discover, OAuth second-account fieldset, WorkBuddy sign-in, brain channel picker including Cloudflare Clef models, Test channel.
- [x] **Routing** — `web/src/pages/routing.tsx` — Acceptance: drag model order, provider chips, mode, baseline, quota guard, token saver, capacities; fast load (cached quota).
- [x] **Clients** — `web/src/pages/clients.tsx` — Acceptance: Connect/Restore ChatGPT and Claude (Desktop + Code); surgical JSONC edits (0.4.0).
- [x] **Keys** — `web/src/pages/keys.tsx` — Acceptance: create (show once), revoke, credit limit edit.
- [x] **Logs + log detail** — `web/src/pages/logs.tsx`, `web/src/pages/log-detail.tsx` — Acceptance: live stream, attempt dots/TTFT, waterfall, brain state/verdict, captured prompt.
- [x] **`/activity` → `/#activity` redirect** — `web/src/App.tsx` — Acceptance: old links land on Overview activity.
- [x] **Theme light/dark/system** — `web/src/components/theme-provider.tsx` — Acceptance: preference persisted.
- [x] **Provider logos** — `web/src/lib/logos.ts`, CHANGELOG 0.5.1 — Acceptance: built-in presets have marks (Lobe or local fallback).

### 9.3 Client connectors (disk helpers)

- [x] **Connect Claude (Desktop + Code)** — `src/clients.ts`, `src/claude-code.ts`, `src/claude-gateway.ts` — Acceptance: third-party gateway profile + `~/.claude/settings.json` surgical JSONC; Restore removes only managed keys; backups + restore state.
- [x] **Connect ChatGPT / Codex** — `src/clients.ts` — Acceptance: `~/.codex/config.toml` + model catalogs; never overwrite existing `auth.json` login; sentinel auth for local.
- [x] **Claude Code model labels / env remap** — `src/claude-code.ts` — Acceptance: Jevonian Auto / Utility; Opus/Sonnet/Haiku aliases; launch env.

---

## 10. Ops (update / tunnel / service)

### 10.1 Tunnel

- [x] **Cloudflare quick tunnel** — `src/tunnel.ts` — Acceptance: `cloudflared` → `*.trycloudflare.com`; only `/v1` published.
- [x] **ngrok tunnel + reserved domain** — `src/tunnel.ts` — Acceptance: uses user authtoken; stable URL for Cursor.
- [x] **Custom tunnel command / URL** — `src/tunnel.ts`, `src/config.ts` — Acceptance: `provider: custom` with command/url.
- [x] **Tunnel state survives restart / HMR** — `src/tunnel.ts`, CHANGELOG 0.5.2 — Acceptance: URL persists until Stop; SIGTERM does not kill adopted tunnel; SIGINT stops.
- [x] **Duplicate cloudflared sweep** — `src/tunnel.ts`, CHANGELOG 0.5.2 — Acceptance: restart does not leave two tunnels; matches `sh -c` wrappers.
- [x] **Hung cloudflared SIGKILL escalation** — `src/tunnel.ts` — Acceptance: SIGTERM grace then SIGKILL.
- [x] **Tunnel requires ≥1 Jevonian key** — `src/tunnel.ts`, `src/admin.ts` — Acceptance: start refused otherwise.

### 10.2 LAN

- [x] **LAN listener bind host/port** — `src/lan.ts`, `src/cli.ts` — Acceptance: default `0.0.0.0` and `listen.port+2`; advertises non-loopback IPv4 base URLs.
- [x] **LAN persistence across LaunchAgent** — `src/config.ts` — Acceptance: setting in config.json, not ephemeral flag-only.

### 10.3 Updates

- [x] **Hourly in-process update poll while serving** — `src/cli.ts`, `src/updates.ts` — Acceptance: registry hit gated by cache interval (24h); announces once per latest.
- [x] **Install channel detection** — `src/updates.ts`, `docs/go-rewrite-cutover.md` — Acceptance: npm/pnpm shim vs direct binary/source/unknown; `JEVONIAN_INSTALL_CHANNEL` / `JEVONIAN_NPM_REGISTRY`. npm path uses thin JS that fetches the Go binary.
- [x] **In-process restart after dashboard install** — `src/admin.ts`, `src/cli.ts` — Acceptance: drain → close listeners → relaunch Go binary (or exit under launchd).

### 10.4 Paths, config, env surface

- [x] **Config load/save `~/.config/jevonian/config.json`** — `src/config.ts`, `src/paths.ts` — Acceptance: full parse of listen, providers, routing, tunnel, lan, modelSync, promptPolicy, tokenSaver, modelAliases, defaultProvider.
- [x] **Example config writer** — `src/config.ts` — Acceptance: `writeExampleConfig` / init path.
- [x] **Data dir layout** — `src/paths.ts`, `docs/configuration.md` — Acceptance: ledger, keys, pricing, leaderboard, model-sync, quota, bodies, clients, tunnel/update state, serve.log.
- [x] **Env overrides documented in configuration.md** — various — Acceptance: `JEVONIAN_CONFIG`, `CREDENTIALS`, `DATA_DIR`, `LEDGER`, `MODEL_SYNC_STATE`, `UPDATE_STATE`, `CAPTURE_BODIES`, `NO_OPEN`, `UPSTREAM_RETRIES`, `SAME_HOST_RETRIES`, `UPSTREAM_*_TIMEOUT_MS`, `PROVIDER_CONCURRENCY`, `PROVIDER_BREAKER_*`, `SYSTEM_PROXY`, `WEB_DIR`, `WEB_DEV`, plus OAuth path envs above.
- [x] **Open browser once (dev-aware)** — `src/browser.ts` — Acceptance: opens dashboard; respects no-open; remembers under `JEVONIAN_WEB_DEV`.

---

## 11. Brain channels (parity required except Vercel)

- [x] **TypeSafe direct SystemOne HTTP** — `src/brain.ts` — Acceptance: `https://api.typesafe.ai/v1/systemone`; key `TYPESAFE_API_KEY` / credentials.
- [x] **OpenRouter decisions API** — `src/brain.ts` — Acceptance: `openrouter.ai/api/alpha/decisions` + `typesafe/jev-1.13` + attribution.
- [x] **OpenCode Zen SystemOne** — `src/brain.ts` — Acceptance: `opencode.ai/zen/v1/systemone`.
- [x] **Cloudflare Workers AI + Clef / Clef-flash** — `src/brain.ts`, CHANGELOG 0.5.0 — Acceptance: account id; Jev envelope vs catalog `@cf/…` path shapes; model picker ids; unwrap `{ success, result }`.
- [x] **Custom SystemOne URL channel** — `src/brain.ts` — Acceptance: free-form baseUrl + key.
- [x] **Ordered brain failover (not low-confidence failover)** — `src/brain.ts` — Acceptance: next channel only on hard failure; low confidence still used + marked.
- [x] **Brain timeout / retry / breaker** — `src/brain.ts`, CHANGELOG 0.5.4 — Acceptance: default ~5s, one retry, short breaker → heuristic; per-channel failure isolation (no cross-turn result swap).
- [x] **`fullPrompt` / compact state** — `src/brain.ts` — Acceptance: compact default; full transcript ≤400k chars when enabled.
- [x] **`brainPicksEffort` / `defaultEffort` / `minConfidence` / `timeoutMs`** — `src/config.ts`, `src/brain.ts` — Acceptance: config fields honored.
- [x] **~OUT OF SCOPE~ Vercel `@ai-sdk/gateway`** — `src/brain.ts` — Acceptance: **not required** in Go; UI/docs may hide or reject `channel: "vercel"`.

---

## 12. Ambiguous / clarify during rewrite

These are present in docs or code with tension; decide explicitly while porting (still listed so nothing is forgotten):

1. **Brain total failure: 502 vs heuristic** — `docs/routing.md` still says unreachable brains → 502; README + CHANGELOG 0.1.7 + code soft-fall back to `classifyPhase`. **Parity target: heuristic fallback (code/README).**
2. **Vercel brain in UI** — Keep rejecting/hiding `vercel` channel vs silently ignoring stored configs. **Recommend: accept config but no-op with clear error, or migrate channel away on load.**
3. **Dashboard tech stack** — Parity is **capability** (pages/APIs), not a React/Vite/Kumo pixel clone. Go may serve the existing `web/` build or a reimplemented UI against the same `/api` contract.
4. **Package install channel** — **Decided:** keep `npm i -g jevonian` / `jevonian update` via a **thin JS shim** that downloads/execs the platform Go binary (see `docs/go-rewrite-cutover.md`). Optional brew/GitHub-release direct installs are extra, not a replacement for the npm UX.
5. **LaunchAgent on non-macOS** — TS foreground-serves elsewhere. **Parity: same split (service where platform supports it).**
6. **models.dev / `@lobehub/icons`** — External data/assets; Go needs equivalent catalog snapshots and logo story, not necessarily the same npm packages.
7. **RTK binary dependency** — Token saver no-ops without `rtk`; Go should keep same graceful behavior, not bundle RTK unless product decision changes.
8. **Cursor / Devin unofficial wires** — Fragile by nature; parity means best-effort protocol compatibility, not vendor SLA.

---

## Checkbox count

Approximate inventory in this file: **276** `[ ]` items (including the out-of-scope Vercel marker). Treat the Vercel/`@ai-sdk/gateway` line as non-blocking for “100% parity.”

Sources walked: `README.md`, `CHANGELOG.md` (0.0.1–0.5.4 emphasis 0.4.x–0.5.4), `docs/{cli,configuration,brain,routing,keys,tunnel,providers}.md`, `src/cli.ts`, `src/server.ts`, `src/admin.ts`, `src/config.ts`, `src/providers.ts`, `src/oauth.ts`, `src/brain.ts`, `src/routing.ts`, `src/quota.ts`, `src/ledger.ts`, and related modules named in acceptance hints.
