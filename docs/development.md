# Development

The router is a native Go binary. The dashboard is the existing React + Vite SPA under `web/`; Vite+ (Oxlint, Oxfmt, tsdown) drives its build. The Node.js tree is gone — only the npm fetch-and-exec shim in `bin/` remains.

```bash
pnpm dev           # Go serve (18888) + Vite dev server (15174), HMR, opens the dashboard
vp check           # format + lint (primary gate, use --fix)
go test ./...      # unit tests
node --test test/*.test.js  # npm shim tests
pnpm smoke         # end-to-end assertions against a mock upstream, no API keys needed
pnpm build         # build the web UI, then the Go binary
pnpm web:dev       # Vite dev server for the dashboard (proxies /api and /v1 to the local server)
pnpm typecheck:web # type check the dashboard
```

`pnpm dev` starts the dashboard on `15174` and the Go server on `18888` — ports deliberately far from the
production defaults (`8787` server, `5173` web) so a dev run never collides with a running Jevonian
service. It also uses its own config, data, and browser-state under `~/.cache/jevonian` (seeded from
the real config on first run, with tunnel disabled), so saving providers, routings, or keys while
developing cannot touch the running production instance. Opening `http://127.0.0.1:18888` redirects to
the dev server, so there is nothing to build while developing. File-watch restarts reuse the same
browser tab instead of opening a new one each time.

`pnpm build && ./jevonian` serves the proxy and the built dashboard on a single port, `127.0.0.1:8787`. A foreground run refuses to bind a `listen.port` another instance is already using, and bare `jevonian` will not repoint a LaunchAgent that belongs to a different install — see [cli.md](cli.md#one-instance-per-port).

## Source layout

Layers run bottom (state/config) to top (HTTP/CLI). `upstream` is one-egress mechanics; `routing`/`brain` decide *who*; `wire` is *what we say*; `provider/*` holds per-vendor quirks.

- `cmd/jevonian/main.go` — sole entrypoint → `internal/cli`
- `internal/cli/` — command dispatch, serve lifecycle, doctor/report, update, keys
- `internal/config/` — JSONC load/save, validation, provider resolution
- `internal/paths/` — XDG / data-dir layout
- `internal/routing/` — phase classification, tier derivation, session store, virtual models
- `internal/brain/` — SystemOne channels, ordered failover, breaker
- `internal/upstream/` — request forwarding, streaming, usage capture, decision headers
- `internal/wire/` — OpenAI / Anthropic / Responses translation and bridges
- `internal/provider/` — Cursor, Devin, WorkBuddy, Freebuff, Gemini, multi-account adapters
- `internal/oauth/` — Claude Code / Codex / Antigravity / Devin / Cursor credential import and refresh
- `internal/quota/` — live usage endpoints, response-header capture, ledger fallback
- `internal/ledger/` — SQLite ledger with JSONL import and window rollups
- `internal/server/` — mux, handlers, middleware, and the `/api/*` admin surface
- `internal/compaction/` — proactive and reactive context compaction
- `internal/tunnel/` `internal/lan/` `internal/service/` — ops surfaces
- `internal/update/` — release check and install
- `web/` — React + Tailwind + shadcn/ui on Base UI components (Vite+, built to `web/dist`, embedded via `embed.FS`)
- `bin/jevonian.js` — the only JS runtime: fetches and execs the platform binary

## Release process

Releases are cut manually with the repo skill [`.agents/skills/release`](../.agents/skills/release/SKILL.md): bump `package.json`, write `CHANGELOG.md`, commit, tag `vX.Y.Z`, push, and create a GitHub Release. **npm is not published automatically.**

Before tagging, the skill runs:

```bash
pnpm exec vp check
go test ./...
node --test test/*.test.js
pnpm build
pnpm smoke
```

Publish to npm only when you explicitly ask for it after the GitHub release. Follow [publish-checklist.md](../.agents/skills/release/publish-checklist.md) (npm login/2FA, `npm pack --dry-run`, then `npm publish --access public`).

Once a release is on the registry, `jevonian serve` checks npm in the background at most once every 24 hours (on open, then hourly while the process stays up); short CLI commands also surface a previously cached update notice. When a newer version is available, run `jevonian update` or use **Update and restart** on the dashboard. The update is installed through the original npm/pnpm channel; on macOS `jevonian update` then restarts the LaunchAgent, while the dashboard stops accepting new inference requests, lets active streams finish (or cancels them after a 60s drain timeout if one is stuck), and starts the new process on the same port. Source checkouts never self-update.
