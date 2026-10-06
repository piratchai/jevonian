package routing

import (
	"encoding/json"
	"github.com/xinyao27/jevonian/internal/config"
	"os"
	"reflect"
	"testing"
)

// Port of src/routing.test.ts "avoids experimental models when picking tiers".
func TestDeriveTiersAvoidsExperimental(t *testing.T) {
	prices := map[string]*Price{
		"p/stable-cheap": {Provider: "p", Input: 0.1, Output: 0.2},
		"p/fast-exp":     {Provider: "p", Input: 0.05, Output: 0.1},
		"p/frontier":     {Provider: "p", Input: 2, Output: 8},
	}
	cfg := defaultCfg()
	cfg.DefaultProvider = "p"
	cfg.Providers = []config.Provider{provider("p", "p/frontier", "p/fast-exp", "p/stable-cheap")}
	tiers := DeriveTiers(cfg, Deps{Prices: func(m, _ string) *Price { return prices[m] }})
	if !reflect.DeepEqual(tiers.Plan, []string{"p/frontier"}) || !reflect.DeepEqual(tiers.Execute, []string{"p/stable-cheap"}) ||
		!reflect.DeepEqual(tiers.Utility, []string{"p/fast-exp"}) {
		t.Fatalf("tiers = %+v", tiers)
	}
}

// Port of "reads Codex Responses tool calls from the input items": shell args
// are redacted before reaching the brain (raw `; curl` patterns trip the
// brain host's WAF).
func TestRecentToolCallsRedactsShellArgs(t *testing.T) {
	body := map[string]any{"model": "auto", "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "fix it"}}},
		map[string]any{"type": "function_call", "name": "shell", "arguments": `{"command":"ls -la"}`},
		map[string]any{"type": "function_call_output", "call_id": "1", "output": "wrote 3 lines"},
	}}
	got := recentToolCalls(body, KindResponses, 3)
	if !reflect.DeepEqual(got, []string{"shell(<command redacted>)"}) {
		t.Fatalf("recent_tool_calls = %v", got)
	}
}

// TestGoldenPhaseFuzz compares ClassifyPhase on 400 random conversations over
// all three wires (greetings, context envelopes, failing/ok tool results,
// tools present or not) against src/routing.ts classifyPhase.
// Regenerate with /tmp/parity/gen9.mts.
func TestGoldenPhaseFuzz(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_phase_fuzz.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Kind string
		Body map[string]any
		Want struct {
			Phase               string
			ConsecutiveFailures int
			HasToolResults      bool
			HasTools            bool
			RecentToolResults   []string
			WithinTurn          bool
		}
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for i, c := range cases {
		got := ClassifyPhase(c.Body, RequestKind(c.Kind))
		want := PhaseSignals{Phase: c.Want.Phase, ConsecutiveFailures: c.Want.ConsecutiveFailures, HasToolResults: c.Want.HasToolResults,
			HasTools: c.Want.HasTools, RecentToolResults: c.Want.RecentToolResults, WithinTurn: c.Want.WithinTurn}
		if len(got.RecentToolResults) == 0 {
			got.RecentToolResults = []string{}
		}
		if len(want.RecentToolResults) == 0 {
			want.RecentToolResults = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			if bad++; bad <= 5 {
				t.Errorf("case %d (%s): got %+v want %+v", i, c.Kind, got, want)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d phase cases differ", bad, len(cases))
	}
}
