package wire_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
	"github.com/xinyao27/jevonian/internal/wire/anthropic"
)

// Differential fixtures from src/prepare.ts and src/anthropic-thinking.ts:
// withEffort, stripForeignEffort, normalizeAnthropicThinking, effortInBody,
// clientEffortOf, fitThinkingMaxTokens, bridgedAnthropicBody.
type effortCase struct {
	Fn           string          `json:"fn"`
	Body         wire.Body       `json:"body"`
	Wire         string          `json:"wire"`
	Effort       *string         `json:"effort"`
	ClientEffort *string         `json:"clientEffort"`
	ClientSetMax bool            `json:"clientSetMax"`
	MaxOutput    int             `json:"maxOutput"`
	Opts         *bridgedOptions `json:"opts"`
	Want         json.RawMessage `json:"want"`
}

type bridgedOptions struct {
	Model        string  `json:"model"`
	Stream       bool    `json:"stream"`
	Effort       *string `json:"effort"`
	ClientEffort *string `json:"clientEffort"`
	MaxOutput    int     `json:"maxOutput"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestEffortAndThinkingMatchTypeScriptFuzz(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_effort_fuzz.json")
	if err != nil {
		t.Skip("no TS effort fixture")
	}
	var cases []effortCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	failures := map[string]int{}
	for i, c := range cases {
		w := anthropic.WireKind(c.Wire)
		var got any
		switch c.Fn {
		case "withEffort":
			got = anthropic.WithEffort(c.Body, str(c.Effort), w, str(c.ClientEffort))
		case "stripForeignEffort":
			got = anthropic.StripForeignEffort(c.Body, w)
		case "normalizeAnthropicThinking":
			got = anthropic.NormalizeThinking(c.Body)
		case "effortInBody":
			got = anthropic.EffortInBody(c.Body, w, str(c.Effort))
			if got == "" {
				got = nil
			}
		case "clientEffortOf":
			got = anthropic.ClientEffortOf(c.Body, w)
			if got == "" {
				got = nil
			}
		case "fitThinkingMaxTokens":
			got = anthropic.FitThinkingMaxTokens(c.Body, anthropic.MaxTokensOptions{ClientSetMax: c.ClientSetMax, MaxOutput: c.MaxOutput})
		case "bridgedAnthropicBody":
			got = anthropic.BridgedAnthropicBody(c.Body, anthropic.BridgedBodyOptions{
				Model: c.Opts.Model, Stream: c.Opts.Stream,
				Effort: str(c.Opts.Effort), ClientEffort: str(c.Opts.ClientEffort), MaxOutput: c.Opts.MaxOutput,
			})
		default:
			t.Fatalf("unknown fn %s", c.Fn)
		}
		var want any
		if err := json.Unmarshal(c.Want, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalizeJSON(t, got), want) {
			failures[c.Fn]++
			if failures[c.Fn] <= 2 {
				gj, _ := json.Marshal(got)
				bj, _ := json.Marshal(c.Body)
				t.Errorf("case %d %s mismatch (wire=%s effort=%s client=%s opts=%+v)\nbody: %s\n  go: %s\n  ts: %s",
					i, c.Fn, c.Wire, str(c.Effort), str(c.ClientEffort), c.Opts, bj, gj, c.Want)
			}
		}
	}
	for fn, n := range failures {
		t.Errorf("%s: %d mismatches", fn, n)
	}
}
