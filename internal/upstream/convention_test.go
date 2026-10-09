package upstream

import (
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestExclusiveInputForServingWires(t *testing.T) {
	cases := []struct {
		name    string
		typ     config.ProviderType
		model   string
		wantExc bool
	}{
		// Inclusive wires: prompt_tokens already contains the cache reads.
		{"openai chat", config.ProviderTypeOpenAI, "gpt-4o", false},
		{"responses", config.ProviderTypeResponses, "gpt-6.1-sol", false},
		{"gemini fold", config.ProviderTypeGemini, "gemini-3.8-flash-tiered", false},
		// Exclusive wires: prompt_tokens is already the uncached part.
		{"anthropic messages", config.ProviderTypeAnthropic, "claude-opus-5-5", true},
		{"devin connect-rpc", config.ProviderTypeDevin, "swe-2-max", true},
		{"cursor connect-rpc", config.ProviderTypeCursor, "cursor-fast", true},
		{"chatgpt-web", config.ProviderTypeChatGPTWeb, "gpt-6-pro", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExclusiveInputFor(config.Provider{Name: "p", Type: tc.typ}, tc.model)
			if !ok {
				t.Fatalf("ExclusiveInputFor(%s, %s) not ok", tc.typ, tc.model)
			}
			if got != tc.wantExc {
				t.Fatalf("ExclusiveInputFor(%s, %s) = %v, want %v", tc.typ, tc.model, got, tc.wantExc)
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
	cases := []struct {
		provider, model string
		wantExc, wantOK bool
	}{
		{"chatgpt-subscription", "gpt-6.1-sol", false, true},
		{"claude-subscription", "claude-opus-5-5", true, true},
		{"antigravity", "gemini-3.8-flash-tiered", false, true},
		{"devin-subscription", "swe-2-max", true, true},
		{"missing", "whatever", false, false},
	}
	for _, tc := range cases {
		got, ok := classify(tc.provider, tc.model)
		if ok != tc.wantOK || (ok && got != tc.wantExc) {
			t.Fatalf("classify(%s, %s) = (%v,%v), want (%v,%v)", tc.provider, tc.model, got, ok, tc.wantExc, tc.wantOK)
		}
	}
}

// A model the provider catalog never listed still classifies by provider type,
// so a backfill covers every row instead of silently skipping unknown models.
func TestConventionClassifierAnswersForUnknownModel(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		{Name: "devin-subscription", Type: config.ProviderTypeDevin, Models: []config.ModelEntry{{ID: "swe-2-max"}}},
	}}
	classify := ConventionClassifierFor(cfg)
	got, ok := classify("devin-subscription", "MODEL_PRIVATE_99")
	if !ok || !got {
		t.Fatalf("unknown devin model = (%v,%v), want (true,true)", got, ok)
	}
}

func TestConventionClassifierNilConfig(t *testing.T) {
	classify := ConventionClassifierFor(nil)
	if _, ok := classify("anything", "model"); ok {
		t.Fatal("nil config classified a provider")
	}
}
