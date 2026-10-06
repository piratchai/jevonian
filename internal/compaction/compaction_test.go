package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Port of src/compaction.test.ts.

func msg(role, text string) Message { return Message{Role: role, Text: text, SourceIndex: -1} }

func callMsg(id, tool string, input map[string]any) Message {
	m := msg("assistant", "")
	m.ToolUses = []ToolUse{{ToolUseID: id, Tool: tool, Input: input, Keys: sortedKeys(input)}}
	return m
}

func resMsg(id, text string, isErr bool) Message {
	m := msg("user", "")
	m.ToolResults = []ToolResult{{ToolUseID: id, Text: text, IsError: isErr}}
	return m
}

var (
	fileA = strings.Repeat("export const a = 1;\n", 50)
	fileB = strings.Repeat("export const b = 2;\n", 50)
)

func transcript() []Message {
	return []Message{
		msg("user", "Never edit anything under src/generated. Fix the failing test."),
		callMsg("tool-1", "Read", map[string]any{"file_path": "src/a.ts"}),
		resMsg("tool-1", fileA, false),
		msg("assistant", "a.ts looks fine; checking b.ts"),
		callMsg("tool-2", "Read", map[string]any{"file_path": "src/b.ts"}),
		resMsg("tool-2", fileB, false),
		callMsg("tool-3", "Bash", map[string]any{"command": "npm test"}),
		resMsg("tool-3", "FAIL b.test.ts: expected 2 to be 3", true),
		msg("assistant", "The failure is in b.test.ts; fixing now."),
		msg("user", "go ahead"),
	}
}

type seen struct {
	state     State
	questions []string
}

// recorder collects Ask calls. Compaction asks batches concurrently, so the
// test double must be safe for concurrent use.
type recorder struct {
	mu    sync.Mutex
	items []seen
}

func (r *recorder) add(s seen) {
	r.mu.Lock()
	r.items = append(r.items, s)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.items...)
}

type fakeJev struct {
	answer func(string) float64
	seen   *recorder
}

func (f fakeJev) Ask(_ context.Context, state State, qs map[string]Question) (map[string]Answer, error) {
	names := []string{}
	out := map[string]Answer{}
	for k := range qs {
		names = append(names, k)
		v := f.answer(k)
		out[k] = Answer{Noul: &v}
	}
	if f.seen != nil {
		f.seen.add(seen{state, names})
	}
	return out, nil
}

func fit(maxState int, preserve int) Resolved {
	return Resolved{MaxStateTokens: maxState, PreserveRecentMessages: preserve, Goal: "fix the test"}
}

func TestOptions(t *testing.T) {
	d := ResolveOptions(Options{})
	if d.KeepThreshold != 0.5 || d.PreserveRecentMessages != 6 || d.MaxStateTokens != 25_000 || d.MaxRequestTokens != 30_000 || d.TruncateHeadChars != 300 {
		t.Fatalf("defaults = %+v", d)
	}
	nan := 0.0
	nan = nan / nan
	r := ResolveOptions(Options{KeepThreshold: F(nan), PreserveRecentMessages: F(2.7), TruncateHeadChars: F(-1.2)})
	if r.KeepThreshold != 0.5 || r.PreserveRecentMessages != 2 || r.TruncateHeadChars != 0 {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestTokenEstimate(t *testing.T) {
	for text, want := range map[string]int{"": 0, "hello world": 2, "internationalization": 4, "12345678": 4} {
		if got := EstimateTokens(text); got != want {
			t.Errorf("estimate(%q) = %d want %d", text, got, want)
		}
	}
	j := `{"file_path":"/Users/x/src/a.ts","old_string":"a = 1;","n":42}`
	if EstimateTokens(j) < (len(j)+2)/3 {
		t.Errorf("undercounts JSON")
	}
	huge := "data:image/png;base64," + strings.Repeat("A", 4_000_000)
	n := EstimateTokens("User asked: look at this screenshot: " + huge)
	if n >= 1500 || n < 1200 {
		t.Errorf("image tokens = %d", n)
	}
}

func TestCollectToolCalls(t *testing.T) {
	calls := CollectToolCalls(transcript(), 3)
	type row struct {
		id, tool  string
		call, res int
		pinned    bool
	}
	got := []row{}
	for _, c := range calls {
		got = append(got, row{c.ID, c.Tool, c.CallIndex, c.ResultIndex, c.Pinned})
	}
	want := []row{{"t1", "Read", 1, 2, false}, {"t2", "Read", 4, 5, false}, {"t3", "Bash", 6, 7, true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if !calls[2].IsError || calls[0].ResultChars != len(fileA) {
		t.Fatalf("calls = %+v", calls)
	}
	if n := len(CollectToolCalls([]Message{msg("user", "hi"), callMsg("x", "Read", map[string]any{})}, 0)); n != 0 {
		t.Fatalf("unpaired call collected: %d", n)
	}
}

func TestFitStateRecentContextOnly(t *testing.T) {
	messages := []Message{msg("user", "Keep this first instruction verbatim")}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("call-%d", i)
		messages = append(messages, callMsg(id, "Read", map[string]any{"file": fmt.Sprintf("src/%d.ts", i)}))
		messages = append(messages, resMsg(id, strings.Repeat("old result ", 200), false))
	}
	messages = append(messages, msg("user", "Continue with the most recent test"))
	calls := CollectToolCalls(messages, 4)
	f, err := FitState(messages, calls, fit(2000, 4))
	if err != nil {
		t.Fatal(err)
	}
	if f.Stage != "recent context only" || f.Tokens > 2000 {
		t.Fatalf("stage=%s tokens=%d", f.Stage, f.Tokens)
	}
	h := f.State.History
	if h[len(h)-1].Text != "Continue with the most recent test" || h[0].I <= 0 || len(calls) != 1000 {
		t.Fatalf("history head=%+v calls=%d", h[0], len(calls))
	}
	s := &recorder{}
	out, err := Compact(context.Background(), messages, fakeJev{answer: func(string) float64 { return 0.05 }, seen: s},
		Options{PreserveRecentMessages: F(4), MaxStateTokens: F(2000), MaxRequestTokens: F(2500)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Requests <= 0 || out.Stats.Requests >= 1000 {
		t.Fatalf("requests = %d", out.Stats.Requests)
	}
	if out.Messages[0].Text != "Keep this first instruction verbatim" || out.Messages[1].ToolUses[0].ToolUseID != "call-0" ||
		!strings.Contains(out.Messages[2].ToolResults[0].Text, "jevonian truncated") {
		t.Fatalf("messages = %+v", out.Messages[:3])
	}
	all := []string{}
	for _, r := range s.snapshot() {
		all = append(all, r.questions...)
	}
	joined := strings.Join(all, ",")
	if !strings.Contains(joined, "result_t1") || regexp.MustCompile(`(^|,)call_t1(,|$)`).MatchString(joined) {
		t.Fatalf("questions asked: %.200s", joined)
	}
}

func TestFitStateFullHistory(t *testing.T) {
	m := transcript()
	f, err := FitState(m, CollectToolCalls(m, 0), fit(25_000, 0))
	if err != nil || f.Stage != "full" {
		t.Fatalf("stage=%s err=%v", f.Stage, err)
	}
	js := jsJSON(f.State)
	if strings.Contains(js, "export const a = 1;") || !strings.Contains(js, "Never edit anything under src/generated") || !strings.Contains(js, "go ahead") {
		t.Fatalf("state = %.300s", js)
	}
	idx := []int{}
	for _, e := range f.State.History {
		idx = append(idx, e.I)
	}
	if !reflect.DeepEqual(idx, []int{0, 1, 3, 4, 6, 8, 9}) {
		t.Fatalf("indices = %v", idx)
	}
	tc := f.State.History[1].ToolCalls.([]HistoryToolCall)[0]
	if tc.ID != "t1" || tc.Tool != "Read" || tc.Result != fmt.Sprintf("ok, %d chars (omitted)", len(fileA)) {
		t.Fatalf("tc = %+v", tc)
	}
	if !strings.HasPrefix(f.State.History[4].ToolCalls.([]HistoryToolCall)[0].Result, "error, ") {
		t.Fatal("failed call not marked")
	}
}

func TestFitStateDefaultGoal(t *testing.T) {
	r := fit(25_000, 0)
	r.Goal = ""
	f, _ := FitState(transcript(), nil, r)
	if !strings.Contains(f.State.Goal, "Fix the failing test") || !strings.Contains(f.State.Goal, "go ahead") {
		t.Fatalf("goal = %q", f.State.Goal)
	}
}

func TestFitStateTruncatesInputsFirst(t *testing.T) {
	m := []Message{msg("user", "start"),
		callMsg("w", "Write", map[string]any{"file_path": "x.ts", "content": strings.Repeat("x", 5000)}),
		resMsg("w", "ok", false), msg("assistant", "written")}
	f, err := FitState(m, CollectToolCalls(m, 0), fit(300, 0))
	if err != nil || f.Stage != "inputs<=200" || f.Tokens > 300 || f.State.History[0].Text != "start" {
		t.Fatalf("stage=%s tokens=%d err=%v", f.Stage, f.Tokens, err)
	}
	if n := ulen(f.State.History[1].ToolCalls.([]HistoryToolCall)[0].Input); n > 200 {
		t.Fatalf("input len %d", n)
	}
}

func TestFitStateOldCallsCompactedAndMerged(t *testing.T) {
	m := []Message{msg("user", "start")}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("c%d", i)
		m = append(m, callMsg(id, "Read", map[string]any{"file_path": fmt.Sprintf("/repo/src/module-%d.ts", i)}), resMsg(id, "x", false))
	}
	m = append(m, msg("assistant", "done"))
	calls := CollectToolCalls(m, 1)
	full, _ := FitState(m, calls, fit(25_000, 1))
	c, err := FitState(m, calls, fit(int(float64(full.Tokens)*0.8), 1))
	if err != nil || c.Stage != "old calls compacted" || c.Tokens > int(float64(full.Tokens)*0.8) {
		t.Fatalf("stage=%s err=%v", c.Stage, err)
	}
	if got := c.State.History[1].ToolCalls.([]string)[0]; got != "t1 Read file_path=/repo/src/module-0.ts → ok 1ch" {
		t.Fatalf("line = %q", got)
	}
	if c.State.History[len(c.State.History)-1].Text != "done" {
		t.Fatal("last entry changed")
	}
	mg, err := FitState(m, calls, fit(int(float64(full.Tokens)*0.45), 1))
	if err != nil || mg.Stage != "old calls merged" || len(mg.State.History) != 3 {
		t.Fatalf("stage=%s len=%d err=%v", mg.Stage, len(mg.State.History), err)
	}
	lines := mg.State.History[1].ToolCalls.([]string)
	if len(lines) != 40 || !strings.HasPrefix(lines[39], "t40 Read ") || mg.State.History[0].Text != "start" || mg.State.History[2].Text != "done" {
		t.Fatalf("merged = %d", len(lines))
	}
}

func TestFitStateAbridgeThenCollapse(t *testing.T) {
	long := func(n int) string { return fmt.Sprintf("%d %s", n, strings.Repeat("lorem ipsum ", 300)) }
	m := []Message{msg("user", long(0)), msg("assistant", long(1)), msg("user", long(2)), msg("assistant", long(3)), msg("user", "latest")}
	a, err := FitState(m, nil, fit(1800, 1))
	if err != nil || a.Stage != "texts abridged" || a.Tokens > 1800 || !strings.Contains(a.State.History[1].Text, "chars omitted") ||
		a.State.History[0].Text != long(0) || a.State.History[4].Text != "latest" {
		t.Fatalf("stage=%s err=%v", a.Stage, err)
	}
	c, err := FitState(m, nil, fit(420, 1))
	if err != nil || c.Stage != "old messages collapsed" || !regexp.MustCompile(`^\[… \d+ chars omitted …\]$`).MatchString(c.State.History[1].Text) ||
		!strings.Contains(c.State.History[0].Text, "lorem") || c.State.History[4].Text != "latest" {
		t.Fatalf("stage=%s err=%v", c.Stage, err)
	}
}

func TestFitStateThrowsWhenTooLarge(t *testing.T) {
	m := []Message{msg("user", strings.Repeat("a", 2000)), msg("assistant", "b")}
	if _, err := FitState(m, nil, fit(50, 0)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestBatching(t *testing.T) {
	calls := make([]ToolCall, 10)
	for i := range calls {
		calls[i] = ToolCall{ID: fmt.Sprintf("t%d", i+1), ToolUseID: fmt.Sprintf("tool-%d", i+1), Tool: "Read", Input: map[string]any{},
			CallIndex: i*2 + 1, ResultIndex: i*2 + 2, ResultChars: 100}
	}
	if b, err := BatchCalls(calls, 1000, 30_000, nil); err != nil || len(b) != 1 {
		t.Fatalf("one batch: %d %v", len(b), err)
	}
	b, err := BatchCalls(calls, 29_600, 30_000, nil)
	if err != nil || len(b) <= 1 {
		t.Fatalf("split: %d %v", len(b), err)
	}
	ids := []string{}
	for _, x := range b {
		for _, c := range x {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) != 10 || ids[0] != "t1" || ids[9] != "t10" {
		t.Fatalf("ids = %v", ids)
	}
	if _, err := BatchCalls(calls, 29_990, 30_000, nil); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecideCall(t *testing.T) {
	c := ToolCall{ID: "t1", Tool: "Read"}
	if DecideCall(c, CallAnswer{0.9, 0.7}, 0.5).Action != "keep" || DecideCall(c, CallAnswer{0.9, 0.2}, 0.5).Action != "drop_result" ||
		DecideCall(c, CallAnswer{0.1, 0.2}, 0.5).Action != "drop_call" {
		t.Fatal("wrong action")
	}
	c.Pinned = true
	if d := DecideCall(c, CallAnswer{0, 0}, 0.5); d.Action != "keep" || d.Reason != "pinned" {
		t.Fatalf("pinned = %+v", d)
	}
}

func TestApplyDecisions(t *testing.T) {
	m := transcript()
	m[2].ToolResults[0].Text = strings.Repeat("x", 2000)
	m[5].ToolResults[0].Text = strings.Repeat("x", 2000)
	calls := CollectToolCalls(m, 0)
	ds := []Decision{DecideCall(calls[0], CallAnswer{0.1, 0.1}, 0.5), DecideCall(calls[1], CallAnswer{0.9, 0.1}, 0.5), DecideCall(calls[2], CallAnswer{0.9, 0.9}, 0.5)}
	kept := ApplyDecisions(m, ds, calls, 300)
	label := func(x Message) string {
		switch {
		case x.Text != "":
			return x.Text
		case len(x.ToolUses) > 0:
			return x.ToolUses[0].ToolUseID
		}
		return x.ToolResults[0].ToolUseID
	}
	got := []string{}
	for _, x := range kept {
		got = append(got, label(x))
	}
	want := []string{"Never edit anything under src/generated. Fix the failing test.", "a.ts looks fine; checking b.ts", "tool-2", "tool-2", "tool-3", "tool-3", "The failure is in b.test.ts; fixing now.", "go ahead"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if !regexp.MustCompile(`^x{300}\n\[jevonian truncated 1700 chars`).MatchString(kept[3].ToolResults[0].Text) || !strings.Contains(kept[5].ToolResults[0].Text, "expected 2 to be 3") {
		t.Fatalf("results = %.80q", kept[3].ToolResults[0].Text)
	}
	short := transcript()
	short[2].ToolResults[0].Text = strings.Repeat("y", 100)
	short[5].ToolResults[0].Text = strings.Repeat("y", 100)
	if k := ApplyDecisions(short, ds, calls, 300); k[3].ToolResults[0].Text != strings.Repeat("y", 100) {
		t.Fatal("short result modified")
	}
}

func TestTruncateHead(t *testing.T) {
	m := transcript()
	calls := CollectToolCalls(m, 0)
	ds := []Decision{DecideCall(calls[0], CallAnswer{0.9, 0.1}, 0.5)}
	orig := m[2].ToolResults[0].Text
	k := ApplyDecisions(m, ds, calls, 50)
	want := fmt.Sprintf("%s\n[jevonian truncated %d chars of this tool result; re-run the tool if needed]", orig[:50], len(orig)-50)
	if k[2].ToolResults[0].Text != want {
		t.Fatalf("got %q", k[2].ToolResults[0].Text)
	}
	k0 := ApplyDecisions(m, ds, calls, 0)
	if k0[2].ToolResults[0].Text != fmt.Sprintf("[jevonian truncated %d chars of this tool result; re-run the tool if needed]", len(orig)) {
		t.Fatalf("got %q", k0[2].ToolResults[0].Text)
	}
}

func TestCompactResendsFullStateAndMerges(t *testing.T) {
	s := &recorder{}
	m := transcript()
	r1 := fit(25_000, 1)
	r1.Goal = ""
	f, _ := FitState(m, CollectToolCalls(m, 1), r1)
	out, err := Compact(context.Background(), m, fakeJev{answer: func(n string) float64 {
		if strings.HasPrefix(n, "call_") {
			return 0.9
		}
		return 0.1
	}, seen: s}, Options{PreserveRecentMessages: F(1), MaxRequestTokens: F(float64(f.Tokens + 220))})
	if err != nil {
		t.Fatal(err)
	}
	recorded := s.snapshot()
	if out.Stats.Requests != len(recorded) || len(recorded) <= 1 {
		t.Fatalf("requests=%d seen=%d", out.Stats.Requests, len(recorded))
	}
	names := []string{}
	states := map[string]bool{}
	for _, r := range recorded {
		names = append(names, r.questions...)
		b, _ := json.Marshal(r.state)
		states[string(b)] = true
	}
	if len(states) != 1 || len(names) != 6 {
		t.Fatalf("states=%d questions=%v", len(states), names)
	}
	for _, d := range out.Decisions {
		if d.Action != "drop_result" {
			t.Fatalf("decisions = %+v", out.Decisions)
		}
	}
	st := out.Stats
	if len(out.Messages) != len(m) || st.ResultsDropped != 3 || st.Kept != 0 || st.CallsDropped != 0 || st.Pinned != 0 || ReductionRatio(out) <= 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestCompactNoCandidates(t *testing.T) {
	s := &recorder{}
	m := []Message{msg("user", "hello"), msg("assistant", "hi")}
	out, err := Compact(context.Background(), m, fakeJev{answer: func(string) float64 { return 0 }, seen: s}, Options{})
	if err != nil || len(s.snapshot()) != 0 || out.Stats.Requests != 0 || out.Stats.StateStage != "" || out.Stats.Calls != 0 || !reflect.DeepEqual(out.Messages, m) {
		t.Fatalf("out=%+v err=%v", out.Stats, err)
	}
}

func TestCompactEverythingKept(t *testing.T) {
	out, err := Compact(context.Background(), transcript(), fakeJev{answer: func(string) float64 { return 0.95 }}, Options{PreserveRecentMessages: F(1)})
	if err != nil || ReductionRatio(out) != 0 {
		t.Fatalf("err=%v ratio=%v", err, ReductionRatio(out))
	}
	for _, d := range out.Decisions {
		if d.Action != "keep" {
			t.Fatalf("decision = %+v", d)
		}
	}
}

type brokenJev struct{}

func (brokenJev) Ask(context.Context, State, map[string]Question) (map[string]Answer, error) {
	v := 0.5
	return map[string]Answer{"call_t1": {Noul: &v}}, nil
}

func TestCompactRejectsMalformedAnswers(t *testing.T) {
	_, err := Compact(context.Background(), transcript(), brokenJev{}, Options{PreserveRecentMessages: F(1)})
	if err == nil || !strings.Contains(err.Error(), "Invalid Jev answer") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizeTranscriptAllWires(t *testing.T) {
	bodies := map[string]string{
		"openai":    `{"messages":[{"role":"user","content":"fix the bug"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"a.ts\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"file contents"}]}`,
		"anthropic": `{"messages":[{"role":"user","content":[{"type":"text","text":"fix the bug"}]},{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"a.ts"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"file contents"}]}]}`,
		"responses": `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},{"type":"function_call","name":"Read","call_id":"c1","arguments":"{\"file_path\":\"a.ts\"}"},{"type":"function_call_output","call_id":"c1","output":"file contents"}]}`,
	}
	for name, raw := range bodies {
		var body map[string]any
		_ = json.Unmarshal([]byte(raw), &body)
		m := NormalizeTranscript(body)
		calls := CollectToolCalls(m, 0)
		if len(calls) != 1 || calls[0].ToolUseID != "c1" || calls[0].Tool != "Read" || calls[0].Input["file_path"] != "a.ts" || calls[0].ResultChars != len("file contents") || m[0].Text != "fix the bug" {
			t.Errorf("%s: calls=%+v first=%q", name, calls, m[0].Text)
		}
	}
}

func TestNormalizeErrorAndBadArgs(t *testing.T) {
	var b1, b2 map[string]any
	_ = json.Unmarshal([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"Bash"}}]},{"role":"tool","tool_call_id":"c1","content":"Error: tests failed"}]}`), &b1)
	if c := CollectToolCalls(NormalizeTranscript(b1), 0); len(c) != 1 || !c[0].IsError {
		t.Fatalf("calls = %+v", c)
	}
	_ = json.Unmarshal([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"X","arguments":"{"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`), &b2)
	if c := CollectToolCalls(NormalizeTranscript(b2), 0); len(c) != 1 || !reflect.DeepEqual(c[0].Input, map[string]any{"raw": "{"}) {
		t.Fatalf("calls = %+v", c)
	}
}
