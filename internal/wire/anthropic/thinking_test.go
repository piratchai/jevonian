package anthropic

import (
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func TestThinkingSupport(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  ThinkingSupport
	}{
		{"claude-sonnet-4-5-20250929", ThinkingSupport{Adaptive: false}},
		{"claude-opus-4-5-20251101", ThinkingSupport{Adaptive: false}},
		{"claude-haiku-4-5-20251001", ThinkingSupport{Adaptive: false}},
		{"claude-opus-4-6", ThinkingSupport{Adaptive: true, RejectsEnabled: false, RejectsDisabled: false}},
		{"claude-opus-4-7", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: false, Xhigh: true}},
		{"claude-sonnet-5", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: false, Xhigh: true}},
		{"claude-opus-5", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: false, Xhigh: true}},
		{"claude-opus-5-5", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: true, Xhigh: true}},
		{"claude-fable-5", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: true, Xhigh: true}},
		{"claude-fable-5-1", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: true, Xhigh: true}},
		{"claude-mythos-5", ThinkingSupport{Adaptive: true, RejectsEnabled: true, RejectsDisabled: true, Xhigh: true}},
		// The preview carries no version digit: MODEL_ID does not match, so it
		// keeps the legacy shape (rejectsEnabled stays false).
		{"claude-mythos-preview", ThinkingSupport{Adaptive: false, RejectsEnabled: false, RejectsDisabled: false, Xhigh: false}},
		{"gpt-5", ThinkingSupport{}},
	} {
		got := ThinkingSupportFor(tc.model)
		if got != tc.want {
			t.Errorf("ThinkingSupportFor(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

func TestRejectsAssistantPrefill(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-4-6":            true,
		"claude-sonnet-5":            true,
		"claude-opus-5-5":            true,
		"anthropic/claude-fable-5":   true,
		"claude-mythos-preview":      true,
		"claude-sonnet-4-5-20250929": false,
		"claude-haiku-4-5-20251001":  false,
		"claude-3-7-sonnet":          false,
		"gpt-5":                      false,
	} {
		if got := RejectsAssistantPrefill(model); got != want {
			t.Errorf("RejectsAssistantPrefill(%q) = %v, want %v", model, got, want)
		}
	}
	if RejectsAssistantPrefill(nil) {
		t.Error("nil model should not reject prefill")
	}
}

func TestWithEffortWireShapes(t *testing.T) {
	if got := WithEffort(wire.Body{"model": "m"}, "high", WireOpenAI, ""); got["reasoning_effort"] != "high" {
		t.Fatalf("openai = %v", got)
	}
	out := WithEffort(wire.Body{"model": "m"}, "medium", WireAnthropic, "")
	thinking := out["thinking"].(wire.Body)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8192) {
		t.Fatalf("anthropic = %v", out)
	}
	out = WithEffort(wire.Body{"model": "m"}, "none", WireAnthropic, "")
	if out["thinking"].(wire.Body)["type"] != "disabled" {
		t.Fatalf("disabled = %v", out)
	}
	out = WithEffort(wire.Body{"model": "m"}, "low", WireResponses, "")
	if out["reasoning"].(wire.Body)["effort"] != "low" {
		t.Fatalf("responses = %v", out)
	}
	out = WithEffort(wire.Body{"model": "m"}, "none", WireResponses, "")
	if v, present := out["reasoning"]; !present || v != nil {
		t.Fatalf("responses none = %v", out)
	}
	out = WithEffort(wire.Body{"model": "m"}, "none", WireOpenAI, "")
	if out["reasoning_effort"] != "none" {
		t.Fatalf("openai none = %v", out)
	}
	// Router has no level → body unchanged.
	body := wire.Body{"model": "m"}
	if got := WithEffort(body, "", WireOpenAI, ""); len(got) != 1 {
		t.Fatalf("no effort = %v", got)
	}
	// Client's own level wins: the body is returned untouched.
	client := wire.Body{"model": "m", "reasoning_effort": "max"}
	if got := WithEffort(client, "low", WireOpenAI, "max"); got["reasoning_effort"] != "max" {
		t.Fatalf("client override = %v", got)
	}
}

func TestWithEffortBudgetDepth(t *testing.T) {
	budget := func(effort string) int {
		out := WithEffort(wire.Body{}, effort, WireAnthropic, "")
		return int(wire.Number(wire.AsRecord(out["thinking"])["budget_tokens"]))
	}
	if !(budget("minimal") < budget("low") && budget("low") < budget("medium") &&
		budget("medium") < budget("high") && budget("high") < budget("max")) {
		t.Fatal("budgets not monotonic")
	}
}

func TestStripForeignEffort(t *testing.T) {
	body := wire.Body{"model": "m", "reasoning_effort": "low"}
	out := StripForeignEffort(body, WireResponses)
	if _, present := out["reasoning_effort"]; present {
		t.Fatalf("reasoning_effort not stripped: %v", out)
	}
	out = WithEffort(wire.Body{"model": "m", "reasoning_effort": "low"}, "none", WireResponses, "")
	if _, present := out["reasoning_effort"]; present {
		t.Fatalf("withEffort kept foreign field: %v", out)
	}
	if v := out["reasoning"]; v != nil {
		t.Fatalf("reasoning should be null: %v", out)
	}
	out = StripForeignEffort(wire.Body{"model": "m", "reasoning": wire.Body{"effort": "low"}}, WireOpenAI)
	if _, present := out["reasoning"]; present {
		t.Fatalf("reasoning not stripped: %v", out)
	}
	out = StripForeignEffort(wire.Body{"model": "m", "reasoning_effort": "low", "thinking": nil}, WireAnthropic)
	if _, present := out["reasoning_effort"]; present {
		t.Fatalf("reasoning_effort not stripped: %v", out)
	}
	if v, present := out["thinking"]; !present || v != nil {
		t.Fatalf("thinking (own field) must stay: %v", out)
	}
	// Own spelling is left in place.
	own := wire.Body{"model": "m", "reasoning_effort": "low"}
	if got := StripForeignEffort(own, WireOpenAI); got["reasoning_effort"] != "low" {
		t.Fatalf("own field = %v", got)
	}
}

func TestEffortInBody(t *testing.T) {
	if got := EffortInBody(wire.Body{"reasoning_effort": "high"}, WireOpenAI, ""); got != "high" {
		t.Fatalf("openai = %q", got)
	}
	if got := EffortInBody(wire.Body{"reasoning": wire.Body{"effort": "low"}}, WireResponses, ""); got != "low" {
		t.Fatalf("responses = %q", got)
	}
	if got := EffortInBody(wire.Body{"reasoning": nil}, WireResponses, ""); got != "none" {
		t.Fatalf("responses null = %q", got)
	}
	if got := EffortInBody(wire.Body{"thinking": wire.Body{"type": "disabled"}}, WireAnthropic, ""); got != "none" {
		t.Fatalf("disabled = %q", got)
	}
	if got := EffortInBody(wire.Body{"thinking": wire.Body{"type": "enabled", "budget_tokens": float64(2048)}}, WireAnthropic, ""); got != "low" {
		t.Fatalf("budget = %q", got)
	}
	// Round-trips every level through each wire.
	for _, w := range []WireKind{WireOpenAI, WireResponses, WireAnthropic} {
		for _, level := range []string{"none", "minimal", "low", "medium", "high", "max"} {
			body := WithEffort(wire.Body{}, level, w, "")
			if got := EffortInBody(body, w, level); got != level {
				t.Errorf("%s/%s: EffortInBody = %q", w, level, got)
			}
		}
	}
	// Hint disambiguates budgets that collide (max/ultra share 32768).
	ultra := WithEffort(wire.Body{}, "ultra", WireAnthropic, "")
	if got := EffortInBody(ultra, WireAnthropic, "ultra"); got != "ultra" {
		t.Fatalf("ultra hint = %q", got)
	}
	if got := EffortInBody(ultra, WireAnthropic, "max"); got != "max" {
		t.Fatalf("max hint = %q", got)
	}
	if got := EffortInBody(wire.Body{}, WireOpenAI, ""); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestClientEffortOf(t *testing.T) {
	if got := ClientEffortOf(wire.Body{"reasoning_effort": "high"}, WireOpenAI); got != "high" {
		t.Fatalf("openai = %q", got)
	}
	if got := ClientEffortOf(wire.Body{"reasoning": wire.Body{"effort": "low"}}, WireResponses); got != "low" {
		t.Fatalf("responses = %q", got)
	}
	if got := ClientEffortOf(wire.Body{"thinking": wire.Body{"budget_tokens": float64(16384)}}, WireAnthropic); got != "high" {
		t.Fatalf("budget = %q", got)
	}
	if got := ClientEffortOf(wire.Body{"thinking": wire.Body{"type": "disabled"}}, WireAnthropic); got != "none" {
		t.Fatalf("disabled = %q", got)
	}
	if got := ClientEffortOf(wire.Body{}, WireOpenAI); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestAdaptiveThinkingEffort(t *testing.T) {
	out := WithEffort(wire.Body{"model": "claude-opus-4-7"}, "medium", WireAnthropic, "")
	if out["thinking"].(wire.Body)["type"] != "adaptive" {
		t.Fatalf("thinking = %v", out["thinking"])
	}
	if out["output_config"].(wire.Body)["effort"] != "medium" {
		t.Fatalf("output_config = %v", out["output_config"])
	}
	// Opus 4.6 lacks xhigh → falls back to high rather than being rejected.
	out = WithEffort(wire.Body{"model": "claude-opus-4-6"}, "xhigh", WireAnthropic, "")
	if out["output_config"].(wire.Body)["effort"] != "high" {
		t.Fatalf("xhigh fallback = %v", out["output_config"])
	}
	out = WithEffort(wire.Body{"model": "claude-opus-4-8"}, "ultra", WireAnthropic, "")
	if out["output_config"].(wire.Body)["effort"] != "max" {
		t.Fatalf("ultra = %v", out["output_config"])
	}
	// Never send disabled to an always-on model.
	out = WithEffort(wire.Body{"model": "claude-fable-5-1"}, "none", WireAnthropic, "")
	if out["thinking"].(wire.Body)["type"] != "adaptive" {
		t.Fatalf("always-on none = %v", out["thinking"])
	}
	if out["output_config"].(wire.Body)["effort"] != "low" {
		t.Fatalf("always-on effort = %v", out["output_config"])
	}
	// Models that accept explicit off still get it.
	out = WithEffort(wire.Body{"model": "claude-sonnet-5"}, "none", WireAnthropic, "")
	if out["thinking"].(wire.Body)["type"] != "disabled" {
		t.Fatalf("sonnet-5 none = %v", out["thinking"])
	}
}

func TestNormalizeThinkingTranslates(t *testing.T) {
	// Client's disabled on an always-on model → adaptive low.
	body := wire.Body{"model": "claude-opus-5-5", "thinking": wire.Body{"type": "disabled"}}
	out := WithEffort(body, "high", WireAnthropic, ClientEffortOf(body, WireAnthropic))
	if out["thinking"].(wire.Body)["type"] != "adaptive" {
		t.Fatalf("thinking = %v", out["thinking"])
	}
	if out["output_config"].(wire.Body)["effort"] != "low" {
		t.Fatalf("output_config = %v", out["output_config"])
	}

	// Client's budget_tokens on a model without extended thinking → nearest level.
	body = wire.Body{
		"model":         "claude-opus-4-7",
		"thinking":      wire.Body{"type": "enabled", "budget_tokens": float64(10000), "display": "summarized"},
		"output_config": wire.Body{"format": wire.Body{"type": "json_schema"}},
	}
	out = WithEffort(body, "", WireAnthropic, "")
	thinking := out["thinking"].(wire.Body)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Fatalf("thinking = %v", thinking)
	}
	output := out["output_config"].(wire.Body)
	if output["effort"] != "high" {
		t.Fatalf("effort = %v", output)
	}
	if output["format"].(wire.Body)["type"] != "json_schema" {
		t.Fatalf("format lost: %v", output)
	}
}

func TestNormalizeThinkingLeavesAcceptedShapes(t *testing.T) {
	legacy := wire.Body{"model": "claude-sonnet-4-5", "thinking": wire.Body{"type": "disabled"}}
	if got := NormalizeThinking(legacy); got["thinking"].(wire.Body)["type"] != "disabled" {
		t.Fatalf("legacy = %v", got)
	}
	accepted := wire.Body{"model": "claude-opus-4-6", "thinking": wire.Body{"type": "enabled", "budget_tokens": float64(2048)}}
	if got := NormalizeThinking(accepted); got["thinking"].(wire.Body)["type"] != "enabled" {
		t.Fatalf("accepted = %v", got)
	}
}

func TestAdaptiveEffortReadback(t *testing.T) {
	body := WithEffort(wire.Body{"model": "claude-opus-4-7"}, "minimal", WireAnthropic, "")
	if got := EffortInBody(body, WireAnthropic, "minimal"); got != "minimal" {
		t.Fatalf("hint = %q", got)
	}
	if got := EffortInBody(body, WireAnthropic, ""); got != "low" {
		t.Fatalf("no hint = %q", got)
	}
	if got := ClientEffortOf(wire.Body{
		"model":         "claude-opus-4-7",
		"output_config": wire.Body{"effort": "xhigh"},
	}, WireAnthropic); got != "xhigh" {
		t.Fatalf("client xhigh = %q", got)
	}
}

func TestFitThinkingMaxTokens(t *testing.T) {
	// Raises max_tokens above a legacy thinking budget.
	body := wire.Body{"max_tokens": float64(4096), "thinking": wire.Body{"type": "enabled", "budget_tokens": float64(16384)}}
	out := FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: false})
	if out["max_tokens"] != float64(16384+ThinkingHeadroom) {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: true})
	if out["max_tokens"] != float64(16384+ThinkingHeadroom) {
		t.Fatalf("client max_tokens = %v", out["max_tokens"])
	}

	// Shrinks the budget instead of exceeding the model's output cap.
	body = wire.Body{"max_tokens": float64(4096), "thinking": wire.Body{"type": "enabled", "budget_tokens": float64(32768)}}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: false, MaxOutput: 16000})
	if out["max_tokens"] != float64(16000) {
		t.Fatalf("capped max = %v", out["max_tokens"])
	}
	if out["thinking"].(wire.Body)["budget_tokens"] != float64(16000-ThinkingHeadroom) {
		t.Fatalf("budget = %v", out["thinking"])
	}

	// Keeps a max_tokens that already covers the budget.
	body = wire.Body{"max_tokens": float64(64000), "thinking": wire.Body{"type": "enabled", "budget_tokens": float64(16384)}}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: true})
	if out["max_tokens"] != float64(64000) {
		t.Fatalf("kept = %v", out["max_tokens"])
	}

	// Adaptive thinking gets a roomier default only when the client set none.
	body = wire.Body{"max_tokens": float64(4096), "thinking": wire.Body{"type": "adaptive"}}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: false})
	if out["max_tokens"] != float64(BridgedThinkingMaxTokens) {
		t.Fatalf("adaptive = %v", out["max_tokens"])
	}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: false, MaxOutput: 8000})
	if out["max_tokens"] != float64(8000) {
		t.Fatalf("adaptive cap = %v", out["max_tokens"])
	}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: true})
	if out["max_tokens"] != float64(4096) {
		t.Fatalf("adaptive client = %v", out["max_tokens"])
	}

	// Leaves bodies without thinking alone.
	body = wire.Body{"max_tokens": float64(4096), "thinking": wire.Body{"type": "disabled"}}
	out = FitThinkingMaxTokens(body, MaxTokensOptions{ClientSetMax: false})
	if out["max_tokens"] != float64(4096) {
		t.Fatalf("disabled = %v", out["max_tokens"])
	}
}

func TestBridgedAnthropicBody(t *testing.T) {
	chat := wire.Body{"messages": []any{wire.Body{"role": "user", "content": "hi"}}}

	// Carries a Chat client's reasoning_effort into legacy thinking.
	body := wire.Body{
		"messages":         chat["messages"],
		"reasoning_effort": "high",
	}
	out := BridgedAnthropicBody(body, BridgedBodyOptions{
		Model:        "claude-sonnet-4-5-20250929",
		Stream:       false,
		Effort:       "low",
		ClientEffort: ClientEffortOf(body, WireOpenAI),
	})
	thinking := out["thinking"].(wire.Body)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) {
		t.Fatalf("thinking = %v", thinking)
	}
	if out["max_tokens"] != float64(16384+ThinkingHeadroom) {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
	if _, present := out["reasoning_effort"]; present {
		t.Fatalf("reasoning_effort leaked: %v", out)
	}

	// Falls back to the router's level and respects the client's max_tokens.
	out = BridgedAnthropicBody(wire.Body{
		"messages":   chat["messages"],
		"max_tokens": float64(2000),
	}, BridgedBodyOptions{Model: "claude-opus-4-7", Stream: false, Effort: "medium"})
	if out["output_config"].(wire.Body)["effort"] != "medium" {
		t.Fatalf("router effort = %v", out["output_config"])
	}
	if out["max_tokens"] != float64(2000) {
		t.Fatalf("client max respected = %v", out["max_tokens"])
	}
}
