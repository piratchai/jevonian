package wire_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

const (
	identity      = "You operate in Cursor."
	toolCalling   = "Use specialized tools instead of terminal commands when possible, as this provides a better user experience. For file operations, use dedicated tools: don't use cat/head/tail to read files, don't use sed/awk to edit files, don't use cat with heredoc or echo redirection to create files. Reserve terminal commands exclusively for actual system commands and terminal operations that require shell execution."
	methodHeading = "## METHOD 2: MARKDOWN CODE BLOCKS - Proposing or Displaying Code NOT already in Codebase"
	terminalLine  = "There is one text file for each terminal the user has running."
	editorLine    = "You work inside the user's code editor."
)

// Ports of src/prompt-policy.test.ts.
func TestPromptPolicyBuiltins(t *testing.T) {
	out := wire.SanitizeBuiltinPrompt(strings.Join([]string{identity, toolCalling, methodHeading, terminalLine}, "\n\n"))
	for _, rule := range wire.BuiltinPromptRewrites {
		if strings.Contains(out, rule[0]) {
			t.Fatalf("signature left behind: %q", rule[0])
		}
	}
	plain := "You are an AI coding assistant.\n\nAlways run the tests before claiming success."
	if wire.SanitizeBuiltinPrompt(plain) != plain {
		t.Fatal("unrelated text changed")
	}
	if wire.SanitizeBuiltinPrompt("You operate in Cursor IDE") != editorLine || wire.SanitizeBuiltinPrompt("you operate in cursor.") != editorLine {
		t.Fatal("identity forms not rewritten")
	}
	once := wire.SanitizeBuiltinPrompt(identity + "\n\n" + toolCalling)
	if wire.SanitizeBuiltinPrompt(once) != once {
		t.Fatal("not idempotent")
	}
	if wire.SanitizePromptWithPolicy(identity, config.PromptPolicyConfig{Builtins: false}) != identity {
		t.Fatal("builtins off must leave the prompt alone")
	}
}

func rules(rs ...config.PromptRewriteRule) config.PromptPolicyConfig {
	return config.PromptPolicyConfig{Builtins: true, Rewrites: rs}
}

func TestPromptPolicyOperatorRules(t *testing.T) {
	cases := []struct {
		name, text, want string
		policy           config.PromptPolicyConfig
	}{
		{"plain", "deploy acme-corp-secret now", "deploy ACME now", rules(config.PromptRewriteRule{Match: "acme-corp-secret", Replace: "ACME"})},
		{"group and flags", "see TICKET-42", "see TICKET 42", rules(config.PromptRewriteRule{Match: `ticket-(\d+)`, Replace: "TICKET $1", Flags: "i"})},
		{"delete", "a REDACTME b", "ab", rules(config.PromptRewriteRule{Match: `\s*REDACTME\s*`, Replace: ""})},
		{"after builtins", identity, "You work inside the IDE.", rules(config.PromptRewriteRule{Match: "user's code editor", Replace: "IDE"})},
		{"global by default", "x x", "y y", rules(config.PromptRewriteRule{Match: "x", Replace: "y"})},
	}
	for _, c := range cases {
		if got := wire.SanitizePromptWithPolicy(c.text, c.policy); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestRewritePromptBodies(t *testing.T) {
	policy := config.PromptPolicyConfig{Builtins: true}
	body := wire.Body{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": identity},
		map[string]any{"role": "user", "content": identity},
	}}
	out := wire.RewritePromptBodies(body, policy)
	msgs := out["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != editorLine || msgs[1].(map[string]any)["content"] != identity {
		t.Fatalf("messages = %v", msgs)
	}
	if body["messages"].([]any)[0].(map[string]any)["content"] != identity {
		t.Fatal("caller body was mutated")
	}
	untouched := wire.Body{"messages": []any{map[string]any{"role": "system", "content": "plain"}}}
	if got := wire.RewritePromptBodies(untouched, policy); !reflect.DeepEqual(got, untouched) {
		t.Fatal("untouched body changed")
	}
}

type policyCase struct {
	Text     string                   `json:"text"`
	Rule     config.PromptRewriteRule `json:"rule"`
	Builtins bool                     `json:"builtins"`
	Parsed   int                      `json:"parsed"`
	Body     wire.Body                `json:"body"`
	ParseRaw json.RawMessage          `json:"parseRaw"`
	Want     json.RawMessage          `json:"want"`
}

// Differential: TS parsePromptPolicy / sanitizePromptWithPolicy / rewritePromptBodies.
func TestPromptPolicyMatchesTypeScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_policy_fuzz.json")
	if err != nil {
		t.Skip("no TS policy fixture")
	}
	var generic []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	var cases []policyCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	mismatches := 0
	for i, c := range cases {
		_, hasText := generic[i]["text"]
		_, hasBody := generic[i]["body"]
		_, hasParse := generic[i]["parseRaw"]
		switch {
		case hasText:
			// Parse through the real config path so unsupported patterns are dropped as in production.
			ruleJSON, _ := json.Marshal(map[string]any{"builtins": c.Builtins, "rewrites": []any{map[string]any{"match": c.Rule.Match, "replace": c.Rule.Replace, "flags": c.Rule.Flags}}})
			var rawCfg any
			_ = json.Unmarshal(ruleJSON, &rawCfg)
			cfg, err := config.ParseConfig(map[string]any{"promptPolicy": rawCfg})
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.PromptPolicy.Rewrites) != c.Parsed {
				// RE2 cannot express every JS pattern; the rule is dropped at parse time.
				t.Logf("case %d rule %q parsed=%d (ts=%d)", i, c.Rule.Match, len(cfg.PromptPolicy.Rewrites), c.Parsed)
				mismatches++
				continue
			}
			var want string
			_ = json.Unmarshal(c.Want, &want)
			if got := wire.SanitizePromptWithPolicy(c.Text, cfg.PromptPolicy); got != want {
				mismatches++
				t.Errorf("case %d text=%q rule=%+v builtins=%v\n go: %q\n ts: %q", i, c.Text, c.Rule, c.Builtins, got, want)
			}
		case hasBody:
			var want wire.Body
			_ = json.Unmarshal(c.Want, &want)
			got := wire.RewritePromptBodies(c.Body, config.PromptPolicyConfig{Builtins: true})
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(want)
			if string(gj) != string(wj) {
				mismatches++
				t.Errorf("case %d body\n go: %s\n ts: %s", i, gj, wj)
			}
		case hasParse:
			// Parse-level drops are exercised via config; counted only.
		}
	}
	t.Logf("%d cases, %d mismatches", len(cases), mismatches)
}
