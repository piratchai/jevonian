package routing

import (
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// Port of src/remote-compaction.test.ts routing half.
func TestRemoteCompactionPinsToChatGPTResponsesProvider(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "deepseek"
	cfg.Providers = []config.Provider{
		provider("deepseek", "deepseek-v4.1-flash"),
		{Name: "chatgpt-subscription", Type: config.ProviderTypeResponses, BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "k",
			Models: []config.ModelEntry{{ID: "gpt-5.4"}}},
	}
	body := map[string]any{"model": "jevonian/auto", "input": []any{map[string]any{"type": "compaction_trigger"}}}
	d, err := RemoteCompactionDecision(cfg, Deps{}, body, map[string]string{}, "rid")
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "chatgpt-subscription" || d.Model != "gpt-5.4" || d.Reason != "remote-compaction" || d.Virtual || !d.Routed || d.RequestedModel != "jevonian/auto" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestRemoteCompactionPrefersCodexOAuthThenChatGPTHost(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{
		{Name: "other", Type: config.ProviderTypeResponses, BaseURL: "https://example.com/v1", Models: []config.ModelEntry{{ID: "o1"}}},
		{Name: "gpt", Type: config.ProviderTypeResponses, BaseURL: "https://chatgpt.com/backend-api/codex", Models: []config.ModelEntry{{ID: "g1"}}},
		{Name: "codex", Type: config.ProviderTypeResponses, BaseURL: "https://x.test", OAuthSource: config.OAuthCodex, Models: []config.ModelEntry{{ID: "c1"}, {ID: "c2"}}},
	}
	body := map[string]any{"model": "c2", "input": []any{}}
	if d, _ := RemoteCompactionDecision(cfg, Deps{}, body, nil, ""); d == nil || d.Provider != "codex" || d.Model != "c2" {
		t.Fatalf("codex oauth first: %+v", d)
	}
	cfg.Providers = cfg.Providers[:2]
	if d, _ := RemoteCompactionDecision(cfg, Deps{}, map[string]any{"model": "zzz"}, nil, ""); d == nil || d.Provider != "gpt" || d.Model != "g1" {
		t.Fatalf("chatgpt host second, first model fallback: %+v", d)
	}
}

func TestRemoteCompactionErrors(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("deepseek", "deepseek-v4.1-flash")}
	_, err := RemoteCompactionDecision(cfg, Deps{}, map[string]any{"model": "deepseek-v4.1-flash"}, nil, "")
	re, ok := err.(*RouteError)
	if !ok || re.Status != 400 || !strings.Contains(re.Message, "ChatGPT subscription") {
		t.Fatalf("err = %v", err)
	}
	cfg.Providers = []config.Provider{{Name: "r", Type: config.ProviderTypeResponses, BaseURL: "https://chatgpt.com"}}
	_, err = RemoteCompactionDecision(cfg, Deps{}, map[string]any{}, nil, "")
	if re, ok := err.(*RouteError); !ok || re.Message != `Provider "r" has no models configured for remote compaction.` {
		t.Fatalf("err = %v", err)
	}
}
