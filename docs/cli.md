# CLI

Every command works against the same `~/.config/jevonian/config.json` the dashboard edits.

- `jevonian` / `jevonian serve` — start the local proxy (default). On macOS this installs a LaunchAgent, keeps it running in the background, and returns; on other platforms it serves in the foreground. Use `--foreground` for an attached process on macOS
- `jevonian start` — macOS alias for the above after `jevonian stop` (reloads the LaunchAgent; same as bare `jevonian`)
- `jevonian stop [--uninstall]` — stop the macOS background service (`--uninstall` also removes the LaunchAgent). Prints how to start again
- `jevonian restart` — macOS: kickstart the LaunchAgent onto this binary (rewrites a stale/missing ProgramArguments path after the Node→Go cutover; refuses a live different install). Prefer this over `stop` + `start` when reloading config or applying an update
- `jevonian status` — LaunchAgent state, pid, installed binary path, LAN state, and recent serve log (macOS)
- `jevonian init` — setup wizard for the first provider (non-interactive: writes an example config)
- `jevonian add [provider]` — add or update a provider; interactive picker, live model discovery, auto tiers
- `jevonian providers` — list configured providers with key source and model count
- `jevonian remove <provider> [--keep-key]` — remove a provider and its stored key
- `jevonian report` — spend, cache hit rate, brain-decided turns, savings vs the baseline model, and tokens the tool-result saver removed
- `jevonian doctor [--network]` — config, LaunchAgent health, providers, tiers, ledger, catalog, pricing health
- `jevonian models [--refresh]` — discovered models per provider (hits each provider's `/models`)
- `jevonian models --sync` — append newly discovered model ids into `config.json` (same pass `serve` runs in the background)
- `jevonian pricing [--refresh]` — price table source and size; `--refresh` pulls from models.dev
- `jevonian quota [--refresh]` — per-provider quota windows, reset times, and 30-day spend
- `jevonian update [--check]` — check for or install the latest release through the detected package manager; on macOS a running LaunchAgent is restarted onto the new build (same rewrite path as `restart`)
- `jevonian launch claude [--model M] [--] [args…]` — run Claude Code through Jevonian (Ollama-style env remap)

`serve` also accepts `--tunnel` and `--no-tunnel` to force the public endpoint on or off for that run (these imply foreground on macOS); see [tunnel.md](tunnel.md). Background serve logs to `~/.local/share/jevonian/serve.log`.

`serve` accepts `--lan` / `--no-lan` (plus `--lan-host HOST` and `--lan-port PORT`) to expose the key-protected `/v1` surface on the local network, so another machine — or another Jevonian — can use this instance as a provider. These persist to `config.json`, so the setting survives the macOS LaunchAgent restart; only `/v1` is served, and the dashboard/admin API stay on loopback. See [providers.md](providers.md#another-jevonian-as-a-provider).

### One instance per port

A machine has one Jevonian service and one `config.json`, so a second instance must not grab the first's ports or its LaunchAgent. Two guards enforce that:

- **Foreground `serve` refuses an occupied `listen.port`.** If something is already listening there, it exits with a message instead of fighting (or appearing to replace) the running instance.
- **A background service is never repointed by accident.** Bare `jevonian` installs a LaunchAgent for _its own_ install. If a service is already installed that points at a different install — e.g. a git checkout while a global install is running — it refuses rather than `bootout` the running one and repoint the agent at the checkout. Override deliberately with `JEVONIAN_SERVICE_TAKEOVER=1`, or remove the old one first with `jevonian stop --uninstall`. Auto-rewrite is allowed when the installed ProgramArguments path is missing on disk, or when it is the same npm package's old Node `dist/cli.mjs` after the Go cutover.
- **`JEVONIAN_SERVICE_PLIST` cannot control the live agent.** That override is for reading and unit tests only. Install/stop/start always act on `~/Library/LaunchAgents/ai.jevonian.serve.plist`; a redirected path is refused so a temp plist cannot still `bootout` production. The agent also never inherits `JEVONIAN_CONFIG` / `JEVONIAN_DATA_DIR` / `JEVONIAN_LEDGER` from the installing shell — those stay on the default paths.

To run a local checkout without touching the running service, use `npm run dev`, which uses separate ports and its own config/browser-state (see [development.md](development.md)); or point a second instance at its own `listen.port` and `JEVONIAN_CONFIG` with `--foreground`.

### Claude Code

Same approach as `ollama launch claude`: point Claude Code at the local Anthropic-compatible endpoint and remap Opus / Sonnet / Haiku onto Jevonian models.

```bash
# one-shot (does not change ~/.claude/settings.json)
jevonian launch claude
jevonian launch claude --model jevonian/auto -- -p "summarize this repo"

# or Connect Claude on the Clients page — writes Desktop + ~/.claude/settings.json
# so both the app and a plain `claude` keep using Jevonian until you Restore
```

In `/model` you should see **Jevonian Auto** (and Haiku labeled **Jevonian Utility** when that routing exists). The built-in Sonnet/Opus/Haiku aliases resolve to those same models.

Running from a source checkout, build the binary once and prefix these with `./`:

```bash
go build -o jevonian ./cmd/jevonian
./jevonian
./jevonian report
```

## Dashboard routes

The local server ships a React + Tailwind dashboard:

- `/` — overview: agent endpoint, savings, cache hit rate, tier summary, provider limits
- `/providers` — add/edit/remove providers, live model discovery, key entry, auth and billing mode, usage & limits, and the routing brain
- `/routing` — pick the models behind plan / execute / utility / chat, mode, baseline, quota guard, the token saver switch, and per-provider health
- `/clients` — connect ChatGPT or Claude (Desktop + Claude Code together) to this machine's Jevonian
- `/keys` — generate and revoke Jevonian API keys (`sk-jev-…`, shown once, stored hashed)
- `/logs` — every proxied request with phase, model, tokens, cache reads, cost, latency, reason; click a row for a detail page with the captured prompt and the routing-brain calls (state + verdict)
