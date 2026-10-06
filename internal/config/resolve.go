package config

import "os"

// ResolveAPIKey returns the provider's API key from inline config or apiKeyEnv.
// Credential-file lookup is not wired in Go yet.
func ResolveAPIKey(provider Provider) string {
	if provider.APIKey != "" {
		return provider.APIKey
	}
	if provider.APIKeyEnv != "" {
		return os.Getenv(provider.APIKeyEnv)
	}
	return ""
}

// ProviderHasModel reports whether the provider lists the model id.
func ProviderHasModel(provider Provider, model string) bool {
	for _, entry := range provider.Models {
		if entry.ID == model {
			return true
		}
	}
	return false
}

// ProvidersForModel returns providers that list model, preserving config order.
// When defaultProvider lists the model it is moved to the front.
func ProvidersForModel(cfg *Config, model string) []Provider {
	if cfg == nil || model == "" {
		return nil
	}
	var out []Provider
	for _, p := range cfg.Providers {
		if ProviderHasModel(p, model) {
			out = append(out, p)
		}
	}
	if cfg.DefaultProvider == "" || len(out) < 2 {
		return out
	}
	for i, p := range out {
		if p.Name != cfg.DefaultProvider {
			continue
		}
		if i == 0 {
			return out
		}
		preferred := out[i]
		rest := append(append([]Provider{}, out[:i]...), out[i+1:]...)
		return append([]Provider{preferred}, rest...)
	}
	return out
}
