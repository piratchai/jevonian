package routing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/quota"
)

func TestBrainRoutingPreservesExecuteWhenExecutePoolIsModelLimited(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY"}}
	cfg.Routing.QuotaGuard = config.QuotaGuardConfig{Enabled: true, LowPercent: 10}
	cfg.Providers = []config.Provider{
		provider("devin", "swe-2-max"),
		provider("chatgpt", "gpt-6.1-sol"),
	}
	cfg.Routing.Routings = []config.RoutingEntry{
		{ID: "plan", Label: "Plan", Models: []string{"gpt-6.1-sol"}},
		{ID: "execute", Label: "Execute", Models: []string{"swe-2-max"}},
		{ID: "utility", Label: "Background", Models: []string{}},
		{ID: "chat", Label: "Chit-chat", Models: []string{}},
	}
	brain := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		for _, raw := range asArray(state["routings"]) {
			if asRecord(raw)["id"] == "execute" {
				t.Fatal("model-limited execute pool must not be presented as a viable route")
			}
		}
		return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.99}}
	})
	quotaView := QuotaSourceFunc(func(p config.Provider, model string, _ int64, _ float64) QuotaView {
		if p.Name == "devin" && model == "swe-2-max" {
			return QuotaView{Status: quota.StatusOK, ModelExhausted: true}
		}
		return QuotaView{Status: quota.StatusOK}
	})
	decision, err := Decide(context.Background(), Deps{Scorer: brain, Quota: quotaView, Sleep: func(_ time.Duration) {}}, Input{
		Config: &cfg,
		Body: map[string]any{
			"model": "auto",
			"messages": []any{
				map[string]any{"role": "user", "content": "implement the change"},
				map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "edit", "arguments": "{}"}}}},
				map[string]any{"role": "tool", "content": "tests passed"},
			},
		},
		Headers: map[string]string{},
		Store:   NewSessionStore(60_000),
		Kind:    KindOpenAI,
		Now:     1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != "execute" {
		t.Fatalf("tool-result turn routed as %q; want execute", decision.Phase)
	}
	if decision.Model != "gpt-6.1-sol" || !strings.Contains(decision.Reason, "quota-fallback") {
		t.Fatalf("fallback decision = %s/%s reason=%q", decision.Provider, decision.Model, decision.Reason)
	}
}
