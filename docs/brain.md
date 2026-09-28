# Routing brain (Jev)

Routing decisions in Jevonian are driven by **Jev**, the fast decision model developed by [TypeSafe](https://typesafe.ai) under the System One decision architecture.

Unlike general-purpose LLMs that generate prose through autoregressive token generation, Jev is purpose-built for discrete classification and decision-making. Given a structured state snapshot and a set of candidate options, Jev evaluates the criteria and returns a choice with calibrated confidence scores and per-option probabilities, rather than free-form text that has to be parsed.

## Channels and models

Routing always consults a Jev brain. Code narrows the candidates — dropping providers that are out of quota — and Jev names the route and thinking effort to use in a single call. There is no offline fallback: with no brain configured, `jevonian/auto` returns an error instead of guessing. Pick whichever channel you already pay for:

| Channel               | Endpoint                                    | Model               | Key                                 |
| --------------------- | ------------------------------------------- | ------------------- | ----------------------------------- |
| TypeSafe (direct)     | `https://api.typesafe.ai/v1/systemone`      | `jev-latest`        | `TYPESAFE_API_KEY`                  |
| OpenRouter            | `https://openrouter.ai/api/alpha/decisions` | `typesafe/jev-1.13` | `OPENROUTER_API_KEY`                |
| OpenCode Zen          | `https://opencode.ai/zen/v1/systemone`      | `jev-1.13`          | `OPENCODE_API_KEY`                  |
| Vercel AI Gateway     | AI SDK `experimental_evaluate`              | `typesafe-ai/jev`   | `AI_GATEWAY_API_KEY`                |
| Cloudflare Workers AI | `/accounts/{accountId}/ai/run`              | `typesafe/jev`      | `CLOUDFLARE_API_TOKEN` + account ID |
| Kev (local)           | `http://127.0.0.1:8009/v1/systemone`        | `kev-latest`        | optional (`KEV_API_KEY`)            |
| Custom                | your SystemOne-compatible URL               | `jev-latest`        | per channel                         |

- Keys are stored per channel in `~/.config/jevonian/credentials.json` (`brain:<channel>`), or referenced through an env var.
- Each consultation is one `state` + one `questions` call: two `choice` questions — which model, and how deeply it should think — whose criteria come from the candidates the router offered (plus a `none_of_the_above` escape hatch for the model). Brains are tried in order for **failover only**: the first channel that returns a verdict wins. A failed or unreachable brain is skipped; a low-confidence verdict is still used (and marked `x-jevonian-brain: jev-low-confidence`) rather than re-asking the same question on the next channel. If every brain fails the request errors instead of choosing a model on its own.
- The state carries enough to actually judge: the **last user message**, the last assistant message, the session goal, the last few messages, recent tool calls and results, tool availability, consecutive failures, the previously used model, turn count, message count, an estimated token count, the candidate list (including **models.dev** benchmark scores when a local snapshot is present — SWE-Bench, Terminal-Bench, Aider, Toolathlon, and related boards keyed by unified model id), `benchmarks_coverage` (`full` / `partial` / `none`), and the constraints code already applied (`requested_effort`, `skipped`) so the brain can weigh them rather than rediscovering them. Context is recognised by shape, not by vendor: a message that is nothing but a wrapped block (`<user_info>`, `<system-reminder>`, `<environment_context>`, any agent's envelope) is never mistaken for the user's request, while pasted markup such as `<div>…</div>` still counts as prose. Standard HTML tag names are exempt, so a new agent's envelope works without a code change. Any agent that speaks OpenAI, Anthropic or Responses shapes gets the same treatment — no per-agent parsing.
- Set `routing.brainPicksEffort: false` and the effort question is not asked at all: `routing.defaultEffort` is the only level in play, and an effort answer that arrives anyway is ignored rather than trusted.
- Each brain picks how much of the prompt Jev sees. The default **compact** state above keeps consultations cheap. Set `fullPrompt: true` on a brain and it also receives a verbatim `transcript` of every message, system prompt included (capped at 400k characters), when routing accuracy is worth the extra tokens.
- Brain calls are recorded in the ledger (`kind: "brain"`, `provider: "brain:<channel>"`); they no longer appear in the Logs list, but each request's detail page shows them (state + verdict, in fallback order) and the Spend summary includes their cost.
- Every response says which brain decided: `x-jevonian-brain: jev | jev-low-confidence`, and the ledger records `brain` and `confidence`.

## Configuring brains

Brains are configured in their own section on the **Providers** page: add as many as you want (`POST /api/brains`), edit or remove any of them (`PUT`/`DELETE /api/brains/:index`), reorder them with the up/down buttons (`POST /api/brains/:index/move`). Each row shows channel, model, key source and context mode, and the form has the channel picker, key entry, confidence threshold, timeout, context (compact/full), and a **Test channel** button. Removing a brain deletes its stored channel key once no other brain uses that channel. Saving routing policy never overwrites the brains; the Routing page shows them and links here.

The Vercel channel calls `experimental_evaluate` from AI SDK 7 through `@ai-sdk/gateway`. The Cloudflare Workers AI channel posts to `/client/v4/accounts/{accountId}/ai/run` with `state` and `questions` wrapped in `input`. Every other channel speaks the System One HTTP API directly.

## Self-hosting with Kev

[Kev](https://github.com/jaredpalmer/kev) is an open-source (Apache-2.0) Jev-like decision model built on Qwen bases, the closest open alternative for the routing-brain job. It serves the same `POST /v1/systemone` contract, so the `kev` channel needs no client changes and no API key unless the server was started with `KEV_API_KEY`.

One command deploys it and makes it the primary brain (you need `git` and [uv](https://docs.astral.sh/uv/)):

```bash
jevonian kev --start          # clone + install + start kev-4b on :8009 + add the brain
jevonian stop && jevonian     # restart Jevonian so it reads the new brain
```

```bash
jevonian kev --status                           # is it answering?
jevonian kev --stop                             # stop the server
jevonian kev --run jaredpalmer/kev-9b --start   # another checkpoint (stop the running one first)
jevonian kev --start --no-config                # deploy without touching config.json
```

The command puts the Kev brain first in the failover list and keeps any hosted brains behind it, so Jev only answers when the local server is down. It sets `minConfidence` to 0.4: Kev's calibrated probabilities run lower than Jev's, and the Jev default of 0.6 would mark most correct Kev verdicts low-confidence.

**Why not Ollama.** A Kev checkpoint is a LoRA adapter plus a pointer head that reads option probabilities from one forward pass. A GGUF chat runtime such as Ollama cannot serve that, so Kev runs through its own `kev.serve`: MLX on Apple Silicon, CUDA or ROCm on Linux.

Pick a checkpoint by hardware: `kev-4b` runs on a laptop; `kev-9b` wants about 24GB of memory (it runs on an M-series Mac with enough unified memory); `kev-27b` wants about 80GB. The first start downloads the adapter and its Qwen base once (~8GB for 4B, ~18GB for 9B).

**How it differs from Jev.** Kev's answer `confidence` field is a separate calibrated score rather than the winning option's probability, so Jevonian reads Kev's confidence from its probability distribution. Hosted Jev channels keep using Jev's own `confidence`.

**Context limits.** `kev-4b` and `kev-9b` were trained on short states (~384 tokens). Jevonian sends the `kev` channel a trimmed state (no benchmark tables, flat candidate list, or tool-result bodies). The server accepts up to 65,536 tokens, but the smaller checkpoints lose accuracy on long `fullPrompt` transcripts; use `kev-27b` if you enable `fullPrompt`.

**Limits of the replacement.** On a local M5 Pro with `kev-9b`, most routed turns got a confident verdict. With four or more routings competing, Kev still sometimes answers `none_of_the_above`; the router then falls back to the first routing, the same path it takes for Jev. A larger checkpoint, or fine-tuning with the Kev repo's training pipeline, narrows that gap.

## Privacy

The compact state described above is sent to your brain channel on every automatic turn, so the brain provider sees the last user message, recent messages, and recent tool calls and results. `fullPrompt: true` widens that to a verbatim transcript of every message, system prompt included, capped at 400k characters. Enable it only when routing accuracy is worth sharing that context.
