package routing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/quota"
)

// ---- helpers -------------------------------------------------------------

// provider builds a provider serving models on the OpenAI wire.
// Mirrors the `provider` helper in src/routing-reset.test.ts.
// defaultCfg returns a fresh default config pointer.
func defaultCfg() *config.Config {
	c := config.DefaultConfig()
	return &c
}

func provider(name string, models ...string) config.Provider {
	entries := make([]config.ModelEntry, len(models))
	for i, m := range models {
		entries[i] = config.ModelEntry{ID: m}
	}
	return config.Provider{
		Name:    name,
		Type:    config.ProviderTypeOpenAI,
		BaseURL: "http://127.0.0.1:9999/" + name + "/v1",
		APIKey:  "test",
		Models:  entries,
	}
}

// testConfig mirrors src/routing.test.ts testConfig: one mock provider with
// the two deepseek models, auto routing, one typesafe brain.
func testConfig(overrides ...func(*config.Config)) *config.Config {
	cfg := defaultCfg()
	cfg.DefaultProvider = "mock"
	cfg.Providers = []config.Provider{
		provider("mock", "deepseek-v4-pro", "deepseek-v4.1-flash"),
	}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY", TimeoutMs: 1000, MinConfidence: 0.6},
	}
	for _, apply := range overrides {
		apply(cfg)
	}
	return cfg
}

func planBody(model string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "build a feature"}},
	}
}

func executeBody(model string, failures int) map[string]any {
	messages := []any{
		map[string]any{"role": "user", "content": "build a feature"},
		map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id": "1", "type": "function",
				"function": map[string]any{"name": "edit", "arguments": "{}"},
			}},
		},
	}
	for i := 0; i < failures; i++ {
		messages = append(messages, map[string]any{
			"role": "tool", "tool_call_id": string(rune('0' + i)),
			"content": "Error: tests failed",
		})
	}
	if failures == 0 {
		messages = append(messages, map[string]any{
			"role": "tool", "tool_call_id": "ok", "content": "wrote 3 lines",
		})
	}
	return map[string]any{"model": model, "messages": messages}
}

func baseInput(body map[string]any, store *SessionStore, headers map[string]string) Input {
	if store == nil {
		store = NewSessionStore(60_000)
	}
	if headers == nil {
		headers = map[string]string{}
	}
	return Input{
		Config:  testConfig(),
		Body:    body,
		Headers: headers,
		Store:   store,
		Kind:    KindOpenAI,
		Now:     1_000_000,
	}
}

// quotaStub is a QuotaSource backed by per-provider windows — the test double
// for internal/quota's accountWindows.
type quotaStub struct {
	byProvider map[string]QuotaView
}

func (s *quotaStub) Standing(p config.Provider, _ string, _ int64, _ float64) QuotaView {
	if v, ok := s.byProvider[p.Name]; ok {
		return v
	}
	return QuotaView{Status: quota.StatusUnknown}
}

// brainStub mirrors the fetch stub in src/routing.test.ts beforeEach: pick
// "execute" when tools are flowing and failures < 2, else "plan".
func brainStub() Scorer {
	return ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		failures := 0
		if f, ok := state["consecutive_failures"].(float64); ok {
			failures = int(f)
		}
		if f, ok := state["consecutive_failures"].(int); ok {
			failures = f
		}
		hasTools, _ := state["has_tool_results"].(bool)
		var routings []string
		for _, raw := range asArray(state["routings"]) {
			if id, ok := asRecord(raw)["id"].(string); ok {
				routings = append(routings, id)
			}
		}
		wantCheap := hasTools && failures < 2
		choice := "none_of_the_above"
		if wantCheap {
			if contains(routings, "execute") {
				choice = "execute"
			} else if len(routings) > 0 {
				choice = routings[0]
			}
		} else {
			if contains(routings, "plan") {
				choice = "plan"
			} else if len(routings) > 0 {
				choice = routings[0]
			}
		}
		return AskResult{Choice: &Choice{Model: choice, Confidence: 0.9}}
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// bundledPrices mirrors src/pricing/models.json — the price table the TS
// tests inherit. pro output 1.98 ranks it the expensive/plan pick; flash 0.6
// the cheap/execute pick.
var bundledPrices = map[string]*Price{
	"deepseek-v4-pro":     {Provider: "deepseek", PeakRule: "deepseek", Input: 0.66, Output: 1.98, CacheRead: 0.022, HasCacheRd: true},
	"deepseek-v4.1-flash": {Provider: "deepseek", PeakRule: "deepseek", Input: 0.15, Output: 0.6, CacheRead: 0.003, HasCacheRd: true},
}

// priceDeps returns Deps with the bundled price table wired, matching the
// fallback price table TS carries into every routing test.
func priceDeps(extra ...func(*Deps)) Deps {
	d := Deps{
		Scorer: brainStub(),
		Prices: func(model, _ string) *Price { return bundledPrices[model] },
	}
	for _, apply := range extra {
		apply(&d)
	}
	return d
}

// route runs Decide with a brain stub, failing the test on a RouteError.
func route(t *testing.T, deps Deps, input Input) *Decision {
	t.Helper()
	if deps.Scorer == nil {
		deps.Scorer = brainStub()
	}
	d, err := Decide(context.Background(), deps, input)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return d
}

// ---- classifyPhase (src/routing.test.ts describe classifyPhase) ----------

func TestExtractUserQuery(t *testing.T) {
	got := ExtractUserQuery("<timestamp>x</timestamp>\n<user_query>fix it</user_query>")
	if got != "fix it" {
		t.Fatalf("got %q", got)
	}
	if ExtractUserQuery("plain") != "plain" {
		t.Fatal("plain")
	}
}

func TestLastUserMessageCursorStyle(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "<user_info>\nOS Version: darwin\nWorkspace: /tmp/x\n</user_info>"},
			map[string]any{"role": "assistant", "content": "Hi! What would you like to work on?"},
			map[string]any{"role": "user", "content": "<user_query>你是什么模型</user_query>"},
		},
	}
	if got := LastUserMessage(body, KindOpenAI); got != "你是什么模型" {
		t.Fatalf("lastUserMessage = %q", got)
	}
	if ClassifyPhase(body, KindOpenAI).Phase != "plan" {
		t.Fatal("phase should be plan")
	}
}

func TestLastUserMessageIgnoresHugeContext(t *testing.T) {
	preamble := "<user_info>\nOS Version: darwin\n" + strings.Repeat("x", 80_000) + "\n</user_info>"
	body := map[string]any{
		"model": "auto",
		"messages": []any{
			map[string]any{"role": "user", "content": preamble + "\n<timestamp>now</timestamp>"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": "<user_query>fix the cache bug</user_query>"},
		},
	}
	if got := LastUserMessage(body, KindOpenAI); got != "fix the cache bug" {
		t.Fatalf("lastUserMessage = %q", got)
	}
}

func TestLastUserMessageIgnoresUnclosedBlock(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "<environment_context>\npath=/tmp\nshell=zsh"},
			map[string]any{"role": "assistant", "content": "ok"},
		},
	}
	if got := LastUserMessage(body, KindOpenAI); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestLastUserMessagePastedHTML(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "<div class='a'>broken layout</div>"},
		},
	}
	if got := LastUserMessage(body, KindOpenAI); got != "<div class='a'>broken layout</div>" {
		t.Fatalf("got %q", got)
	}
}

func TestLastUserMessageSkipsClaudeReminder(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "make the build green"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "<system-reminder>\nMode is plan.\n</system-reminder>"},
			}},
		},
	}
	if got := LastUserMessage(body, KindAnthropic); got != "make the build green" {
		t.Fatalf("got %q", got)
	}
}

func TestClassifyPhaseFreshIsPlan(t *testing.T) {
	signals := ClassifyPhase(planBody("auto"), KindOpenAI)
	if signals.Phase != "plan" || signals.ConsecutiveFailures != 0 {
		t.Fatalf("signals = %+v", signals)
	}
}

func TestClassifyPhaseNewUserMessageEndsPriorToolTurn(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "implement the change"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "edit", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "content": "done"},
		map[string]any{"role": "assistant", "content": "implementation finished"},
		map[string]any{"role": "user", "content": "Plan the next change. Do not edit files."},
	}}
	signals := ClassifyPhase(body, KindOpenAI)
	if signals.Phase != "plan" || signals.HasToolResults || signals.WithinTurn {
		t.Fatalf("new user turn inherited old tool results: %+v", signals)
	}
}

func TestClassifyPhaseOpenAIToolResults(t *testing.T) {
	signals := ClassifyPhase(executeBody("auto", 0), KindOpenAI)
	if signals.Phase != "execute" || !signals.HasToolResults {
		t.Fatalf("signals = %+v", signals)
	}
}

func TestClassifyPhaseAnthropicToolResult(t *testing.T) {
	signals := ClassifyPhase(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "edit", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "ok"},
			}},
		},
	}, KindAnthropic)
	if signals.Phase != "execute" {
		t.Fatalf("phase = %q", signals.Phase)
	}
}

func TestClassifyPhaseResponsesFunctionCallOutput(t *testing.T) {
	signals := ClassifyPhase(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "go"},
			}},
			map[string]any{"type": "function_call_output", "call_id": "1", "output": "wrote 3 lines"},
		},
	}, KindResponses)
	if signals.Phase != "execute" || !signals.HasToolResults {
		t.Fatalf("signals = %+v", signals)
	}
}

func TestClassifyPhaseConsecutiveFailures(t *testing.T) {
	signals := ClassifyPhase(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "tool", "content": "Error: boom"},
			map[string]any{"role": "tool", "content": "ok"},
			map[string]any{"role": "tool", "content": "Error: tests failed"},
			map[string]any{"role": "tool", "content": "FAIL src/a.test.ts"},
		},
	}, KindOpenAI)
	if signals.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive = %d", signals.ConsecutiveFailures)
	}
}

// ---- deriveTiers / deriveRoutings (src/routing.test.ts) ------------------

func TestDeriveTiersUnconfigured(t *testing.T) {
	tiers := DeriveTiers(testConfig(), priceDeps())
	if len(tiers.Plan) != 1 || tiers.Plan[0] != "deepseek-v4-pro" {
		t.Fatalf("plan = %v", tiers.Plan)
	}
	if len(tiers.Execute) != 1 || tiers.Execute[0] != "deepseek-v4.1-flash" {
		t.Fatalf("execute = %v", tiers.Execute)
	}
	if len(tiers.Chat) == 0 {
		t.Fatal("chat should be non-empty")
	}
}

func TestDeriveTiersKeepsDeclared(t *testing.T) {
	cfg := testConfig()
	// Tiers declared in config are kept verbatim (parseRoutings migrates
	// tiers → routings; both point at deepseek-v4-pro).
	for i := range cfg.Routing.Routings {
		cfg.Routing.Routings[i].Models = []string{"deepseek-v4-pro"}
	}
	tiers := DeriveTiers(cfg, priceDeps())
	if len(tiers.Plan) != 1 || tiers.Plan[0] != "deepseek-v4-pro" {
		t.Fatalf("plan = %v", tiers.Plan)
	}
	if len(tiers.Execute) != 1 || tiers.Execute[0] != "deepseek-v4-pro" {
		t.Fatalf("execute = %v", tiers.Execute)
	}
}

func TestDeriveRoutingsPricedOverUnpriced(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "codex"
	cfg.Providers = []config.Provider{
		{
			Name: "codex", Type: config.ProviderTypeResponses,
			BaseURL:     "https://chatgpt.com/backend-api/codex",
			Auth:        config.AuthOAuth,
			OAuthSource: config.OAuthCodex,
			Models:      []config.ModelEntry{{ID: "gpt-6-sol"}, {ID: "gpt-6-astra"}},
		},
	}
	deps := Deps{
		Prices: func(model, _ string) *Price {
			if model == "gpt-6-astra" {
				return &Price{Provider: "openai", Input: 2, Output: 10}
			}
			return nil
		},
	}
	routings := DeriveRoutings(cfg, deps)
	modelOf := func(id string) []string {
		for _, r := range routings {
			if r.ID == id {
				return r.Models
			}
		}
		return nil
	}
	if got := modelOf("plan"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Fatalf("plan = %v", got)
	}
	// A cheap routing is a cost claim an unknown price cannot back: it reuses
	// the priced plan model rather than pinning the unpriced id.
	if got := modelOf("execute"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Fatalf("execute = %v", got)
	}
}

func TestDeriveRoutingsUnpricedFallback(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{
		{
			Name: "codex", Type: config.ProviderTypeResponses,
			BaseURL:     "https://chatgpt.com/backend-api/codex",
			Auth:        config.AuthOAuth,
			OAuthSource: config.OAuthCodex,
			Models:      []config.ModelEntry{{ID: "gpt-6-sol"}},
		},
	}
	routings := DeriveRoutings(cfg, Deps{})
	for _, r := range routings {
		if r.ID == "plan" {
			if len(r.Models) != 1 || r.Models[0] != "gpt-6-sol" {
				t.Fatalf("plan = %v", r.Models)
			}
			return
		}
	}
	t.Fatal("no plan routing")
}

func TestDeriveRoutingsNeverUnpricedCheap(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{
		{
			Name: "codex", Type: config.ProviderTypeResponses,
			BaseURL:     "https://chatgpt.com/backend-api/codex",
			Auth:        config.AuthOAuth,
			OAuthSource: config.OAuthCodex,
			Models:      []config.ModelEntry{{ID: "gpt-6-sol"}, {ID: "gpt-6-astra"}, {ID: "gpt-mini"}},
		},
	}
	deps := Deps{
		Prices: func(model, _ string) *Price {
			switch model {
			case "gpt-6-astra":
				return &Price{Provider: "openai", Input: 2, Output: 10}
			case "gpt-mini":
				return &Price{Provider: "openai", Input: 0.1, Output: 0.4}
			}
			return nil
		},
	}
	routings := DeriveRoutings(cfg, deps)
	for _, r := range routings {
		if r.ID != "plan" {
			for _, m := range r.Models {
				if m == "gpt-6-sol" {
					t.Fatalf("unpriced model in cheap slot %q", r.ID)
				}
			}
		}
		if r.ID == "execute" && (len(r.Models) != 1 || r.Models[0] != "gpt-mini") {
			t.Fatalf("execute = %v", r.Models)
		}
	}
}

// ---- decideRoute (src/routing.test.ts) -----------------------------------

func TestDecideVirtualPlanForNewSession(t *testing.T) {
	d := route(t, priceDeps(), baseInput(planBody("auto"), nil, nil))
	if !d.Virtual || !d.Routed {
		t.Fatal("should be virtual+routed")
	}
	if d.Phase != "plan" || d.Model != "deepseek-v4-pro" {
		t.Fatalf("phase=%q model=%q", d.Phase, d.Model)
	}
	if d.Reason != "brain:plan" {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Brain != BrainJev {
		t.Fatalf("brain = %q", d.Brain)
	}
}

func TestDecideKeepsFrontierForGreeting(t *testing.T) {
	cfg := testConfig()
	// Every tier points at the same model: a greeting still lands on it.
	for i := range cfg.Routing.Routings {
		cfg.Routing.Routings[i].Models = []string{"deepseek-v4-pro"}
	}
	input := baseInput(map[string]any{
		"model":    "jevonian/auto",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if d.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.Brain != BrainJev {
		t.Fatalf("brain = %q", d.Brain)
	}
}

func TestDecideNoBrainConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Brains = nil
	input := baseInput(planBody("auto"), nil, nil)
	input.Config = cfg
	input.Now = 1_000
	_, err := Decide(context.Background(), Deps{Scorer: brainStub(), Prices: func(m, _ string) *Price { return bundledPrices[m] }}, input)
	re, ok := err.(*RouteError)
	if !ok {
		t.Fatalf("expected RouteError, got %v", err)
	}
	if !strings.Contains(re.Message, "No Jev brain is configured") {
		t.Fatalf("message = %q", re.Message)
	}
	if re.Status != 400 {
		t.Fatalf("status = %d", re.Status)
	}
}

func TestDecideNamespacedVirtual(t *testing.T) {
	d := route(t, priceDeps(), baseInput(planBody("jevonian/auto"), nil, nil))
	if !d.Virtual {
		t.Fatal("should be virtual")
	}
	if d.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.RequestedModel != "jevonian/auto" {
		t.Fatalf("requestedModel = %q", d.RequestedModel)
	}
}

func TestDecideStripsPrefixFromPinned(t *testing.T) {
	d := route(t, priceDeps(), baseInput(planBody("jevonian/deepseek-v4.1-flash"), nil, nil))
	if d.Routed {
		t.Fatal("should not be routed")
	}
	if d.Reason != ReasonPinnedModel {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", d.Model)
	}
}

func TestDecideVirtualWinsOverCatalogCollision(t *testing.T) {
	cfg := testConfig()
	cfg.Providers = []config.Provider{
		provider("mock", "auto", "deepseek-v4-pro", "deepseek-v4.1-flash"),
	}
	input := baseInput(planBody("jevonian/auto"), nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if !d.Virtual {
		t.Fatal("should be virtual")
	}
	if d.Model != "deepseek-v4-pro" || d.Provider != "mock" {
		t.Fatalf("model=%q provider=%q", d.Model, d.Provider)
	}
}

func TestDecideProviderQualifiedAutoIsNotVirtual(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "agg"
	cfg.Providers = []config.Provider{provider("agg", "auto")}
	cfg.Routing.Brains = nil
	input := baseInput(planBody("agg/auto"), nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d, err := Decide(context.Background(), Deps{Scorer: brainStub(), Prices: func(m, _ string) *Price { return bundledPrices[m] }}, input)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Virtual {
		t.Fatal("should not be virtual")
	}
	if d.Reason != ReasonCanonicalModel {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Model != "auto" || d.Provider != "agg" {
		t.Fatalf("model=%q provider=%q", d.Model, d.Provider)
	}
}

func TestDecideHonoursPinnedModel(t *testing.T) {
	d := route(t, priceDeps(), baseInput(planBody("deepseek-v4.1-flash"), nil, nil))
	if d.Routed {
		t.Fatal("should not be routed")
	}
	if d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.Reason != ReasonPinnedModel {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideDropsToCheapOnceToolsFlow(t *testing.T) {
	store := NewSessionStore(60_000)
	route(t, priceDeps(), baseInput(planBody("auto"), store, nil))
	d := route(t, priceDeps(), baseInput(executeBody("auto", 0), store, nil))
	if d.Phase != "execute" {
		t.Fatalf("phase = %q", d.Phase)
	}
	if d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.Reason != "brain:execute" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideReturnsToFrontierAfterFailures(t *testing.T) {
	store := NewSessionStore(60_000)
	route(t, priceDeps(), baseInput(planBody("auto"), store, nil))
	route(t, priceDeps(), baseInput(executeBody("auto", 0), store, nil))
	d := route(t, priceDeps(), baseInput(executeBody("auto", 2), store, nil))
	if d.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.Phase != "plan" {
		t.Fatalf("phase = %q", d.Phase)
	}
}

func TestDecideResponsesProvider(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "codex"
	cfg.Providers = []config.Provider{
		{
			Name: "codex", Type: config.ProviderTypeResponses,
			BaseURL:     "https://chatgpt.com/backend-api/codex",
			Auth:        config.AuthOAuth,
			OAuthSource: config.OAuthCodex,
			Billing:     config.BillingSubscription,
			Models:      []config.ModelEntry{{ID: "gpt-5.6-codex"}},
		},
	}
	for i := range cfg.Routing.Routings {
		cfg.Routing.Routings[i].Models = []string{"gpt-5.6-codex"}
	}
	input := baseInput(map[string]any{"model": "auto", "input": "hi"}, nil,
		map[string]string{"x-jevonian-phase": "plan"})
	input.Config = cfg
	input.Kind = KindResponses
	input.Now = 1_000
	d, err := Decide(context.Background(), Deps{Scorer: brainStub(), Prices: func(m, _ string) *Price { return bundledPrices[m] }}, input)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Model != "gpt-5.6-codex" || d.Provider != "codex" {
		t.Fatalf("model=%q provider=%q", d.Model, d.Provider)
	}
}

func TestDecideExplicitPhaseHeader(t *testing.T) {
	input := baseInput(planBody("auto"), nil, map[string]string{"x-jevonian-phase": "execute"})
	d := route(t, priceDeps(), input)
	if d.Phase != "execute" {
		t.Fatalf("phase = %q", d.Phase)
	}
	if d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.Reason != "explicit:execute" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideRejectsVirtualWhenRoutingOff(t *testing.T) {
	input := baseInput(planBody("auto"), nil, nil)
	input.Config = testConfig()
	input.Config.Routing.Mode = "off"
	_, err := Decide(context.Background(), Deps{Scorer: brainStub(), Prices: func(m, _ string) *Price { return bundledPrices[m] }}, input)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDecideExplicitSessionHeader(t *testing.T) {
	store := NewSessionStore(60_000)
	headers := map[string]string{"x-session-id": "session-1"}
	first := route(t, priceDeps(), baseInput(planBody("auto"), store, headers))
	second := route(t, priceDeps(), baseInput(planBody("auto"), store, headers))
	if first.Session != "session-1" {
		t.Fatalf("session = %q", first.Session)
	}
	if second.Phase != "plan" {
		t.Fatalf("phase = %q", second.Phase)
	}
	if second.Reason != "brain:plan" {
		t.Fatalf("reason = %q", second.Reason)
	}
}

// ---- SessionStore (src/routing.test.ts) ----------------------------------

func TestSessionStoreExpiresAfterTTL(t *testing.T) {
	store := NewSessionStore(1_000)
	store.Set("a", SessionState{
		Phase: "plan", Model: "m", Provider: "p", Turns: 1, UpdatedAt: 0,
	})
	if _, ok := store.Get("a", 500); !ok {
		t.Fatal("session should exist at 500")
	}
	if _, ok := store.Get("a", 2_000); ok {
		t.Fatal("session should be expired at 2000")
	}
}

// ---- brainStateFor (src/routing.test.ts) ---------------------------------

func readyState() map[string]any {
	return map[string]any{
		"last_user_message":   "fix the bug",
		"recent_tool_results": []any{"wrote 3 lines"},
		"benchmark_focus":     map[string]any{"prefer_boards": []any{"swe-bench"}},
		"benchmarks_coverage": "full",
		"candidates":          []any{map[string]any{"model": "m", "provider": "p"}},
		"routings": []any{
			map[string]any{
				"id":              "execute",
				"benchmark_focus": map[string]any{"prefer_boards": []any{"swe-bench"}},
				"models": []any{
					map[string]any{
						"model": "m", "provider": "p",
						"benchmarks": map[string]any{"by_board": map[string]any{"swe-bench": 70}},
					},
				},
			},
		},
	}
}

func TestStateForBrainHostedGetsFullState(t *testing.T) {
	ready := readyState()
	state := StateForBrain(
		config.BrainConfig{Channel: "typesafe", TimeoutMs: 1000, MinConfidence: 0.6, FullPrompt: true},
		ready,
		"[user] fix the bug",
	)
	if state["transcript"] != "[user] fix the bug" {
		t.Fatalf("transcript = %v", state["transcript"])
	}
	if state["candidates"] == nil || state["benchmark_focus"] == nil {
		t.Fatal("hosted channel should keep full state")
	}
}

func TestStateForBrainCompactTrimsSoftEvidence(t *testing.T) {
	ready := readyState()
	state := StateForBrain(
		config.BrainConfig{Channel: "kev", TimeoutMs: 1000, MinConfidence: 0.4},
		ready,
		"",
	)
	if _, has := state["candidates"]; has {
		t.Fatal("kev should not receive candidates")
	}
	if _, has := state["recent_tool_results"]; has {
		t.Fatal("kev should not receive recent_tool_results")
	}
	if _, has := state["benchmark_focus"]; has {
		t.Fatal("kev should not receive benchmark_focus")
	}
	routings, ok := state["routings"].([]any)
	if !ok || len(routings) != 1 {
		t.Fatalf("routings = %v", state["routings"])
	}
	routing := routings[0].(map[string]any)
	if _, has := routing["benchmark_focus"]; has {
		t.Fatal("routing benchmark_focus should be trimmed")
	}
	models := routing["models"].([]any)
	if _, has := models[0].(map[string]any)["benchmarks"]; has {
		t.Fatal("model benchmarks should be trimmed")
	}
	// The shared ready map must not be mutated.
	if _, has := ready["candidates"]; !has {
		t.Fatal("input ready map was mutated")
	}
	origRouting := ready["routings"].([]any)[0].(map[string]any)
	if _, has := origRouting["benchmark_focus"]; !has {
		t.Fatal("input routing was mutated")
	}
}

// ---- decideRoute with the Jev brain (src/routing.test.ts) -----------------

func TestDecideAcceptsConfidentVerdict(t *testing.T) {
	var sentState map[string]any
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		sentState = state
		return AskResult{Choice: &Choice{
			Model: "execute", Confidence: 0.9, ModelName: "jev-1.13.0",
			Usage: &Usage{Input: 120, Output: 5},
		}}
	})
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), baseInput(planBody("auto"), nil, nil))
	if sentState["last_user_message"] != "build a feature" {
		t.Fatalf("last_user_message = %v", sentState["last_user_message"])
	}
	if d.Phase != "execute" || d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("phase=%q model=%q", d.Phase, d.Model)
	}
	if d.Brain != BrainJev {
		t.Fatalf("brain = %q", d.Brain)
	}
	if d.Reason != "brain:execute" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideLowConfidenceSkipsNextBrain(t *testing.T) {
	var called []string
	scorer := ScorerFunc(func(_ context.Context, b config.BrainConfig, _ map[string]any, _ bool) AskResult {
		called = append(called, b.BaseURL)
		return AskResult{Choice: &Choice{Model: "execute", Confidence: 0.4}}
	})
	cfg := defaultCfg()
	cfg.DefaultProvider = "mock"
	cfg.Providers = []config.Provider{provider("mock", "deepseek-v4-pro", "deepseek-v4.1-flash")}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "custom", BaseURL: "http://first-brain/systemone", APIKeyEnv: "JEV_TEST_BRAIN_KEY", TimeoutMs: 500, MinConfidence: 0.6},
		{Channel: "custom", BaseURL: "http://second-brain/systemone", APIKeyEnv: "JEV_TEST_BRAIN_KEY", TimeoutMs: 500, MinConfidence: 0.6},
	}
	input := baseInput(planBody("auto"), nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if d.Brain != BrainJevLowConfidence {
		t.Fatalf("brain = %q", d.Brain)
	}
	if d.Reason != "brain:execute:brain-low-confidence" {
		t.Fatalf("reason = %q", d.Reason)
	}
	if len(called) != 1 || called[0] != "http://first-brain/systemone" {
		t.Fatalf("called = %v", called)
	}
}

func TestDecideSingleBrainLowConfidence(t *testing.T) {
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		return AskResult{Choice: &Choice{Model: "execute", Confidence: 0.4}}
	})
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), baseInput(planBody("auto"), nil, nil))
	if d.Phase != "execute" || d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("phase=%q model=%q", d.Phase, d.Model)
	}
	if d.Brain != BrainJevLowConfidence {
		t.Fatalf("brain = %q", d.Brain)
	}
	if d.Reason != "brain:execute:brain-low-confidence" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideSendsTranscriptWhenBrainAsks(t *testing.T) {
	var sentState map[string]any
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		sentState = state
		return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.9}}
	})
	cfg := testConfig()
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "custom", BaseURL: "http://brain/systemone", APIKeyEnv: "JEV_TEST_BRAIN_KEY", FullPrompt: true},
	}
	input := baseInput(planBody("auto"), nil, nil)
	input.Config = cfg
	input.Now = 1_000
	route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	tr, _ := sentState["transcript"].(string)
	if !strings.Contains(tr, "[user] build a feature") {
		t.Fatalf("transcript = %q", tr)
	}
}

func TestDecideFallsBackToNextBrain(t *testing.T) {
	var calls []string
	scorer := ScorerFunc(func(_ context.Context, b config.BrainConfig, _ map[string]any, _ bool) AskResult {
		calls = append(calls, b.BaseURL)
		if b.BaseURL == "http://first-brain/systemone" {
			return AskResult{Failure: &Failure{Error: "boom"}}
		}
		return AskResult{Choice: &Choice{Model: "execute", Confidence: 0.9}}
	})
	cfg := defaultCfg()
	cfg.DefaultProvider = "mock"
	cfg.Providers = []config.Provider{provider("mock", "deepseek-v4-pro", "deepseek-v4.1-flash")}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "custom", BaseURL: "http://first-brain/systemone", APIKeyEnv: "JEV_TEST_BRAIN_KEY", TimeoutMs: 500},
		{Channel: "custom", BaseURL: "http://second-brain/systemone", APIKeyEnv: "JEV_TEST_BRAIN_KEY", TimeoutMs: 500},
	}
	input := baseInput(planBody("auto"), nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if d.Brain != BrainJev {
		t.Fatalf("brain = %q", d.Brain)
	}
	if d.BrainChannel != "custom" {
		t.Fatalf("channel = %q", d.BrainChannel)
	}
	if d.Reason != "brain:execute" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideHeuristicWhenEveryBrainFails(t *testing.T) {
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		return AskResult{Failure: &Failure{Status: 500, Error: "boom"}}
	})
	slept := 0
	deps := priceDeps(func(d *Deps) { d.Scorer = scorer; d.Sleep = func(time.Duration) { slept++ } })
	d := route(t, deps, baseInput(planBody("auto"), nil, nil))
	if d.Brain != BrainHeuristic {
		t.Fatalf("brain = %q", d.Brain)
	}
	if !strings.HasPrefix(d.Reason, "brain-fallback:") {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Phase != "plan" {
		t.Fatalf("phase = %q", d.Phase)
	}
	if slept != 1 {
		t.Fatalf("expected 1 inter-round sleep, got %d", slept)
	}
}

func TestDecideRetriesWholeBrainRound(t *testing.T) {
	// First round's channel calls all fail; the second round succeeds.
	hits := 0
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		hits++
		if hits <= 1 {
			return AskResult{Failure: &Failure{Status: 502, Error: "down"}}
		}
		return AskResult{Choice: &Choice{Model: "execute", Confidence: 0.9}}
	})
	slept := 0
	deps := priceDeps(func(d *Deps) { d.Scorer = scorer; d.Sleep = func(time.Duration) { slept++ } })
	d := route(t, deps, baseInput(planBody("auto"), nil, nil))
	if d.Brain != BrainJev {
		t.Fatalf("brain = %q", d.Brain)
	}
	if hits != 2 {
		t.Fatalf("expected 2 channel calls (round 1 + round 2), got %d", hits)
	}
	if slept != 1 {
		t.Fatalf("expected 1 inter-round sleep, got %d", slept)
	}
}

func TestDecideUnknownModelFallsBackToFirstCandidate(t *testing.T) {
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		return AskResult{Choice: &Choice{Model: "gpt-9-nonexistent", Confidence: 0.9}}
	})
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), baseInput(planBody("auto"), nil, nil))
	if d.Model != "deepseek-v4-pro" && d.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", d.Model)
	}
}

// ---- protocol parity (src/routing.test.ts) --------------------------------

var parityASK = "fix the flaky auth test"
var parityPREAMBLE = "<user_info>\nOS Version: darwin\nWorkspace: /repo\n</user_info>"
var parityTOOL = "shell"
var parityFAILED = "Exit code: 1\n\nCommand output:\n\n```\nnpm err assert failed\n```"

func parityBody(kind RequestKind) map[string]any {
	switch kind {
	case KindAnthropic:
		return map[string]any{
			"model": "auto",
			"messages": []any{
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": parityPREAMBLE}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": parityASK}}},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "tool_use", "id": "1", "name": parityTOOL, "input": map[string]any{"cmd": "npm test"}},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "1", "content": parityFAILED},
				}},
			},
		}
	case KindResponses:
		return map[string]any{
			"model": "auto",
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": parityPREAMBLE},
				}},
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": parityASK},
				}},
				map[string]any{"type": "function_call", "name": parityTOOL, "arguments": `{"cmd":"npm test"}`},
				map[string]any{"type": "function_call_output", "call_id": "1", "output": parityFAILED},
			},
		}
	default: // openai
		return map[string]any{
			"model": "auto",
			"messages": []any{
				map[string]any{"role": "user", "content": parityPREAMBLE + "\n<timestamp>t</timestamp>"},
				map[string]any{"role": "user", "content": "<user_query>" + parityASK + "</user_query>"},
				map[string]any{"role": "assistant", "content": "",
					"tool_calls": []any{map[string]any{
						"id": "1", "function": map[string]any{"name": parityTOOL, "arguments": `{"cmd":"npm test"}`},
					}},
				},
				map[string]any{"role": "tool", "tool_call_id": "1", "content": parityFAILED},
			},
		}
	}
}

func TestProtocolParityReadsSameAskEveryShape(t *testing.T) {
	for _, kind := range []RequestKind{KindOpenAI, KindAnthropic, KindResponses} {
		var state map[string]any
		scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, s map[string]any, _ bool) AskResult {
			state = s
			return AskResult{Choice: &Choice{Model: "execute", Confidence: 0.9}}
		})
		// "both" provider serves every shape.
		cfg := testConfig()
		cfg.Providers[0].Type = config.ProviderTypeBoth
		input := baseInput(parityBody(kind), nil, nil)
		input.Config = cfg
		input.Kind = kind
		route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)

		if state["last_user_message"] != parityASK {
			t.Fatalf("%s: last_user_message = %v", kind, state["last_user_message"])
		}
		if state["session_goal"] != parityASK {
			t.Fatalf("%s: session_goal = %v", kind, state["session_goal"])
		}
		calls, _ := state["recent_tool_calls"].([]string)
		if len(calls) != 1 || calls[0] != parityTOOL+"(<command redacted>)" {
			t.Fatalf("%s: recent_tool_calls = %v", kind, state["recent_tool_calls"])
		}
		if f, _ := state["consecutive_failures"].(int); f != 1 {
			t.Fatalf("%s: consecutive_failures = %v", kind, state["consecutive_failures"])
		}
		results, _ := state["recent_tool_results"].([]string)
		joined := strings.Join(results, "\n")
		if !strings.Contains(joined, "assert failed") {
			t.Fatalf("%s: recent_tool_results = %v", kind, results)
		}
	}
}

// ---- custom routings (src/routing.test.ts) --------------------------------

func TestCustomRoutingAliasAdvertised(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "mock"
	cfg.Providers = []config.Provider{provider("mock", "frontend-model", "deepseek-v4.1-flash")}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY", TimeoutMs: 1000},
	}
	cfg.Routing.Routings = append(cfg.Routing.Routings, config.RoutingEntry{
		ID: "frontend", Label: "Frontend", Description: "React, CSS, UI polish",
		Models: []string{"frontend-model"},
	})
	if !contains(VirtualModels(cfg), "jevonian/frontend") {
		t.Fatalf("virtualModels = %v", VirtualModels(cfg))
	}
	if !IsVirtualModel("jevonian/frontend", cfg) {
		t.Fatal("jevonian/frontend should be virtual")
	}
	if IsVirtualModel("jevonian/frontend", nil) {
		t.Fatal("frontend is not builtin; nil-config check should be false")
	}
	input := baseInput(map[string]any{
		"model":    "jevonian/frontend",
		"messages": []any{map[string]any{"role": "user", "content": "style the button"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if d.Phase != "frontend" || d.Model != "frontend-model" {
		t.Fatalf("phase=%q model=%q", d.Phase, d.Model)
	}
	if d.Reason != "explicit:frontend" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// ---- provider allow-list (src/routing.test.ts) ----------------------------

func TestApplyProviderPreference(t *testing.T) {
	picks := []TierPick{
		{Model: "m", Provider: "a"},
		{Model: "m", Provider: "b"},
		{Model: "m", Provider: "c"},
	}
	providers := func(out []TierPick) []string {
		var names []string
		for _, p := range out {
			names = append(names, p.Provider)
		}
		return names
	}
	if got := providers(ApplyProviderPreference(picks, nil, false)); !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("no list = %v", got)
	}
	if got := providers(ApplyProviderPreference(picks, []string{"c", "a"}, true)); !equalStrings(got, []string{"c", "a"}) {
		t.Fatalf("ordered = %v", got)
	}
	if got := providers(ApplyProviderPreference(picks, []string{"b"}, true)); !equalStrings(got, []string{"b"}) {
		t.Fatalf("single = %v", got)
	}
	if got := providers(ApplyProviderPreference(picks, []string{}, true)); len(got) != 0 {
		t.Fatalf("empty list should withhold, got %v", got)
	}
	if got := providers(ApplyProviderPreference(picks, []string{"gone", "a"}, true)); !equalStrings(got, []string{"a"}) {
		t.Fatalf("gone names = %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRoutingCandidatesProviderList(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "alpha"
	cfg.Providers = []config.Provider{
		provider("alpha", "shared-model"),
		provider("beta", "shared-model"),
	}
	entry := config.RoutingEntry{
		ID: "plan", Models: []string{"shared-model"},
		Providers: map[string][]string{"shared-model": {"beta", "alpha"}},
	}
	picks := RoutingCandidates(cfg, Deps{}, entry, KindOpenAI)
	if len(picks) != 2 || picks[0].Provider != "beta" || picks[1].Provider != "alpha" {
		t.Fatalf("picks = %+v", picks)
	}
}

func TestRoutingCandidatesDropsProvider(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{
		provider("alpha", "shared-model"),
		provider("beta", "shared-model"),
	}
	entry := config.RoutingEntry{
		ID: "plan", Models: []string{"shared-model"},
		Providers: map[string][]string{"shared-model": {"alpha"}},
	}
	picks := RoutingCandidates(cfg, Deps{}, entry, KindOpenAI)
	if len(picks) != 1 || picks[0].Provider != "alpha" {
		t.Fatalf("picks = %+v", picks)
	}
}

func TestRoutingCandidatesWithholdsWhenAllRemoved(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("alpha", "shared-model")}
	entry := config.RoutingEntry{
		ID: "plan", Models: []string{"shared-model"},
		Providers: map[string][]string{"shared-model": {}},
	}
	picks := RoutingCandidates(cfg, Deps{}, entry, KindOpenAI)
	if len(picks) != 0 {
		t.Fatalf("picks = %+v", picks)
	}
}

func TestDecideBrainServesPreferredProvider(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "alpha"
	cfg.Providers = []config.Provider{
		provider("alpha", "shared-model"),
		provider("beta", "shared-model"),
	}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY", TimeoutMs: 1000},
	}
	for i := range cfg.Routing.Routings {
		cfg.Routing.Routings[i].Models = []string{"shared-model"}
	}
	// Plan's allow-list puts beta first.
	for i := range cfg.Routing.Routings {
		if cfg.Routing.Routings[i].ID == "plan" {
			cfg.Routing.Routings[i].Providers = map[string][]string{
				"shared-model": {"beta", "alpha"},
			}
		}
	}
	var brainModels []string
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		for _, raw := range asArray(state["routings"]) {
			r := asRecord(raw)
			if r["id"] == "plan" {
				for _, m := range asArray(r["models"]) {
					brainModels = append(brainModels, asRecord(m)["provider"].(string))
				}
			}
		}
		return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.95}}
	})
	input := baseInput(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "design the API"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if d.Provider != "beta" || d.Model != "shared-model" {
		t.Fatalf("provider=%q model=%q", d.Provider, d.Model)
	}
	if !equalStrings(brainModels, []string{"beta", "alpha"}) {
		t.Fatalf("brain models = %v", brainModels)
	}
}

func TestDecideExplicitRoutingHonoursProviderList(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "alpha"
	cfg.Providers = []config.Provider{
		provider("alpha", "shared-model"),
		provider("beta", "shared-model"),
	}
	for i := range cfg.Routing.Routings {
		cfg.Routing.Routings[i].Models = []string{"shared-model"}
		if cfg.Routing.Routings[i].ID == "execute" {
			cfg.Routing.Routings[i].Providers = map[string][]string{
				"shared-model": {"beta"},
			}
		}
	}
	input := baseInput(map[string]any{
		"model":    "jevonian/execute",
		"messages": []any{map[string]any{"role": "user", "content": "implement it"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if d.Provider != "beta" {
		t.Fatalf("provider = %q", d.Provider)
	}
	if d.Reason != "explicit:execute" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// ---- cache-aware routing (src/routing.test.ts) ----------------------------

func TestDecideCacheAffinityObserved(t *testing.T) {
	store := NewSessionStore(600_000)
	now := int64(1_000_000)
	input := baseInput(planBody("auto"), store, map[string]string{"x-session-id": "cache-test"})
	input.Now = now
	deps := Deps{
		Scorer: brainStub(),
		Prices: func(_, _ string) *Price {
			return &Price{Input: 1, Output: 1, CacheRead: 0.1, HasCacheRd: true}
		},
	}
	first := route(t, deps, input)
	store.ObserveCache(first.Session, CacheObservation{
		Provider:            first.Provider,
		Model:               first.Model,
		At:                  now,
		UncachedInputTokens: 100,
		CacheReadTokens:     900,
		CacheWriteTokens:    0,
		Success:             true,
		UsageKnown:          true,
	})
	next := route(t, deps, Input{
		Config: input.Config, Body: planBody("auto"), Headers: map[string]string{"x-session-id": "cache-test"},
		Store: store, Kind: KindOpenAI, Now: now + 1_000,
	})
	if next.Cache == nil {
		t.Fatal("expected cache evidence")
	}
	if next.Cache.State != CacheHot {
		t.Fatalf("state = %q", next.Cache.State)
	}
	if next.Cache.PrefixMatch != "unknown" {
		t.Fatalf("prefixMatch = %q", next.Cache.PrefixMatch)
	}
	if next.Cache.ObservedHitRatio < 0.89 || next.Cache.ObservedHitRatio > 0.91 {
		t.Fatalf("hitRatio = %v", next.Cache.ObservedHitRatio)
	}
	if next.Cache.ExpectedReadTokens <= 0 {
		t.Fatalf("expectedRead = %d", next.Cache.ExpectedReadTokens)
	}
	stale := route(t, deps, Input{
		Config: input.Config, Body: planBody("auto"), Headers: map[string]string{"x-session-id": "cache-test"},
		Store: store, Kind: KindOpenAI, Now: now + 300_000,
	})
	if stale.Cache == nil || stale.Cache.State != CacheStale {
		t.Fatalf("stale state = %+v", stale.Cache)
	}
	if stale.Cache.ExpectedReadTokens != 0 {
		t.Fatalf("stale expectedRead = %d", stale.Cache.ExpectedReadTokens)
	}
}

func TestSessionStoreCacheObservationIsolation(t *testing.T) {
	store := NewSessionStore(60_000)
	store.Set("s", SessionState{Phase: "plan", Provider: "p", Model: "m", Turns: 1, UpdatedAt: 100})
	obs := CacheObservation{
		Provider: "p", Model: "m", At: 100,
		UncachedInputTokens: 10, CacheReadTokens: 90, Success: true, UsageKnown: true,
	}
	store.ObserveCache("s", obs)
	// Older observation does not overwrite.
	older := obs
	older.At = 99
	older.CacheReadTokens = 0
	store.ObserveCache("s", older)
	// Another provider's observation does not land.
	other := obs
	other.Provider = "other"
	other.At = 101
	store.ObserveCache("s", other)
	// Failed usage does not establish a reusable prefix.
	failed := obs
	failed.Success = false
	failed.At = 102
	store.ObserveCache("s", failed)
	state, ok := store.Get("s", 103)
	if !ok {
		t.Fatal("session missing")
	}
	if state.Cache == nil || state.Cache.CacheReadTokens != 90 || state.Cache.At != 100 {
		t.Fatalf("cache = %+v", state.Cache)
	}
	if _, ok := store.Get("s", 60_101); ok {
		t.Fatal("session should be expired")
	}
}

// ---- orderByQuotaReset (src/routing-reset.test.ts) -------------------------

func TestOrderByQuotaResetSoonestFirst(t *testing.T) {
	// `sooner` renews in 1 day, `later` in 6 — the allowance that renews
	// soonest is used first.
	q := &quotaStub{byProvider: map[string]QuotaView{
		"later":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{6 * 86_400_000}},
		"sooner": {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{1 * 86_400_000}},
	}}
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("later", "glm-5.2"), provider("sooner", "glm-5.2")}
	picks := []TierPick{
		{Provider: "later", Model: "glm-5.2"},
		{Provider: "sooner", Model: "glm-5.2"},
	}
	out := OrderByQuotaReset(picks, cfg, Deps{Quota: q}, orderOptions{now: 1_000})
	if out[0].Provider != "sooner" || out[1].Provider != "later" {
		t.Fatalf("order = %+v", out)
	}
}

func TestOrderByQuotaResetSameHourKeepsOrder(t *testing.T) {
	// Ten minutes apart: too close to matter; reordering would break a warm cache.
	base := int64(2 * 86_400_000)
	q := &quotaStub{byProvider: map[string]QuotaView{
		"first":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{base}},
		"second": {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{base + 600_000}},
	}}
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("first", "glm-5.2"), provider("second", "glm-5.2")}
	picks := []TierPick{
		{Provider: "first", Model: "glm-5.2"},
		{Provider: "second", Model: "glm-5.2"},
	}
	out := OrderByQuotaReset(picks, cfg, Deps{Quota: q}, orderOptions{now: 1_000})
	if out[0].Provider != "first" || out[1].Provider != "second" {
		t.Fatalf("order = %+v", out)
	}
}

func TestOrderByQuotaResetLongestWindowDecides(t *testing.T) {
	// The 5h window is the inverse of the 7d one — only the week can break the tie.
	hour := int64(3_600_000)
	day := 24 * hour
	q := &quotaStub{byProvider: map[string]QuotaView{
		"week-later":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{6 * day, 1 * hour}},
		"week-sooner": {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{1 * day, 4 * hour}},
	}}
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("week-later", "glm-5.2"), provider("week-sooner", "glm-5.2")}
	picks := []TierPick{
		{Provider: "week-later", Model: "glm-5.2"},
		{Provider: "week-sooner", Model: "glm-5.2"},
	}
	out := OrderByQuotaReset(picks, cfg, Deps{Quota: q}, orderOptions{now: 1_000})
	if out[0].Provider != "week-sooner" || out[1].Provider != "week-later" {
		t.Fatalf("order = %+v", out)
	}
}

func TestOrderByQuotaResetSilentWindowBehind(t *testing.T) {
	q := &quotaStub{byProvider: map[string]QuotaView{
		"silent": {Status: quota.StatusOK, UsedPercent: 5},
		"known":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{3 * 86_400_000}},
	}}
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("silent", "glm-5.2"), provider("known", "glm-5.2")}
	picks := []TierPick{
		{Provider: "silent", Model: "glm-5.2"},
		{Provider: "known", Model: "glm-5.2"},
	}
	out := OrderByQuotaReset(picks, cfg, Deps{Quota: q}, orderOptions{now: 1_000})
	if out[0].Provider != "known" || out[1].Provider != "silent" {
		t.Fatalf("order = %+v", out)
	}
}

func TestOrderByQuotaResetRoomLowSpent(t *testing.T) {
	q := &quotaStub{byProvider: map[string]QuotaView{
		"spent": {Status: quota.StatusExhausted, UsedPercent: 100, Renews: []int64{1 * 86_400_000}},
		"low":   {Status: quota.StatusLow, UsedPercent: 95, Renews: []int64{4 * 86_400_000}},
		"fine":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{6 * 86_400_000}},
	}}
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{
		provider("spent", "glm-5.2"), provider("low", "glm-5.2"), provider("fine", "glm-5.2"),
	}
	picks := []TierPick{
		{Provider: "spent", Model: "glm-5.2"},
		{Provider: "low", Model: "glm-5.2"},
		{Provider: "fine", Model: "glm-5.2"},
	}
	out := OrderByQuotaReset(picks, cfg, Deps{Quota: q}, orderOptions{now: 1_000})
	if out[0].Provider != "fine" || out[1].Provider != "low" || out[2].Provider != "spent" {
		t.Fatalf("order = %+v", out)
	}
}

func TestOrderByQuotaResetSingleCandidateUntouched(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("only", "glm-5.2")}
	picks := []TierPick{{Provider: "only", Model: "glm-5.2"}}
	out := OrderByQuotaReset(picks, cfg, Deps{}, orderOptions{now: 1_000})
	if len(out) != 1 || out[0].Provider != "only" {
		t.Fatalf("out = %+v", out)
	}
}

// ---- decideRoute reset-aware pinning (src/routing-reset.test.ts) ----------

func TestDecidePinnedResetAware(t *testing.T) {
	// Two providers serve one model; `sooner` refills tomorrow, `later` in six
	// days. Pinned `glm-5.2` lands on the provider whose allowance renews first.
	q := &quotaStub{byProvider: map[string]QuotaView{
		"later":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{6 * 86_400_000}},
		"sooner": {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{1 * 86_400_000}},
	}}
	cfg := defaultCfg()
	cfg.DefaultProvider = "later"
	cfg.Providers = []config.Provider{provider("later", "glm-5.2"), provider("sooner", "glm-5.2")}
	input := baseInput(map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, NewSessionStore(720_000), nil)
	input.Config = cfg
	d := route(t, priceDeps(func(d *Deps) { d.Quota = q }), input)
	if d.Provider != "sooner" {
		t.Fatalf("provider = %q", d.Provider)
	}
}

func TestDecidePinnedResetAwareOff(t *testing.T) {
	q := &quotaStub{byProvider: map[string]QuotaView{
		"later":  {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{6 * 86_400_000}},
		"sooner": {Status: quota.StatusOK, UsedPercent: 5, Renews: []int64{1 * 86_400_000}},
	}}
	cfg := defaultCfg()
	cfg.DefaultProvider = "later"
	cfg.Providers = []config.Provider{provider("later", "glm-5.2"), provider("sooner", "glm-5.2")}
	cfg.Routing.QuotaGuard.ResetAware = false
	input := baseInput(map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, NewSessionStore(720_000), nil)
	input.Config = cfg
	d := route(t, priceDeps(func(d *Deps) { d.Quota = q }), input)
	if d.Provider != "later" {
		t.Fatalf("provider = %q", d.Provider)
	}
}

// ---- partitionByCapability (src/capability-routing.test.ts) ----------------

func TestPartitionByCapabilityContext(t *testing.T) {
	candidates := []CapableCandidate{
		{TierPick: TierPick{Model: "small-model", Provider: "sub"}, Capabilities: ModelCapabilities{ContextWindow: 8_000}},
		{TierPick: TierPick{Model: "big-model", Provider: "sub"}, Capabilities: ModelCapabilities{ContextWindow: 200_000}},
	}
	usable, skipped := PartitionByCapability(candidates, 50_000, "", true)
	if len(usable) != 1 || usable[0].Model != "big-model" {
		t.Fatalf("usable = %+v", usable)
	}
	if len(skipped) != 1 || skipped[0].Model != "small-model" || skipped[0].Reason != "context" {
		t.Fatalf("skipped = %+v", skipped)
	}
	if !strings.Contains(skipped[0].Detail, "exceeds the 8000 window") {
		t.Fatalf("detail = %q", skipped[0].Detail)
	}
}

func TestPartitionByCapabilityUnknownWindow(t *testing.T) {
	candidates := []CapableCandidate{
		{TierPick: TierPick{Model: "mystery", Provider: "sub"}, Capabilities: ModelCapabilities{}},
	}
	usable, skipped := PartitionByCapability(candidates, 900_000, "", true)
	if len(usable) != 1 || len(skipped) != 0 {
		t.Fatalf("usable=%+v skipped=%+v", usable, skipped)
	}
}

func TestPartitionByCapabilityHeadroom(t *testing.T) {
	candidates := []CapableCandidate{
		{TierPick: TierPick{Model: "m", Provider: "sub"}, Capabilities: ModelCapabilities{ContextWindow: 10_000}},
	}
	if usable, _ := PartitionByCapability(candidates, 9_500, "", true); len(usable) != 0 {
		t.Fatal("9.5k should not fit a 10k window")
	}
	if usable, _ := PartitionByCapability(candidates, 8_000, "", true); len(usable) != 1 {
		t.Fatal("8k should fit a 10k window")
	}
}

func TestPartitionByCapabilityEffortFloor(t *testing.T) {
	candidates := []CapableCandidate{
		{TierPick: TierPick{Model: "shallow", Provider: "sub"}, Capabilities: ModelCapabilities{Efforts: []string{"low", "medium"}}},
		{TierPick: TierPick{Model: "deep", Provider: "sub"}, Capabilities: ModelCapabilities{Efforts: []string{"low", "high"}}},
	}
	usable, skipped := PartitionByCapability(candidates, 100, "high", true)
	if len(usable) != 1 || usable[0].Model != "deep" {
		t.Fatalf("usable = %+v", usable)
	}
	if len(skipped) != 1 || skipped[0].Reason != "effort" {
		t.Fatalf("skipped = %+v", skipped)
	}
	if skipped[0].Detail != `supports up to "medium", needs "high"` {
		t.Fatalf("detail = %q", skipped[0].Detail)
	}
}

func TestPartitionByCapabilityNoFloor(t *testing.T) {
	candidates := []CapableCandidate{
		{TierPick: TierPick{Model: "shallow", Provider: "sub"}, Capabilities: ModelCapabilities{Efforts: []string{"low"}}},
	}
	usable, _ := PartitionByCapability(candidates, 100, "high", false)
	if len(usable) != 1 {
		t.Fatalf("usable = %+v", usable)
	}
}

// ---- clampEffort (src/capability-routing.test.ts) --------------------------

func TestClampEffort(t *testing.T) {
	if ClampEffort("high", []string{"low", "high"}) != "high" {
		t.Fatal("keeps supported level")
	}
	if ClampEffort("high", []string{"low", "max"}) != "max" {
		t.Fatal("nearest deeper level")
	}
	if ClampEffort("max", []string{"low", "medium"}) != "medium" {
		t.Fatal("clamps down to deepest available")
	}
	if ClampEffort("", []string{"low", "medium", "high"}) != "medium" {
		t.Fatal("middle when nothing asked")
	}
	if ClampEffort("high", nil) != "high" {
		t.Fatal("passthrough when no levels stated")
	}
	if ClampEffort("high", []string{}) != "high" {
		t.Fatal("passthrough on empty levels")
	}
}

// ---- estimator (src/capability-routing.test.ts) ----------------------------

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("hello world") != 2 {
		t.Fatalf("hello world = %d", EstimateTokens("hello world"))
	}
	if EstimateTokens(`{"a":1}`) < 3 {
		t.Fatalf(`{"a":1} = %d`, EstimateTokens(`{"a":1}`))
	}
}

// ---- routing with capability constraints (src/capability-routing.test.ts) --

func capabilityConfig(capacities map[string]config.ModelCapacityConfig) *config.Config {
	cfg := defaultCfg()
	cfg.DefaultProvider = "sub"
	cfg.Providers = []config.Provider{
		{
			Name: "sub", Type: config.ProviderTypeOpenAI,
			BaseURL: "http://127.0.0.1:1/v1", APIKey: "test",
			Billing: config.BillingSubscription,
			Models:  []config.ModelEntry{{ID: "small-model"}, {ID: "big-model"}},
		},
	}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY"},
	}
	// src/capability-routing.test.ts declares both tiers pointing at both
	// models; capability partitioning is what narrows them.
	for i := range cfg.Routing.Routings {
		cfg.Routing.Routings[i].Models = []string{"small-model", "big-model"}
	}
	cfg.Routing.Capacities = capacities
	return cfg
}

// picksFirstRouting mirrors the capability test's brain: picks the first
// offered routing and asks for "max" effort.
func picksFirstRouting() Scorer {
	return ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
		choice := "none_of_the_above"
		for _, raw := range asArray(state["routings"]) {
			if id, ok := asRecord(raw)["id"].(string); ok {
				choice = id
				break
			}
		}
		return AskResult{Choice: &Choice{
			Model: choice, Confidence: 0.9, Effort: "max",
		}}
	})
}

func TestDecideContextSkipReported(t *testing.T) {
	cfg := capabilityConfig(map[string]config.ModelCapacityConfig{
		"small-model": {ContextWindow: intPtr(1_000)},
	})
	body := map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("summarise ", 4_000)}},
	}
	input := baseInput(body, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = picksFirstRouting() }), input)
	if d.Model != "big-model" {
		t.Fatalf("model = %q", d.Model)
	}
	if !strings.Contains(d.Reason, "context-skip") {
		t.Fatalf("reason = %q", d.Reason)
	}
	found := false
	for _, s := range d.Skipped {
		if s.Model == "small-model" && s.Reason == "context" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skipped = %+v", d.Skipped)
	}
}

func TestDecideOffersAllWhenNoneFits(t *testing.T) {
	cfg := capabilityConfig(map[string]config.ModelCapacityConfig{
		"small-model": {ContextWindow: intPtr(100)},
		"big-model":   {ContextWindow: intPtr(100)},
	})
	body := map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("go ", 2_000)}},
	}
	input := baseInput(body, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = picksFirstRouting() }), input)
	if d.Model != "small-model" {
		t.Fatalf("model = %q", d.Model)
	}
	if len(d.Skipped) != 2 {
		t.Fatalf("skipped = %+v", d.Skipped)
	}
}

func TestDecideEffortFloorFromHeader(t *testing.T) {
	cfg := capabilityConfig(map[string]config.ModelCapacityConfig{
		"small-model": {Efforts: []string{"low"}},
		"big-model":   {Efforts: []string{"low", "high"}},
	})
	body := map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "plan the migration"}},
	}
	input := baseInput(body, nil, map[string]string{"x-jevonian-effort": "high"})
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = picksFirstRouting() }), input)
	if d.Model != "big-model" {
		t.Fatalf("model = %q", d.Model)
	}
	if !strings.Contains(d.Reason, "effort-skip") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideEffortClamped(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "sub"
	cfg.Providers = []config.Provider{
		{
			Name: "sub", Type: config.ProviderTypeOpenAI,
			BaseURL: "http://127.0.0.1:1/v1", APIKey: "test",
			Billing: config.BillingSubscription,
			Models:  []config.ModelEntry{{ID: "small-model"}, {ID: "big-model"}},
		},
	}
	// plan → big-model (efforts capped at medium), execute → small-model.
	for i := range cfg.Routing.Routings {
		switch cfg.Routing.Routings[i].ID {
		case "plan":
			cfg.Routing.Routings[i].Models = []string{"big-model"}
		default:
			cfg.Routing.Routings[i].Models = []string{"small-model"}
		}
	}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY"},
	}
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{
		"big-model": {Efforts: []string{"low", "medium"}},
	}
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.9, Effort: "max"}}
	})
	input := baseInput(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if d.Model != "big-model" {
		t.Fatalf("model = %q", d.Model)
	}
	if d.Effort != "medium" {
		t.Fatalf("effort = %q", d.Effort)
	}
	if d.EffortNote != `clamped "max" to "medium"` {
		t.Fatalf("effortNote = %q", d.EffortNote)
	}
	if !strings.Contains(d.Reason, "effort-clamped") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestDecideEffortUndefinedWhenBrainNotAsked(t *testing.T) {
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.9}}
	})
	cfg := capabilityConfig(nil)
	// brainPicksEffort defaults true in DefaultRouting; the question is asked
	// but the scorer omits an effort answer, so effort stays unset.
	_ = cfg
	input := baseInput(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if d.Effort != "" {
		t.Fatalf("effort = %q", d.Effort)
	}
	if d.EffortNote != "" {
		t.Fatalf("effortNote = %q", d.EffortNote)
	}
}

// ---- explicit routing carries the pinned tier's effort ---------------------

func pinnedEffortConfig(effort string, capacities map[string]config.ModelCapacityConfig) *config.Config {
	cfg := defaultCfg()
	cfg.DefaultProvider = "sub"
	cfg.Providers = []config.Provider{
		{
			Name: "sub", Type: config.ProviderTypeOpenAI,
			BaseURL: "http://127.0.0.1:1/v1", APIKey: "test",
			Models: []config.ModelEntry{{ID: "big-model"}, {ID: "small-model"}},
		},
	}
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY"},
	}
	cfg.Routing.Routings = []config.RoutingEntry{
		{ID: "execute", Label: "Execute", Models: []string{"big-model", "small-model"}, Effort: effort},
	}
	cfg.Routing.Capacities = capacities
	return cfg
}

func TestDecideExplicitTierEffort(t *testing.T) {
	cfg := pinnedEffortConfig("high", map[string]config.ModelCapacityConfig{
		"big-model": {Efforts: []string{"low", "medium", "high"}},
	})
	input := baseInput(map[string]any{
		"model":    "jevonian/execute",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if d.Phase != "execute" {
		t.Fatalf("phase = %q", d.Phase)
	}
	if d.Effort != "high" {
		t.Fatalf("effort = %q", d.Effort)
	}
	if d.EffortNote != `tier set "high"` {
		t.Fatalf("effortNote = %q", d.EffortNote)
	}
}

func TestDecideExplicitTierEffortClamped(t *testing.T) {
	cfg := pinnedEffortConfig("high", map[string]config.ModelCapacityConfig{
		"big-model": {Efforts: []string{"low", "medium"}},
	})
	input := baseInput(map[string]any{
		"model":    "jevonian/execute",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if d.Effort != "medium" {
		t.Fatalf("effort = %q", d.Effort)
	}
	if d.EffortNote != `clamped "high" to "medium"` {
		t.Fatalf("effortNote = %q", d.EffortNote)
	}
}

func TestDecideExplicitTierEffortBeatsHeader(t *testing.T) {
	cfg := pinnedEffortConfig("high", map[string]config.ModelCapacityConfig{
		"big-model": {Efforts: []string{"low", "medium", "high"}},
	})
	input := baseInput(map[string]any{
		"model":    "jevonian/execute",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, map[string]string{"x-jevonian-effort": "low"})
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(), input)
	if d.Effort != "high" {
		t.Fatalf("effort = %q", d.Effort)
	}
}

// ---- brainPicksEffort off (src/capability-routing.test.ts) -----------------

func TestDecideBrainPicksEffortOff(t *testing.T) {
	var modelOnly []bool
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, mo bool) AskResult {
		modelOnly = append(modelOnly, mo)
		choice := "none_of_the_above"
		for _, raw := range asArray(state["routings"]) {
			if id, ok := asRecord(raw)["id"].(string); ok {
				choice = id
				break
			}
		}
		return AskResult{Choice: &Choice{Model: choice, Confidence: 0.9}}
	})
	cfg := defaultCfg()
	cfg.DefaultProvider = "sub"
	cfg.Providers = []config.Provider{
		{
			Name: "sub", Type: config.ProviderTypeOpenAI,
			BaseURL: "http://127.0.0.1:1/v1", APIKey: "test",
			Models: []config.ModelEntry{{ID: "small-model"}},
		},
	}
	cfg.Routing.BrainPicksEffort = false
	cfg.Routing.DefaultEffort = "low"
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY"},
	}
	input := baseInput(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if len(modelOnly) == 0 || !modelOnly[0] {
		t.Fatalf("modelOnly should be true, got %v", modelOnly)
	}
	if d.Effort != "low" {
		t.Fatalf("effort = %q", d.Effort)
	}
}

func TestDecideIgnoresBrainEffortWhenNotAsked(t *testing.T) {
	scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) AskResult {
		return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.9, Effort: "max"}}
	})
	cfg := defaultCfg()
	cfg.DefaultProvider = "sub"
	cfg.Providers = []config.Provider{
		{
			Name: "sub", Type: config.ProviderTypeOpenAI,
			BaseURL: "http://127.0.0.1:1/v1", APIKey: "test",
			Models: []config.ModelEntry{{ID: "small-model"}},
		},
	}
	cfg.Routing.BrainPicksEffort = false
	cfg.Routing.DefaultEffort = "medium"
	cfg.Routing.Brains = []config.BrainConfig{
		{Channel: "typesafe", APIKeyEnv: "TYPESAFE_API_KEY"},
	}
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{
		"small-model": {Efforts: []string{"low", "medium"}},
	}
	input := baseInput(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, nil, nil)
	input.Config = cfg
	input.Now = 1_000
	d := route(t, priceDeps(func(d *Deps) { d.Scorer = scorer }), input)
	if d.Effort != "medium" {
		t.Fatalf("effort = %q", d.Effort)
	}
}

func intPtr(v int) *int { return &v }

// ---- failover plan + reason chain (seam for internal/server) ---------------

func TestNextFromPlanWalksOrder(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultProvider = "a"
	cfg.Providers = []config.Provider{
		provider("a", "glm-5.2"), provider("b", "glm-5.2"), provider("c", "glm-5.2"),
	}
	cfg.Routing.QuotaGuard.ResetAware = false
	input := baseInput(map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil, nil)
	input.Config = cfg
	d := route(t, priceDeps(), input)
	if d.Provider != "a" {
		t.Fatalf("provider = %q", d.Provider)
	}
	// Rank 0 is the chosen target; the caller seeds exclude with it.
	exclude := map[string]bool{PlanKey(d.Provider, d.Model): true}
	next := d.NextFromPlan(exclude)
	if next == nil || next.Provider != "b" {
		t.Fatalf("next = %+v", next)
	}
	// The re-pointed decision keeps phase/session/reason; only target moves.
	if next.Session != d.Session || next.Reason != d.Reason {
		t.Fatal("next should keep reporting fields")
	}
	exclude[PlanKey(next.Provider, next.Model)] = true
	last := next.NextFromPlan(exclude)
	if last == nil || last.Provider != "c" {
		t.Fatalf("last = %+v", last)
	}
	exclude[PlanKey(last.Provider, last.Model)] = true
	if last.NextFromPlan(exclude) != nil {
		t.Fatal("plan should be exhausted")
	}
}

func TestReasonChain(t *testing.T) {
	r := AppendReason(ReasonBrain("execute"), SuffixBrainLowConfidence)
	r = withCanonical(r, "openrouter/glm:free")
	r = AppendReason(r, SuffixCacheHot)
	r = WithQuotaFailover(r)
	if r != "brain:execute:brain-low-confidence:canonical:openrouter/glm:free:cache-hot:quota-failover" {
		t.Fatalf("reason = %q", r)
	}
	parts := ParseReason(r)
	if parts.Base != "brain" || parts.Routing != "execute" {
		t.Fatalf("parts = %+v", parts)
	}
	for _, want := range []string{SuffixBrainLowConfidence, "canonical:openrouter/glm:free", SuffixCacheHot, SuffixQuotaFailover} {
		if !parts.Has(want) {
			t.Fatalf("missing %q in %+v", want, parts.Suffixes)
		}
	}
	if ReasonBrain("") != "brain:unset" {
		t.Fatal("unset verdict")
	}
	if p := ParseReason(ReasonPinnedModel); p.Base != "pinned-model" || p.Routing != "" {
		t.Fatalf("pinned = %+v", p)
	}
	if p := ParseReason(WithContextRetry(ReasonExplicit("plan"))); p.Routing != "plan" || !p.Has(SuffixContextRetry) {
		t.Fatalf("explicit = %+v", p)
	}
}
