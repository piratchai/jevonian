package responses

import (
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func TestToChatRequestStringInput(t *testing.T) {
	body := ToChatRequest(wire.Body{"input": "Say pong."}, "swe-1-6-slow")
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m := msgs[0].(wire.Body)
	if m["role"] != "user" || m["content"] != "Say pong." {
		t.Fatalf("message = %v", m)
	}
}

func TestToChatRequestMapsInputToolsReasoning(t *testing.T) {
	body := ToChatRequest(wire.Body{
		"model":        "ignored",
		"instructions": "be brief",
		"input": []any{
			wire.Body{"type": "message", "role": "user", "content": []any{wire.Body{"type": "input_text", "text": "hi"}}},
			wire.Body{"type": "function_call", "call_id": "call_1", "name": "edit", "arguments": "{}"},
			wire.Body{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
		"tools": []any{wire.Body{
			"type":        "function",
			"name":        "edit",
			"description": "edit a file",
			"parameters":  wire.Body{"type": "object", "properties": wire.Body{}},
		}},
		"reasoning":         wire.Body{"effort": "low"},
		"max_output_tokens": float64(512),
		"stream":            true,
	}, "openai/gpt-6-astra")
	if body["model"] != "openai/gpt-6-astra" || body["stream"] != true {
		t.Fatalf("head = %v", body)
	}
	if body["reasoning_effort"] != "low" || body["max_tokens"] != float64(512) {
		t.Fatalf("effort/max = %v", body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d: %v", len(msgs), msgs)
	}
	if msgs[0].(wire.Body)["role"] != "system" || msgs[0].(wire.Body)["content"] != "be brief" {
		t.Fatalf("system = %v", msgs[0])
	}
	if msgs[1].(wire.Body)["role"] != "user" || msgs[1].(wire.Body)["content"] != "hi" {
		t.Fatalf("user = %v", msgs[1])
	}
	assistant := msgs[2].(wire.Body)
	if assistant["role"] != "assistant" || assistant["content"] != nil {
		t.Fatalf("assistant = %v", assistant)
	}
	call := wire.AsSlice(assistant["tool_calls"])[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Fatalf("call = %v", call)
	}
	fn := wire.AsRecord(call["function"])
	if fn["name"] != "edit" || fn["arguments"] != "{}" {
		t.Fatalf("fn = %v", fn)
	}
	tool := msgs[3].(wire.Body)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "done" {
		t.Fatalf("tool = %v", tool)
	}
	tools := body["tools"].([]any)
	outFn := wire.AsRecord(tools[0].(map[string]any)["function"])
	if outFn["name"] != "edit" || outFn["description"] != "edit a file" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestChatToResponsesMapsMessagesTools(t *testing.T) {
	body := ChatToResponses(wire.Body{
		"model": "ignored",
		"messages": []any{
			wire.Body{"role": "system", "content": "be brief"},
			wire.Body{"role": "user", "content": "fix the bug"},
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{wire.Body{
					"id": "call_1", "type": "function",
					"function": wire.Body{"name": "edit", "arguments": "{}"},
				}},
			},
			wire.Body{"role": "tool", "tool_call_id": "call_1", "content": "Error: tests failed"},
		},
		"tools": []any{wire.Body{
			"type": "function",
			"function": wire.Body{
				"name":        "edit",
				"description": "edit a file",
				"parameters":  wire.Body{"type": "object", "properties": wire.Body{}},
			},
		}},
		"max_tokens":       float64(512),
		"temperature":      0.2,
		"reasoning_effort": "low",
	}, "gpt-5.6-codex")
	if body["model"] != "gpt-5.6-codex" || body["instructions"] != "be brief" {
		t.Fatalf("head = %v", body)
	}
	if body["max_output_tokens"] != float64(512) || body["store"] != false {
		t.Fatalf("fields = %v", body)
	}
	if _, present := body["temperature"]; present {
		t.Fatalf("Responses request must omit temperature: %v", body)
	}
	if body["reasoning"].(wire.Body)["effort"] != "low" {
		t.Fatalf("reasoning = %v", body["reasoning"])
	}
	input := body["input"].([]any)
	first := input[0].(wire.Body)
	if first["type"] != "message" || first["role"] != "user" {
		t.Fatalf("input[0] = %v", first)
	}
	part := wire.AsSlice(first["content"])[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "fix the bug" {
		t.Fatalf("part = %v", part)
	}
	call := input[1].(wire.Body)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "edit" {
		t.Fatalf("call = %v", call)
	}
	output := input[2].(wire.Body)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" || output["output"] != "Error: tests failed" {
		t.Fatalf("output = %v", output)
	}
	tools := body["tools"].([]any)
	tool := tools[0].(wire.Body)
	if tool["type"] != "function" || tool["name"] != "edit" || tool["strict"] != false {
		t.Fatalf("tool = %v", tool)
	}
	if tool["parameters"].(wire.Body)["type"] != "object" {
		t.Fatalf("parameters = %v", tool["parameters"])
	}
}

func TestChatToResponsesSynthesizesCallIDs(t *testing.T) {
	body := ChatToResponses(wire.Body{
		"messages": []any{
			wire.Body{"role": "user", "content": "fix"},
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{wire.Body{
					"id": "", "type": "function",
					"function": wire.Body{"name": "edit", "arguments": "{}"},
				}},
			},
			wire.Body{"role": "tool", "tool_call_id": "", "content": "done"},
		},
	}, "gpt-5.6-codex")
	input := body["input"].([]any)
	var call, output wire.Body
	for _, raw := range input {
		item := wire.AsRecord(raw)
		if item["type"] == "function_call" {
			call = item
		}
		if item["type"] == "function_call_output" {
			output = item
		}
	}
	callID := wire.AsString(call["call_id"])
	if callID == "" {
		t.Fatal("no call_id synthesized")
	}
	if output["call_id"] != callID {
		t.Fatalf("pairing broken: %v vs %v", output["call_id"], callID)
	}
}

func TestChatToResponsesPairsOrphanResults(t *testing.T) {
	body := ChatToResponses(wire.Body{
		"messages": []any{
			wire.Body{"role": "user", "content": "go"},
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					wire.Body{"id": "call_a", "type": "function", "function": wire.Body{"name": "a", "arguments": "{}"}},
					wire.Body{"type": "function", "function": wire.Body{"name": "b", "arguments": "{}"}},
				},
			},
			wire.Body{"role": "tool", "tool_call_id": "call_a", "content": "A"},
			wire.Body{"role": "tool", "content": "B"},
		},
	}, "gpt-5.6-codex")
	input := body["input"].([]any)
	var calls, outputs []wire.Body
	for _, raw := range input {
		item := wire.AsRecord(raw)
		if item["type"] == "function_call" {
			calls = append(calls, item)
		}
		if item["type"] == "function_call_output" {
			outputs = append(outputs, item)
		}
	}
	if len(calls) != 2 || len(outputs) != 2 {
		t.Fatalf("calls %v outputs %v", calls, outputs)
	}
	if calls[0]["call_id"] != "call_a" {
		t.Fatalf("call_a = %v", calls[0])
	}
	if wire.AsString(calls[1]["call_id"]) == "" {
		t.Fatal("second call missing id")
	}
	if outputs[0]["call_id"] != "call_a" {
		t.Fatalf("output0 = %v", outputs[0])
	}
	if outputs[1]["call_id"] != calls[1]["call_id"] {
		t.Fatalf("output1 %v != call1 %v", outputs[1]["call_id"], calls[1]["call_id"])
	}
}

func TestEnsureCallIDs(t *testing.T) {
	t.Run("fills empty ids and pairs outputs", func(t *testing.T) {
		body := EnsureCallIDs(wire.Body{
			"model": "gpt-5.6-codex",
			"input": []any{
				wire.Body{"type": "message", "role": "user", "content": []any{wire.Body{"type": "input_text", "text": "hi"}}},
				wire.Body{"type": "function_call", "call_id": "", "name": "edit", "arguments": "{}"},
				wire.Body{"type": "function_call_output", "call_id": "", "output": "done"},
			},
		})
		input := wire.AsSlice(body["input"])
		callID := wire.AsString(wire.AsRecord(input[1])["call_id"])
		if callID == "" {
			t.Fatal("call_id still empty")
		}
		if wire.AsRecord(input[2])["call_id"] != callID {
			t.Fatalf("output id %v != call id %v", wire.AsRecord(input[2])["call_id"], callID)
		}
	})

	t.Run("fills empty name from preceding sibling", func(t *testing.T) {
		body := EnsureCallIDs(wire.Body{
			"model": "gpt-6-astra",
			"input": []any{
				wire.Body{"type": "function_call", "call_id": "call_1", "name": "Read", "arguments": "{}"},
				wire.Body{"type": "function_call", "call_id": "", "name": "", "arguments": `{"path":"/tmp/shot.png"}`},
				wire.Body{"type": "function_call", "call_id": "call_3", "name": "Read", "arguments": "{}"},
			},
		})
		input := wire.AsSlice(body["input"])
		if wire.AsRecord(input[1])["name"] != "Read" {
			t.Fatalf("name = %v", wire.AsRecord(input[1]))
		}
		if wire.AsString(wire.AsRecord(input[1])["call_id"]) == "" {
			t.Fatal("call_id still empty")
		}
	})

	t.Run("falls back to tool when no sibling name", func(t *testing.T) {
		body := EnsureCallIDs(wire.Body{
			"model": "gpt-6-astra",
			"input": []any{wire.Body{"type": "function_call", "call_id": "call_1", "name": "", "arguments": "{}"}},
		})
		if wire.AsRecord(wire.AsSlice(body["input"])[0])["name"] != "tool" {
			t.Fatalf("name = %v", wire.AsSlice(body["input"])[0])
		}
	})

	t.Run("leaves valid call_id untouched", func(t *testing.T) {
		original := wire.Body{
			"model": "gpt-5.6-codex",
			"input": []any{
				wire.Body{"type": "function_call", "call_id": "call_1", "name": "edit", "arguments": "{}"},
				wire.Body{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			},
		}
		if got := EnsureCallIDs(original); got["input"] == nil || len(wire.AsSlice(got["input"])) != 2 {
			t.Fatalf("changed = %v", got)
		} else {
			if wire.AsRecord(wire.AsSlice(got["input"])[0])["call_id"] != "call_1" {
				t.Fatalf("call_id changed: %v", got)
			}
		}
	})

	t.Run("clamps oversized ids and keeps pairs", func(t *testing.T) {
		longID := "call_" + strings.Repeat("a", 80)
		body := EnsureCallIDs(wire.Body{
			"model": "gpt-5.6-codex",
			"input": []any{
				wire.Body{"type": "function_call", "call_id": longID, "name": "edit", "arguments": "{}"},
				wire.Body{"type": "function_call_output", "call_id": longID, "output": "ok"},
			},
		})
		input := wire.AsSlice(body["input"])
		callID := wire.AsString(wire.AsRecord(input[0])["call_id"])
		if callID == "" || len(callID) > 64 || callID == longID {
			t.Fatalf("call_id %q", callID)
		}
		if wire.AsRecord(input[1])["call_id"] != callID {
			t.Fatal("pairing broken")
		}
	})

	t.Run("splits newline-glued ids to short first segment", func(t *testing.T) {
		longID := "call-a627c24a-c58f-430e-a8e9-17999e8c4178-10\nfc_648db95d-2895-9d22-b9b6-4e975dc82a58_3"
		body := EnsureCallIDs(wire.Body{
			"model": "gpt-5.6-codex",
			"input": []any{
				wire.Body{"type": "function_call", "call_id": longID, "name": "edit", "arguments": "{}"},
				wire.Body{"type": "function_call_output", "call_id": longID, "output": "ok"},
			},
		})
		input := wire.AsSlice(body["input"])
		if wire.AsRecord(input[0])["call_id"] != "call-a627c24a-c58f-430e-a8e9-17999e8c4178-10" {
			t.Fatalf("call_id = %v", wire.AsRecord(input[0])["call_id"])
		}
		if wire.AsRecord(input[1])["call_id"] != wire.AsRecord(input[0])["call_id"] {
			t.Fatal("pairing broken")
		}
	})

	t.Run("same oversized id maps to same short id", func(t *testing.T) {
		longID := "fc_" + strings.Repeat("x", 86)
		body := EnsureCallIDs(wire.Body{
			"model": "gpt-5.6-codex",
			"input": []any{
				wire.Body{"type": "function_call", "call_id": longID, "name": "Read", "arguments": "{}"},
				wire.Body{"type": "function_call_output", "call_id": longID, "output": "a"},
				wire.Body{"type": "function_call", "call_id": longID, "name": "Read", "arguments": "{}"},
				wire.Body{"type": "function_call_output", "call_id": longID, "output": "b"},
			},
		})
		ids := map[string]bool{}
		for _, raw := range wire.AsSlice(body["input"]) {
			ids[wire.AsString(wire.AsRecord(raw)["call_id"])] = true
		}
		if len(ids) != 1 {
			t.Fatalf("ids = %v", ids)
		}
		for id := range ids {
			if len(id) > 64 {
				t.Fatalf("id %q too long", id)
			}
		}
	})
}

func TestChatToResponsesClampsLongToolCallID(t *testing.T) {
	longID := "toolu_" + strings.Repeat("b", 80)
	body := ChatToResponses(wire.Body{
		"messages": []any{
			wire.Body{"role": "user", "content": "go"},
			wire.Body{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{wire.Body{
					"id": longID, "type": "function",
					"function": wire.Body{"name": "edit", "arguments": "{}"},
				}},
			},
			wire.Body{"role": "tool", "tool_call_id": longID, "content": "done"},
		},
	}, "gpt-5.6-codex")
	input := body["input"].([]any)
	var call, output wire.Body
	for _, raw := range input {
		item := wire.AsRecord(raw)
		if item["type"] == "function_call" {
			call = item
		}
		if item["type"] == "function_call_output" {
			output = item
		}
	}
	if len(wire.AsString(call["call_id"])) > 64 {
		t.Fatalf("call_id = %v", call["call_id"])
	}
	if output["call_id"] != call["call_id"] {
		t.Fatal("pairing broken")
	}
}

func TestChatToResponsesEmptyToolName(t *testing.T) {
	body := ChatToResponses(wire.Body{
		"messages": []any{
			wire.Body{
				"role": "assistant",
				"tool_calls": []any{
					wire.Body{"id": "call_a", "type": "function", "function": wire.Body{"name": "Read", "arguments": "{}"}},
					wire.Body{"id": "", "type": "function", "function": wire.Body{"name": "", "arguments": `{"path":"/tmp/shot.png"}`}},
				},
			},
		},
	}, "gpt-6-astra")
	var calls []wire.Body
	for _, raw := range body["input"].([]any) {
		item := wire.AsRecord(raw)
		if item["type"] == "function_call" {
			calls = append(calls, item)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0]["name"] != "Read" || calls[0]["call_id"] != "call_a" {
		t.Fatalf("call0 = %v", calls[0])
	}
	if calls[1]["name"] != "Read" {
		t.Fatalf("call1 name = %v", calls[1])
	}
	if wire.AsString(calls[1]["call_id"]) == "" {
		t.Fatal("call1 missing id")
	}
}

func TestToChatRequestEmptyCallIDFallsBackToItemID(t *testing.T) {
	body := ToChatRequest(wire.Body{
		"input": []any{
			wire.Body{"type": "function_call", "call_id": "", "id": "fc_real", "name": "edit", "arguments": "{}"},
			wire.Body{"type": "function_call_output", "call_id": "", "id": "fc_real", "output": "done"},
		},
	}, "openai/gpt-6-astra")
	msgs := body["messages"].([]any)
	assistant := msgs[0].(wire.Body)
	call := wire.AsSlice(assistant["tool_calls"])[0].(map[string]any)
	if call["id"] != "fc_real" {
		t.Fatalf("call id = %v", call["id"])
	}
	tool := msgs[1].(wire.Body)
	if tool["tool_call_id"] != "fc_real" {
		t.Fatalf("tool_call_id = %v", tool["tool_call_id"])
	}
}

func TestToChatBridge(t *testing.T) {
	bridge := NewToChatBridge("gpt-5.6-codex")
	var sink wire.Collector
	bridge.Handle(wire.Body{"type": "response.created"}, &sink)
	bridge.Handle(wire.Body{"type": "response.output_text.delta", "delta": "hello "}, &sink)
	bridge.Handle(wire.Body{"type": "response.output_text.delta", "delta": "world"}, &sink)
	bridge.Handle(wire.Body{
		"type": "response.output_item.added",
		"item": wire.Body{"type": "function_call", "id": "fc_1", "call_id": "call_9", "name": "edit"},
	}, &sink)
	bridge.Handle(wire.Body{
		"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `{"path"`,
	}, &sink)
	bridge.Handle(wire.Body{
		"type": "response.completed", "response": wire.Body{"usage": wire.Body{"input_tokens": float64(12)}},
	}, &sink)

	events := wire.SplitSseEvents(sink.String()).Events
	if len(events) != 6 {
		t.Fatalf("events = %d: %s", len(events), sink.String())
	}
	deltaAt := func(i int) wire.Body {
		return wire.AsRecord(wire.AsRecord(wire.AsSlice(events[i]["choices"])[0])["delta"])
	}
	if d := deltaAt(0); d["role"] != "assistant" || d["content"] != "" {
		t.Fatalf("delta0 = %v", d)
	}
	if d := deltaAt(1); d["content"] != "hello " {
		t.Fatalf("delta1 = %v", d)
	}
	calls := wire.AsSlice(deltaAt(3)["tool_calls"])
	call := wire.AsRecord(calls[0])
	if call["id"] != "call_9" || wire.AsRecord(call["function"])["name"] != "edit" {
		t.Fatalf("call = %v", call)
	}
	argsDelta := wire.AsRecord(wire.AsSlice(deltaAt(4)["tool_calls"])[0])
	if wire.AsRecord(argsDelta["function"])["arguments"] != `{"path"` {
		t.Fatalf("args delta = %v", argsDelta)
	}
	last := wire.AsRecord(wire.AsSlice(events[len(events)-1]["choices"])[0])
	if last["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", last)
	}
	result := bridge.Result()
	if result.Content != "hello world" {
		t.Fatalf("content = %q", result.Content)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "call_9" ||
		result.ToolCalls[0].Name != "edit" || result.ToolCalls[0].Arguments != `{"path"` {
		t.Fatalf("tool calls = %+v", result.ToolCalls)
	}
	if result.Usage.Input != 12 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	// After response.completed, Finish emits nothing more.
	var tail wire.Collector
	bridge.Finish(&tail)
	if len(tail.Bytes()) != 0 {
		t.Fatalf("finish emitted: %s", tail.String())
	}
}

func TestToChatBridgeSoftFailure(t *testing.T) {
	bridge := NewToChatBridge("gpt-5.6-codex")
	var sink wire.Collector
	bridge.Handle(wire.Body{
		"type": "response.failed", "response": wire.Body{"error": wire.Body{"message": "boom"}},
	}, &sink)
	if bridge.Result().Failure != "boom" {
		t.Fatalf("failure = %q", bridge.Result().Failure)
	}
	bridge.Finish(&sink)
	out := sink.String()
	if !strings.Contains(out, "Jevonian hit an internal error") || !strings.Contains(out, "boom") {
		t.Fatalf("soft text = %s", out)
	}
	events := wire.SplitSseEvents(out).Events
	last := events[len(events)-1]
	choice := wire.AsRecord(wire.AsSlice(last["choices"])[0])
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choice)
	}
}

func TestFromChatBridgeErrorClosesSoftly(t *testing.T) {
	bridge := NewFromChatBridge("swe-1-6-slow")
	bridge.Handle(wire.Body{"choices": []any{wire.Body{"delta": wire.Body{"role": "assistant", "content": "partial"}}}})
	failed := bridge.Handle(wire.Body{"error": wire.Body{"message": "upstream refused", "type": "upstream_error"}})
	for _, event := range failed {
		if wire.AsRecord(event)["type"] == "response.failed" {
			t.Fatalf("hard failure emitted: %v", event)
		}
	}
	var sawDelta bool
	for _, event := range failed {
		if wire.AsRecord(event)["type"] == "response.output_text.delta" {
			sawDelta = true
			if !strings.Contains(wire.MarshalJSON(event), "Jevonian hit an internal error") ||
				!strings.Contains(wire.MarshalJSON(event), "upstream refused") {
				t.Fatalf("delta = %v", event)
			}
		}
	}
	if !sawDelta {
		t.Fatal("no output_text.delta emitted")
	}
	if got := bridge.Handle(wire.Body{"choices": []any{wire.Body{"delta": wire.Body{"content": "late"}}}}); len(got) != 0 {
		t.Fatalf("late = %v", got)
	}
	finish := bridge.Finish()
	last := wire.AsRecord(finish[len(finish)-1])
	if last["type"] != "response.completed" {
		t.Fatalf("last = %v", last)
	}
	for _, event := range finish {
		if wire.AsRecord(event)["type"] == "response.failed" {
			t.Fatalf("hard failure in finish: %v", event)
		}
	}
	if bridge.Result().Failure != "upstream refused" {
		t.Fatalf("failure = %q", bridge.Result().Failure)
	}
}

func TestFromChatBridgeChunksToEvents(t *testing.T) {
	bridge := NewFromChatBridge("openai/gpt-6-astra")
	var events []any
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{"role": "assistant", "content": "hello "}}},
	})...)
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{"content": "world"}}},
	})...)
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{}, "finish_reason": "stop"}},
		"usage":   wire.Body{"prompt_tokens": float64(3), "completion_tokens": float64(2)},
	})...)
	events = append(events, bridge.Finish()...)
	if wire.AsRecord(events[0])["type"] != "response.created" {
		t.Fatalf("first = %v", events[0])
	}
	var textDelta wire.Body
	for _, raw := range events {
		e := wire.AsRecord(raw)
		if e["type"] == "response.output_text.delta" {
			textDelta = e
			break
		}
	}
	if textDelta == nil {
		t.Fatal("no text delta")
	}
	if !strings.HasPrefix(wire.AsString(textDelta["item_id"]), "msg_") || textDelta["delta"] != "hello " {
		t.Fatalf("delta = %v", textDelta)
	}
	var sawDone bool
	for _, raw := range events {
		if wire.AsRecord(raw)["type"] == "response.output_item.done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("no output_item.done")
	}
	last := wire.AsRecord(events[len(events)-1])
	if last["type"] != "response.completed" {
		t.Fatalf("last = %v", last)
	}
	response := wire.AsRecord(last["response"])
	if response["status"] != "completed" {
		t.Fatalf("status = %v", response)
	}
	usage := wire.AsRecord(response["usage"])
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage = %v", usage)
	}
	if bridge.Result().Content != "hello world" {
		t.Fatalf("content = %q", bridge.Result().Content)
	}
}

func TestFromChatBridgeReasoningOnly(t *testing.T) {
	bridge := NewFromChatBridge("deepseek/deepseek-v4.1-flash")
	var events []any
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{"reasoning": "thinking hard"}}},
	})...)
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{}, "finish_reason": "stop"}},
		"usage":   wire.Body{"prompt_tokens": float64(1), "completion_tokens": float64(40)},
	})...)
	events = append(events, bridge.Finish()...)
	var sawReasoning bool
	for _, raw := range events {
		if wire.AsRecord(raw)["type"] == "response.reasoning_summary_text.delta" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatal("no reasoning delta")
	}
	last := wire.AsRecord(events[len(events)-1])
	if !strings.Contains(wire.MarshalJSON(wire.AsRecord(last["response"])["output"]), "thinking hard") {
		t.Fatalf("output = %v", last["response"])
	}
}

func TestFromChatBridgeToolCallDoneEvents(t *testing.T) {
	bridge := NewFromChatBridge("deepseek/deepseek-v4.1-flash")
	var events []any
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{
			"index": float64(0),
			"delta": wire.Body{"tool_calls": []any{wire.Body{
				"index": float64(0), "id": "call_abc", "type": "function",
				"function": wire.Body{"name": "exec_command", "arguments": `{"cmd":"ls"}`},
			}}},
		}},
	})...)
	events = append(events, bridge.Handle(wire.Body{
		"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{}, "finish_reason": "tool_calls"}},
	})...)
	events = append(events, bridge.Finish()...)
	var sawArgsDone bool
	doneItems := 0
	for _, raw := range events {
		e := wire.AsRecord(raw)
		if e["type"] == "response.function_call_arguments.done" {
			sawArgsDone = true
		}
		if e["type"] == "response.output_item.done" {
			doneItems++
		}
	}
	if !sawArgsDone {
		t.Fatal("no arguments.done")
	}
	if doneItems < 1 {
		t.Fatal("no output_item.done")
	}
	calls := bridge.Result().ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_abc" || calls[0].Name != "exec_command" || calls[0].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestChatResultFromResponse(t *testing.T) {
	result := ChatResultFromResponse(wire.Body{
		"output": []any{
			wire.Body{"type": "message", "content": []any{wire.Body{"type": "output_text", "text": "hi"}}},
			wire.Body{"type": "function_call", "call_id": "call_1", "name": "edit", "arguments": "{}"},
		},
		"usage": wire.Body{
			"input_tokens": float64(5), "output_tokens": float64(2),
			"input_tokens_details": wire.Body{"cached_tokens": float64(1)},
		},
	})
	if result.Content != "hi" || result.FinishReason != "tool_calls" {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage != (wire.Usage{Input: 5, Output: 2, CacheRead: 1, CacheWrite: 0}) {
		t.Fatalf("usage = %+v", result.Usage)
	}
	completion := ChatCompletionFrom(result, "gpt-5.6-codex", "chatcmpl-1", 0)
	if completion["object"] != "chat.completion" {
		t.Fatalf("object = %v", completion["object"])
	}
	choice := wire.AsRecord(wire.AsSlice(completion["choices"])[0])
	message := wire.AsRecord(choice["message"])
	if message["role"] != "assistant" || message["content"] != "hi" {
		t.Fatalf("message = %v", message)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", choice)
	}
}

func TestSplitSseEvents(t *testing.T) {
	result := wire.SplitSseEvents("event: response.output_text.delta\ndata: {\"type\":\"a\"}\n\ndata: {\"type\":\"b\"}\n\ndata: {\"type\":")
	if len(result.Events) != 2 || result.Events[0]["type"] != "a" || result.Events[1]["type"] != "b" {
		t.Fatalf("events = %v", result.Events)
	}
	if result.Rest != `data: {"type":` {
		t.Fatalf("rest = %q", result.Rest)
	}
	crlf := wire.SplitSseEvents("data: {\"type\":\"a\"}\r\n\r\ndata: {\"type\":\"b\"}\r\n\r\ndata: {\"type\":")
	if len(crlf.Events) != 2 || crlf.Events[0]["type"] != "a" || crlf.Events[1]["type"] != "b" {
		t.Fatalf("crlf events = %v", crlf.Events)
	}
	if crlf.Rest != `data: {"type":` {
		t.Fatalf("crlf rest = %q", crlf.Rest)
	}
}

func TestUsageCachedTokens(t *testing.T) {
	got := Usage(wire.Body{
		"input_tokens":         float64(10),
		"output_tokens":        float64(3),
		"input_tokens_details": wire.Body{"cached_tokens": float64(4)},
	})
	if got != (wire.Usage{Input: 10, Output: 3, CacheRead: 4, CacheWrite: 0}) {
		t.Fatalf("usage = %+v", got)
	}
}

func TestRemoteCompactionV2(t *testing.T) {
	if IsRemoteCompactionV2(wire.Body{"input": []any{wire.Body{"type": "message", "role": "user", "content": "x"}}}) {
		t.Fatal("false positive")
	}
	if !IsRemoteCompactionV2(wire.Body{"input": []any{wire.Body{"type": "compaction_trigger"}}}) {
		t.Fatal("compaction_trigger missed")
	}
	if !IsRemoteCompactionV2(wire.Body{"input": []any{wire.Body{"type": "context_compaction"}}}) {
		t.Fatal("context_compaction missed")
	}
}

func TestCompactionMarkersNotChatMessages(t *testing.T) {
	body := ToChatRequest(wire.Body{
		"input": []any{
			wire.Body{"type": "message", "role": "user", "content": []any{wire.Body{"type": "input_text", "text": "hi"}}},
			wire.Body{"type": "compaction_trigger"},
			wire.Body{"type": "compaction", "id": "cmp_1", "encrypted_content": "secret"},
		},
	}, "gpt-5.4")
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m := msgs[0].(wire.Body)
	if m["role"] != "user" || m["content"] != "hi" {
		t.Fatalf("message = %v", m)
	}
}

func TestRepairOutputFromItemDone(t *testing.T) {
	compaction := wire.Body{"type": "compaction", "id": "cmp_1", "encrypted_content": "payload"}
	events := []wire.Body{
		{"type": "response.output_item.done", "output_index": float64(0), "item": compaction},
		{"type": "response.completed", "response": wire.Body{
			"id": "resp_1", "output": []any{},
			"usage": wire.Body{"input_tokens": float64(1), "output_tokens": float64(1)},
		}},
	}
	collected := CollectOutputItems(events)
	if len(collected) != 1 {
		t.Fatalf("collected = %v", collected)
	}
	if wire.AsRecord(collected[0])["id"] != "cmp_1" {
		t.Fatalf("item = %v", collected[0])
	}
	repaired := RepairOutput(wire.Body{"id": "resp_1", "output": []any{}}, events)
	out := wire.AsSlice(repaired["output"])
	if len(out) != 1 || wire.AsRecord(out[0])["id"] != "cmp_1" {
		t.Fatalf("repaired = %v", repaired["output"])
	}
	untouched := RepairOutput(wire.Body{"id": "resp_1", "output": []any{wire.Body{"type": "message"}}}, events)
	if len(wire.AsSlice(untouched["output"])) != 1 || wire.AsRecord(wire.AsSlice(untouched["output"])[0])["type"] != "message" {
		t.Fatalf("untouched = %v", untouched["output"])
	}
}

func TestPassthroughRepairStream(t *testing.T) {
	var completed wire.Body
	stream := NewPassthroughRepairStream(func(response wire.Body) {
		completed = response
	})
	upstream := `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","id":"cmp_1","encrypted_content":"x"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4}}}` + "\n\n"
	output := string(wire.Pipe(stream, []byte(upstream)))
	result := wire.SplitSseEvents(output)
	var done wire.Body
	for _, event := range result.Events {
		if event["type"] == "response.completed" {
			done = event
		}
	}
	if done == nil {
		t.Fatalf("no completed event in %s", output)
	}
	out := wire.AsSlice(wire.AsRecord(done["response"])["output"])
	if len(out) != 1 || wire.AsRecord(out[0])["id"] != "cmp_1" {
		t.Fatalf("output = %v", done["response"])
	}
	if completed == nil {
		t.Fatal("onCompleted not called")
	}
	completedOut := wire.AsSlice(completed["output"])
	if len(completedOut) != 1 || wire.AsRecord(completedOut[0])["encrypted_content"] != "x" {
		t.Fatalf("completed = %v", completed["output"])
	}
}
