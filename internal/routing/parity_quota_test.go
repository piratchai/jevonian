package routing

import (
	"context"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/quota"
)

// Port of src/quota-routing.test.ts "the quota guard narrows what the brain is
// offered". TS drives quota through the ledger; Go injects a QuotaSource, so
// "spend(provider, 10)" becomes an exhausted standing for that provider.

type spentQuota struct {
	providers map[string]bool   // account-wide exhausted
	models    map[string]bool   // "provider/model" window spent
	low       map[string]bool   // account low
	asked     map[string]string // unused diagnostics
}

func (s spentQuota) Standing(p config.Provider, model string, _ int64, _ float64) QuotaView {
	switch {
	case s.providers[p.Name]:
		return QuotaView{Status: quota.StatusExhausted, UsedPercent: 100}
	case s.low[p.Name]:
		return QuotaView{Status: quota.StatusLow, UsedPercent: 95, ModelExhausted: s.models[p.Name+"/"+model]}
	}
	return QuotaView{Status: quota.StatusOK, ModelExhausted: s.models[p.Name+"/"+model]}
}

func spend(names ...string) spentQuota {
	q := spentQuota{providers: map[string]bool{}, models: map[string]bool{}, low: map[string]bool{}}
	for _, n := range names {
		q.providers[n] = true
	}
	return q
}

func qprov(name, typ, baseURL string, models ...string) map[string]any {
	ms := make([]any, len(models))
	for i, m := range models {
		ms[i] = m
	}
	return map[string]any{"name": name, "type": typ, "baseUrl": baseURL, "apiKey": "test", "models": ms}
}

func withSub(p map[string]any) map[string]any {
	p["billing"] = "subscription"
	p["quota"] = map[string]any{"fiveHourUsd": 10}
	return p
}

func qcfg(t *testing.T, defaultProvider string, providers []any, tiers map[string]any, guard map[string]any) *config.Config {
	t.Helper()
	routing := map[string]any{
		"brains": []any{map[string]any{"channel": "typesafe", "apiKeyEnv": "TYPESAFE_API_KEY"}},
		"tiers":  tiers,
	}
	if guard != nil {
		routing["quotaGuard"] = guard
	}
	cfg, err := config.ParseConfig(map[string]any{"defaultProvider": defaultProvider, "providers": providers, "routing": routing})
	if err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func tiersOf(plan, execute, utility []any) map[string]any {
	n := func(v []any) []any {
		if v == nil {
			return []any{}
		}
		return v
	}
	return map[string]any{"plan": n(plan), "execute": n(execute), "utility": n(utility), "chat": []any{}}
}

func twoProviders(t *testing.T, guard map[string]any) *config.Config {
	return qcfg(t, "sub-a", []any{
		withSub(qprov("sub-a", "openai", "http://127.0.0.1:1/v1", "model-a")),
		withSub(qprov("sub-b", "openai", "http://127.0.0.1:2/v1", "model-b")),
	}, tiersOf([]any{"model-a", "model-b"}, nil, nil), guard)
}

type shownResult struct {
	shown    []string // provider/model
	routings []string
	d        *Decision
}

func shown(t *testing.T, cfg *config.Config, q QuotaSource, kind RequestKind, body map[string]any) shownResult {
	t.Helper()
	var res shownResult
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		res.shown, res.routings = nil, nil
		state = normalize(state).(map[string]any)
		for _, c := range asArray(state["candidates"]) {
			r := asRecord(c)
			res.shown = append(res.shown, r["provider"].(string)+"/"+r["model"].(string))
		}
		for _, r := range asArray(state["routings"]) {
			res.routings = append(res.routings, asRecord(r)["id"].(string))
		}
		choice := "none_of_the_above"
		if len(res.routings) > 0 {
			choice = res.routings[0]
		}
		return AskResult{Choice: &Choice{Model: choice, Confidence: 0.9}}
	})
	if body == nil {
		body = map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "build a cache layer"}}}
	}
	d, err := Decide(context.Background(), Deps{Scorer: scorer, Quota: q}, Input{
		Config: cfg, Body: body, Headers: map[string]string{}, Store: NewSessionStore(60_000), Kind: kind, Now: 1_000,
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	res.d = d
	return res
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestQuotaRoutesAroundOneRejectedModel(t *testing.T) {
	cfg := qcfg(t, "devin-subscription", []any{
		map[string]any{"name": "devin-subscription", "type": "devin", "baseUrl": "https://server.codeium.com", "apiKey": "k",
			"billing": "subscription", "models": []any{"swe-1-6-slow", "claude-opus-4-8-medium"}},
	}, tiersOf([]any{"swe-1-6-slow", "claude-opus-4-8-medium"}, nil, nil), nil)
	q := spend()
	q.models["devin-subscription/swe-1-6-slow"] = true
	r := shown(t, cfg, q, KindOpenAI, nil)
	eq(t, r.shown, []string{"devin-subscription/claude-opus-4-8-medium"})
	if r.d.Model != "claude-opus-4-8-medium" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Model, r.d.Reason)
	}
	d, err := Decide(context.Background(), Deps{Scorer: brainStub(), Quota: q}, Input{
		Config: cfg, Body: map[string]any{"model": "jevonian/plan", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		Headers: map[string]string{}, Store: NewSessionStore(60_000), Kind: KindOpenAI, Now: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model != "claude-opus-4-8-medium" || !strings.Contains(d.Reason, "quota-skip") {
		t.Fatalf("explicit = %s %q", d.Model, d.Reason)
	}
}

func TestQuotaSharedRefusalBlocksAllModels(t *testing.T) {
	r := shown(t, twoProviders(t, nil), spend("sub-a"), KindOpenAI, nil)
	eq(t, r.shown, []string{"sub-b/model-b"})
}

func TestQuotaHidesExhaustedProvider(t *testing.T) {
	r := shown(t, twoProviders(t, nil), spend("sub-a"), KindOpenAI, nil)
	eq(t, r.shown, []string{"sub-b/model-b"})
}

func TestQuotaHidesOneProviderOfSharedModel(t *testing.T) {
	cfg := qcfg(t, "sub-a", []any{
		withSub(qprov("sub-a", "openai", "http://127.0.0.1:1/v1", "shared-model")),
		withSub(qprov("sub-b", "openai", "http://127.0.0.1:2/v1", "shared-model")),
	}, tiersOf([]any{"shared-model"}, nil, nil), nil)
	r := shown(t, cfg, spend("sub-a"), KindOpenAI, nil)
	eq(t, r.shown, []string{"sub-b/shared-model"})
	if r.d.Provider != "sub-b" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaGuardDisabledOffersWholeList(t *testing.T) {
	r := shown(t, twoProviders(t, map[string]any{"enabled": false}), spend("sub-a"), KindOpenAI, nil)
	eq(t, r.shown, []string{"sub-a/model-a", "sub-b/model-b"})
	if r.d.Provider != "sub-a" || strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaKeepsExhaustedWhenNothingHealthier(t *testing.T) {
	cfg := twoProviders(t, nil)
	for i := range cfg.Routing.Routings {
		switch cfg.Routing.Routings[i].ID {
		case "plan":
			cfg.Routing.Routings[i].Models = []string{"model-a"}
		default:
			cfg.Routing.Routings[i].Models = nil
		}
	}
	r := shown(t, cfg, spend("sub-a"), KindOpenAI, nil)
	eq(t, r.shown, []string{"sub-a/model-a"})
	if r.d.Provider != "sub-a" || strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaWidensExhaustedExplicitUtilityTier(t *testing.T) {
	cfg := qcfg(t, "opencode-go", []any{
		withSub(qprov("opencode-go", "both", "https://opencode.ai/zen/go/v1", "claude-haiku-4-5-20251001")),
		withSub(qprov("deepseek", "openai", "http://127.0.0.1:2/v1", "deepseek-v4.1-flash")),
	}, tiersOf([]any{"deepseek-v4.1-flash"}, nil, []any{"claude-haiku-4-5-20251001"}), nil)
	d, err := Decide(context.Background(), Deps{Scorer: brainStub(), Quota: spend("opencode-go")}, Input{
		Config: cfg, Body: map[string]any{"model": "jevonian/utility", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		Headers: map[string]string{}, Store: NewSessionStore(60_000), Kind: KindAnthropic, Now: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "deepseek" || d.Model != "deepseek-v4.1-flash" || !strings.Contains(d.Reason, "quota-fallback") {
		t.Fatalf("decision = %s/%s %q", d.Provider, d.Model, d.Reason)
	}
}

func openrouterSpentCfg(t *testing.T, tiers map[string]any) *config.Config {
	cfg := qcfg(t, "opencode-go", []any{
		withSub(qprov("opencode-go", "both", "https://opencode.ai/zen/go/v1", "deepseek-v4.1-flash")),
		withSub(qprov("commandcode", "both", "https://api.commandcode.ai/provider/v1", "deepseek/deepseek-v4.1-flash")),
		qprov("openrouter", "openai", "https://openrouter.ai/api/v1", "deepseek/deepseek-v4.1-flash"),
	}, tiers, nil)
	for _, p := range cfg.Providers {
		if p.Name == "openrouter" && p.Type != config.ProviderTypeBoth {
			t.Fatalf("openrouter type = %q, want both (legacy promotion)", p.Type)
		}
	}
	return cfg
}

func TestQuotaLegacyOpenRouterPromotedForAnthropic(t *testing.T) {
	cfg := openrouterSpentCfg(t, tiersOf(nil, nil, []any{"deepseek-v4.1-flash"}))
	d, err := Decide(context.Background(), Deps{Scorer: brainStub(), Quota: spend("opencode-go", "commandcode")}, Input{
		Config: cfg, Body: map[string]any{"model": "jevonian/utility", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		Headers: map[string]string{}, Store: NewSessionStore(60_000), Kind: KindAnthropic, Now: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "openrouter" || !strings.Contains(d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", d.Provider, d.Reason)
	}
}

func TestQuotaAutoWidensToOpenAIProviders(t *testing.T) {
	cfg := openrouterSpentCfg(t, tiersOf(nil, []any{"deepseek-v4.1-flash"}, nil))
	r := shown(t, cfg, spend("opencode-go", "commandcode"), KindAnthropic, nil)
	if r.d.Provider != "openrouter" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaAutoPrefersDeepSeekDualWire(t *testing.T) {
	cfg := qcfg(t, "opencode-go", []any{
		withSub(qprov("opencode-go", "both", "https://opencode.ai/zen/go/v1", "deepseek-v4.1-flash")),
		qprov("deepseek", "openai", "https://api.deepseek.com/v1", "deepseek-v4.1-flash"),
	}, tiersOf(nil, []any{"deepseek-v4.1-flash"}, nil), nil)
	for _, p := range cfg.Providers {
		if p.Name == "deepseek" && p.Type != config.ProviderTypeBoth {
			t.Fatalf("deepseek type = %q, want both", p.Type)
		}
	}
	r := shown(t, cfg, spend("opencode-go"), KindAnthropic, nil)
	if r.d.Provider != "deepseek" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaAnthropicClientsBridgeToOpenAIWire(t *testing.T) {
	cfg := qcfg(t, "opencode-go", []any{
		withSub(qprov("opencode-go", "both", "https://opencode.ai/zen/go/v1", "deepseek-v4.1-flash")),
		qprov("reseller", "openai", "http://127.0.0.1:9/v1", "deepseek-v4.1-flash"),
	}, tiersOf(nil, []any{"deepseek-v4.1-flash"}, nil), nil)
	r := shown(t, cfg, spend("opencode-go"), KindAnthropic, nil)
	if r.d.Provider != "reseller" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaResponsesClientsBridgeToOpenAIWire(t *testing.T) {
	cfg := qcfg(t, "chatgpt-subscription", []any{
		withSub(qprov("chatgpt-subscription", "responses", "https://chatgpt.com/backend-api/codex", "gpt-6-astra")),
		qprov("openrouter", "both", "https://openrouter.ai/api/v1", "openai/gpt-6-astra", "google/gemini-3.8-flash"),
	}, tiersOf([]any{"gpt-6-astra", "gemini-3-8-flash"}, []any{"gpt-6-astra"}, nil), nil)
	body := map[string]any{"model": "auto", "input": []any{map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "hi"}}}}}
	r := shown(t, cfg, spend("chatgpt-subscription"), KindResponses, body)
	if r.d.Provider != "openrouter" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %q", r.d.Provider, r.d.Reason)
	}
}

func TestQuotaCrossesTiers(t *testing.T) {
	cfg := qcfg(t, "sub-a", []any{
		withSub(qprov("sub-a", "openai", "http://127.0.0.1:1/v1", "model-a")),
		withSub(qprov("sub-b", "openai", "http://127.0.0.1:2/v1", "model-b")),
		qprov("cheap", "openai", "http://127.0.0.1:3/v1", "model-cheap"),
	}, tiersOf([]any{"model-a"}, []any{"model-b"}, []any{"model-cheap"}), nil)
	r := shown(t, cfg, spend("sub-a", "sub-b"), KindOpenAI, nil)
	eq(t, r.shown, []string{"cheap/model-cheap"})
	if r.d.Provider != "cheap" || r.d.Phase != "utility" || !strings.Contains(r.d.Reason, "quota-skip") {
		t.Fatalf("decision = %s %s %q", r.d.Provider, r.d.Phase, r.d.Reason)
	}
}

func TestHeuristicToolResultKeepsExecutePhaseWhenCooldownRoutesElsewhere(t *testing.T) {
	cfg := qcfg(t, "sub-a", []any{
		withSub(qprov("sub-a", "openai", "http://127.0.0.1:1/v1", "model-execute")),
		qprov("fallback", "openai", "http://127.0.0.1:2/v1", "model-plan"),
	}, tiersOf([]any{"model-plan"}, []any{"model-execute"}, nil), nil)
	body := map[string]any{
		"model": "auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "implement this"},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "done"},
		},
	}
	d, err := Decide(context.Background(), Deps{Quota: spend("sub-a")}, Input{
		Config: cfg, Body: body, Headers: map[string]string{}, Store: NewSessionStore(60_000), Kind: KindOpenAI, Now: 1_000,
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Phase != "execute" {
		t.Fatalf("phase = %q, want execute", d.Phase)
	}
	if d.Provider != "fallback" || d.Model != "model-plan" {
		t.Fatalf("fallback = %s/%s, want fallback/model-plan", d.Provider, d.Model)
	}
	if d.Brain != BrainHeuristic {
		t.Fatalf("brain = %q, want heuristic", d.Brain)
	}
	if !strings.Contains(d.Reason, "execute") {
		t.Fatalf("reason = %q, want execute heuristic", d.Reason)
	}
}
