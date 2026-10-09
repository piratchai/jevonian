package upstream

import (
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/ledger"
)

// GoEngineCutover is when the Go engine replaced the TypeScript runtime. Usage
// recorded before it follows the legacy conventions; usage recorded after it
// follows the Go egress conventions. The Connect-RPC hosts are the ones that
// changed: the legacy runtime stored their exclusive (uncached) input, while
// the Go egress renders OpenAI-shaped usage whose prompt count includes the
// cache reads.
var GoEngineCutover = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

// LedgerExclusive reports whether the prompt count Jevonian records for one
// turn already excludes cache reads — the convention the ledger column
// `exclusive_input` stores. It follows the translator that produced the usage,
// not just the provider:
//
//   - Anthropic Messages usage is always exclusive (input_tokens is uncached).
//   - A streaming Chat upstream bridged to an Anthropic client is re-split by
//     anthropicwire.usageFromChat, so its recorded prompt count is exclusive.
//   - A non-streaming fold keeps the upstream's inclusive OpenAI count, as does
//     every OpenAI and Responses translation.
func LedgerExclusive(plan WirePlan, client ClientKind, stream bool) bool {
	if plan.Wire == KindAnthropic {
		return true
	}
	return client == KindAnthropic && plan.Bridge == "to-openai" && stream
}

// ClientKindForPath maps a recorded `/v1` path to the client wire that wrote
// that row, so a migration can re-derive each row's convention.
func ClientKindForPath(path string) ClientKind {
	switch path {
	case "/messages":
		return KindAnthropic
	case "/responses":
		return KindResponses
	default:
		return KindOpenAI
	}
}

// legacyExclusive reports the pre-cutover convention for a provider type. The
// legacy runtime forced the Connect-RPC hosts onto Anthropic-style exclusive
// accounting (`meta.usageKind = "anthropic"`) regardless of the client wire.
func legacyExclusive(p config.Provider) bool {
	switch p.Type {
	case config.ProviderTypeAnthropic, config.ProviderTypeDevin, config.ProviderTypeCursor:
		return true
	default:
		return false
	}
}

// ConventionClassifier answers whether one ledger row's stored prompt count
// already excludes cache reads, given its provider, model, client path, stream
// flag, and the instant it was written. ok=false leaves the row's convention
// unset. It aliases the ledger's own type so a classifier passes to the
// reconcile call without a conversion.
type ConventionClassifier = ledger.ConventionClassifier

// ConventionClassifierFor maps a configured provider name to the convention it
// served with. `model` and `path` are preferred; the provider's own type and
// era decide when the model is unknown.
func ConventionClassifierFor(cfg *config.Config) ConventionClassifier {
	byName := map[string]config.Provider{}
	if cfg != nil {
		for _, p := range cfg.Providers {
			byName[p.Name] = p
		}
	}
	return func(provider, model, path string, stream bool, at time.Time) (bool, bool) {
		p, found := byName[provider]
		if !found {
			return false, false
		}
		if at.Before(GoEngineCutover) {
			return legacyExclusive(p), true
		}
		client := ClientKindForPath(path)
		plan, err := PlanUpstreamWire(p, client, model)
		if err != nil {
			// The catalog may not list this model; the provider's own type still
			// answers for the plan-independent part.
			return p.Type == config.ProviderTypeAnthropic, true
		}
		return LedgerExclusive(plan, client, stream), true
	}
}
