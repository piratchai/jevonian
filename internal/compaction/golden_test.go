package compaction

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestGoldenAgainstTS replays testdata/ts_golden.json (src/compaction.ts run on
// 120 random bodies across the OpenAI, Anthropic and Responses wires):
// normalizeTranscript, collectToolCalls, fitState (every stage), decideCall,
// applyDecisions and reencodeMessages. Regenerate with /tmp/parity/gen8.mts.
func TestGoldenAgainstTS(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Kind     string
		Body     map[string]any
		Preserve int
		MaxState int
		Norm     []struct {
			Role     string
			Text     string
			ToolUses []struct {
				Tool_use_id, Tool string
				Input             map[string]any
			}
			ToolResults []struct {
				Tool_use_id, Text string
				IsError           bool
			}
			SourceIndex int
		}
		Calls []struct {
			Id, Tool_use_id, Tool               string
			CallIndex, ResultIndex, ResultChars int
			IsError, Pinned                     bool
		}
		Fitted    map[string]any
		Decisions []struct {
			Id, Action, Reason string
		}
		KeptCount  int
		CharsAfter int
		Reencoded  map[string]any
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		m := NormalizeTranscript(c.Body)
		if len(m) != len(c.Norm) {
			t.Fatalf("case %d (%s): %d messages want %d", i, c.Kind, len(m), len(c.Norm))
		}
		for j, w := range c.Norm {
			g := m[j]
			if g.Role != w.Role || g.Text != w.Text || g.SourceIndex != w.SourceIndex || len(g.ToolUses) != len(w.ToolUses) || len(g.ToolResults) != len(w.ToolResults) {
				t.Fatalf("case %d msg %d: got %+v want %+v", i, j, g, w)
			}
			for k, u := range w.ToolUses {
				if g.ToolUses[k].ToolUseID != u.Tool_use_id || g.ToolUses[k].Tool != u.Tool || !reflect.DeepEqual(norm(g.ToolUses[k].Input), norm(u.Input)) {
					t.Fatalf("case %d msg %d use %d differs", i, j, k)
				}
			}
			for k, r := range w.ToolResults {
				if g.ToolResults[k].ToolUseID != r.Tool_use_id || g.ToolResults[k].Text != r.Text || g.ToolResults[k].IsError != r.IsError {
					t.Fatalf("case %d msg %d result %d differs", i, j, k)
				}
			}
		}
		calls := CollectToolCalls(m, c.Preserve)
		if len(calls) != len(c.Calls) {
			t.Fatalf("case %d: %d calls want %d", i, len(calls), len(c.Calls))
		}
		for j, w := range c.Calls {
			g := calls[j]
			if g.ID != w.Id || g.ToolUseID != w.Tool_use_id || g.Tool != w.Tool || g.CallIndex != w.CallIndex || g.ResultIndex != w.ResultIndex ||
				g.ResultChars != w.ResultChars || g.IsError != w.IsError || g.Pinned != w.Pinned {
				t.Fatalf("case %d call %d: got %+v want %+v", i, j, g, w)
			}
		}
		f, ferr := FitState(m, calls, Resolved{MaxStateTokens: c.MaxState, PreserveRecentMessages: c.Preserve})
		if want, ok := c.Fitted["error"].(string); ok {
			if ferr == nil || ferr.Error() != want {
				t.Fatalf("case %d: fit error %v want %q", i, ferr, want)
			}
		} else {
			if ferr != nil {
				t.Fatalf("case %d: fit: %v", i, ferr)
			}
			got := norm(map[string]any{"stage": f.Stage, "tokens": f.Tokens, "state": f.State})
			if !reflect.DeepEqual(got, norm(c.Fitted)) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(c.Fitted)
				t.Fatalf("case %d (%s) fitState differs:\n got  %.600s\n want %.600s", i, c.Kind, gb, wb)
			}
		}
		ds := make([]Decision, len(calls))
		for j, cl := range calls {
			a := CallAnswer{KeepCall: 0.9, KeepResult: 0.9}
			if j%3 == 0 {
				a.KeepCall = 0.1
			}
			if j%2 == 0 {
				a.KeepResult = 0.1
			}
			ds[j] = DecideCall(cl, a, 0.5)
			if ds[j].Action != c.Decisions[j].Action || ds[j].Reason != c.Decisions[j].Reason {
				t.Fatalf("case %d decision %d: %+v want %+v", i, j, ds[j], c.Decisions[j])
			}
		}
		kept := ApplyDecisions(m, ds, calls, 100)
		chars := 0
		for _, k := range kept {
			chars += MessageChars(k)
		}
		if len(kept) != c.KeptCount || chars != c.CharsAfter {
			t.Fatalf("case %d: kept=%d chars=%d want %d/%d", i, len(kept), chars, c.KeptCount, c.CharsAfter)
		}
		if re := ReencodeMessages(c.Body, kept); !reflect.DeepEqual(norm(re), norm(c.Reencoded)) {
			gb, _ := json.Marshal(norm(re))
			wb, _ := json.Marshal(c.Reencoded)
			t.Fatalf("case %d (%s) reencode differs:\n got  %.500s\n want %.500s", i, c.Kind, gb, wb)
		}
	}
}

// norm round-trips through JSON. A marked lone surrogate (see
// loneSurrogateBase) becomes U+FFFD, which is what Go's decoder makes of the
// golden's `\ud83d`, so both sides compare equal.
func norm(v any) any {
	b, _ := json.Marshal(v)
	b = []byte(strings.Map(func(r rune) rune {
		if r >= loneSurrogateBase+0xd800 && r <= loneSurrogateBase+0xdfff {
			return 0xfffd
		}
		return r
	}, string(b)))
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
