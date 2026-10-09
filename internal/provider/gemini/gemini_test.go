package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func parse(t *testing.T, s string) wire.Body {
	t.Helper()
	var b wire.Body
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChatToGeminiMapsSystemToolsAndResults(t *testing.T) {
	req := ChatToGemini(parse(t, `{
	  "messages":[
	    {"role":"system","content":"be brief"},
	    {"role":"user","content":"fix the bug"},
	    {"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read/file","arguments":"{\"p\":\"a\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"Error: test failed"}],
	  "tools":[{"type":"function","function":{"name":"read/file","description":"read a file","parameters":{"$schema":"x","type":"object","properties":{"mode":{"const":"text"}},"required":["mode"]}}}],
	  "temperature":0.2,"max_tokens":512,"stop":["END"],
	  "tool_choice":{"type":"function","function":{"name":"read/file"}}}`))
	if wire.MarshalJSON(req["systemInstruction"]) != `{"parts":[{"text":"be brief"}]}` {
		t.Fatalf("system %v", req["systemInstruction"])
	}
	contents := req["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents %v", contents)
	}
	call := wire.AsRecord(wire.AsSlice(wire.AsRecord(contents[1])["parts"])[0])
	if wire.AsRecord(call["functionCall"])["name"] != "read_file" || call["thoughtSignature"] != skipThoughtSignature {
		t.Fatalf("call %v", call)
	}
	resp := wire.AsRecord(wire.AsRecord(wire.AsSlice(wire.AsRecord(contents[2])["parts"])[0])["functionResponse"])
	if resp["name"] != "read_file" || wire.AsRecord(resp["response"])["result"] != "Error: test failed" {
		t.Fatalf("response %v", resp)
	}
	decl := wire.AsRecord(wire.AsSlice(wire.AsRecord(wire.AsSlice(req["tools"])[0])["functionDeclarations"])[0])
	params := wire.MarshalJSON(decl["parameters"])
	if strings.Contains(params, "$schema") || !strings.Contains(params, `"enum":["text"]`) {
		t.Fatalf("schema not sanitized: %s", params)
	}
	cfg := wire.AsRecord(req["generationConfig"])
	if cfg["temperature"] != 0.2 || cfg["maxOutputTokens"] != float64(512) || wire.MarshalJSON(cfg["stopSequences"]) != `["END"]` {
		t.Fatalf("cfg %v", cfg)
	}
	if wire.MarshalJSON(req["toolConfig"]) != `{"functionCallingConfig":{"allowedFunctionNames":["read_file"],"mode":"ANY"}}` {
		t.Fatalf("toolConfig %v", req["toolConfig"])
	}
}

func TestChatToGeminiStampsOnlyFirstParallelCallAndMergesRoles(t *testing.T) {
	req := ChatToGemini(parse(t, `{"messages":[
	  {"role":"user","content":"a"},{"role":"user","content":"b"},
	  {"role":"assistant","tool_calls":[
	    {"id":"c1","type":"function","function":{"name":"par_a","arguments":"{}"}},
	    {"id":"c2","type":"function","function":{"name":"par_b","arguments":"{}"}}]}]}`))
	contents := req["contents"].([]any)
	if len(wire.AsSlice(wire.AsRecord(contents[0])["parts"])) != 2 {
		t.Fatalf("consecutive user messages not merged: %v", contents[0])
	}
	parts := wire.AsSlice(wire.AsRecord(contents[1])["parts"])
	if wire.AsRecord(parts[0])["thoughtSignature"] != skipThoughtSignature || wire.AsRecord(parts[1])["thoughtSignature"] != nil {
		t.Fatalf("signatures %v", parts)
	}
}

func TestSanitizeToolName(t *testing.T) {
	if SanitizeToolName("a b/c") != "a_b_c" || SanitizeToolName("9x") != "_9x" || SanitizeToolName("") != "tool" {
		t.Fatal("sanitize")
	}
}

func TestEndpointAndUnwrapAndEnvelope(t *testing.T) {
	if Endpoint("https://x/v1internal/", true) != "https://x/v1internal:streamGenerateContent?alt=sse" ||
		Endpoint("https://x", false) != "https://x/v1internal:generateContent" {
		t.Fatal("endpoint")
	}
	if Unwrap(parse(t, `{"response":{"a":1}}`))["a"] != float64(1) || Unwrap(parse(t, `{"a":2}`))["a"] != float64(2) {
		t.Fatal("unwrap")
	}
	env := Envelope("", "m", wire.Body{})
	if env["project"] != "default-cli-project" || env["userAgent"] != "antigravity" || env["requestId"] == "" {
		t.Fatalf("env %v", env)
	}
}

func TestChatCompletionCollectsTextCallsUsageAndFinish(t *testing.T) {
	chat := ChatCompletion(parse(t, `{"candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"sig-1"},{"text":"hi"},{"functionCall":{"name":"f","args":{"x":1}}}]},"finishReason":"STOP"}],
	  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":2,"cachedContentTokenCount":4}}`), "m")
	choice := wire.AsRecord(wire.AsSlice(chat["choices"])[0])
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish %v", choice["finish_reason"])
	}
	msg := wire.AsRecord(choice["message"])
	if msg["content"] != "hi" || len(wire.AsSlice(msg["tool_calls"])) != 1 {
		t.Fatalf("msg %v", msg)
	}
	if u := wire.AsRecord(chat["usage"]); u["prompt_tokens"] != 10 || u["completion_tokens"] != 5 {
		t.Fatalf("usage %v", u)
	}
	// A cache read must survive the fold into Chat Completions: Gemini counts it
	// inside promptTokenCount, so it rides as prompt_tokens_details.cached_tokens
	// (without this, Antigravity turns read as cache misses in the ledger).
	if cached := wire.Number(wire.AsRecord(wire.AsRecord(chat["usage"])["prompt_tokens_details"])["cached_tokens"]); cached != 4 {
		t.Fatalf("cached_tokens = %v, want 4 (usage %v)", cached, chat["usage"])
	}
	// The signature on the thought part is replayed on the next turn.
	if sig, ok := ThoughtSignatureFor("f", wire.Body{"x": float64(1)}); !ok || sig != "sig-1" {
		t.Fatalf("signature %q %v", sig, ok)
	}
	maxed := ChatCompletion(parse(t, `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"MAX_TOKENS"}]}`), "m")
	if wire.AsRecord(wire.AsSlice(maxed["choices"])[0])["finish_reason"] != "length" {
		t.Fatal("MAX_TOKENS -> length")
	}
}

func TestToChatStreamTranslatesChunks(t *testing.T) {
	var finished wire.Usage
	st := NewToChatStream("m", func(u wire.Usage) { finished = u }, nil)
	sink := &wire.Collector{}
	st.Handle(parse(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}}`), sink)
	st.Handle(parse(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"lo"},{"functionCall":{"name":"g","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"cachedContentTokenCount":5}}}`), sink)
	st.Finish(sink)
	out := sink.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"Hel"`, `"content":"lo"`, `"tool_calls"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":7`, `"cached_tokens":5`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if finished.Input != 7 || finished.Output != 2 {
		t.Fatalf("usage %+v", finished)
	}
}
