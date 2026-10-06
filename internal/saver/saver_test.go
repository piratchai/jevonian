package saver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

// fakeRtk writes a shell double for `rtk pipe` in a temp dir. The real tool is
// never invoked.
func fakeRtk(t *testing.T, behavior string) config.TokenSaverConfig {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "rtk")
	body := map[string]string{
		"compress": "cat >/dev/null; printf 'Pytest: 2 passed, 1 failed\\n'",
		"echo":     "cat",
		"fail":     "exit 1",
		"sleep":    "sleep 10",
	}[behavior]
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultTokenSaver
	cfg.Command = script
	return cfg
}

var bigTestLog = strings.Join(append(append([]string{
	"=== test session starts ===",
	"platform darwin -- Python 3.13",
}, func() []string {
	out := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		out = append(out, "collected item")
	}
	return out
}()...), "tests/test_a.py PASSED", "tests/test_b.py FAILED", "=== 1 failed, 1 passed ==="), "\n")

func toolBody(id, text string) wire.Body {
	return wire.Body{"model": "gpt", "messages": []any{
		wire.Body{"role": "user", "content": "run the tests"},
		wire.Body{"role": "assistant", "content": "", "tool_calls": []any{
			wire.Body{"id": id, "type": "function", "function": wire.Body{"name": "bash", "arguments": "{}"}},
		}},
		wire.Body{"role": "tool", "tool_call_id": id, "content": text},
	}}
}

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		in   string
		bin  string
		args []string
	}{
		{"rtk", "rtk", nil},
		{"/opt/homebrew/bin/rtk", "/opt/homebrew/bin/rtk", nil},
		{"rtk --ultra-compact", "rtk", []string{"--ultra-compact"}},
		{`"/Applications/My Tools/rtk" --ultra-compact`, "/Applications/My Tools/rtk", []string{"--ultra-compact"}},
		{"   ", "rtk", nil},
	} {
		bin, args := ParseCommand(tc.in)
		if bin != tc.bin || !slices.Equal(args, tc.args) {
			t.Fatalf("ParseCommand(%q) = %q %v, want %q %v", tc.in, bin, args, tc.bin, tc.args)
		}
	}
}

func TestFilterForCommand(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"tsc --noEmit", "tsc"},
		{"npx tsc --noEmit", "tsc"},
		{"git status", "git-status"},
		{"git status | head", "git-status"},
		{"git diff HEAD~1", "git-diff"},
		{"git log --oneline", "git-log"},
		{"go test ./...", "go-test"},
		{"cargo test", "cargo-test"},
		{"python -m pytest", "pytest"},
		{"pytest -q", "pytest"},
		// TS splits at '&' before stripping cd, so this form has no filter.
		{"cd /tmp && rg foo", ""},
		{"sudo /usr/bin/find .", "find"},
		{"vim notes.md", ""},
	} {
		if got := FilterForCommand(tc.in); got != tc.want {
			t.Fatalf("FilterForCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSaveTokensDisabledReturnsSameBody(t *testing.T) {
	body := toolBody("c1", bigTestLog)
	cfg := fakeRtk(t, "compress")
	cfg.Enabled = false
	got := SaveTokens(context.Background(), body, cfg)
	if !reflect.DeepEqual(got.Body, body) {
		t.Fatalf("disabled body changed: %v", got.Body)
	}
	if got.Stats.SavedTokens != 0 || got.Stats.ResultsCompressed != 0 {
		t.Fatalf("disabled stats = %+v", got.Stats)
	}
}

func TestSaveTokensCompressesOpenAIToolMessage(t *testing.T) {
	ClearCache()
	body := toolBody("c1", bigTestLog)
	got := SaveTokens(context.Background(), body, fakeRtk(t, "compress"))
	msgs := got.Body["messages"].([]any)
	tool := msgs[2].(wire.Body)
	if tool["content"] != "Pytest: 2 passed, 1 failed\n" {
		t.Fatalf("content = %q", tool["content"])
	}
	if got.Stats.ResultsCompressed != 1 || got.Stats.SavedTokens <= 0 {
		t.Fatalf("stats = %+v", got.Stats)
	}
	if got.Stats.CharsBefore != len(bigTestLog) {
		t.Fatalf("charsBefore = %d", got.Stats.CharsBefore)
	}
	// The caller's body is untouched.
	if body["messages"].([]any)[2].(wire.Body)["content"] != bigTestLog {
		t.Fatal("original body mutated")
	}
}

func TestSaveTokensPassesConfiguredFlagsBeforePipe(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rtk")
	os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" != "--ultra-compact" ] || [ "$2" != "pipe" ]; then exit 2; fi
cat >/dev/null
printf 'ok\n'
`), 0o755)
	cfg := config.DefaultTokenSaver
	cfg.Command = script + " --ultra-compact"
	got := SaveTokens(context.Background(), toolBody("c", bigTestLog), cfg)
	if got.Body["messages"].([]any)[2].(wire.Body)["content"] != "ok\n" || got.Stats.Failures != 0 {
		t.Fatalf("flags: %+v %+v", got.Body, got.Stats)
	}
}

func TestSaveTokensSkipsTinyResults(t *testing.T) {
	cfg := fakeRtk(t, "fail") // would exit 1 if ever spawned
	body := wire.Body{"messages": []any{wire.Body{"role": "tool", "tool_call_id": "c", "content": "ok"}}}
	got := SaveTokens(context.Background(), body, cfg)
	if !reflect.DeepEqual(got.Body, body) || got.Stats.Failures != 0 {
		t.Fatalf("tiny: %+v", got.Stats)
	}
}

func TestSaveTokensMemoizes(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rtk")
	counter := filepath.Join(dir, "count")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'ok\\n'\nprintf x >> \""+counter+"\"\n"), 0o755)
	cfg := config.DefaultTokenSaver
	cfg.Command = script
	ClearCache()
	body := toolBody("c", bigTestLog)
	SaveTokens(context.Background(), body, cfg)
	SaveTokens(context.Background(), body, cfg)
	raw, _ := os.ReadFile(counter)
	if len(raw) != 1 {
		t.Fatalf("rtk invoked %d times", len(raw))
	}
}

func TestSaveTokensArrayContentAndAnthropicAndResponses(t *testing.T) {
	ClearCache()
	cfg := fakeRtk(t, "compress")

	// OpenAI tool message with array content.
	body := wire.Body{"messages": []any{wire.Body{"role": "tool", "tool_call_id": "c1",
		"content": []any{wire.Body{"type": "text", "text": bigTestLog}}}}}
	got := SaveTokens(context.Background(), body, cfg)
	blocks := got.Body["messages"].([]any)[0].(wire.Body)["content"].([]any)
	if blocks[0].(wire.Body)["text"] != "Pytest: 2 passed, 1 failed\n" {
		t.Fatalf("array content = %v", blocks)
	}

	// Anthropic tool_result with a string content.
	body = wire.Body{"model": "claude", "messages": []any{
		wire.Body{"role": "assistant", "content": []any{wire.Body{"type": "tool_use", "id": "t1", "name": "bash", "input": wire.Body{}}}},
		wire.Body{"role": "user", "content": []any{wire.Body{"type": "tool_result", "tool_use_id": "t1", "content": bigTestLog}}},
	}}
	got = SaveTokens(context.Background(), body, cfg)
	block := got.Body["messages"].([]any)[1].(wire.Body)["content"].([]any)[0].(wire.Body)
	if block["content"] != "Pytest: 2 passed, 1 failed\n" || got.Stats.SavedTokens <= 0 {
		t.Fatalf("anthropic = %v %+v", block, got.Stats)
	}

	// Anthropic tool_result with text blocks.
	body = wire.Body{"messages": []any{wire.Body{"role": "user", "content": []any{
		wire.Body{"type": "tool_result", "tool_use_id": "t1", "content": []any{wire.Body{"type": "text", "text": bigTestLog}}}}}}}
	got = SaveTokens(context.Background(), body, cfg)
	block = got.Body["messages"].([]any)[0].(wire.Body)["content"].([]any)[0].(wire.Body)
	if block["content"].([]any)[0].(wire.Body)["text"] != "Pytest: 2 passed, 1 failed\n" {
		t.Fatalf("anthropic blocks = %v", block)
	}

	// Responses function_call_output.
	body = wire.Body{"input": []any{
		wire.Body{"type": "message", "role": "user", "content": []any{wire.Body{"type": "input_text", "text": "go"}}},
		wire.Body{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": "{}"},
		wire.Body{"type": "function_call_output", "call_id": "c1", "output": bigTestLog},
	}}
	got = SaveTokens(context.Background(), body, cfg)
	if got.Body["input"].([]any)[2].(wire.Body)["output"] != "Pytest: 2 passed, 1 failed\n" {
		t.Fatalf("responses = %v", got.Body)
	}
}

func TestSaveTokensFailureModes(t *testing.T) {
	ClearCache()
	body := toolBody("c", bigTestLog)
	got := SaveTokens(context.Background(), body, fakeRtk(t, "echo"))
	if got.Stats.ResultsCompressed != 0 || got.Stats.SavedTokens != 0 || got.Stats.Failures != 0 {
		t.Fatalf("echo stats = %+v", got.Stats)
	}
	if !reflect.DeepEqual(got.Body, body) {
		t.Fatal("echo body changed")
	}

	ClearCache()
	got = SaveTokens(context.Background(), body, fakeRtk(t, "fail"))
	if got.Stats.Failures != 1 || got.Stats.Unavailable {
		t.Fatalf("fail stats = %+v", got.Stats)
	}

	ClearCache()
	missing := fakeRtk(t, "compress")
	missing.Command = "/nonexistent/rtk-binary"
	got = SaveTokens(context.Background(), body, missing)
	if got.Stats.Failures != 1 || !got.Stats.Unavailable {
		t.Fatalf("missing stats = %+v", got.Stats)
	}

	ClearCache()
	slow := fakeRtk(t, "sleep")
	slow.TimeoutMs = 200
	got = SaveTokens(context.Background(), body, slow)
	if got.Stats.Failures != 1 || !reflect.DeepEqual(got.Body, body) {
		t.Fatalf("timeout stats = %+v", got.Stats)
	}
}

func TestSaveTokensNoToolResultsAndSumsSavings(t *testing.T) {
	ClearCache()
	plain := wire.Body{"messages": []any{wire.Body{"role": "user", "content": "hi"}}}
	if got := SaveTokens(context.Background(), plain, fakeRtk(t, "compress")); !reflect.DeepEqual(got.Body, plain) {
		t.Fatalf("plain body changed")
	}
	ClearCache()
	two := wire.Body{"messages": []any{
		wire.Body{"role": "tool", "tool_call_id": "a", "content": bigTestLog},
		wire.Body{"role": "tool", "tool_call_id": "b", "content": bigTestLog},
	}}
	got := SaveTokens(context.Background(), two, fakeRtk(t, "compress"))
	if got.Stats.ResultsCompressed != 2 || got.Stats.CharsBefore != len(bigTestLog)*2 {
		t.Fatalf("sums = %+v", got.Stats)
	}
}

// selectiveRtk compresses only when a filter is passed, like rtk auto-detect
// failing on output it does not recognise.
func selectiveRtk(t *testing.T) config.TokenSaverConfig {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "rtk")
	log := filepath.Join(dir, "argv")
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+log+"\"\nif [ \"$2\" = \"-f\" ]; then cat >/dev/null; printf 'FILTERED via %s\\n' \"$3\"; else cat; fi\n"), 0o755)
	cfg := config.DefaultTokenSaver
	cfg.Command = script
	return cfg
}

func TestSaveTokensFilterRetry(t *testing.T) {
	ClearCache()
	cfg := selectiveRtk(t)

	openai := wire.Body{"messages": []any{
		wire.Body{"role": "assistant", "tool_calls": []any{wire.Body{"id": "c1", "type": "function",
			"function": wire.Body{"name": "bash", "arguments": `{"command":"tsc --noEmit"}`}}}},
		wire.Body{"role": "tool", "tool_call_id": "c1", "content": bigTestLog},
	}}
	got := SaveTokens(context.Background(), openai, cfg)
	if got.Body["messages"].([]any)[1].(wire.Body)["content"] != "FILTERED via tsc\n" || got.Stats.ResultsCompressed != 1 {
		t.Fatalf("openai filter = %v %+v", got.Body, got.Stats)
	}

	ClearCache()
	anthropic := wire.Body{"messages": []any{
		wire.Body{"role": "assistant", "content": []any{wire.Body{"type": "tool_use", "id": "t1", "name": "bash", "input": wire.Body{"command": "git status"}}}},
		wire.Body{"role": "user", "content": []any{wire.Body{"type": "tool_result", "tool_use_id": "t1", "content": bigTestLog}}},
	}}
	got = SaveTokens(context.Background(), anthropic, cfg)
	block := got.Body["messages"].([]any)[1].(wire.Body)["content"].([]any)[0].(wire.Body)
	if block["content"] != "FILTERED via git-status\n" {
		t.Fatalf("anthropic filter = %v", block)
	}

	ClearCache()
	responses := wire.Body{"input": []any{
		wire.Body{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": `{"command":"pytest -q"}`},
		wire.Body{"type": "function_call_output", "call_id": "c1", "output": bigTestLog},
	}}
	got = SaveTokens(context.Background(), responses, cfg)
	if got.Body["input"].([]any)[1].(wire.Body)["output"] != "FILTERED via pytest\n" {
		t.Fatalf("responses filter = %v", got.Body)
	}

	// No known filter: no retry, no compression.
	ClearCache()
	unknown := wire.Body{"messages": []any{
		wire.Body{"role": "assistant", "tool_calls": []any{wire.Body{"id": "c1", "type": "function",
			"function": wire.Body{"name": "bash", "arguments": `{"command":"vim notes.md"}`}}}},
		wire.Body{"role": "tool", "tool_call_id": "c1", "content": bigTestLog},
	}}
	if got = SaveTokens(context.Background(), unknown, cfg); got.Stats.ResultsCompressed != 0 {
		t.Fatalf("unknown filter = %+v", got.Stats)
	}
}

func TestEstimateTokensGolden(t *testing.T) {
	// Fixed reference values avoid importing routing, which depends on upstream/saver.
	for _, tc := range []struct {
		text string
		want int
	}{
		{"hello world", 2},
		{`{"a":1}`, 6},
		{"data:image/png;base64," + strings.Repeat("A", 400), 1200},
		{"😀", 2},
	} {
		if got := estimateTokens(tc.text); got != tc.want {
			t.Fatalf("estimateTokens(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}
