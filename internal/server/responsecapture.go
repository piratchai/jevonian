package server

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/xinyao27/jevonian/internal/upstream"
	"github.com/xinyao27/jevonian/internal/wire"
)

// The response half of a body capture: how much of the model's answer the log
// detail view can show beside the request. Everything here is defensive — a
// capture must never break or slow a live turn.

const (
	// maxCaptureStreamBytes bounds the raw client-wire bytes teed off a stream
	// before the tee stops buffering and marks the capture truncated.
	maxCaptureStreamBytes = 512 << 10
	// maxCaptureFieldBytes bounds the text and reasoning fields of a normalized
	// response so one huge answer cannot grow the body file without limit.
	maxCaptureFieldBytes = 256 << 10
)

// streamCapture accumulates client-wire stream bytes into a bounded buffer. The
// streaming goroutine writes it and the recorder reads it once after the stream
// ends, so every access is mutex-guarded.
type streamCapture struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

// write appends bytes until the cap, then stops and marks the capture
// truncated. It never blocks or fails the stream.
func (c *streamCapture) write(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return
	}
	remaining := maxCaptureStreamBytes - len(c.buf)
	if remaining <= 0 {
		c.truncated = true
		return
	}
	if len(p) > remaining {
		c.buf = append(c.buf, p[:remaining]...)
		c.truncated = true
		return
	}
	c.buf = append(c.buf, p...)
}

// snapshot returns a copy of the buffered bytes and whether the capture hit the
// cap. The copy keeps the caller independent of the streaming goroutine.
func (c *streamCapture) snapshot() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...), c.truncated
}

// teeReadCloser copies every byte it reads into capture, then hands it on
// unchanged. The stream seam wraps the client-wire body with it.
type teeReadCloser struct {
	source  io.ReadCloser
	capture *streamCapture
}

// Read implements io.Reader.
func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.source.Read(p)
	if n > 0 {
		t.capture.write(p[:n])
	}
	return n, err
}

// Close implements io.Closer.
func (t *teeReadCloser) Close() error { return t.source.Close() }

// normalizeResponse extracts a wire-agnostic summary of a model answer from the
// client-wire bytes a turn delivered. kind is the client wire; stream selects
// the SSE grammar. Unknown or malformed input yields an empty text and never
// panics. status is a placeholder the recorder overwrites with the ledger row's
// real status.
func normalizeResponse(kind upstream.ClientKind, stream bool, data []byte) map[string]any {
	out := map[string]any{
		"wire":   string(kind),
		"status": 200,
		"stream": stream,
	}

	var text, reasoning string
	var toolCalls []any
	var finishReason string
	switch kind {
	case upstream.KindAnthropic:
		if stream {
			text, reasoning, toolCalls, finishReason = parseAnthropicSSE(data)
		} else {
			text, reasoning, toolCalls, finishReason = parseAnthropicJSON(data)
		}
	case upstream.KindResponses:
		if stream {
			text, reasoning, toolCalls, finishReason = parseResponsesSSE(data)
		} else {
			text, reasoning, toolCalls, finishReason = parseResponsesJSON(data)
		}
	default:
		if stream {
			text, reasoning, toolCalls, finishReason = parseOpenAISSE(data)
		} else {
			text, reasoning, toolCalls, finishReason = parseOpenAIJSON(data)
		}
	}

	truncated := false
	var cut bool
	if text, cut = capCaptureText(text); cut {
		truncated = true
	}
	if reasoning, cut = capCaptureText(reasoning); cut {
		truncated = true
	}

	out["text"] = text
	if reasoning != "" {
		out["reasoning"] = reasoning
	}
	if len(toolCalls) > 0 {
		out["toolCalls"] = toolCalls
	}
	if finishReason != "" {
		out["finishReason"] = finishReason
	}
	if truncated {
		out["truncated"] = true
	}
	return out
}

// capCaptureText cuts text to the field cap on a rune boundary so the result
// stays valid UTF-8.
func capCaptureText(s string) (string, bool) {
	if len(s) <= maxCaptureFieldBytes {
		return s, false
	}
	cut := maxCaptureFieldBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// argumentsString renders a tool call's arguments as the raw JSON text the wire
// carries. It is always a string: a streamed OpenAI/Responses value passes
// through, an Anthropic structured `input` is marshaled.
func argumentsString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if value == nil {
		return ""
	}
	return wire.MarshalJSON(value)
}

// ---- OpenAI Chat Completions ----------------------------------------------

// parseOpenAIJSON reads a non-stream chat.completion.
func parseOpenAIJSON(data []byte) (text, reasoning string, toolCalls []any, finishReason string) {
	var body wire.Body
	if err := json.Unmarshal(data, &body); err != nil {
		return "", "", nil, ""
	}
	return openAIFields(body)
}

// openAIFields pulls the assistant turn out of a chat completion or chunk.
func openAIFields(body wire.Body) (text, reasoning string, toolCalls []any, finishReason string) {
	choices := wire.AsSlice(body["choices"])
	if len(choices) == 0 {
		return "", "", nil, ""
	}
	choice := wire.AsRecord(choices[0])
	finishReason = wire.AsString(choice["finish_reason"])
	message := wire.AsRecord(choice["message"])
	if len(message) == 0 {
		message = wire.AsRecord(choice["delta"])
	}
	text = wire.TextOf(message["content"])
	reasoning = wire.FirstNonEmpty(message["reasoning_content"], message["reasoning"])
	toolCalls = openAIToolCalls(message["tool_calls"])
	return text, reasoning, toolCalls, finishReason
}

// openAIToolCalls normalizes a chat completion `tool_calls` array.
func openAIToolCalls(raw any) []any {
	var out []any
	for i, item := range wire.AsSlice(raw) {
		call := wire.AsRecord(item)
		fn := wire.AsRecord(call["function"])
		id := wire.AsString(call["id"])
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out = append(out, map[string]any{
			"id":        id,
			"name":      wire.AsString(fn["name"]),
			"arguments": argumentsString(fn["arguments"]),
		})
	}
	return out
}

// parseOpenAISSE folds a chat.completion.chunk stream, accumulating text,
// reasoning, and tool-call argument fragments split across chunks.
func parseOpenAISSE(data []byte) (text, reasoning string, toolCalls []any, finishReason string) {
	var textB, reasonB strings.Builder
	type pendingCall struct{ id, name, args string }
	calls := map[int]*pendingCall{}
	var order []int
	for _, event := range wire.SplitSseEvents(string(data)).Events {
		choices := wire.AsSlice(event["choices"])
		if len(choices) == 0 {
			continue
		}
		choice := wire.AsRecord(choices[0])
		if fr := wire.AsString(choice["finish_reason"]); fr != "" {
			finishReason = fr
		}
		delta := wire.AsRecord(choice["delta"])
		textB.WriteString(wire.TextOf(delta["content"]))
		reasonB.WriteString(wire.FirstNonEmpty(delta["reasoning_content"], delta["reasoning"]))
		for _, raw := range wire.AsSlice(delta["tool_calls"]) {
			call := wire.AsRecord(raw)
			index := int(wire.Number(call["index"]))
			pending, ok := calls[index]
			if !ok {
				pending = &pendingCall{}
				calls[index] = pending
				order = append(order, index)
			}
			if id := wire.AsString(call["id"]); id != "" {
				pending.id = id
			}
			fn := wire.AsRecord(call["function"])
			if name := wire.AsString(fn["name"]); name != "" {
				pending.name = name
			}
			pending.args += wire.AsString(fn["arguments"])
		}
	}
	sort.Ints(order)
	for _, index := range order {
		pending := calls[index]
		id := pending.id
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":        id,
			"name":      pending.name,
			"arguments": pending.args,
		})
	}
	return textB.String(), reasonB.String(), toolCalls, finishReason
}

// ---- Anthropic Messages ----------------------------------------------------

// parseAnthropicJSON reads a non-stream Messages response.
func parseAnthropicJSON(data []byte) (text, reasoning string, toolCalls []any, finishReason string) {
	var body wire.Body
	if err := json.Unmarshal(data, &body); err != nil {
		return "", "", nil, ""
	}
	return anthropicFields(body)
}

// anthropicFields pulls text, thinking, and tool_use blocks out of a message.
func anthropicFields(body wire.Body) (text, reasoning string, toolCalls []any, finishReason string) {
	var textB, reasonB strings.Builder
	for i, raw := range wire.AsSlice(body["content"]) {
		block := wire.AsRecord(raw)
		switch block["type"] {
		case "text":
			textB.WriteString(wire.AsString(block["text"]))
		case "thinking":
			reasonB.WriteString(wire.FirstNonEmpty(block["thinking"], block["text"]))
		case "tool_use":
			id := wire.AsString(block["id"])
			if id == "" {
				id = fmt.Sprintf("toolu_%d", i)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":        id,
				"name":      wire.AsString(block["name"]),
				"arguments": argumentsString(block["input"]),
			})
		}
	}
	return textB.String(), reasonB.String(), toolCalls, wire.AsString(body["stop_reason"])
}

// parseAnthropicSSE folds Messages SSE events: text_delta, thinking_delta,
// input_json_delta fragments, and the terminal message_delta stop_reason.
func parseAnthropicSSE(data []byte) (text, reasoning string, toolCalls []any, finishReason string) {
	var textB, reasonB strings.Builder
	type pendingTool struct{ id, name, args string }
	tools := map[int]*pendingTool{}
	var order []int
	for _, event := range wire.SplitSseEvents(string(data)).Events {
		switch event["type"] {
		case "content_block_start":
			block := wire.AsRecord(event["content_block"])
			if block["type"] != "tool_use" {
				continue
			}
			index := int(wire.Number(event["index"]))
			tools[index] = &pendingTool{id: wire.AsString(block["id"]), name: wire.AsString(block["name"])}
			order = append(order, index)
		case "content_block_delta":
			delta := wire.AsRecord(event["delta"])
			switch delta["type"] {
			case "text_delta":
				textB.WriteString(wire.AsString(delta["text"]))
			case "thinking_delta":
				reasonB.WriteString(wire.FirstNonEmpty(delta["thinking"], delta["text"]))
			case "input_json_delta":
				index := int(wire.Number(event["index"]))
				if pending, ok := tools[index]; ok {
					pending.args += wire.AsString(delta["partial_json"])
				}
			}
		case "message_delta":
			if stop := wire.AsString(wire.AsRecord(event["delta"])["stop_reason"]); stop != "" {
				finishReason = stop
			}
		}
	}
	sort.Ints(order)
	for _, index := range order {
		pending := tools[index]
		id := pending.id
		if id == "" {
			id = fmt.Sprintf("toolu_%d", index)
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":        id,
			"name":      pending.name,
			"arguments": pending.args,
		})
	}
	return textB.String(), reasonB.String(), toolCalls, finishReason
}

// ---- OpenAI Responses ------------------------------------------------------

// parseResponsesJSON reads a non-stream `response` object.
func parseResponsesJSON(data []byte) (text, reasoning string, toolCalls []any, finishReason string) {
	var body wire.Body
	if err := json.Unmarshal(data, &body); err != nil {
		return "", "", nil, ""
	}
	return responsesFields(body)
}

// responsesFields pulls message, reasoning, and function_call output items out
// of a `response` object.
func responsesFields(body wire.Body) (text, reasoning string, toolCalls []any, finishReason string) {
	var textB, reasonB strings.Builder
	for _, raw := range wire.AsSlice(body["output"]) {
		item := wire.AsRecord(raw)
		switch item["type"] {
		case "message":
			for _, rawPart := range wire.AsSlice(item["content"]) {
				part := wire.AsRecord(rawPart)
				if part["type"] == "output_text" || part["type"] == "text" {
					textB.WriteString(wire.AsString(part["text"]))
				}
			}
		case "function_call":
			id := wire.FirstNonEmpty(item["call_id"], item["id"])
			if id == "" {
				id = fmt.Sprintf("call_%d", len(toolCalls))
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":        id,
				"name":      wire.AsString(item["name"]),
				"arguments": argumentsString(item["arguments"]),
			})
		case "reasoning":
			for _, rawPart := range wire.AsSlice(item["summary"]) {
				reasonB.WriteString(wire.AsString(wire.AsRecord(rawPart)["text"]))
			}
			for _, rawPart := range wire.AsSlice(item["content"]) {
				reasonB.WriteString(wire.AsString(wire.AsRecord(rawPart)["text"]))
			}
		}
	}
	return textB.String(), reasonB.String(), toolCalls, wire.AsString(body["status"])
}

// capturedToolCall accumulates one tool call while a stream is parsed. Its
// fields are strings because argument fragments arrive as raw JSON text.
type capturedToolCall struct {
	id   string
	name string
	args string
}

// parseResponsesSSE folds Responses SSE events: output text / reasoning deltas,
// function-call argument fragments, and the terminal response.completed object
// (used for status and to fill anything the deltas missed).
func parseResponsesSSE(data []byte) (text, reasoning string, toolCalls []any, finishReason string) {
	var textB, reasonB strings.Builder
	calls := map[string]*capturedToolCall{}
	var order []string
	var completed wire.Body
	for _, event := range wire.SplitSseEvents(string(data)).Events {
		switch event["type"] {
		case "response.output_text.delta":
			textB.WriteString(wire.AsString(event["delta"]))
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			reasonB.WriteString(wire.AsString(event["delta"]))
		case "response.output_item.added", "response.output_item.done":
			item := wire.AsRecord(event["item"])
			if item["type"] != "function_call" {
				continue
			}
			key := responsesItemKey(item, event)
			pending := ensurePendingCall(calls, &order, key)
			if name := wire.AsString(item["name"]); name != "" {
				pending.name = name
			}
			if id := wire.FirstNonEmpty(item["call_id"], item["id"]); id != "" {
				pending.id = id
			}
			if args := wire.AsString(item["arguments"]); args != "" {
				// A `.done` item carries the full argument text.
				pending.args = args
			}
		case "response.function_call_arguments.delta":
			key := wire.AsString(event["item_id"])
			if key == "" {
				key = fmt.Sprintf("idx_%d", int(wire.Number(event["output_index"])))
			}
			pending := ensurePendingCall(calls, &order, key)
			pending.args += wire.AsString(event["delta"])
		case "response.completed", "response.done":
			completed = wire.AsRecord(event["response"])
		}
	}
	for _, key := range order {
		pending := calls[key]
		id := pending.id
		if id == "" {
			id = key
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":        id,
			"name":      pending.name,
			"arguments": pending.args,
		})
	}
	if len(completed) > 0 {
		finishReason = wire.AsString(completed["status"])
		if textB.Len() == 0 || reasonB.Len() == 0 || len(toolCalls) == 0 {
			cText, cReason, cCalls, _ := responsesFields(completed)
			if textB.Len() == 0 {
				textB.WriteString(cText)
			}
			if reasonB.Len() == 0 {
				reasonB.WriteString(cReason)
			}
			if len(toolCalls) == 0 {
				toolCalls = cCalls
			}
		}
	}
	return textB.String(), reasonB.String(), toolCalls, finishReason
}

// responsesItemKey picks a stable key for one streamed function call so its
// argument fragments merge across events.
func responsesItemKey(item, event wire.Body) string {
	if key := wire.FirstNonEmpty(item["id"], item["call_id"], event["item_id"]); key != "" {
		return key
	}
	return fmt.Sprintf("idx_%d", int(wire.Number(event["output_index"])))
}

// ensurePendingCall returns the accumulator for key, appending it to order on
// first sight.
func ensurePendingCall(calls map[string]*capturedToolCall, order *[]string, key string) *capturedToolCall {
	if pending, ok := calls[key]; ok {
		return pending
	}
	pending := &capturedToolCall{}
	calls[key] = pending
	*order = append(*order, key)
	return pending
}
