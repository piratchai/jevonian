package routing

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
	"github.com/xinyao27/jevonian/internal/wire/anthropic"
)

// TestGoldenAnthropicThinking replays testdata/ts_thinking_golden.json (from
// src/anthropic-thinking.ts and src/prepare.ts) through internal/wire/anthropic:
// thinking support per model id, prefill rejection, adaptive effort mapping,
// max_tokens fitting, effort-to-thinking shapes and thinking normalisation.
// Checklist 3.2 "Effort clamp ... adaptive vs legacy thinking shapes" and
// "rejectsDisabled for Sonnet >=5.5 / Opus". Regenerate with /tmp/parity/gen6.mts.
func TestGoldenAnthropicThinking(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_thinking_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Support []struct {
			Model                                            string
			Adaptive, RejectsEnabled, RejectsDisabled, Xhigh bool
		}
		Prefill []struct {
			Model string
			Want  bool
		}
		Adaptive []struct {
			Effort string
			Xhigh  bool
			Want   string
		}
		Fit []struct {
			Body         map[string]any
			ClientSetMax bool
			MaxOutput    *int
			Want         map[string]any
		}
		WithEffort []struct {
			Body   map[string]any
			Effort string
			Client string
			Want   map[string]any
		}
		Normalize []struct {
			Body map[string]any
			Want map[string]any
		}
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	norm := func(v any) any {
		b, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(b, &out)
		return out
	}
	for _, c := range g.Support {
		got := anthropic.ThinkingSupportFor(c.Model)
		want := anthropic.ThinkingSupport{Adaptive: c.Adaptive, RejectsEnabled: c.RejectsEnabled, RejectsDisabled: c.RejectsDisabled, Xhigh: c.Xhigh}
		if got != want {
			t.Errorf("support(%q): got %+v want %+v", c.Model, got, want)
		}
	}
	for _, c := range g.Prefill {
		if got := anthropic.RejectsAssistantPrefill(c.Model); got != c.Want {
			t.Errorf("rejectsPrefill(%q): got %v want %v", c.Model, got, c.Want)
		}
	}
	for _, c := range g.Adaptive {
		if got := anthropic.AdaptiveEffort(c.Effort, anthropic.ThinkingSupport{Xhigh: c.Xhigh}); got != c.Want {
			t.Errorf("adaptiveEffort(%q, xhigh=%v): got %q want %q", c.Effort, c.Xhigh, got, c.Want)
		}
	}
	for i, c := range g.Fit {
		opts := anthropic.MaxTokensOptions{ClientSetMax: c.ClientSetMax}
		if c.MaxOutput != nil {
			opts.MaxOutput = *c.MaxOutput
		}
		got := anthropic.FitThinkingMaxTokens(wire.Body(c.Body), opts)
		if !reflect.DeepEqual(norm(got), norm(c.Want)) {
			t.Errorf("fit %d: got %v want %v", i, norm(got), norm(c.Want))
		}
	}
	bad := 0
	for i, c := range g.WithEffort {
		got := anthropic.WithEffort(wire.Body(c.Body), c.Effort, anthropic.WireAnthropic, c.Client)
		if !reflect.DeepEqual(norm(got), norm(c.Want)) {
			if bad++; bad <= 10 {
				t.Errorf("withEffort %d (%v effort=%q client=%q): got %v want %v", i, c.Body, c.Effort, c.Client, norm(got), norm(c.Want))
			}
		}
	}
	for i, c := range g.Normalize {
		got := anthropic.NormalizeThinking(wire.Body(c.Body))
		if !reflect.DeepEqual(norm(got), norm(c.Want)) {
			t.Errorf("normalize %d (%v): got %v want %v", i, c.Body, norm(got), norm(c.Want))
		}
	}
}
