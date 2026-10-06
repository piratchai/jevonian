package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

// runToChat feeds Anthropic SSE frames through the bridge and returns output text.
func runToChat(t *testing.T, model string, frames []string, onFinish func(wire.Usage, string)) string {
	t.Helper()
	stream := NewToChatStream(model, onFinish, nil)
	input := strings.Join(frames, "")
	return string(wire.Pipe(stream, []byte(input)))
}

func sseFrame(payload wire.Body) string {
	return "event: x\ndata: " + wire.MarshalJSON(payload) + "\n\n"
}

func chatDeltas(t *testing.T, output string) []wire.Body {
	t.Helper()
	var chunks []wire.Body
	for _, block := range strings.Split(output, "\n\n") {
		if !strings.HasPrefix(block, "data: ") || strings.Contains(block, "[DONE]") {
			continue
		}
		var chunk wire.Body
		if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "data: ")), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", block, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

func TestToChatStream(t *testing.T) {
	var finished *wire.Usage
	stream := NewToChatStream("claude-fable-5-1", func(u wire.Usage, _ string) {
		finished = &u
	}, nil)
	frames := []string{
		sseFrame(wire.Body{"type": "message_start", "message": wire.Body{
			"usage": wire.Body{"input_tokens": float64(12), "cache_read_input_tokens": float64(5)},
		}}),
		sseFrame(wire.Body{"type": "content_block_start", "index": float64(0), "content_block": wire.Body{
			"type": "tool_use", "id": "toolu_1", "name": "edit",
		}}),
		sseFrame(wire.Body{"type": "content_block_delta", "index": float64(0), "delta": wire.Body{
			"type": "input_json_delta", "partial_json": `{"path"`,
		}}),
		sseFrame(wire.Body{"type": "content_block_delta", "index": float64(0), "delta": wire.Body{
			"type": "input_json_delta", "partial_json": `:"a"}`,
		}}),
		sseFrame(wire.Body{"type": "message_delta", "delta": wire.Body{"stop_reason": "tool_use"},
			"usage": wire.Body{"output_tokens": float64(7)}}),
	}
	output := string(wire.Pipe(stream, []byte(strings.Join(frames, ""))))
	chunks := chatDeltas(t, output)
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d, output = %s", len(chunks), output)
	}
	delta0 := wire.AsSlice(chunks[0]["choices"])[0]
	if d := wire.AsRecord(wire.AsRecord(delta0)["delta"]); d["role"] != "assistant" || d["content"] != "" {
		t.Fatalf("first delta = %v", d)
	}
	delta1 := wire.AsRecord(wire.AsRecord(wire.AsSlice(chunks[1]["choices"])[0])["delta"])
	calls := wire.AsSlice(delta1["tool_calls"])
	call := wire.AsRecord(calls[0])
	if call["id"] != "toolu_1" {
		t.Fatalf("call = %v", call)
	}
	if wire.AsRecord(call["function"])["name"] != "edit" {
		t.Fatalf("fn = %v", call)
	}
	delta2 := wire.AsRecord(wire.AsRecord(wire.AsSlice(chunks[2]["choices"])[0])["delta"])
	partial := wire.AsRecord(wire.AsSlice(delta2["tool_calls"])[0])["function"]
	if wire.AsRecord(partial)["arguments"] != `{"path"` {
		t.Fatalf("args delta = %v", delta2)
	}
	last := wire.AsSlice(chunks[len(chunks)-1]["choices"])[0]
	if wire.AsRecord(last)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", last)
	}
	if !strings.Contains(output, "data: [DONE]") {
		t.Fatal("missing [DONE]")
	}
	if finished == nil || finished.Input != 12 || finished.Output != 7 {
		t.Fatalf("finished = %+v", finished)
	}
}

func TestToChatStreamCacheUsageThroughResponsesHop(t *testing.T) {
	// Anthropic → Chat: cache reads ride on prompt_tokens_details.
	var anthUsage *wire.Usage
	toChat := NewToChatStream("claude-opus-4-7", func(u wire.Usage, _ string) {
		anthUsage = &u
	}, nil)
	events := []wire.Body{
		{"type": "message_start", "message": wire.Body{"usage": wire.Body{
			"input_tokens":                float64(10),
			"cache_read_input_tokens":     float64(900),
			"cache_creation_input_tokens": float64(50),
		}}},
		{"type": "content_block_delta", "index": float64(0), "delta": wire.Body{
			"type": "text_delta", "text": "ok"}},
		{"type": "message_delta", "delta": wire.Body{"stop_reason": "end_turn"},
			"usage": wire.Body{"output_tokens": float64(3)}},
	}
	var sse strings.Builder
	for _, event := range events {
		sse.WriteString(sseFrame(event))
	}
	chatOut := string(wire.Pipe(toChat, []byte(sse.String())))
	if anthUsage == nil || *anthUsage != (wire.Usage{Input: 10, Output: 3, CacheRead: 900, CacheWrite: 50}) {
		t.Fatalf("usage = %+v", anthUsage)
	}
	if !strings.Contains(chatOut, `"cached_tokens":900`) {
		t.Fatalf("cached tokens missing: %s", chatOut)
	}
}

func TestToChatStreamErrorAfterFinish(t *testing.T) {
	var failure string
	stream := NewToChatStream("claude-opus-4-7", func(_ wire.Usage, f string) {
		failure = f
	}, nil)
	frames := []string{
		sseFrame(wire.Body{"type": "message_start", "message": wire.Body{"usage": wire.Body{"input_tokens": float64(1)}}}),
		sseFrame(wire.Body{"type": "content_block_delta", "index": float64(0), "delta": wire.Body{
			"type": "text_delta", "text": "hi"}}),
		sseFrame(wire.Body{"type": "message_delta", "delta": wire.Body{"stop_reason": "end_turn"}, "usage": wire.Body{}}),
		sseFrame(wire.Body{"type": "error", "error": wire.Body{"type": "overloaded_error", "message": "Overloaded"}}),
	}
	output := string(wire.Pipe(stream, []byte(strings.Join(frames, ""))))
	var reasons []string
	for _, chunk := range chatDeltas(t, output) {
		for _, c := range wire.AsSlice(chunk["choices"]) {
			if r := wire.AsString(wire.AsRecord(c)["finish_reason"]); r != "" {
				reasons = append(reasons, r)
			}
		}
	}
	if len(reasons) != 1 || reasons[0] != "stop" {
		t.Fatalf("finish reasons = %v", reasons)
	}
	if failure != "Overloaded" {
		t.Fatalf("failure = %q", failure)
	}
}

// runChatTo feeds Chat chunks through the Chat→Anthropic bridge.
func runChatTo(t *testing.T, model string, chunks []wire.Body, options ChatToStreamOptions) []wire.Body {
	t.Helper()
	stream := NewChatToStream(model, options)
	var input strings.Builder
	for _, chunk := range chunks {
		input.WriteString("data: " + wire.MarshalJSON(chunk) + "\n\n")
	}
	input.WriteString("data: [DONE]\n\n")
	output := string(wire.Pipe(stream, []byte(input.String())))
	var events []wire.Body
	for _, block := range strings.Split(output, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event wire.Body
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatalf("bad event %q: %v", line, err)
			}
			events = append(events, event)
		}
	}
	return events
}

func chatDelta(delta wire.Body, finish any) wire.Body {
	return wire.Body{
		"id":     "chatcmpl-1",
		"object": "chat.completion.chunk",
		"choices": []any{wire.Body{
			"index":         float64(0),
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

func TestChatToStreamThinkingTextAndUsage(t *testing.T) {
	var finished *wire.Usage
	events := runChatTo(t, "claude-opus-4-8-medium", []wire.Body{
		chatDelta(wire.Body{"role": "assistant", "content": ""}, nil),
		chatDelta(wire.Body{"reasoning_content": "Let me think"}, nil),
		chatDelta(wire.Body{"reasoning_content": " harder."}, nil),
		chatDelta(wire.Body{"content": "pong"}, nil),
		chatDelta(wire.Body{}, "stop"),
		{"id": "chatcmpl-1", "choices": []any{}, "usage": wire.Body{"prompt_tokens": float64(10), "completion_tokens": float64(3)}},
	}, ChatToStreamOptions{
		Usage: func() *wire.Usage {
			return &wire.Usage{Input: 4, Output: 3, CacheRead: 6, CacheWrite: 2}
		},
		OnFinish: func(u wire.Usage) { finished = &u },
	})
	var types []string
	for _, e := range events {
		types = append(types, wire.AsString(e["type"]))
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types[%d] = %q, want %q (all: %v)", i, types[i], want[i], types)
		}
	}
	if wire.AsRecord(events[1]["content_block"])["type"] != "thinking" {
		t.Fatalf("block1 = %v", events[1])
	}
	if wire.AsRecord(events[2]["delta"])["thinking"] != "Let me think" {
		t.Fatalf("delta = %v", events[2])
	}
	if wire.AsRecord(events[5]["content_block"])["type"] != "text" {
		t.Fatalf("block5 = %v", events[5])
	}
	if wire.AsRecord(events[6]["delta"])["text"] != "pong" {
		t.Fatalf("text delta = %v", events[6])
	}
	msgDelta := events[8]
	if wire.AsRecord(msgDelta["delta"])["stop_reason"] != "end_turn" {
		t.Fatalf("stop = %v", msgDelta)
	}
	usage := wire.AsRecord(msgDelta["usage"])
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(3) ||
		usage["cache_read_input_tokens"] != float64(6) || usage["cache_creation_input_tokens"] != float64(2) {
		t.Fatalf("usage = %v", usage)
	}
	if finished == nil || *finished != (wire.Usage{Input: 4, Output: 3, CacheRead: 6, CacheWrite: 2}) {
		t.Fatalf("finished = %+v", finished)
	}
}

func TestChatToStreamToolCalls(t *testing.T) {
	events := runChatTo(t, "claude-opus-4-8-medium", []wire.Body{
		chatDelta(wire.Body{"content": "Checking."}, nil),
		chatDelta(wire.Body{"tool_calls": []any{wire.Body{
			"index": float64(0), "id": "call_abc", "type": "function",
			"function": wire.Body{"name": "get_weather", "arguments": ""},
		}}}, nil),
		chatDelta(wire.Body{"tool_calls": []any{wire.Body{
			"index": float64(0), "function": wire.Body{"arguments": `{"city":`},
		}}}, nil),
		chatDelta(wire.Body{"tool_calls": []any{wire.Body{
			"index": float64(0), "function": wire.Body{"arguments": `"Paris"}`},
		}}}, nil),
		chatDelta(wire.Body{}, "tool_calls"),
	}, ChatToStreamOptions{})
	var start wire.Body
	for _, e := range events {
		if e["type"] == "content_block_start" && wire.AsRecord(e["content_block"])["type"] == "tool_use" {
			start = e
		}
	}
	if start == nil {
		t.Fatal("no tool_use start")
	}
	block := wire.AsRecord(start["content_block"])
	if block["id"] != "call_abc" || block["name"] != "get_weather" {
		t.Fatalf("block = %v", block)
	}
	if start["index"] != float64(1) {
		t.Fatalf("index = %v", start["index"])
	}
	var jsonArgs strings.Builder
	for _, e := range events {
		if e["type"] == "content_block_delta" && wire.AsRecord(e["delta"])["type"] == "input_json_delta" {
			jsonArgs.WriteString(wire.AsString(wire.AsRecord(e["delta"])["partial_json"]))
		}
	}
	var parsed wire.Body
	if err := json.Unmarshal([]byte(jsonArgs.String()), &parsed); err != nil {
		t.Fatalf("args %q: %v", jsonArgs.String(), err)
	}
	if parsed["city"] != "Paris" {
		t.Fatalf("args = %v", parsed)
	}
	var stop wire.Body
	for _, e := range events {
		if e["type"] == "message_delta" {
			stop = e
		}
	}
	if wire.AsRecord(stop["delta"])["stop_reason"] != "tool_use" {
		t.Fatalf("stop = %v", stop)
	}
	opened, closed := 0, 0
	for _, e := range events {
		if e["type"] == "content_block_start" {
			opened++
		}
		if e["type"] == "content_block_stop" {
			closed++
		}
	}
	if opened != closed {
		t.Fatalf("opened %d != closed %d", opened, closed)
	}
}

func TestChatToStreamParallelToolsContiguous(t *testing.T) {
	events := runChatTo(t, "claude-opus-4-8-medium", []wire.Body{
		chatDelta(wire.Body{"tool_calls": []any{
			wire.Body{"index": float64(0), "id": "call_a", "function": wire.Body{"name": "a", "arguments": `{"a":`}},
			wire.Body{"index": float64(1), "id": "call_b", "function": wire.Body{"name": "b", "arguments": `{"b":`}},
		}}, nil),
		chatDelta(wire.Body{"tool_calls": []any{
			wire.Body{"index": float64(1), "function": wire.Body{"arguments": "2}"}},
			wire.Body{"index": float64(0), "function": wire.Body{"arguments": "1}"}},
		}}, nil),
		chatDelta(wire.Body{}, "tool_calls"),
	}, ChatToStreamOptions{})
	var names []string
	for _, e := range events {
		if e["type"] == "content_block_start" {
			names = append(names, wire.AsString(wire.AsRecord(e["content_block"])["name"]))
		}
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %v", names)
	}
	firstStop := -1
	secondStart := -1
	for i, e := range events {
		if e["type"] == "content_block_stop" && firstStop < 0 {
			firstStop = i
		}
		if e["type"] == "content_block_start" && e["index"] == float64(1) && secondStart < 0 {
			secondStart = i
		}
	}
	if firstStop < 0 || secondStart < 0 || firstStop >= secondStart {
		t.Fatalf("firstStop %d secondStart %d", firstStop, secondStart)
	}
	type idxDelta struct {
		index float64
		delta string
	}
	var args []idxDelta
	for _, e := range events {
		if e["type"] == "content_block_delta" {
			args = append(args, idxDelta{
				index: wire.Number(e["index"]),
				delta: wire.AsString(wire.AsRecord(e["delta"])["partial_json"]),
			})
		}
	}
	want := []idxDelta{
		{0, `{"a":`}, {0, "1}"},
		{1, `{"b":`}, {1, "2}"},
	}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %+v, want %+v (all: %v)", i, args[i], want[i], args)
		}
	}
}

func TestChatToStreamInclusiveUsageFallback(t *testing.T) {
	events := runChatTo(t, "claude-opus-4-8-medium", []wire.Body{
		chatDelta(wire.Body{"content": "hi"}, nil),
		{
			"choices": []any{wire.Body{"index": float64(0), "delta": wire.Body{}, "finish_reason": "length"}},
			"usage": wire.Body{
				"prompt_tokens":         float64(100),
				"completion_tokens":     float64(5),
				"prompt_tokens_details": wire.Body{"cached_tokens": float64(80)},
			},
		},
	}, ChatToStreamOptions{})
	var stop wire.Body
	for _, e := range events {
		if e["type"] == "message_delta" {
			stop = e
		}
	}
	if wire.AsRecord(stop["delta"])["stop_reason"] != "max_tokens" {
		t.Fatalf("stop = %v", stop)
	}
	usage := wire.AsRecord(stop["usage"])
	if usage["input_tokens"] != float64(20) || usage["output_tokens"] != float64(5) ||
		usage["cache_read_input_tokens"] != float64(80) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatToStreamSoftError(t *testing.T) {
	events := runChatTo(t, "claude-opus-4-8-medium", []wire.Body{
		chatDelta(wire.Body{"content": "partial"}, nil),
		{"error": wire.Body{"message": "rate limited", "type": "rate_limit_error"}},
	}, ChatToStreamOptions{})
	var text strings.Builder
	for _, e := range events {
		if e["type"] == "content_block_delta" {
			text.WriteString(wire.AsString(wire.AsRecord(e["delta"])["text"]))
		}
	}
	if !strings.Contains(text.String(), "partial") {
		t.Fatalf("text = %q", text.String())
	}
	if !strings.Contains(text.String(), "Jevonian hit an internal error") || !strings.Contains(text.String(), "rate limited") {
		t.Fatalf("soft error = %q", text.String())
	}
	for _, e := range events {
		if e["type"] == "error" {
			t.Fatalf("error event emitted: %v", e)
		}
	}
	if events[len(events)-1]["type"] != "message_stop" {
		t.Fatalf("last = %v", events[len(events)-1])
	}
}
