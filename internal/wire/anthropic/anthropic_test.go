package anthropic

import (
	"regexp"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func TestNeedsWire(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-fable-5-1":            true,
		"anthropic/claude-sonnet-4.6": true,
		"deepseek-v4.1-flash":         false,
		"z-ai/glm-5.3-flash":          false,
	} {
		if got := NeedsWire(model); got != want {
			t.Errorf("NeedsWire(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestToChatRequestRoundTrip(t *testing.T) {
	request := ToChatRequest(wire.Body{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": float64(16),
		"messages":   []any{wire.Body{"role": "user", "content": "hi"}},
	}, "deepseek/deepseek-v4.1-flash")
	if request["model"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("model = %v", request["model"])
	}
	if request["max_tokens"] != float64(16) {
		t.Fatalf("max_tokens = %v", request["max_tokens"])
	}
	msgs := request["messages"].([]any)
	first := msgs[0].(wire.Body)
	if first["role"] != "user" || first["content"] != "hi" {
		t.Fatalf("message = %v", first)
	}

	response := ChatToMessage(wire.Body{
		"id":    "chatcmpl-1",
		"model": "deepseek/deepseek-v4.1-flash",
		"choices": []any{wire.Body{
			"message":       wire.Body{"role": "assistant", "content": "hello"},
			"finish_reason": "stop",
		}},
		"usage": wire.Body{"prompt_tokens": float64(3), "completion_tokens": float64(1)},
	}, "deepseek/deepseek-v4.1-flash")
	if response["type"] != "message" || response["role"] != "assistant" {
		t.Fatalf("response head = %v", response)
	}
	content := response["content"].([]any)
	if content[0].(wire.Body)["text"] != "hello" {
		t.Fatalf("content = %v", content)
	}
	if response["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", response["stop_reason"])
	}
	usage := response["usage"].(wire.Body)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(1) {
		t.Fatalf("usage = %v", usage)
	}
}

// A Chat body's prompt_tokens includes the cache reads, but Anthropic reports
// the uncached share in input_tokens plus a separate cache_read count. The fold
// must split them so a Messages client sees the same accounting as the stream.
func TestChatToMessageSplitsCachedInput(t *testing.T) {
	response := ChatToMessage(wire.Body{
		"id":    "chatcmpl-1",
		"model": "swe-2-max",
		"choices": []any{wire.Body{
			"message":       wire.Body{"role": "assistant", "content": "hello"},
			"finish_reason": "stop",
		}},
		"usage": wire.Body{
			"prompt_tokens":         float64(18519),
			"completion_tokens":     float64(16),
			"prompt_tokens_details": wire.Body{"cached_tokens": float64(9473)},
		},
	}, "swe-2-max")
	usage := response["usage"].(wire.Body)
	if usage["input_tokens"] != float64(9046) || usage["output_tokens"] != float64(16) {
		t.Fatalf("usage = %v", usage)
	}
	if usage["cache_read_input_tokens"] != float64(9473) {
		t.Fatalf("cache_read_input_tokens = %v", usage["cache_read_input_tokens"])
	}
}

func TestChatToAnthropicMapsMessagesToolsAndResults(t *testing.T) {
	request := ChatToAnthropic(wire.Body{
		"model": "ignored",
		"messages": []any{
			wire.Body{"role": "system", "content": "be brief"},
			wire.Body{"role": "user", "content": "fix the bug"},
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{wire.Body{
					"id":   "call_1",
					"type": "function",
					"function": wire.Body{
						"name":      "read",
						"arguments": `{"p":"a"}`,
					},
				}},
			},
			wire.Body{"role": "tool", "tool_call_id": "call_1", "content": "Error: test failed"},
		},
		"tools": []any{wire.Body{
			"type": "function",
			"function": wire.Body{
				"name":        "read",
				"description": "read a file",
				"parameters":  wire.Body{"type": "object"},
			},
		}},
		"max_tokens":  float64(512),
		"temperature": 0.2,
		"tool_choice": wire.Body{"type": "function", "function": wire.Body{"name": "read"}},
	})

	system := request["system"].([]any)
	sysBlock := system[0].(wire.Body)
	if sysBlock["text"] != "be brief" {
		t.Fatalf("system = %v", system)
	}
	if sysBlock["cache_control"].(wire.Body)["type"] != "ephemeral" {
		t.Fatalf("system cache_control missing: %v", sysBlock)
	}
	if request["max_tokens"] != float64(512) || request["temperature"] != 0.2 {
		t.Fatalf("scalars = %v", request)
	}
	messages := request["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, %v", len(messages), messages)
	}
	user := messages[0].(wire.Body)
	if user["role"] != "user" {
		t.Fatalf("user = %v", user)
	}
	assistant := messages[1].(wire.Body)
	toolUse := assistant["content"].([]any)[0].(wire.Body)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call_1" || toolUse["name"] != "read" {
		t.Fatalf("tool_use = %v", toolUse)
	}
	input := toolUse["input"].(wire.Body)
	if input["p"] != "a" {
		t.Fatalf("tool input = %v", input)
	}
	toolResult := messages[2].(wire.Body)["content"].([]any)[0].(wire.Body)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result = %v", toolResult)
	}
	if toolResult["content"] != "Error: test failed" {
		t.Fatalf("tool_result content = %v", toolResult["content"])
	}
	if toolResult["cache_control"].(wire.Body)["type"] != "ephemeral" {
		t.Fatalf("tool_result cache_control = %v", toolResult)
	}
	tools := request["tools"].([]any)
	tool := tools[0].(wire.Body)
	if tool["name"] != "read" || tool["description"] != "read a file" {
		t.Fatalf("tool = %v", tool)
	}
	if tool["input_schema"].(wire.Body)["type"] != "object" {
		t.Fatalf("input_schema = %v", tool["input_schema"])
	}
	if tool["cache_control"].(wire.Body)["type"] != "ephemeral" {
		t.Fatalf("tool cache_control = %v", tool)
	}
	choice := request["tool_choice"].(wire.Body)
	if choice["type"] != "tool" || choice["name"] != "read" {
		t.Fatalf("tool_choice = %v", choice)
	}
}

func TestChatToAnthropicCacheBreakpoints(t *testing.T) {
	request := ChatToAnthropic(wire.Body{
		"messages": []any{
			wire.Body{"role": "system", "content": "rules"},
			wire.Body{"role": "user", "content": "one"},
			wire.Body{"role": "assistant", "content": "two"},
			wire.Body{"role": "user", "content": "three"},
		},
		"tools": []any{
			wire.Body{"type": "function", "function": wire.Body{"name": "a", "parameters": wire.Body{"type": "object"}}},
			wire.Body{"type": "function", "function": wire.Body{"name": "b", "parameters": wire.Body{"type": "object"}}},
		},
	})
	system := request["system"].([]any)[0].(wire.Body)
	if system["cache_control"] == nil {
		t.Fatal("system cache_control missing")
	}
	tools := request["tools"].([]any)
	if tools[0].(wire.Body)["cache_control"] != nil {
		t.Fatalf("first tool should not be cached: %v", tools[0])
	}
	if tools[1].(wire.Body)["cache_control"] == nil {
		t.Fatalf("last tool must be cached: %v", tools[1])
	}
	messages := request["messages"].([]any)
	first := messages[0].(wire.Body)["content"].([]any)[0].(wire.Body)
	if first["cache_control"] != nil {
		t.Fatalf("first message should not be cached: %v", first)
	}
	last := messages[len(messages)-1].(wire.Body)["content"].([]any)
	lastBlock := last[len(last)-1].(wire.Body)
	if lastBlock["cache_control"] == nil {
		t.Fatalf("last message block must be cached: %v", lastBlock)
	}
}

func TestChatToAnthropicDefaultsMaxTokens(t *testing.T) {
	request := ChatToAnthropic(wire.Body{
		"messages": []any{wire.Body{"role": "user", "content": "hi"}},
	})
	if request["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v", request["max_tokens"])
	}
}

func TestChatToAnthropicFlattensArrayToolContent(t *testing.T) {
	request := ChatToAnthropic(wire.Body{
		"messages": []any{
			wire.Body{
				"role":         "tool",
				"tool_call_id": "call_1",
				"content":      []any{wire.Body{"type": "text", "text": "line one"}},
			},
		},
	})
	messages := request["messages"].([]any)
	result := messages[0].(wire.Body)["content"].([]any)[0].(wire.Body)
	if result["content"] != "line one" {
		t.Fatalf("content = %v", result)
	}
}

func TestToolIDSanitizesCharsetAndPairs(t *testing.T) {
	dirty := "call:abc.def/1"
	request := ChatToAnthropic(wire.Body{
		"messages": []any{
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{wire.Body{
					"id":       dirty,
					"type":     "function",
					"function": wire.Body{"name": "read", "arguments": "{}"},
				}},
			},
			wire.Body{"role": "tool", "tool_call_id": dirty, "content": "ok"},
		},
	})
	messages := request["messages"].([]any)
	useID := messages[0].(wire.Body)["content"].([]any)[0].(wire.Body)["id"].(string)
	resultID := messages[1].(wire.Body)["content"].([]any)[0].(wire.Body)["tool_use_id"].(string)
	if !regexp.MustCompile(`^call_abc_def_1_[a-f0-9]{8}$`).MatchString(useID) {
		t.Fatalf("use id %q", useID)
	}
	if resultID != useID {
		t.Fatalf("result id %q != use id %q", resultID, useID)
	}
}

func TestToolIDDistinctForPunctuation(t *testing.T) {
	ids := map[string]bool{}
	for _, raw := range []string{"call:a", "call.a", "call_a"} {
		ids[ToolID(raw)] = true
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	if ToolID("call_a") != "call_a" {
		t.Fatalf("clean id changed: %q", ToolID("call_a"))
	}
	for id := range ids {
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(id) {
			t.Fatalf("id %q fails charset", id)
		}
	}
}

func TestToolIDTruncatesLongIDs(t *testing.T) {
	id := ToolID("call." + strings.Repeat("x", 200))
	if len(id) > 64 {
		t.Fatalf("len = %d", len(id))
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(id) {
		t.Fatalf("id %q fails charset", id)
	}
}

func TestToolIDHashesEmptyDeterministically(t *testing.T) {
	request := ChatToAnthropic(wire.Body{
		"messages": []any{
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{wire.Body{
					"id":       "",
					"type":     "function",
					"function": wire.Body{"name": "read", "arguments": "{}"},
				}},
			},
			wire.Body{"role": "tool", "tool_call_id": "", "content": "ok"},
		},
	})
	messages := request["messages"].([]any)
	useID := messages[0].(wire.Body)["content"].([]any)[0].(wire.Body)["id"].(string)
	if !regexp.MustCompile(`^tool_[a-f0-9]{24}$`).MatchString(useID) {
		t.Fatalf("use id %q", useID)
	}
	resultID := messages[1].(wire.Body)["content"].([]any)[0].(wire.Body)["tool_use_id"].(string)
	if resultID != useID {
		t.Fatalf("paired id mismatch: %q vs %q", resultID, useID)
	}
}

func TestNormalizePrefill(t *testing.T) {
	body := func(model string, messages ...any) wire.Body {
		return wire.Body{"model": model, "messages": messages}
	}
	roles := func(b wire.Body) []string {
		var out []string
		for _, m := range wire.AsSlice(b["messages"]) {
			out = append(out, wire.AsString(wire.AsRecord(m)["role"]))
		}
		return out
	}

	t.Run("drops trailing assistant prefill on rejecting model", func(t *testing.T) {
		out := NormalizePrefill(body("claude-opus-5-5",
			wire.Body{"role": "user", "content": "do it"},
			wire.Body{"role": "assistant", "content": "working on it"},
			wire.Body{"role": "user", "content": "continue"},
			wire.Body{"role": "assistant", "content": "Sure, here is the "},
		))
		got := roles(out)
		if len(got) != 3 || got[0] != "user" || got[1] != "assistant" || got[2] != "user" {
			t.Fatalf("roles = %v", got)
		}
		msgs := wire.AsSlice(out["messages"])
		if wire.AsRecord(msgs[len(msgs)-1])["content"] != "continue" {
			t.Fatalf("last = %v", msgs[len(msgs)-1])
		}
	})

	t.Run("leaves user-terminated conversation untouched", func(t *testing.T) {
		in := body("claude-opus-5-5",
			wire.Body{"role": "user", "content": "hi"},
			wire.Body{"role": "assistant", "content": "hello"},
			wire.Body{"role": "user", "content": "next"},
		)
		out := NormalizePrefill(in)
		if len(wire.AsSlice(out["messages"])) != 3 {
			t.Fatalf("messages = %v", out["messages"])
		}
	})

	t.Run("does not touch prefill-supporting model", func(t *testing.T) {
		in := body("claude-sonnet-4-5-20250929",
			wire.Body{"role": "user", "content": "hi"},
			wire.Body{"role": "assistant", "content": "Sure, "},
		)
		out := NormalizePrefill(in)
		if len(wire.AsSlice(out["messages"])) != 2 {
			t.Fatalf("messages = %v", out["messages"])
		}
	})

	t.Run("keeps trailing tool_use awaiting result", func(t *testing.T) {
		in := body("claude-opus-5-5",
			wire.Body{"role": "user", "content": "read the file"},
			wire.Body{
				"role": "assistant",
				"content": []any{wire.Body{
					"type": "tool_use", "id": "toolu_1", "name": "read",
					"input": wire.Body{"path": "a"},
				}},
			},
		)
		out := NormalizePrefill(in)
		if len(wire.AsSlice(out["messages"])) != 2 {
			t.Fatalf("messages = %v", out["messages"])
		}
	})

	t.Run("does not strip assistant preceding tool_result", func(t *testing.T) {
		in := body("claude-opus-5-5",
			wire.Body{"role": "user", "content": "read the file"},
			wire.Body{
				"role": "assistant",
				"content": []any{wire.Body{
					"type": "tool_use", "id": "toolu_1", "name": "read",
					"input": wire.Body{"path": "a"},
				}},
			},
			wire.Body{
				"role": "user",
				"content": []any{wire.Body{
					"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok",
				}},
			},
		)
		out := NormalizePrefill(in)
		if len(wire.AsSlice(out["messages"])) != 3 {
			t.Fatalf("messages = %v", out["messages"])
		}
	})

	t.Run("relocates assistant-only body to user turn", func(t *testing.T) {
		out := NormalizePrefill(body("claude-opus-5-5",
			wire.Body{"role": "assistant", "content": "Continue from here:"},
		))
		msgs := wire.AsSlice(out["messages"])
		if len(msgs) != 1 {
			t.Fatalf("messages = %v", msgs)
		}
		m := wire.AsRecord(msgs[0])
		if m["role"] != "user" || m["content"] != "Continue from here:" {
			t.Fatalf("message = %v", m)
		}
	})

	t.Run("falls back when assistant body is empty", func(t *testing.T) {
		out := NormalizePrefill(body("claude-opus-5-5",
			wire.Body{"role": "assistant", "content": ""},
		))
		msgs := wire.AsSlice(out["messages"])
		m := wire.AsRecord(msgs[0])
		if m["role"] != "user" {
			t.Fatalf("message = %v", m)
		}
		content, ok := m["content"].(string)
		if !ok || len(content) == 0 {
			t.Fatalf("content = %v", m["content"])
		}
	})

	t.Run("normalizes the real bridge repro", func(t *testing.T) {
		bridged := ChatToAnthropic(wire.Body{
			"model": "ignored",
			"messages": []any{
				wire.Body{"role": "user", "content": "read the file"},
				wire.Body{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{wire.Body{
						"id": "call_1", "type": "function",
						"function": wire.Body{"name": "read", "arguments": `{"p":"a"}`},
					}},
				},
				wire.Body{"role": "tool", "tool_call_id": "call_1", "content": "file contents"},
				wire.Body{"role": "assistant", "content": "The file says hello."},
			},
		})
		bridged["model"] = "claude-opus-5-5"
		repaired := NormalizePrefill(bridged)
		got := roles(repaired)
		if len(got) != 3 || got[2] != "user" {
			t.Fatalf("roles = %v", got)
		}
		msgs := wire.AsSlice(repaired["messages"])
		last := wire.AsRecord(msgs[len(msgs)-1])
		first := wire.AsSlice(last["content"])[0]
		if wire.AsRecord(first)["type"] != "tool_result" {
			t.Fatalf("last = %v", last)
		}
	})
}

func TestToChatTranslatesToolUse(t *testing.T) {
	completion := ToChat(wire.Body{
		"id":    "msg_1",
		"model": "claude-fable-5-1",
		"content": []any{
			wire.Body{"type": "text", "text": "hello "},
			wire.Body{"type": "tool_use", "id": "toolu_1", "name": "edit", "input": wire.Body{"path": "a"}},
		},
		"stop_reason": "tool_use",
		"usage": wire.Body{
			"input_tokens":            float64(10),
			"output_tokens":           float64(4),
			"cache_read_input_tokens": float64(2),
		},
	}, "fallback")
	if completion["id"] != "msg_1" {
		t.Fatalf("id = %v", completion["id"])
	}
	choices := completion["choices"].([]any)
	message := choices[0].(wire.Body)["message"].(wire.Body)
	if message["content"] != "hello " {
		t.Fatalf("content = %v", message["content"])
	}
	calls := message["tool_calls"].([]any)
	call := calls[0].(wire.Body)
	if call["id"] != "toolu_1" {
		t.Fatalf("call id = %v", call["id"])
	}
	fn := call["function"].(wire.Body)
	if fn["name"] != "edit" || fn["arguments"] != `{"path":"a"}` {
		t.Fatalf("fn = %v", fn)
	}
	if choices[0].(wire.Body)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", choices[0])
	}
	usage := completion["usage"].(wire.Body)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(4) || usage["total_tokens"] != float64(14) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestResultStopReasons(t *testing.T) {
	for stop, want := range map[string]string{
		"end_turn":   "stop",
		"max_tokens": "length",
		"refusal":    "content_filter",
	} {
		if got := ResultOf(wire.Body{"stop_reason": stop}).FinishReason; got != want {
			t.Errorf("stop %q -> %q, want %q", stop, got, want)
		}
	}
}
