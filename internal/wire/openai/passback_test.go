package openai

import (
	"encoding/json"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func bodyFromJSON(t *testing.T, text string) wire.Body {
	t.Helper()
	var body wire.Body
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("bad fixture JSON: %v", err)
	}
	return body
}

func messages(body wire.Body) []wire.Body {
	var out []wire.Body
	for _, raw := range wire.AsSlice(body["messages"]) {
		out = append(out, wire.AsRecord(raw))
	}
	return out
}

func TestNeedsReasoningPassback(t *testing.T) {
	cases := []struct {
		provider, model, baseURL string
		want                     bool
	}{
		{"deepseek", "deepseek-v4-pro", "", true},
		{"openrouter", "deepseek/deepseek-v4-flash", "", true},
		{"moonshotai", "kimi-k2.5", "", true},
		{"custom", "gpt-4o", "https://api.deepseek.com/v1", true},
		{"openai", "gpt-4o", "", false},
	}
	for _, tc := range cases {
		if got := NeedsReasoningPassback(tc.provider, tc.model, tc.baseURL); got != tc.want {
			t.Errorf("NeedsReasoningPassback(%q, %q, %q) = %v, want %v",
				tc.provider, tc.model, tc.baseURL, got, tc.want)
		}
	}
}

func testTools() []any {
	return []any{
		wire.Body{"type": "function", "function": wire.Body{"name": "edit", "parameters": wire.Body{}}},
	}
}

func TestRepairRestoresCachedReasoning(t *testing.T) {
	cache := &PassbackCache{}
	namespace := "session-a"
	prior := []wire.Body{{"role": "user", "content": "fix the bug"}}
	assistant := wire.Body{
		"role":              "assistant",
		"content":           "",
		"reasoning_content": "I should call edit",
		"tool_calls": []any{
			wire.Body{
				"id":   "call_1",
				"type": "function",
				"function": wire.Body{
					"name":      "edit",
					"arguments": `{"path":"a.ts"}`,
				},
			},
		},
	}
	cache.RememberAssistantReasoning(assistant, prior, namespace)

	followUp := wire.Body{
		"model": "deepseek-v4-pro",
		"tools": testTools(),
		"messages": []any{
			prior[0],
			wire.Body{
				"role":       "assistant",
				"content":    "",
				"tool_calls": assistant["tool_calls"],
			},
			wire.Body{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
			wire.Body{"role": "user", "content": "continue"},
		},
	}

	repaired, stats := cache.RepairReasoningContent(followUp, namespace)
	msgs := messages(repaired)
	if got := msgs[1]["reasoning_content"]; got != "I should call edit" {
		t.Fatalf("reasoning_content = %v, want restored", got)
	}
	if stats.Patched != 1 || stats.EmptyFilled != 0 {
		t.Fatalf("stats = %+v, want patched=1 emptyFilled=0", stats)
	}
}

func TestRepairFillsEmptyStringOnMiss(t *testing.T) {
	cache := &PassbackCache{}
	body := wire.Body{
		"tools": testTools(),
		"messages": []any{
			wire.Body{"role": "user", "content": "hi"},
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					wire.Body{
						"id":   "call_missing",
						"type": "function",
						"function": wire.Body{
							"name":      "edit",
							"arguments": "{}",
						},
					},
				},
			},
			wire.Body{"role": "tool", "tool_call_id": "call_missing", "content": "done"},
		},
	}
	repaired, stats := cache.RepairReasoningContent(body, "ns")
	msgs := messages(repaired)
	got, ok := msgs[1]["reasoning_content"].(string)
	if !ok || got != "" {
		t.Fatalf("reasoning_content = %v (ok=%v), want \"\"", msgs[1]["reasoning_content"], ok)
	}
	if stats.EmptyFilled != 1 {
		t.Fatalf("stats = %+v, want emptyFilled=1", stats)
	}
}

func TestRepairLeavesBodiesWithoutToolsUntouched(t *testing.T) {
	cache := &PassbackCache{}
	body := wire.Body{
		"messages": []any{
			wire.Body{"role": "user", "content": "hi"},
			wire.Body{"role": "assistant", "content": "hello"},
		},
	}
	repaired, stats := cache.RepairReasoningContent(body, "ns")
	if stats.Patched != 0 || stats.EmptyFilled != 0 || stats.AlreadyPresent != 0 {
		t.Fatalf("stats = %+v, want all zero", stats)
	}
	if _, touched := repaired["messages"].([]any); !touched {
		t.Fatal("expected messages to pass through")
	}
}

func TestRepairKeepsClientReasoningAndRefreshesCache(t *testing.T) {
	cache := &PassbackCache{}
	namespace := "ns"
	call := wire.Body{
		"id":   "call_2",
		"type": "function",
		"function": wire.Body{
			"name":      "edit",
			"arguments": "{}",
		},
	}
	body := wire.Body{
		"tools": testTools(),
		"messages": []any{
			wire.Body{"role": "user", "content": "hi"},
			wire.Body{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "from client",
				"tool_calls":        []any{call},
			},
		},
	}
	_, stats := cache.RepairReasoningContent(body, namespace)
	if stats.AlreadyPresent != 1 {
		t.Fatalf("stats = %+v, want alreadyPresent=1", stats)
	}
	if cache.Size() == 0 {
		t.Fatal("cache should hold the client-supplied reasoning")
	}

	stripped := wire.Body{
		"tools": testTools(),
		"messages": []any{
			wire.Body{"role": "user", "content": "hi"},
			wire.Body{
				"role":       "assistant",
				"content":    "",
				"tool_calls": []any{call},
			},
		},
	}
	repaired, _ := cache.RepairReasoningContent(stripped, namespace)
	msgs := messages(repaired)
	if got := msgs[1]["reasoning_content"]; got != "from client" {
		t.Fatalf("reasoning_content = %v, want \"from client\"", got)
	}
}

func TestRepairLooksUpByToolCallIDWhenContentChanges(t *testing.T) {
	cache := &PassbackCache{}
	namespace := "ns"
	prior := []wire.Body{{"role": "user", "content": "task"}}
	cache.RememberAssistantReasoning(
		wire.Body{
			"role":              "assistant",
			"content":           "original",
			"reasoning_content": "think",
			"tool_calls": []any{
				wire.Body{
					"id":   "call_stable",
					"type": "function",
					"function": wire.Body{
						"name":      "read",
						"arguments": `{"path":"a"}`,
					},
				},
			},
		},
		prior,
		namespace,
	)

	repaired, _ := cache.RepairReasoningContent(wire.Body{
		"tools": testTools(),
		"messages": []any{
			prior[0],
			wire.Body{
				"role":    "assistant",
				"content": "", // Cursor often clears content on replay
				"tool_calls": []any{
					wire.Body{
						"id":   "call_stable",
						"type": "function",
						"function": wire.Body{
							"name":      "read",
							"arguments": `{"path":"a"}`,
						},
					},
				},
			},
		},
	}, namespace)
	msgs := messages(repaired)
	if got := msgs[1]["reasoning_content"]; got != "think" {
		t.Fatalf("reasoning_content = %v, want \"think\"", got)
	}
}

func TestRememberFromChatCompletion(t *testing.T) {
	cache := &PassbackCache{}
	prior := []wire.Body{{"role": "user", "content": "go"}}
	stored := cache.RememberFromChatCompletion(
		wire.Body{
			"choices": []any{
				wire.Body{
					"message": wire.Body{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "plan",
						"tool_calls": []any{
							wire.Body{
								"id":   "call_x",
								"type": "function",
								"function": wire.Body{
									"name":      "edit",
									"arguments": "{}",
								},
							},
						},
					},
				},
			},
		},
		prior,
		"ns",
	)
	if stored == 0 {
		t.Fatal("expected reasoning to be stored")
	}

	repaired, _ := cache.RepairReasoningContent(wire.Body{
		"tools": []any{wire.Body{"type": "function", "function": wire.Body{"name": "edit"}}},
		"messages": []any{
			prior[0],
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					wire.Body{
						"id":   "call_x",
						"type": "function",
						"function": wire.Body{
							"name":      "edit",
							"arguments": "{}",
						},
					},
				},
			},
		},
	}, "ns")
	msgs := messages(repaired)
	if got := msgs[1]["reasoning_content"]; got != "plan" {
		t.Fatalf("reasoning_content = %v, want \"plan\"", got)
	}
}

func TestReasoningStreamAccumulator(t *testing.T) {
	cache := &PassbackCache{}
	prior := []wire.Body{{"role": "user", "content": "stream"}}
	acc := NewReasoningStreamAccumulator(cache)
	acc.Ingest(wire.Body{
		"choices": []any{
			wire.Body{
				"index": 0.0,
				"delta": wire.Body{"role": "assistant", "reasoning_content": "step "},
			},
		},
	})
	acc.Ingest(wire.Body{
		"choices": []any{
			wire.Body{
				"index": 0.0,
				"delta": wire.Body{
					"reasoning_content": "two",
					"tool_calls": []any{
						wire.Body{
							"index": 0.0,
							"id":    "call_s",
							"type":  "function",
							"function": wire.Body{
								"name":      "edit",
								"arguments": "",
							},
						},
					},
				},
			},
		},
	})
	acc.Ingest(wire.Body{
		"choices": []any{
			wire.Body{
				"index":         0.0,
				"delta":         wire.Body{"tool_calls": []any{wire.Body{"index": 0.0, "function": wire.Body{"arguments": "{}"}}}},
				"finish_reason": "tool_calls",
			},
		},
	})
	if stored := acc.Store(prior, "stream-ns"); stored == 0 {
		t.Fatal("expected reasoning to be stored")
	}

	repaired, _ := cache.RepairReasoningContent(wire.Body{
		"tools": []any{wire.Body{"type": "function", "function": wire.Body{"name": "edit"}}},
		"messages": []any{
			prior[0],
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					wire.Body{
						"id":   "call_s",
						"type": "function",
						"function": wire.Body{
							"name":      "edit",
							"arguments": "{}",
						},
					},
				},
			},
		},
	}, "stream-ns")
	msgs := messages(repaired)
	if got := msgs[1]["reasoning_content"]; got != "step two" {
		t.Fatalf("reasoning_content = %v, want \"step two\"", got)
	}
}

func TestFingerprintsIgnoreReasoningContent(t *testing.T) {
	withReasoning := wire.Body{
		"role":              "assistant",
		"content":           "hi",
		"reasoning_content": "secret",
		"tool_calls": []any{
			wire.Body{"id": "c1", "type": "function", "function": wire.Body{"name": "a", "arguments": "{}"}},
		},
	}
	without := wire.Body{
		"role":    "assistant",
		"content": "hi",
		"tool_calls": []any{
			wire.Body{"id": "c1", "type": "function", "function": wire.Body{"name": "a", "arguments": "{}"}},
		},
	}
	if MessageSignature(withReasoning) != MessageSignature(without) {
		t.Fatal("message signature must ignore reasoning_content")
	}
	if ConversationScope([]wire.Body{withReasoning}, "") != ConversationScope([]wire.Body{without}, "") {
		t.Fatal("conversation scope must ignore reasoning_content")
	}
}

func TestReasoningCaptureTee(t *testing.T) {
	cache := &PassbackCache{}
	prior := []wire.Body{{"role": "user", "content": "stream"}}
	tee := NewReasoningCapture(prior, "ns", cache)

	stream := wire.NewTranslatorStream(&noopTranslator{}).WithTee(tee)
	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = stream.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"r1 \"}}]}\n\n"))
		_, _ = stream.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"r2\",\"tool_calls\":[{\"index\":0,\"id\":\"call_t\",\"type\":\"function\",\"function\":{\"name\":\"edit\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = stream.Write([]byte("data: [DONE]\n\n"))
		_ = stream.Close()
	}()
	buf := make([]byte, 4096)
	_, _ = stream.Read(buf)
	// The tee ingests on the writer goroutine; wait for it to finish every
	// chunk before reading what it stored, or the repair can run first.
	<-written

	repaired, _ := cache.RepairReasoningContent(wire.Body{
		"tools": []any{wire.Body{"type": "function", "function": wire.Body{"name": "edit"}}},
		"messages": []any{
			prior[0],
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					wire.Body{
						"id":   "call_t",
						"type": "function",
						"function": wire.Body{
							"name":      "edit",
							"arguments": "{}",
						},
					},
				},
			},
		},
	}, "ns")
	msgs := messages(repaired)
	if got := msgs[1]["reasoning_content"]; got != "r1 r2" {
		t.Fatalf("reasoning_content = %v, want \"r1 r2\"", got)
	}
}

type noopTranslator struct{}

func (noopTranslator) Handle(event wire.Body, sink wire.EventSink) { sink.EmitData(event) }
func (noopTranslator) Finish(sink wire.EventSink)                  { sink.Emit(wire.SSEDone) }
