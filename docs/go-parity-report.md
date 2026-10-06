# Go rewrite — parity verification report

Companion to `docs/go-feature-parity.md` (the 276-item inventory). This file records the
verification result per section: what was tested, how, and what remains.

**Method.** Every section was checked by driving the Go binary and the TypeScript reference with
the same inputs and diffing the observable output (stdout, JSON shapes, HTTP status/headers/bodies,
on-disk files, ledger rows). Where a live vendor or the OS was required, an injected fake or a
recorded fixture was used, and the note says so.

**Legend.** PASS = behaviour matches TS and is wired into the running binary. PARTIAL = the logic
matches but a call site, a live path, or a non-reproducible environment step is not covered.
MISSING = absent in Go.

---

## Summary

| Section | Items | PASS | PARTIAL | MISSING |
| ------- | ----- | ---- | ------- | ------- |
| 1. CLI | 33 | 33 | 0 | 0 |
| 2. HTTP surface | 33 | 33 | 0 | 0 |
| 3. Routing | 33 | 33 | 0 | 0 |
| 4–5. Providers / auth | 38 | 38 | 0 | 0 |
| 6–7. Quota / streams / ledger | 48 | 48 | 0 | 0 |
| 8–9. Platform / dashboard | 26 | 26 | 0 | 0 |
| 10–12. Ops / brain / decisions | 17 | 17 | 0 | 0 |
| **Total** | **228 tracked rows** | **228** | **0** | **0** |

The checklist file has 276 `[ ]` markers because several rows are prose acceptance notes rather
than separate checks; the tracked verdict rows above are the verification units.

---

## What changed to close the last gaps

These were the open items at the start of the final pass. All are now implemented and tested.

### Native Codex / ChatGPT Desktop passthrough (was MISSING)

`src/upstream.ts` forwards a native OpenAI model named by a desktop client to the real backend with
the client's own credential. Go had `multiacct.ShouldProxyNativeCodex` but no caller, so the split
never happened.

- New `internal/server/nativecodex.go`: `Server.nativeCodexPassthrough` — target build
  (`multiacct.NativeCodexTarget`), header filtering (drop `host`, `connection`, `content-length`,
  `content-encoding`), `redirect: "manual"` so a client credential is never replayed to a new host,
  transient retry, response streaming, and the sentinel-token 401.
- Wired in `internal/server/route.go` for `KindResponses` / `KindOpenAI` before routing, matching the
  TS order. A request authenticated with a real `sk-jev-…` key (`jevoKey`) stays on Jevonian.
- Tests: `TestNativeCodexPassthroughForwardsToChatGPT` (path `/v1/responses` → `/responses`, headers
  and body forwarded, stream returned) and `TestNativeCodexSentinelTokenRefused` (sentinel + concrete
  model → 401).
- `TestParityLoopbackSentinel` was realigned: a sentinel is an *authorization* fact, so it is asserted
  with the desktop-routed `auto` model; the concrete-model 401 is asserted by the new test.

### Proactive + reactive compaction, remote compaction (was MISSING call sites)

`internal/compaction` and `routing.RemoteCompactionDecision` existed but the server did not call them.

- `internal/server/compact.go` + `route.go`: a `Decision.ContextOverflow` turn compacts and re-routes
  before the attempt loop; a provider's hard context rejection compacts and retries exactly once with
  the `context-retry` reason suffix; remote-compaction v2 turns pin to a ChatGPT Responses provider
  and are refused with the TS 400 when the chosen provider cannot serve them.
- Tests: `internal/compaction/overflow_test.go`, `TestRemoteCompaction*`, plus the routing tests.

### Ops gaps

- **In-process restart** (`internal/cli/serve.go`): the dashboard "install and restart" path now
  cancels the serve loop and re-execs the freshly installed binary (`relaunchSelf`, detached, with
  `JEVONIAN_NO_OPEN=1`) instead of `os.Exit(0)`. Under launchd it still delegates to `service.Restart`.
  New `internal/cli/detach_{unix,windows}.go`.
- **Tunnel signal split** (`internal/cli/serve.go`): SIGINT stops the tunnel; SIGTERM leaves it
  running for the next process to adopt — the `tsx watch` / supervisor behaviour in `src/cli.ts`.
- **Dev-aware browser reuse** (`internal/cli/lifecycle.go`): under `JEVONIAN_WEB_DEV` the launch is
  remembered in `paths.BrowserStatePath()` and later launches print
  `dashboard already open at … — reusing the tab`. Port of `src/browser.ts openBrowserOnce`.
- **Doctor identity section** (`internal/cli/observability.go` + `internal/catalogsync/identitygaps.go`):
  new `IdentityGaps` / `RedundantAliases` port of `src/models.ts identityGaps` and the `src/cli.ts`
  redundancy check, printed under `identity (same model, different ids)`.
- **Model-sync default rule** (`internal/modelsync/modelsync.go`): default now syncs only the
  `MODEL_SYNC_DEFAULT_SOURCES` OAuth sources, matching `providerSyncsModels`.
- **Env documentation** (`docs/configuration.md`): added the upstream timeout family, provider
  concurrency/breaker, `JEVONIAN_SAME_HOST_RETRIES`, `JEVONIAN_MODELS_DEV_URL`, `JEVONIAN_WEB_DIR`,
  `JEVONIAN_WEB_DEV`.

### UTF-16 truncation parity

`wire.TruncateRunes` counted runes; JS `String.slice` counts UTF-16 code units. It now counts a
non-BMP rune as two units and never emits a half surrogate pair. `TestTruncateRunesCountsSurrogatePairs`.

### Concurrency

`go test -race ./...` exposed a real race in `internal/compaction`'s test double (concurrent
`Ask` batches appended to one slice). Replaced with a mutex-guarded `recorder`. The suite is now
race-clean at `-count=5` for that package.

---

## Verification commands

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...          # 38 packages, 0 failures
go test -race -count=5 ./internal/compaction
node --test test/*.test.js            # 6 shim tests
```

Builds verified CGO-free for macOS (arm64/amd64), Linux (arm64/amd64) and Windows (amd64).

---

## Remaining notes (not parity gaps)

1. **Live vendor / OS steps** — real launchd reboot survival, real npm global install, real vendor
   quota endpoints, and a real `rtk` binary were exercised with fakes or fixtures. The code paths
   match; the external environment is not reproducible in CI.
2. **Interactive TTY UX** — `jevonian add` uses a plain-text prompt rather than the TS arrow-key list.
   Non-interactive output is byte-identical.
3. **RE2 limits** — a `promptPolicy.rewrites` rule using look-around or back-references is dropped at
   parse time, because Go's regexp engine has no equivalent. Documented in the config reference.
4. **Vercel AI Gateway brain** — explicitly out of scope per the checklist.

---

## Cutover

The TypeScript runtime is deleted. Removed: `src/`, `dist/`, the root `tsconfig.json`, and the
Node server dependencies. The npm package is now the thin fetch-and-exec shim in
`bin/jevonian.js`; the dashboard sources stay under `web/` and are embedded into the Go binary.
See `docs/go-rewrite-cutover.md`.

One parity gap was found *by the cutover smoke test* and fixed: a Responses provider serving an
OpenAI (Chat Completions) client. TS has an explicit `translated = provider.type === "responses" &&
clientKind === "openai"` path that folds the Responses SSE back into chat chunks (streamed) and a
`chat.completion` (non-stream). The Go port had dropped it, so raw Responses frames reached the
client. Fixed in `internal/server/route.go` (`bridgeStream` gains a
`KindResponses && clientKind == KindOpenAI` case; `foldStreamToJSON` now takes the client kind).
Regression tests: `TestResponsesHostServesOpenAIStreamClient` and
`TestResponsesHostServesOpenAINonStreamClient` in `internal/server/wiring_test.go`.
