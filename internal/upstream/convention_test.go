package upstream

import (
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestLedgerExclusiveFollowsServingTranslator(t *testing.T) {
	cases := []struct {
		name   string
		plan   WirePlan
		client ClientKind
		stream bool
		want   bool
	}{
		// Anthropic Messages usage is exclusive in every shape.
		{"anthropic native", WirePlan{Wire: KindAnthropic}, KindAnthropic, false, true},
		{"anthropic to chat client", WirePlan{Wire: KindAnthropic, Bridge: "to-anthropic"}, KindOpenAI, true, true},
		// A streaming Chat upstream bridged to Anthropic is re-split to uncached input.
		{"chat to anthropic stream", WirePlan{Wire: KindOpenAI, Bridge: "to-openai"}, KindAnthropic, true, true},
		// A folded non-stream reply keeps the upstream's inclusive count.
		{"chat to anthropic fold", WirePlan{Wire: KindOpenAI, Bridge: "to-openai"}, KindAnthropic, false, false},
		// Everything else is inclusive.
		{"openai native", WirePlan{Wire: KindOpenAI}, KindOpenAI, true, false},
		{"responses", WirePlan{Wire: KindResponses}, KindResponses, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LedgerExclusive(tc.plan, tc.client, tc.stream); got != tc.want {
				t.Fatalf("LedgerExclusive(%+v, %s, %v) = %v, want %v", tc.plan, tc.client, tc.stream, got, tc.want)
			}
		})
	}
}

func TestConventionClassifierSplitsWires(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		{Name: "chatgpt-subscription", Type: config.ProviderTypeResponses},
		{Name: "claude-subscription", Type: config.ProviderTypeAnthropic},
		{Name: "antigravity", Type: config.ProviderTypeGemini},
		{Name: "devin-subscription", Type: config.ProviderTypeDevin},
	}}
	classify := ConventionClassifierFor(cfg)
	after := GoEngineCutover.Add(time.Hour)
	before := GoEngineCutover.Add(-time.Hour)
	cases := []struct {
		name            string
		provider, model string
		path            string
		stream          bool
		at              time.Time
		wantExc, wantOK bool
	}{
		{"openai inclusive", "chatgpt-subscription", "gpt-6.1-sol", "/chat/completions", true, after, false, true},
		{"anthropic exclusive", "claude-subscription", "claude-opus-5-5", "/messages", true, after, true, true},
		{"gemini inclusive", "antigravity", "gemini-3.8-flash-tiered", "/chat/completions", true, after, false, true},
		// The Go egress renders an inclusive prompt count for the Connect-RPC hosts.
		{"devin Go-era inclusive", "devin-subscription", "swe-2-max", "/chat/completions", true, after, false, true},
		// The legacy runtime stored their exclusive count.
		{"devin legacy exclusive", "devin-subscription", "swe-2-max", "/chat/completions", true, before, true, true},
		{"missing", "missing", "whatever", "/chat/completions", true, after, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(tc.provider, tc.model, tc.path, tc.stream, tc.at)
			if ok != tc.wantOK || (ok && got != tc.wantExc) {
				t.Fatalf("classify(%s, %s) = (%v,%v), want (%v,%v)", tc.provider, tc.model, got, ok, tc.wantExc, tc.wantOK)
			}
		})
	}
}

// A model the provider catalog never listed still classifies by provider type
// and era, so a reconcile covers every row instead of silently skipping unknown
// models.
func TestConventionClassifierAnswersForUnknownModel(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		{Name: "devin-subscription", Type: config.ProviderTypeDevin, Models: []config.ModelEntry{{ID: "swe-2-max"}}},
	}}
	classify := ConventionClassifierFor(cfg)
	got, ok := classify("devin-subscription", "MODEL_PRIVATE_99", "/chat/completions", true, GoEngineCutover.Add(-time.Hour))
	if !ok || !got {
		t.Fatalf("unknown legacy devin model = (%v,%v), want (true,true)", got, ok)
	}
	got, ok = classify("devin-subscription", "MODEL_PRIVATE_99", "/chat/completions", true, GoEngineCutover.Add(time.Hour))
	if !ok || got {
		t.Fatalf("unknown Go-era devin model = (%v,%v), want (false,true)", got, ok)
	}
}

func TestConventionClassifierNilConfig(t *testing.T) {
	classify := ConventionClassifierFor(nil)
	if _, ok := classify("anything", "model", "/chat/completions", true, time.Now()); ok {
		t.Fatal("nil config classified a provider")
	}
}

func TestClientKindForPath(t *testing.T) {
	cases := map[string]ClientKind{
		"/messages":         KindAnthropic,
		"/responses":        KindResponses,
		"/chat/completions": KindOpenAI,
		"":                  KindOpenAI,
	}
	for path, want := range cases {
		if got := ClientKindForPath(path); got != want {
			t.Fatalf("ClientKindForPath(%q) = %v, want %v", path, got, want)
		}
	}
}
