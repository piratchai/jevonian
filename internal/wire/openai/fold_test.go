package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFoldChatStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")
	out := FoldChatBytes([]byte(sse), "primary-model")
	if out["id"] != "chatcmpl-1" {
		t.Fatalf("id: %v", out["id"])
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "Hello" {
		t.Fatalf("content: %+v", choice)
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(2) || usage["completion_tokens"] != float64(1) {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestFoldChatStreamToolCallsAndReasoning(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_content":"think"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"two","arguments":"{"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"one","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`
	out := FoldChatBytes([]byte(sse), "m")
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish: %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != nil || msg["reasoning_content"] != "think" {
		t.Fatalf("message: %+v", msg)
	}
	raw, _ := json.Marshal(msg["tool_calls"])
	want := `[{"id":"a","type":"function","function":{"name":"one","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"two","arguments":"{}"}}]`
	if string(raw) != want {
		t.Fatalf("tool calls:\n got %s\nwant %s", raw, want)
	}
}

func TestFoldChatStreamNilBody(t *testing.T) {
	out := FoldChatStream(nil, "m")
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "" {
		t.Fatalf("empty body: %+v", choice)
	}
}
