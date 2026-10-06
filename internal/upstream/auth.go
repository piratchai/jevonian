package upstream

import (
	"fmt"
	"net/http"

	"github.com/xinyao27/jevonian/internal/config"
)

// AuthHeaders builds outbound headers for an OpenAI-shaped Chat Completions POST.
func AuthHeaders(provider config.Provider) (http.Header, error) {
	h := make(http.Header)
	h.Set("content-type", "application/json")
	for k, v := range provider.Headers {
		if k == "" {
			continue
		}
		h.Set(k, v)
	}

	if provider.NoKey {
		return h, nil
	}
	token := config.ResolveAPIKey(provider)
	if token == "" {
		return nil, fmt.Errorf("missing API key for provider %q", provider.Name)
	}
	// Minimal openai / both wire: Bearer. Anthropic / OAuth wires land in a later pass.
	if h.Get("authorization") == "" && h.Get("Authorization") == "" {
		h.Set("authorization", "Bearer "+token)
	}
	return h, nil
}
