package responses

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return "0123456789abcdef0123456789abcdef"[:n]
	}
	return hex.EncodeToString(buf)[:n]
}

// ChatBridgeCall is one tool call folded out of a Responses result.
type ChatBridgeCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatBridgeResult is the folded outcome of a bridged stream: what
// ChatCompletionFrom needs to build the final JSON body, and what the ledger
// needs to record the turn.
type ChatBridgeResult struct {
	Content      string
	ToolCalls    []ChatBridgeCall
	FinishReason string
	Usage        wire.Usage
	Failure      string
}

// ToChatBridge turns Responses SSE events into Chat Completions chunks — the
// port of ResponsesChatBridge in src/responses.ts.
type ToChatBridge struct {
	ID           string
	Created      int64
	model        string
	calls        map[string]*pendingCall
	order        []string
	content      string
	finishReason string
	usage        wire.Usage
	failure      string
	completed    bool
}

type pendingCall struct {
	index     int
	id        string
	name      string
	arguments string
}

// NewToChatBridge builds the Responses→Chat stream bridge.
func NewToChatBridge(model string) *ToChatBridge {
	return &ToChatBridge{
		model:        model,
		ID:           "chatcmpl-" + randomHex(24),
		Created:      time.Now().Unix(),
		calls:        map[string]*pendingCall{},
		finishReason: "stop",
	}
}

// Delivered reports whether anything renderable reached the client: text or a
// tool call. A client cancel after this point closed a finished-looking turn,
// not an abandoned request — the difference between a 200 and a 499 on the
// ledger.
func (b *ToChatBridge) Delivered() bool {
	return len(b.content) > 0 || len(b.calls) > 0 || b.completed
}

func (b *ToChatBridge) chunk(delta wire.Body, finishReason any) wire.Body {
	return wire.Body{
		"id":      b.ID,
		"object":  "chat.completion.chunk",
		"created": float64(b.Created),
		"model":   b.model,
		"choices": []any{wire.Body{
			"index":         float64(0),
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

// Handle consumes one decoded Responses SSE event.
func (b *ToChatBridge) Handle(raw wire.Body, sink wire.EventSink) {
	event := wire.AsRecord(raw)
	switch event["type"] {
	case "response.created":
		sink.EmitData(b.chunk(wire.Body{"role": "assistant", "content": ""}, nil))
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return
		}
		b.content += delta
		sink.EmitData(b.chunk(wire.Body{"content": delta}, nil))
	case "response.output_item.added":
		item := wire.AsRecord(event["item"])
		if item["type"] != "function_call" {
			return
		}
		itemID := wire.AsString(item["id"])
		if itemID == "" {
			itemID = wire.AsString(event["item_id"])
		}
		arguments, _ := item["arguments"].(string)
		call := &pendingCall{
			index:     len(b.order),
			id:        callIDOf(item["call_id"], item["id"]),
			name:      wire.AsString(item["name"]),
			arguments: arguments,
		}
		b.calls[itemID] = call
		b.order = append(b.order, itemID)
		sink.EmitData(b.chunk(wire.Body{
			"tool_calls": []any{wire.Body{
				"index": float64(call.index),
				"id":    call.id,
				"type":  "function",
				"function": wire.Body{
					"name":      call.name,
					"arguments": "",
				},
			}},
		}, nil))
	case "response.function_call_arguments.delta":
		itemID := wire.AsString(event["item_id"])
		call, ok := b.calls[itemID]
		delta, _ := event["delta"].(string)
		if !ok || delta == "" {
			return
		}
		call.arguments += delta
		sink.EmitData(b.chunk(wire.Body{
			"tool_calls": []any{wire.Body{
				"index":    float64(call.index),
				"function": wire.Body{"arguments": delta},
			}},
		}, nil))
	case "response.completed":
		response := wire.AsRecord(event["response"])
		b.usage = Usage(response["usage"])
		if len(b.order) > 0 {
			b.finishReason = "tool_calls"
		} else {
			b.finishReason = "stop"
		}
		b.completed = true
		sink.EmitData(b.chunk(wire.Body{}, b.finishReason))
	case "response.incomplete":
		b.finishReason = "length"
		b.completed = true
		sink.EmitData(b.chunk(wire.Body{}, b.finishReason))
	case "response.failed":
		response := wire.AsRecord(event["response"])
		errObj := wire.AsRecord(response["error"])
		if msg, ok := errObj["message"].(string); ok {
			b.failure = msg
		} else {
			b.failure = "upstream response failed"
		}
	case "error":
		errObj := wire.AsRecord(event["error"])
		if msg, ok := errObj["message"].(string); ok {
			b.failure = msg
		} else {
			b.failure = "upstream error"
		}
	}
}

// Result is the folded bridge state after the stream ends.
func (b *ToChatBridge) Result() ChatBridgeResult {
	var toolCalls []ChatBridgeCall
	for _, key := range b.order {
		call, ok := b.calls[key]
		if !ok {
			continue
		}
		toolCalls = append(toolCalls, ChatBridgeCall{
			ID:        call.id,
			Name:      call.name,
			Arguments: call.arguments,
		})
	}
	return ChatBridgeResult{
		Content:      b.content,
		ToolCalls:    toolCalls,
		FinishReason: b.finishReason,
		Usage:        b.usage,
		Failure:      b.failure,
	}
}

// Finish emits terminal chunks: a soft-error tail on failure, or the finish
// chunk when the stream ended without `response.completed`.
func (b *ToChatBridge) Finish(sink wire.EventSink) {
	if b.failure != "" {
		// A chat-wire client reads a bare `{ error }` chunk as an invalid
		// response and can roll the user message back. Close the turn as a
		// normal assistant message instead; the ledger still records the real
		// failure via Result().Failure.
		sink.EmitData(b.chunk(wire.Body{"content": wire.SoftErrorMessage(b.failure)}, nil))
		sink.EmitData(b.chunk(wire.Body{}, "stop"))
		return
	}
	if !b.completed {
		sink.EmitData(b.chunk(wire.Body{}, b.finishReason))
	}
}

// ToChatStream adapts ToChatBridge to the wire.Translator interface: feed raw
// upstream bytes in via a TranslatorStream, get `data:`-only chat chunks plus
// `data: [DONE]` out.
type ToChatStream struct {
	bridge   *ToChatBridge
	onFinish func(ChatBridgeResult)
}

// NewToChatStream builds a Translator emitting Chat Completions SSE.
func NewToChatStream(model string, onFinish func(ChatBridgeResult)) *ToChatStream {
	return &ToChatStream{bridge: NewToChatBridge(model), onFinish: onFinish}
}

// Bridge exposes the underlying bridge for mid-stream state.
func (s *ToChatStream) Bridge() *ToChatBridge { return s.bridge }

// Handle implements wire.Translator.
func (s *ToChatStream) Handle(event wire.Body, sink wire.EventSink) {
	s.bridge.Handle(event, sink)
}

// Finish implements wire.Translator.
func (s *ToChatStream) Finish(sink wire.EventSink) {
	s.bridge.Finish(sink)
	sink.Emit(wire.SSEDone)
	if s.onFinish != nil {
		s.onFinish(s.bridge.Result())
	}
}

// FromChatBridge is the inverse of ToChatBridge: Chat Completions SSE →
// Responses SSE. Lets Codex keep talking `/v1/responses` while the upstream
// is OpenRouter / DeepSeek / …
//
// Codex is picky about the Responses event shape: bare `{type, delta}` text
// events are dropped. Emit the same item/content indexes the native API does,
// plus `.done` markers, and surface OpenRouter reasoning deltas so a
// thinking-only reply is not silent.
type FromChatBridge struct {
	ID      string
	Created int64
	model   string

	calls              map[int]*pendingOutputCall
	content            string
	reasoning          string
	usage              wire.Usage
	finishReason       string
	started            bool
	completed          bool
	failure            *chatFailure
	messageItemID      string
	messageOutputIndex int
	hasMessage         bool
	nextOutputIndex    int
}

type pendingOutputCall struct {
	id          string
	name        string
	arguments   string
	itemID      string
	outputIndex int
}

type chatFailure struct {
	message string
	typ     string
}

// NewFromChatBridge builds the Chat→Responses stream bridge.
func NewFromChatBridge(model string) *FromChatBridge {
	return &FromChatBridge{
		model:        model,
		ID:           "resp_" + randomHex(24),
		Created:      time.Now().Unix(),
		calls:        map[int]*pendingOutputCall{},
		finishReason: "stop",
	}
}

// Delivered reports whether anything renderable reached the client.
func (b *FromChatBridge) Delivered() bool {
	return len(b.content) > 0 || len(b.reasoning) > 0 || len(b.calls) > 0 || b.completed
}

// SeenUsage is the usage the upstream reported, as far as the stream got.
func (b *FromChatBridge) SeenUsage() wire.Usage { return b.usage }

func (b *FromChatBridge) responseSkeleton(status string, output []any) wire.Body {
	// A nil slice would serialize as `"output": null`; Responses clients expect
	// an array (TS emits `[]`).
	if output == nil {
		output = []any{}
	}
	return wire.Body{
		"id":         b.ID,
		"object":     "response",
		"created_at": float64(b.Created),
		"status":     status,
		"model":      b.model,
		"output":     output,
		"usage": wire.Body{
			"input_tokens":         float64(b.usage.Input),
			"output_tokens":        float64(b.usage.Output),
			"total_tokens":         float64(b.usage.Input + b.usage.Output),
			"input_tokens_details": wire.Body{"cached_tokens": float64(b.usage.CacheRead)},
		},
	}
}

func (b *FromChatBridge) ensureMessageItem(events *[]any) (itemID string, outputIndex int) {
	if b.hasMessage {
		return b.messageItemID, b.messageOutputIndex
	}
	itemID = "msg_" + b.ID
	outputIndex = b.nextOutputIndex
	b.nextOutputIndex++
	b.messageItemID = itemID
	b.messageOutputIndex = outputIndex
	b.hasMessage = true
	*events = append(*events, wire.Body{
		"type":         "response.output_item.added",
		"output_index": float64(outputIndex),
		"item": wire.Body{
			"type":    "message",
			"id":      itemID,
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		},
	})
	*events = append(*events, wire.Body{
		"type":          "response.content_part.added",
		"item_id":       itemID,
		"output_index":  float64(outputIndex),
		"content_index": float64(0),
		"part":          wire.Body{"type": "output_text", "text": ""},
	})
	return itemID, outputIndex
}

func reasoningText(delta wire.Body) string {
	if s, ok := delta["reasoning"].(string); ok && s != "" {
		return s
	}
	if s, ok := delta["reasoning_content"].(string); ok && s != "" {
		return s
	}
	var parts []string
	for _, raw := range wire.AsSlice(delta["reasoning_details"]) {
		detail := wire.AsRecord(raw)
		if s, ok := detail["text"].(string); ok && s != "" {
			parts = append(parts, s)
		} else if s, ok := detail["content"].(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	result := ""
	for _, p := range parts {
		result += p
	}
	return result
}

// Handle consumes one decoded Chat Completions SSE chunk, returning the
// Responses events it produces. Events are returned rather than emitted so
// callers can batch/inspect; FromChatStream pushes them through the sink.
func (b *FromChatBridge) Handle(raw wire.Body) []any {
	if b.completed || b.failure != nil {
		return nil
	}
	chunk := wire.AsRecord(raw)
	if errObj, ok := chunk["error"].(map[string]any); ok && errObj != nil {
		message := wire.AsString(errObj["message"])
		if message == "" {
			message = "upstream error"
		}
		typ := wire.AsString(errObj["type"])
		if typ == "" {
			typ = "upstream_error"
		}
		b.failure = &chatFailure{message: message, typ: typ}
		// A Responses client (Codex) reads `response.failed` as a hard failure
		// and can drop the whole turn, so the failure is written into a normal
		// assistant message instead and Finish completes the turn. The real
		// failure still rides on Result().Failure for the ledger.
		var events []any
		if !b.started {
			b.started = true
			events = append(events,
				wire.Body{"type": "response.created", "response": b.responseSkeleton("in_progress", nil)},
				wire.Body{"type": "response.in_progress", "response": b.responseSkeleton("in_progress", nil)},
			)
		}
		itemID, outputIndex := b.ensureMessageItem(&events)
		text := wire.SoftErrorMessage(b.failure.message)
		b.content += text
		events = append(events, wire.Body{
			"type":          "response.output_text.delta",
			"item_id":       itemID,
			"output_index":  float64(outputIndex),
			"content_index": float64(0),
			"delta":         text,
		})
		return events
	}

	var events []any
	if !b.started {
		b.started = true
		events = append(events,
			wire.Body{"type": "response.created", "response": b.responseSkeleton("in_progress", nil)},
			wire.Body{"type": "response.in_progress", "response": b.responseSkeleton("in_progress", nil)},
		)
	}

	if chunk["usage"] != nil {
		usage := wire.AsRecord(chunk["usage"])
		b.usage = wire.Usage{
			Input:     int(wire.Number(usage["prompt_tokens"])),
			Output:    int(wire.Number(usage["completion_tokens"])),
			CacheRead: int(wire.Number(wire.AsRecord(usage["prompt_tokens_details"])["cached_tokens"])),
		}
	}

	for _, rawChoice := range wire.AsSlice(chunk["choices"]) {
		choice := wire.AsRecord(rawChoice)
		delta := wire.AsRecord(choice["delta"])

		if thinking := reasoningText(delta); thinking != "" {
			b.reasoning += thinking
			events = append(events, wire.Body{
				"type":  "response.reasoning_summary_text.delta",
				"delta": thinking,
			})
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			itemID, outputIndex := b.ensureMessageItem(&events)
			b.content += content
			events = append(events, wire.Body{
				"type":          "response.output_text.delta",
				"item_id":       itemID,
				"output_index":  float64(outputIndex),
				"content_index": float64(0),
				"delta":         content,
			})
		}

		for _, rawCall := range wire.AsSlice(delta["tool_calls"]) {
			call := wire.AsRecord(rawCall)
			index := int(wire.Number(call["index"]))
			fn := wire.AsRecord(call["function"])
			pending, ok := b.calls[index]
			if !ok {
				id := wire.AsString(call["id"])
				if id == "" {
					id = "call_" + randomHex(16)
				}
				itemID := "fc_" + id
				outputIndex := b.nextOutputIndex
				b.nextOutputIndex++
				pending = &pendingOutputCall{
					id:          id,
					name:        wire.AsString(fn["name"]),
					itemID:      itemID,
					outputIndex: outputIndex,
				}
				b.calls[index] = pending
				events = append(events, wire.Body{
					"type":         "response.output_item.added",
					"output_index": float64(outputIndex),
					"item_id":      itemID,
					"item": wire.Body{
						"type":      "function_call",
						"id":        itemID,
						"call_id":   id,
						"name":      pending.name,
						"arguments": "",
						"status":    "in_progress",
					},
				})
			} else if id := wire.AsString(call["id"]); id != "" && len(pending.id) >= 5 && pending.id[:5] == "call_" {
				// First chunk sometimes omits the id; adopt it when it arrives.
				pending.id = id
			}
			if name, ok := fn["name"].(string); ok && name != "" {
				pending.name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				pending.arguments += args
				events = append(events, wire.Body{
					"type":         "response.function_call_arguments.delta",
					"item_id":      pending.itemID,
					"output_index": float64(pending.outputIndex),
					"delta":        args,
				})
			}
		}

		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			b.finishReason = reason
		}
	}
	return events
}

// Result is the folded bridge state after the stream ends.
func (b *FromChatBridge) Result() ChatBridgeResult {
	var toolCalls []ChatBridgeCall
	// Iterate calls in insertion order — Go maps are unordered, so keep an
	// index-sorted walk.
	indexes := make([]int, 0, len(b.calls))
	for index := range b.calls {
		indexes = append(indexes, index)
	}
	for i := 0; i < len(indexes); i++ {
		for j := i + 1; j < len(indexes); j++ {
			if indexes[j] < indexes[i] {
				indexes[i], indexes[j] = indexes[j], indexes[i]
			}
		}
	}
	for _, index := range indexes {
		call := b.calls[index]
		toolCalls = append(toolCalls, ChatBridgeCall{
			ID:        call.id,
			Name:      call.name,
			Arguments: call.arguments,
		})
	}
	finish := "stop"
	if b.finishReason == "tool_calls" || len(b.calls) > 0 {
		finish = "tool_calls"
	}
	failure := ""
	if b.failure != nil {
		failure = b.failure.message
	}
	return ChatBridgeResult{
		Content:      b.content,
		ToolCalls:    toolCalls,
		FinishReason: finish,
		Usage:        b.usage,
		Failure:      failure,
	}
}

// Finish emits the terminal Responses events: `.done` markers for the message
// item and each function call, then `response.completed`.
func (b *FromChatBridge) Finish() []any {
	if b.completed {
		return nil
	}
	b.completed = true
	var events []any
	var output []any

	if b.hasMessage {
		events = append(events, wire.Body{
			"type":          "response.output_text.done",
			"item_id":       b.messageItemID,
			"output_index":  float64(b.messageOutputIndex),
			"content_index": float64(0),
			"text":          b.content,
		})
		events = append(events, wire.Body{
			"type":          "response.content_part.done",
			"item_id":       b.messageItemID,
			"output_index":  float64(b.messageOutputIndex),
			"content_index": float64(0),
			"part":          wire.Body{"type": "output_text", "text": b.content},
		})
		messageItem := wire.Body{
			"type":    "message",
			"id":      b.messageItemID,
			"role":    "assistant",
			"status":  "completed",
			"content": []any{wire.Body{"type": "output_text", "text": b.content}},
		}
		events = append(events, wire.Body{
			"type":         "response.output_item.done",
			"output_index": float64(b.messageOutputIndex),
			"item":         messageItem,
		})
		output = append(output, messageItem)
	} else if len(b.content) > 0 || (len(b.reasoning) > 0 && len(b.calls) == 0) {
		// No streamed message item (e.g. reasoning-only). Still surface
		// something Codex can show.
		text := b.content
		if text == "" {
			text = b.reasoning
		}
		output = append(output, wire.Body{
			"type":    "message",
			"id":      "msg_" + b.ID,
			"role":    "assistant",
			"status":  "completed",
			"content": []any{wire.Body{"type": "output_text", "text": text}},
		})
	}

	indexes := make([]int, 0, len(b.calls))
	for index := range b.calls {
		indexes = append(indexes, index)
	}
	for i := 0; i < len(indexes); i++ {
		for j := i + 1; j < len(indexes); j++ {
			if indexes[j] < indexes[i] {
				indexes[i], indexes[j] = indexes[j], indexes[i]
			}
		}
	}
	for _, index := range indexes {
		call := b.calls[index]
		events = append(events, wire.Body{
			"type":         "response.function_call_arguments.done",
			"item_id":      call.itemID,
			"output_index": float64(call.outputIndex),
			"arguments":    call.arguments,
		})
		item := wire.Body{
			"type":      "function_call",
			"id":        call.itemID,
			"call_id":   call.id,
			"name":      call.name,
			"arguments": call.arguments,
			"status":    "completed",
		}
		events = append(events, wire.Body{
			"type":         "response.output_item.done",
			"output_index": float64(call.outputIndex),
			"item":         item,
		})
		output = append(output, item)
	}

	events = append(events, wire.Body{
		"type":     "response.completed",
		"response": b.responseSkeleton("completed", output),
	})
	return events
}

// FromChatStream adapts FromChatBridge to wire.Translator, emitting
// `event: <type>` frames for a Responses client.
type FromChatStream struct {
	bridge   *FromChatBridge
	onFinish func(ChatBridgeResult)
}

// NewFromChatStream builds a Translator emitting Responses SSE.
func NewFromChatStream(model string, onFinish func(ChatBridgeResult)) *FromChatStream {
	return &FromChatStream{bridge: NewFromChatBridge(model), onFinish: onFinish}
}

// Bridge exposes the underlying bridge for mid-stream state.
func (s *FromChatStream) Bridge() *FromChatBridge { return s.bridge }

// Handle implements wire.Translator.
func (s *FromChatStream) Handle(event wire.Body, sink wire.EventSink) {
	for _, payload := range s.bridge.Handle(event) {
		sink.EmitEvent(payload)
	}
}

// Finish implements wire.Translator.
func (s *FromChatStream) Finish(sink wire.EventSink) {
	for _, payload := range s.bridge.Finish() {
		sink.EmitEvent(payload)
	}
	if s.onFinish != nil {
		s.onFinish(s.bridge.Result())
	}
}

// ChatCompletionFrom folds a ChatBridgeResult into a non-streaming
// chat.completion body.
func ChatCompletionFrom(result ChatBridgeResult, model, id string, created int64) wire.Body {
	message := wire.Body{"role": "assistant"}
	if result.Content != "" {
		message["content"] = result.Content
	} else {
		message["content"] = nil
	}
	if len(result.ToolCalls) > 0 {
		var calls []any
		for index, call := range result.ToolCalls {
			calls = append(calls, wire.Body{
				"index": float64(index),
				"id":    call.ID,
				"type":  "function",
				"function": wire.Body{
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			})
		}
		message["tool_calls"] = calls
	}
	return wire.Body{
		"id":      id,
		"object":  "chat.completion",
		"created": float64(created),
		"model":   model,
		"choices": []any{wire.Body{
			"index":         float64(0),
			"message":       message,
			"finish_reason": result.FinishReason,
		}},
		"usage": wire.Body{
			"prompt_tokens":     float64(result.Usage.Input),
			"completion_tokens": float64(result.Usage.Output),
			"total_tokens":      float64(result.Usage.Input + result.Usage.Output),
		},
	}
}

// ChatResultFromResponse folds a non-streaming Responses object into a
// ChatBridgeResult.
func ChatResultFromResponse(response wire.Body) ChatBridgeResult {
	var content string
	var toolCalls []ChatBridgeCall
	for _, raw := range wire.AsSlice(response["output"]) {
		item := wire.AsRecord(raw)
		switch item["type"] {
		case "message":
			for _, rawPart := range wire.AsSlice(item["content"]) {
				part := wire.AsRecord(rawPart)
				if s, ok := part["text"].(string); ok {
					content += s
				}
			}
		case "function_call":
			arguments, _ := item["arguments"].(string)
			toolCalls = append(toolCalls, ChatBridgeCall{
				ID:        callIDOf(item["call_id"], item["id"]),
				Name:      wire.AsString(item["name"]),
				Arguments: arguments,
			})
		}
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	return ChatBridgeResult{
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: finish,
		Usage:        Usage(response["usage"]),
	}
}

// ErrorMessage pulls the first `response.failed` error message out of an
// event list, for logging the real reason a Responses stream died.
func ErrorMessage(events []wire.Body) string {
	for _, event := range events {
		if event["type"] != "response.failed" {
			continue
		}
		response := wire.AsRecord(event["response"])
		errObj := wire.AsRecord(response["error"])
		if msg, ok := errObj["message"].(string); ok {
			return msg
		}
		return "upstream response failed"
	}
	return ""
}

// PassthroughRepairStream is a Translator that forwards Responses SSE frames
// unchanged except for rewriting an empty `response.completed.output` from
// earlier `output_item.done` events — needed for ChatGPT remote compaction
// v2, where the compaction payload often arrives only on the item event.
type PassthroughRepairStream struct {
	seen        []wire.Body
	onCompleted func(wire.Body)
}

// NewPassthroughRepairStream builds the passthrough Translator.
func NewPassthroughRepairStream(onCompleted func(wire.Body)) *PassthroughRepairStream {
	return &PassthroughRepairStream{onCompleted: onCompleted}
}

// Handle implements wire.Translator.
func (s *PassthroughRepairStream) Handle(event wire.Body, sink wire.EventSink) {
	s.seen = append(s.seen, event)
	typ := event["type"]
	if typ != "response.completed" && typ != "response.done" {
		sink.EmitData(event)
		return
	}
	response := wire.AsRecord(event["response"])
	repaired := RepairOutput(response, s.seen)
	next := event
	if !outputEqual(repaired["output"], response["output"]) {
		next = cloneMap(event)
		next["response"] = repaired
	}
	if next["type"] == "response.completed" && s.onCompleted != nil {
		s.onCompleted(wire.AsRecord(next["response"]))
	}
	sink.EmitData(next)
}

// Finish implements wire.Translator.
func (s *PassthroughRepairStream) Finish(sink wire.EventSink) {}

func outputEqual(a, b any) bool {
	return wire.MarshalJSON(a) == wire.MarshalJSON(b)
}
