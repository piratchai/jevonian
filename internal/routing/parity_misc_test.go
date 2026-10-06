package routing

import (
	"encoding/json"
	"github.com/xinyao27/jevonian/internal/config"
	"os"
	"reflect"
	"strings"
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
//
// Deliberate divergence from the TS port: the golden's phase, hasToolResults
// and withinTurn count tool results from the whole conversation. Go scopes
// them to the active turn — a fresh user message after a tool turn reopens
// the plan phase. Failure streaks and the recent-results window stay
// conversation-wide, so those fields must still match the golden exactly.
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
		want := PhaseSignals{ConsecutiveFailures: c.Want.ConsecutiveFailures,
			HasTools: c.Want.HasTools, RecentToolResults: c.Want.RecentToolResults}
		if len(got.RecentToolResults) == 0 {
			got.RecentToolResults = []string{}
		}
		if len(want.RecentToolResults) == 0 {
			want.RecentToolResults = []string{}
		}
		shared := PhaseSignals{ConsecutiveFailures: got.ConsecutiveFailures,
			HasTools: got.HasTools, RecentToolResults: got.RecentToolResults}
		if !reflect.DeepEqual(shared, want) {
			if bad++; bad <= 5 {
				t.Errorf("case %d (%s): global signals %+v want %+v", i, c.Kind, shared, want)
			}
			continue
		}
		// Turn-scoped fields: execute only when tool results follow the
		// latest user message. When the golden agrees, this matches the TS
		// port; where it disagrees, the new boundary is the fix.
		active := activeToolResultsAfterLatestUser(c.Body, RequestKind(c.Kind))
		wantPhase := "plan"
		if len(active) > 0 {
			wantPhase = "execute"
		}
		if got.Phase != wantPhase || got.WithinTurn != (len(active) > 0) || got.HasToolResults != (len(active) > 0) {
			if bad++; bad <= 5 {
				t.Errorf("case %d (%s): turn scope got phase=%s withinTurn=%v hasToolResults=%v, active=%d",
					i, c.Kind, got.Phase, got.WithinTurn, got.HasToolResults, len(active))
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d phase cases differ", bad, len(cases))
	}
}

// activeToolResultsAfterLatestUser replicates the turn boundary rule for the
// golden bodies: tool results count only when they come after the latest
// user-authored message (an Anthropic user message needs a non-empty text
// block; tool_result-only user messages carry the turn, not the voice).
func activeToolResultsAfterLatestUser(body map[string]any, kind RequestKind) []string {
	messages := asArray(body["messages"])
	latest := -1
	for i, raw := range messages {
		message := asRecord(raw)
		if message["role"] != "user" {
			continue
		}
		if kind == KindAnthropic {
			for _, rawBlock := range asArray(message["content"]) {
				block := asRecord(rawBlock)
				if block["type"] == "text" && strings.TrimSpace(stringField(block, "text")) != "" {
					latest = i
					break
				}
			}
			continue
		}
		latest = i
	}
	input := asArray(body["input"])
	if kind == KindResponses {
		latest = -1
		for i, raw := range input {
			item := asRecord(raw)
			if item["type"] == "message" && item["role"] == "user" {
				latest = i
			}
		}
		out := []string{}
		for i, raw := range input {
			item := asRecord(raw)
			if i > latest && item["type"] == "function_call_output" {
				out = append(out, stringifyContent(item["output"]))
			}
		}
		return out
	}
	out := []string{}
	for i := latest + 1; i < len(messages); i++ {
		message := asRecord(messages[i])
		if kind == KindOpenAI && message["role"] == "tool" {
			out = append(out, stringifyContent(message["content"]))
		}
		if kind == KindAnthropic {
			for _, rawBlock := range asArray(message["content"]) {
				block := asRecord(rawBlock)
				if block["type"] == "tool_result" {
					out = append(out, stringifyContent(block["content"]))
				}
			}
		}
	}
	return out
}
