# Routing brain (Jev)

Routing decisions in Jevonian are driven by **Jev**, the fast decision model developed by [TypeSafe](https://typesafe.ai) under the System One decision architecture. Cloudflare's Jev-API-compatible **Clef** models can serve the same role as a drop-in on the Cloudflare channel.

Unlike general-purpose LLMs that generate prose through autoregressive token generation, Jev is purpose-built for discrete classification and decision-making. Given a structured state snapshot and a set of candidate options, Jev evaluates the criteria and returns a choice with calibrated confidence scores and per-option probabilities, rather than free-form text that has to be parsed.

## Channels and models

Routing always consults a Jev brain. Code narrows the candidates — dropping providers that are out of quota — and Jev names the route and thinking effort to use in a single call. There is no offline fallback: with no brain configured, `jevonian/auto` returns an error instead of guessing. Pick whichever channel you already pay for:

| Channel               | Endpoint                                    | Model                                                 | Key                                 |
| --------------------- | ------------------------------------------- | ----------------------------------------------------- | ----------------------------------- |
| TypeSafe (direct)     | `https://api.typesafe.ai/v1/systemone`      | `jev-latest`                                          | `TYPESAFE_API_KEY`                  |
| OpenRouter            | `https://openrouter.ai/api/alpha/decisions` | `typesafe/jev-1.13`                                   | `OPENROUTER_API_KEY`                |
| OpenCode Zen          | `https://opencode.ai/zen/v1/systemone`      | `jev-1.13`                                            | `OPENCODE_API_KEY`                  |
| Cloudflare Workers AI | `/accounts/{accountId}/ai/run`              | `typesafe/jev` · `@cf/cloudflare/clef` · `clef-flash` | `CLOUDFLARE_API_TOKEN` + account ID |
| Custom                | your SystemOne-compatible URL               | `jev-latest`                                          | per channel                         |

The Go build dropped the Vercel AI Gateway brain (`channel: "vercel"`). A stored Vercel brain stays visible and returns a clear error — switch to TypeSafe, OpenRouter, OpenCode Zen, Cloudflare, or custom.

The Cloudflare channel hosts three Jev-API-compatible decision models, offered as a picker on the Providers page (any other id can still be typed into the advanced **Model (override)** field):

- **`typesafe/jev`** — TypeSafe's System One, 32k context, text only. The default.
- **`@cf/cloudflare/clef`** — Cloudflare's [Clef](https://blog.cloudflare.com/clef-decision-models/), 64k context with a vision encoder. Leads the [Jev Decision Index](https://blog.cloudflare.com/clef-decision-models/) on most boards (BFCL, API-Bank, BANKING77, Home appliances) and answers in ~210ms median versus Jev's ~520ms. ~$0.24 per M input tokens — about 5.7x Jev's ~$0.042 — with a 0.46M/day free allotment at the standard 10,000-neuron tier.
- **`@cf/cloudflare/clef-flash`** — the smaller Clef base, tuned for latency (~40ms median). ~$0.09 per M input tokens — about 2.1x Jev — useful when routing sits on the critical path and the state is short.

Clef speaks the same `state` + `questions` contract as Jev, so it needs no channel changes — pick it in the brain form and the router parses the verdict identically. Because Cloudflare and TypeSafe both report an authoritative `confidence`, Clef uses Jev's confidence handling (no `confidenceFromDistribution`). The compact/full context toggle applies unchanged.

Under the hood the channel sends two different request shapes by model id: the `typesafe/jev` alias posts `model` + `input:{state,questions}` to the bare `/ai/run` endpoint, while a Workers AI catalog id (`@cf/...`) puts the model in the path (`.../ai/run/@cf/cloudflare/clef`) and sends `model:"clef"` + `state` + `questions` in the body — matching Cloudflare's own curl docs.

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

## Privacy

The compact state described above is sent to your brain channel on every automatic turn, so the brain provider sees the last user message, recent messages, and recent tool calls and results. `fullPrompt: true` widens that to a verbatim transcript of every message, system prompt included, capped at 400k characters. Enable it only when routing accuracy is worth sharing that context.
