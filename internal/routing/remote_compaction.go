package routing

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
)

// RemoteCompactionDecision pins a Codex remote-compaction v2 turn
// (`compaction_trigger`) to a native Responses host. Bridging it to Chat
// Completions returns a plain message item and Codex aborts with "expected
// exactly one compaction output item", so it must never go through the brain
// or a wire bridge. Provider preference: oauthSource "codex", then a chatgpt.com
// host, then the first Responses provider.
// src/upstream.ts remoteCompactionDecision.
func RemoteCompactionDecision(cfg *config.Config, deps Deps, body map[string]any, headers map[string]string, requestID string) (*Decision, error) {
	var providers []config.Provider
	for _, p := range cfg.Providers {
		if p.Type == config.ProviderTypeResponses {
			providers = append(providers, p)
		}
	}
	var preferred *config.Provider
	for i := range providers {
		if providers[i].OAuthSource == config.OAuthCodex {
			preferred = &providers[i]
			break
		}
	}
	if preferred == nil {
		for i := range providers {
			if u, err := url.Parse(providers[i].BaseURL); err == nil && strings.Contains(u.Hostname(), "chatgpt.com") {
				preferred = &providers[i]
				break
			}
		}
	}
	if preferred == nil && len(providers) > 0 {
		preferred = &providers[0]
	}
	if preferred == nil {
		return nil, &RouteError{Status: 400, Message: "Remote compaction requires a ChatGPT subscription (Responses) provider. Add chatgpt-subscription under Providers."}
	}
	requestedRaw, _ := body["model"].(string)
	requested := strings.TrimPrefix(requestedRaw, "jevonian/")
	model := ""
	for _, m := range preferred.Models {
		if m.ID == requested {
			model = requested
			break
		}
	}
	if model == "" {
		for _, m := range preferred.Models {
			if m.ID != "" {
				model = m.ID
				break
			}
		}
	}
	if model == "" {
		return nil, &RouteError{Status: 400, Message: fmt.Sprintf("Provider %q has no models configured for remote compaction.", preferred.Name)}
	}
	if requestID == "" {
		requestID = "remote-compaction"
	}
	if requestedRaw == "" {
		requestedRaw = model
	}
	return &Decision{
		Model: model, Provider: preferred.Name, Phase: PhaseOfModel(cfg, deps, model),
		RequestedModel: requestedRaw, Virtual: false, Routed: true, Reason: ReasonRemoteCompaction,
		Session: ResolveSessionKey(body, headers), RequestID: requestID,
	}, nil
}
