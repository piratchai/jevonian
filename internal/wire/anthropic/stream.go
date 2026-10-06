package anthropic

import (
	"fmt"
	"sort"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

// ToChatStream translates Anthropic Messages SSE events into OpenAI Chat
// Completions chunks — the port of anthropicToChatStream in src/anthropic.ts.
// Emit `data:`-only frames and a final `data: [DONE]`.
type ToChatStream struct {
	model       string
	onFinish    func(usage wire.Usage, failure string)
	onEvent     func(wire.StreamEvent)
	id          string
	created     int64
	toolIndexes map[int]int
	usage       wire.Usage
	roleSent    bool
	finished    bool
	failure     string
}

// NewToChatStream builds the bridge. `onFinish` runs once at flush with the
// usage Anthropic reported and any folded-in failure; `onEvent` surfaces
// content/usage/finish/error marks for the ledger.
func NewToChatStream(model string, onFinish func(wire.Usage, string), onEvent func(wire.StreamEvent)) *ToChatStream {
	return &ToChatStream{
		model:       model,
		onFinish:    onFinish,
		onEvent:     onEvent,
		id:          "chatcmpl-" + randomHex(24),
		created:     time.Now().Unix(),
		toolIndexes: map[int]int{},
	}
}

func (s *ToChatStream) chunk(delta wire.Body, finishReason any) wire.Body {
	return wire.Body{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": float64(s.created),
		"model":   s.model,
		"choices": []any{wire.Body{
			"index":         float64(0),
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

func (s *ToChatStream) usagePayload() wire.Body {
	payload := wire.Body{
		"prompt_tokens":     float64(s.usage.Input),
		"completion_tokens": float64(s.usage.Output),
		"total_tokens":      float64(s.usage.Input + s.usage.Output),
	}
	// Cache reads ride along as OpenAI's `prompt_tokens_details.cached_tokens`,
	// so a Chat or Responses client downstream still sees them.
	if s.usage.CacheRead > 0 {
		payload["prompt_tokens_details"] = wire.Body{"cached_tokens": float64(s.usage.CacheRead)}
	}
	return payload
}

func (s *ToChatStream) mark(event wire.StreamEvent) {
	if s.onEvent != nil {
		s.onEvent(event)
	}
}

// Handle consumes one decoded Anthropic SSE event.
func (s *ToChatStream) Handle(event wire.Body, sink wire.EventSink) {
	switch event["type"] {
	case "message_start":
		message := wire.AsRecord(event["message"])
		s.usage = Usage(message["usage"])
		if !s.roleSent {
			sink.EmitData(s.chunk(wire.Body{"role": "assistant", "content": ""}, nil))
			s.roleSent = true
		}
	case "content_block_start":
		block := wire.AsRecord(event["content_block"])
		if block["type"] != "tool_use" {
			return
		}
		index := int(wire.Number(event["index"]))
		toolIndex := len(s.toolIndexes)
		s.toolIndexes[index] = toolIndex
		s.mark(wire.StreamEvent{Kind: wire.StreamContent})
		id := wire.AsString(block["id"])
		if id == "" {
			id = fmt.Sprintf("call_%d", toolIndex)
		}
		sink.EmitData(s.chunk(wire.Body{
			"tool_calls": []any{wire.Body{
				"index": float64(toolIndex),
				"id":    id,
				"type":  "function",
				"function": wire.Body{
					"name":      wire.AsString(block["name"]),
					"arguments": "",
				},
			}},
		}, nil))
	case "content_block_delta":
		delta := wire.AsRecord(event["delta"])
		if delta["type"] == "text_delta" {
			if text, ok := delta["text"].(string); ok && len(text) > 0 {
				s.mark(wire.StreamEvent{Kind: wire.StreamContent})
				sink.EmitData(s.chunk(wire.Body{"content": text}, nil))
			}
			return
		}
		if delta["type"] == "input_json_delta" {
			if partial, ok := delta["partial_json"].(string); ok {
				toolIndex := s.toolIndexes[int(wire.Number(event["index"]))]
				s.mark(wire.StreamEvent{Kind: wire.StreamContent})
				sink.EmitData(s.chunk(wire.Body{
					"tool_calls": []any{wire.Body{
						"index":    float64(toolIndex),
						"function": wire.Body{"arguments": partial},
					}},
				}, nil))
			}
		}
	case "message_delta":
		delta := wire.AsRecord(event["delta"])
		stop := wire.AsString(delta["stop_reason"])
		if output := int(wire.Number(wire.AsRecord(event["usage"])["output_tokens"])); output > 0 {
			s.usage.Output = output
		}
		if stop != "" && !s.finished {
			s.mark(wire.StreamEvent{Kind: wire.StreamUsage, Usage: s.usage})
			s.mark(wire.StreamEvent{Kind: wire.StreamFinish})
			s.finished = true
			reason, ok := stopReasons[stop]
			if !ok {
				reason = "stop"
			}
			frame := s.chunk(wire.Body{}, reason)
			frame["usage"] = s.usagePayload()
			sink.EmitData(frame)
		}
	case "error":
		errObj := wire.AsRecord(event["error"])
		s.failure = wire.AsString(errObj["message"])
		if s.failure == "" {
			s.failure = "upstream error"
		}
		s.mark(wire.StreamEvent{Kind: wire.StreamError, Message: s.failure})
	}
}

// Finish flushes the terminal state: a soft-error tail when the upstream
// folded an error in mid-stream, the finish chunk when none was seen, then
// `data: [DONE]`.
func (s *ToChatStream) Finish(sink wire.EventSink) {
	if s.failure != "" && !s.finished {
		// Never hand a mid-stream refusal to the client as a bare `{ error }`
		// frame: the harness reads that as an invalid response and can roll the
		// user message back. Finish the turn with a readable assistant message
		// instead; the ledger still records the real failure.
		sink.EmitData(s.chunk(wire.Body{"content": wire.SoftErrorMessage(s.failure)}, nil))
	}
	if !s.finished {
		frame := s.chunk(wire.Body{}, "stop")
		frame["usage"] = s.usagePayload()
		sink.EmitData(frame)
	}
	sink.Emit(wire.SSEDone)
	if s.onFinish != nil {
		s.onFinish(s.usage, s.failure)
	}
}

// ChatToAnthropicStream translates OpenAI Chat Completions SSE chunks into
// Anthropic Messages SSE — the port of chatToAnthropicStream in
// src/chat-anthropic-stream.ts. Emits `event: <type>` frames.
type ChatToStream struct {
	model    string
	usageFn  func() *wire.Usage
	onFinish func(wire.Usage)
	id       string

	started      bool
	nextIndex    int
	open         *openBlock
	pendingTools map[int]*pendingTool
	toolCount    int
	finishReason string
	chatUsage    *wire.Usage
	failure      *streamFailure
}

type openBlock struct {
	kind  string // "text" | "thinking"
	index int
}

type pendingTool struct {
	id   string
	name string
	args []string
}

type streamFailure struct {
	message string
	typ     string
}

var chatToAnthropicStops = map[string]string{
	"stop":           "end_turn",
	"length":         "max_tokens",
	"tool_calls":     "tool_use",
	"function_call":  "tool_use",
	"content_filter": "refusal",
}

// ChatToStreamOptions customizes the bridge.
type ChatToStreamOptions struct {
	// Usage reads the exact usage at flush time; wins over the usage the chat
	// chunks carry (those have no cache-write field).
	Usage func() *wire.Usage
	// OnFinish runs once with the usage reported to the client.
	OnFinish func(wire.Usage)
}

// NewChatToStream builds the Chat→Anthropic SSE bridge.
func NewChatToStream(model string, options ChatToStreamOptions) *ChatToStream {
	return &ChatToStream{
		model:        model,
		usageFn:      options.Usage,
		onFinish:     options.OnFinish,
		id:           "msg_" + randomHex(24),
		pendingTools: map[int]*pendingTool{},
	}
}

func (s *ChatToStream) start(sink wire.EventSink) {
	if s.started {
		return
	}
	s.started = true
	sink.EmitEvent(wire.Body{
		"type": "message_start",
		"message": wire.Body{
			"id":            s.id,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         wire.Body{"input_tokens": float64(0), "output_tokens": float64(0)},
		},
	})
}

func (s *ChatToStream) closeBlock(sink wire.EventSink) {
	if s.open == nil {
		return
	}
	sink.EmitEvent(wire.Body{"type": "content_block_stop", "index": float64(s.open.index)})
	s.open = nil
}

func (s *ChatToStream) openBlock(sink wire.EventSink, kind string) int {
	if s.open != nil && s.open.kind == kind {
		return s.open.index
	}
	s.closeBlock(sink)
	index := s.nextIndex
	s.nextIndex++
	s.open = &openBlock{kind: kind, index: index}
	var block wire.Body
	if kind == "text" {
		block = wire.Body{"type": "text", "text": ""}
	} else {
		block = wire.Body{"type": "thinking", "thinking": "", "signature": ""}
	}
	sink.EmitEvent(wire.Body{
		"type":          "content_block_start",
		"index":         float64(index),
		"content_block": block,
	})
	return index
}

func (s *ChatToStream) flushTools(sink wire.EventSink) {
	s.closeBlock(sink)
	indexes := make([]int, 0, len(s.pendingTools))
	for index := range s.pendingTools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, toolIndex := range indexes {
		tool := s.pendingTools[toolIndex]
		index := s.nextIndex
		s.nextIndex++
		sink.EmitEvent(wire.Body{
			"type":  "content_block_start",
			"index": float64(index),
			"content_block": wire.Body{
				"type":  "tool_use",
				"id":    tool.id,
				"name":  tool.name,
				"input": wire.Body{},
			},
		})
		for _, argumentsDelta := range tool.args {
			sink.EmitEvent(wire.Body{
				"type":  "content_block_delta",
				"index": float64(index),
				"delta": wire.Body{"type": "input_json_delta", "partial_json": argumentsDelta},
			})
		}
		sink.EmitEvent(wire.Body{"type": "content_block_stop", "index": float64(index)})
		delete(s.pendingTools, toolIndex)
	}
}

func reasoningTextOf(delta wire.Body) string {
	if s, ok := delta["reasoning_content"].(string); ok {
		return s
	}
	if s, ok := delta["reasoning"].(string); ok {
		return s
	}
	return ""
}

// usageFromChat splits OpenAI's inclusive prompt_tokens into uncached input
// plus a cache-read count — Anthropic's usage is not inclusive.
func usageFromChat(raw any) wire.Usage {
	usage := wire.AsRecord(raw)
	cached := int(wire.Number(wire.AsRecord(usage["prompt_tokens_details"])["cached_tokens"]))
	input := int(wire.Number(usage["prompt_tokens"])) - cached
	if input < 0 {
		input = 0
	}
	return wire.Usage{
		Input:  input,
		Output: int(wire.Number(usage["completion_tokens"])),
		// CacheRead: cached
		CacheRead: cached,
	}
}

// Handle consumes one decoded Chat Completions SSE chunk.
func (s *ChatToStream) Handle(chunk wire.Body, sink wire.EventSink) {
	if errObj, ok := chunk["error"].(map[string]any); ok && errObj != nil {
		message := wire.AsString(errObj["message"])
		if message == "" {
			message = "upstream error"
		}
		typ := wire.AsString(errObj["type"])
		if typ == "" {
			typ = "api_error"
		}
		s.failure = &streamFailure{message: message, typ: typ}
		return
	}
	s.start(sink)
	if chunk["usage"] != nil {
		usage := usageFromChat(chunk["usage"])
		s.chatUsage = &usage
	}
	for _, rawChoice := range wire.AsSlice(chunk["choices"]) {
		choice := wire.AsRecord(rawChoice)
		delta := wire.AsRecord(choice["delta"])

		if thinking := reasoningTextOf(delta); thinking != "" {
			index := s.openBlock(sink, "thinking")
			sink.EmitEvent(wire.Body{
				"type":  "content_block_delta",
				"index": float64(index),
				"delta": wire.Body{"type": "thinking_delta", "thinking": thinking},
			})
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			index := s.openBlock(sink, "text")
			sink.EmitEvent(wire.Body{
				"type":  "content_block_delta",
				"index": float64(index),
				"delta": wire.Body{"type": "text_delta", "text": content},
			})
		}

		// Chat may interleave deltas for parallel tools; Anthropic requires
		// each content block to remain contiguous, so tool deltas are flushed
		// in order at finish.
		for _, rawCall := range wire.AsSlice(delta["tool_calls"]) {
			call := wire.AsRecord(rawCall)
			toolIndex := int(wire.Number(call["index"]))
			fn := wire.AsRecord(call["function"])
			tool, ok := s.pendingTools[toolIndex]
			if !ok {
				rawID := wire.AsString(call["id"])
				var id string
				if rawID != "" {
					id = ToolID(rawID)
				} else {
					id = "toolu_" + randomHex(8)
				}
				tool = &pendingTool{
					id:   id,
					name: wire.AsString(fn["name"]),
				}
				s.pendingTools[toolIndex] = tool
				s.toolCount++
			}
			if name, ok := fn["name"].(string); ok && name != "" {
				tool.name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				tool.args = append(tool.args, args)
			}
		}

		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			s.finishReason = reason
		}
	}
}

// Finish closes open blocks, flushes pending tool calls contiguously, and
// emits message_delta + message_stop. An upstream `{ error }` chunk becomes a
// soft assistant message rather than an `error` event: a bare error frame
// makes the harness roll the user message back.
func (s *ChatToStream) Finish(sink wire.EventSink) {
	var usage wire.Usage
	if s.usageFn != nil {
		if u := s.usageFn(); u != nil {
			usage = *u
		}
	}
	if usage == (wire.Usage{}) && s.chatUsage != nil {
		usage = *s.chatUsage
	}
	usageFrame := wire.Body{
		"input_tokens":                float64(usage.Input),
		"output_tokens":               float64(usage.Output),
		"cache_read_input_tokens":     float64(usage.CacheRead),
		"cache_creation_input_tokens": float64(usage.CacheWrite),
	}
	if s.failure != nil {
		s.start(sink)
		index := s.openBlock(sink, "text")
		sink.EmitEvent(wire.Body{
			"type":  "content_block_delta",
			"index": float64(index),
			"delta": wire.Body{
				"type": "text_delta",
				"text": wire.SoftErrorMessage(s.failure.message),
			},
		})
		s.flushTools(sink)
		sink.EmitEvent(wire.Body{
			"type":  "message_delta",
			"delta": wire.Body{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": usageFrame,
		})
		sink.EmitEvent(wire.Body{"type": "message_stop"})
		if s.onFinish != nil {
			s.onFinish(usage)
		}
		return
	}
	s.start(sink)
	s.flushTools(sink)
	var stop string
	if s.toolCount > 0 && (s.finishReason == "" || s.finishReason == "stop") {
		stop = "tool_use"
	} else {
		reason, ok := chatToAnthropicStops[s.finishReason]
		if !ok {
			reason = "end_turn"
		}
		stop = reason
	}
	sink.EmitEvent(wire.Body{
		"type":  "message_delta",
		"delta": wire.Body{"stop_reason": stop, "stop_sequence": nil},
		"usage": usageFrame,
	})
	sink.EmitEvent(wire.Body{"type": "message_stop"})
	if s.onFinish != nil {
		s.onFinish(usage)
	}
}
