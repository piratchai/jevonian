# Go rewrite cutover policy

## Constraint

When the Go binary reaches **100% feature parity** (except `@ai-sdk/gateway`, which is dropped):

1. Ship the Go `jevonian` binary as the only runtime.
2. **Delete the TypeScript/Node serve path as far as practical** — do not leave a dual runtime.
3. Keep non-runtime artifacts that still make sense: `LICENSE`, `README`, `CHANGELOG`, `docs/`, release skill, etc.

## Status: cutover complete

Parity is verified (see [go-parity-report.md](go-parity-report.md)) and the TypeScript runtime is
gone. Removed: `src/**`, `dist/**`, the root `tsconfig.json`, and the Node server dependencies
(`hono`, `undici`, `ai`, `@ai-sdk/gateway`). What remains from the JS world is the single fetch-and-exec
shim and the dashboard sources.

## Keep: thin npm install / update shim

npm install and `jevonian update` stay. They do **not** run the router in Node.

The shim `bin/jevonian.js`:

1. Resolves / downloads the platform Go binary for the published version
2. `exec`s that binary with the same argv (`serve`, `update`, `doctor`, …)
3. Preserves `npm i -g jevonian` and the existing update UX

The npm package is a fetcher + exec wrapper only — not a second implementation.

## Removed at cutover

- `src/**` (TS router/runtime)
- `dist/**` (the bundled Node CLI + old dashboard build path)
- Root `tsconfig.json` and the `vp pack` entry that produced `dist/cli.mjs`
- Runtime dependencies that existed only for the Node server (`hono`, `undici`, `ai`, `@ai-sdk/gateway`)
- The nested `npm/` package — the shim now lives at `bin/jevonian.js` in the root package

## Remains (decided at cutover)

- `web/` sources — the React + Vite SPA is the dashboard UI, embedded into the Go binary via
  `embed.FS` from `web/dist` and served at `/`. A future `jevonian app` desktop/tray mode is
  additive over the same build.
- `scripts/` — `dev.mjs` (Go serve + Vite dev), `smoke.sh` (Go binary end-to-end), and
  `mock-upstream.mjs` (test fixture server).

## Final layout (post-cutover)

Package layout follows responsibility layers — state/config at the bottom, HTTP/CLI at the top; `upstream` is one-egress mechanics, `routing`/`brain` decide *who*, `wire` is *what we say*, `provider/*` is *per-vendor quirks*:

```
cmd/jevonian/main.go          # sole entrypoint → internal/cli
internal/
  cli/        command dispatch: serve.go lifecycle.go providers.go keycommands.go
              observability.go (doctor/report) pricing.go catalog.go kev.go
  config/     JSONC load, TS-compatible
  paths/      XDG / dataDir
  ledger/     SQLite + JSONL import + rollups
  keys/       keys.json (sha256) + credit limit
  quota/      window health + in-memory cooldown (account spend only)
  guard/      per-provider concurrency cap + circuit breaker (host health only)
  server/     mux + handlers: route.go (attempt loop driver) chat.go streaming.go
              gateway.go probes.go localclient.go nativecodex.go exposure.go
    admin/    /api/* dashboard endpoints
  upstream/   one-egress: forward.go (Runner.Try) adapter.go classify.go post.go
              retry.go timeout.go stream.go auth.go + per-family adapters
              (providers.go = Connect-RPC, freebuff.go, gemini.go)
  routing/    decide.go plan.go narrow.go phase.go reason.go   # candidate ordering
  brain/      client.go types.go                   # hand-rolled scorer (replaces @ai-sdk/gateway)
  catalogsync/ models.dev capabilities + identity index, leaderboard refresh
  modelsync/  provider model-list sync
  clients/    client config writers (launch / kev)
  saver/      RTK token saver
  wire/
    openai/ anthropic/ responses/
  provider/
    cursor/ devin/ workbuddy/ freebuff/ gemini/ multiacct/ parity/
  oauth/      credential import + refresh (Claude Code / Codex / Antigravity / Devin / Cursor)
  softstream/ soft-error SSE resume
  service/    macOS LaunchAgent control
  update/     registry check + npm / native install
  tunnel/     cloudflared lifecycle
  proxy/      system proxy detect
  platform/darwin/ scutil glue
  compaction/ proactive + reactive context compaction
web/          React+Vite SPA — dist/ embedded via web/embed.go (dist not committed)
bin/jevonian.js  # sole JS — fetch+exec platform binary
docs/ go.mod go.sum LICENSE README.md CHANGELOG.md
```

Deleted wholesale at cutover: `src/`, `dist/`, the root `tsconfig.json`, the nested `npm/` package,
and the Node server dependencies. `package.json` is name/version/bin → `bin/jevonian.js` plus the
dashboard dev tooling.

## Architecture rules (do not regress to the TS design)

Found in review of the TS codebase; the Go port fixes them deliberately:

| TS problem | Go rule |
|---|---|
| `upstream.ts` `forward()` — 1214 lines, 16 closures, `devinWire`/`cursorWire`/`workbuddyWire` flags | `upstream/forward.go` attempt loop over `Decision.Order` + per-provider `Adapter`. `AdapterFor` is the only place that maps a provider to behavior; quirks are optional adapter interfaces (`attemptRunner`, `concurrencyLimiter`, `tokenSaverOptOut`, `exclusiveInputUsage`, `responseWrapper`). New providers never touch the loop. |
| "Should we fail over?" spread over quota.ts, upstream.ts, cursor.ts, devin.ts | One classifier: `upstream.Classify(status, body, err) Outcome`. Provider packages map their errors onto `Outcome`. |
| Transport failures, rate limits and quota mixed into one cooldown | Host failures (reset/timeout/5xx) → `internal/guard` breaker only. Quota/billing/rate-limit → `quota.MarkSpent` only. A flaky host must never show as "exhausted". |
| No per-provider isolation in the hot path | `internal/guard`: per-provider concurrency cap + breaker with single half-open probe. Saturated/open → next candidate immediately, never queue. |
| 47 module-level mutable singletons, `reset*()` for tests | State is owned by injected values (`*guard.Guard`, `*quota.Tracker`, `*keys.Store`, `*brain.Client`, `routing.SessionStore`) built in `cli/serve.go`. Avoid new package-level `var` state. |
| `verifyKey` rewrites keys.json synchronously per request | `keys.Store` caches in memory (mtime reload), batches usage writes, `Close()` flushes. |
| `state.config = next` mutated in place | `atomic.Pointer[config.Config]`; a request loads one snapshot for its lifetime; reload swaps. |
| JS `slice` on strings | `wire.TruncateRunes` — never byte-slice user/error text. |
| Duplicated helpers | One soft-error wording (`wire.SoftErrorMessage`), one keepalive (`softstream.Keepalive`). |

## UI delivery

One `web/` SPA, one Go backend, two delivery modes:

1. **Headless / CLI** — `jevonian serve` (default): pure HTTP server on `:8787`, SPA embedded via `embed.FS`, no window. For SSH boxes, headless Linux, or users who prefer a browser tab.
2. **Desktop / Tray** (future) — a native webview window + macOS menu-bar tray over the same server. Desktop mode is **additive**, not a fork: one `web/` build artifact, one Go binary, one set of routes/services.
