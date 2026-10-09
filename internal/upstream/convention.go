package upstream

import "github.com/xinyao27/jevonian/internal/config"

// ExclusiveInputFor reports the usage convention a provider+model serves with:
// true when the wire's prompt token count already excludes cache reads
// (Anthropic Messages and the Connect-RPC hosts), false when it includes them
// (OpenAI chat completions, Responses, and the Gemini fold).
//
// Readers divide cache coverage by cached reads plus uncached input, and the
// uncached half depends on this convention. Historical ledger rows predate the
// per-row `exclusive_input` column, so a migration asks this to label them
// instead of reading every prompt count as already-uncached.
//
// ok=false means the provider cannot serve the model at all, so the caller
// leaves the row unset.
func ExclusiveInputFor(p config.Provider, model string) (exclusive bool, ok bool) {
	plan, err := PlanUpstreamWire(p, KindOpenAI, model)
	if err != nil {
		return false, false
	}
	adapter, err := AdapterFor(p, KindOpenAI, plan)
	if err != nil {
		return false, false
	}
	return ExclusiveInput(adapter), true
}

// ConventionClassifierFor maps a configured provider name to the convention
// that also answers for models the catalog has not listed. `model` is
// preferred; the provider's own type decides when the model is unknown.
func ConventionClassifierFor(cfg *config.Config) func(provider, model string) (bool, bool) {
	byName := map[string]config.Provider{}
	if cfg != nil {
		for _, p := range cfg.Providers {
			byName[p.Name] = p
		}
	}
	return func(provider, model string) (bool, bool) {
		p, found := byName[provider]
		if !found {
			return false, false
		}
		if exclusive, ok := ExclusiveInputFor(p, model); ok {
			return exclusive, true
		}
		return providerExclusiveByName(p), true
	}
}

// providerExclusiveByName answers for a provider whose model list never named
// the model. Only the provider type matters: the Connect-RPC and Anthropic
// egresses report exclusive usage, every other egress reports inclusive.
func providerExclusiveByName(p config.Provider) bool {
	switch p.Type {
	case config.ProviderTypeAnthropic, config.ProviderTypeDevin,
		config.ProviderTypeCursor, config.ProviderTypeChatGPTWeb:
		return true
	default:
		return false
	}
}
