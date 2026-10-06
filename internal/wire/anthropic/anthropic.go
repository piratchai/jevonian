// Package anthropic translates between the canonical OpenAI Chat Completions
// shape and Anthropic's Messages wire, in both directions, plus the SSE
// grammars that carry them. Port of src/anthropic.ts,
// src/chat-anthropic-stream.ts and src/anthropic-thinking.ts.
package anthropic

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

// Anthropic rejects `tool_use.id` / `tool_result.tool_use_id` outside this charset.
var anthropicToolIDCharset = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var invalidToolIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

const maxAnthropicToolIDLength = 64

// randomHex returns n hex chars from crypto/rand (uuid-less; stdlib only).
func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand never realistically fails; fall back deterministically.
		sum := sha256.Sum256([]byte(fmt.Sprintf("jevonian-%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:])[:n]
	}
	return hex.EncodeToString(buf)[:n]
}

// ToolID maps an OpenAI / Responses call id onto Anthropic's `^[a-zA-Z0-9_-]+$`
// charset. Deterministic so a tool_use and its tool_result stay paired after
// sanitization. Any id that had to change carries a hash of the original:
// stripping punctuation alone would fold `call:a` and `call.a` onto one id,
// and Anthropic rejects duplicate tool_use ids in a turn.
func ToolID(raw any) string {
	id := wire.AsString(raw)
	if anthropicToolIDCharset.MatchString(id) && len(id) <= maxAnthropicToolIDLength {
		return id
	}
	sum := sha256.Sum256([]byte(nonEmpty(id, "empty")))
	hash := hex.EncodeToString(sum[:])
	sanitized := invalidToolIDChars.ReplaceAllString(id, "_")
	sanitized = regexp.MustCompile(`_+`).ReplaceAllString(sanitized, "_")
	sanitized = strings.Trim(sanitized, "_")
	if len(sanitized) == 0 {
		return "tool_" + hash[:24]
	}
	suffix := "_" + hash[:8]
	if len(sanitized) > maxAnthropicToolIDLength-len(suffix) {
		sanitized = sanitized[:maxAnthropicToolIDLength-len(suffix)]
	}
	return sanitized + suffix
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// NeedsWire reports whether a model id should speak the Anthropic Messages
// wire: Claude-family ids, or any id carrying an `anthropic` vendor marker.
func NeedsWire(model string) bool {
	tail := wire.BareModelID(model)
	lower := strings.ToLower(model)
	return strings.HasPrefix(strings.ToLower(tail), "claude") || strings.Contains(lower, "anthropic")
}

func parseArgs(value any) wire.Body {
	if s, ok := value.(string); ok {
		var parsed wire.Body
		if err := json.Unmarshal([]byte(s), &parsed); err == nil && parsed != nil {
			return parsed
		}
		return wire.Body{}
	}
	return wire.AsRecord(value)
}

func toolResultContent(value any) string {
	text := wire.TextOf(value)
	if len(text) > 0 {
		return text
	}
	return "(no output)"
}

// chatToolsToAnthropic converts OpenAI `tools` to Anthropic `tools`.
func chatToolsToAnthropic(raw any) []any {
	tools := wire.AsSlice(raw)
	if tools == nil {
		return nil
	}
	var out []any
	for _, entry := range tools {
		tool := wire.AsRecord(entry)
		if tool["type"] != "function" {
			continue
		}
		fn := wire.AsRecord(tool["function"])
		name := wire.AsString(fn["name"])
		if name == "" {
			continue
		}
		converted := wire.Body{
			"name":         name,
			"input_schema": wire.AsRecord(fn["parameters"]),
		}
		if desc, ok := fn["description"].(string); ok {
			converted["description"] = desc
		}
		out = append(out, converted)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toolChoiceFor(choice any) wire.Body {
	if choice == "required" {
		return wire.Body{"type": "any"}
	}
	if choice == "none" {
		return wire.Body{"type": "none"}
	}
	if m, ok := choice.(map[string]any); ok {
		name := wire.AsString(wire.AsRecord(m["function"])["name"])
		if name != "" {
			return wire.Body{"type": "tool", "name": name}
		}
	}
	return nil
}

// isPlainAssistantMessage reports whether an Anthropic message is an assistant
// turn carrying no `tool_use` block.
func isPlainAssistantMessage(message wire.Body) bool {
	if message["role"] != "assistant" {
		return false
	}
	content := message["content"]
	if _, ok := content.(string); ok {
		return true
	}
	blocks, ok := content.([]any)
	if !ok {
		return true
	}
	for _, block := range blocks {
		if wire.AsRecord(block)["type"] == "tool_use" {
			return false
		}
	}
	return true
}

// NormalizePrefill drops trailing plain-assistant prefill messages on models
// that reject prefill, so the request ends on the user/tool turn before them.
// A trailing assistant that carries `tool_use` is left alone: that is an
// in-flight tool call awaiting its result, not a prefill, and dropping it
// would orphan the call. Models that still support prefill (Claude 4.5 /
// Haiku 4.5 and older) are never touched.
func NormalizePrefill(body wire.Body) wire.Body {
	if !RejectsAssistantPrefill(body["model"]) {
		return body
	}
	messages := wire.AsSlice(body["messages"])
	if len(messages) == 0 {
		return body
	}
	if !isPlainAssistantMessage(wire.AsRecord(messages[len(messages)-1])) {
		return body
	}

	end := len(messages) - 1
	for end > 0 && isPlainAssistantMessage(wire.AsRecord(messages[end-1])) {
		end--
	}
	trimmed := messages[:end]
	if len(trimmed) == 0 {
		// A body that is nothing but assistant turns cannot be trimmed to
		// empty (Anthropic requires at least one message). Relocate the
		// newest one to a user turn instead, flattening to text so no
		// assistant-only block (e.g. `thinking`) ends up in a user turn.
		only := wire.AsRecord(messages[len(messages)-1])
		text := strings.TrimSpace(wire.TextOf(only["content"]))
		if text == "" {
			// An assistant turn can be empty; Anthropic rejects empty user
			// content, so fall back to a placeholder that still asks the
			// model to continue.
			text = "Continue."
		}
		next := cloneMap(body)
		next["messages"] = []any{wire.Body{"role": "user", "content": text}}
		return next
	}
	next := cloneMap(body)
	next["messages"] = trimmed
	return next
}

// ephemeralCache is the Anthropic prompt-cache breakpoint marker.
func ephemeralCache() wire.Body {
	return wire.Body{"type": "ephemeral"}
}

// ChatToAnthropic folds an OpenAI Chat Completions body into an Anthropic
// Messages body. System/developer messages become the `system` field, tool
// results become `tool_result` blocks inside a user turn, tools are re-spelled
// with `input_schema`, and three prompt-cache breakpoints are marked (system,
// last tool, last message block).
func ChatToAnthropic(body wire.Body) wire.Body {
	messages := wire.AsSlice(body["messages"])
	var systemTexts []string
	type converted struct {
		role    string
		content []any
	}
	var conv []converted

	push := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if len(conv) > 0 && conv[len(conv)-1].role == role {
			conv[len(conv)-1].content = append(conv[len(conv)-1].content, blocks...)
			return
		}
		conv = append(conv, converted{role: role, content: blocks})
	}

	for _, raw := range messages {
		message := wire.AsRecord(raw)
		role := message["role"]
		if role == "system" || role == "developer" {
			if text := wire.TextOf(message["content"]); text != "" {
				systemTexts = append(systemTexts, text)
			}
			continue
		}
		if role == "tool" || role == "function" {
			push("user", []any{wire.Body{
				"type":        "tool_result",
				"tool_use_id": ToolID(message["tool_call_id"]),
				"content":     toolResultContent(message["content"]),
			}})
			continue
		}
		if role == "assistant" {
			var blocks []any
			if text := wire.TextOf(message["content"]); text != "" {
				blocks = append(blocks, wire.Body{"type": "text", "text": text})
			}
			for _, rawCall := range wire.AsSlice(message["tool_calls"]) {
				call := wire.AsRecord(rawCall)
				fn := wire.AsRecord(call["function"])
				blocks = append(blocks, wire.Body{
					"type":  "tool_use",
					"id":    ToolID(call["id"]),
					"name":  wire.AsString(fn["name"]),
					"input": parseArgs(fn["arguments"]),
				})
			}
			push("assistant", blocks)
			continue
		}
		text := wire.TextOf(message["content"])
		if text != "" {
			push("user", []any{wire.Body{"type": "text", "text": text}})
		} else {
			push("user", nil)
		}
	}

	max := body["max_completion_tokens"]
	if max == nil {
		max = body["max_tokens"]
	}
	maxTokens := 4096
	if wire.IsNumber(max) && wire.Number(max) > 0 {
		maxTokens = int(wire.Number(max))
	}
	out := wire.Body{
		"messages":   conv,
		"max_tokens": float64(maxTokens),
	}
	// Prompt caching: mark stable prefixes so Claude subscription turns reuse
	// the system prompt, tool schemas, and conversation prefix. Anthropic
	// allows up to four breakpoints; these three cover the usual Chat
	// Completions shape without needing the client to opt in.
	if len(systemTexts) > 0 {
		out["system"] = []any{wire.Body{
			"type":          "text",
			"text":          strings.Join(systemTexts, "\n\n"),
			"cache_control": ephemeralCache(),
		}}
	}
	if v, ok := body["temperature"]; ok && wire.IsNumber(v) {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok && wire.IsNumber(v) {
		out["top_p"] = v
	}
	switch stop := body["stop"].(type) {
	case string:
		out["stop_sequences"] = []any{stop}
	case []any:
		var sequences []any
		for _, item := range stop {
			if s, ok := item.(string); ok {
				sequences = append(sequences, s)
			}
		}
		if len(sequences) > 0 {
			out["stop_sequences"] = sequences
		}
	}
	if tools := chatToolsToAnthropic(body["tools"]); tools != nil {
		last := wire.AsRecord(tools[len(tools)-1])
		withCache := cloneMap(last)
		withCache["cache_control"] = ephemeralCache()
		tools[len(tools)-1] = withCache
		out["tools"] = tools
	}
	if choice := toolChoiceFor(body["tool_choice"]); choice != nil {
		out["tool_choice"] = choice
	}
	// Cache up through the last message block so growing agent history still
	// hits the prefix written on the previous turn.
	if len(conv) > 0 && len(conv[len(conv)-1].content) > 0 {
		blocks := conv[len(conv)-1].content
		last := wire.AsRecord(blocks[len(blocks)-1])
		if last != nil {
			withCache := cloneMap(last)
			withCache["cache_control"] = ephemeralCache()
			blocks[len(blocks)-1] = withCache
		}
	}
	// Materialize `messages` as []any (the `converted` slice is internal).
	out["messages"] = func() []any {
		msgs := make([]any, 0, len(conv))
		for _, c := range conv {
			msgs = append(msgs, wire.Body{"role": c.role, "content": c.content})
		}
		return msgs
	}()
	return out
}

// ToChatRequest is the inverse of ChatToAnthropic: an Anthropic `/messages`
// body as OpenAI chat. Needed when an Anthropic client is routed to a
// `both` provider's OpenAI-only model — that model must leave on
// /chat/completions, then the reply is folded back with ChatToMessage.
func ToChatRequest(body wire.Body, model string) wire.Body {
	messages := []any{}
	switch system := body["system"].(type) {
	case string:
		if system != "" {
			messages = append(messages, wire.Body{"role": "system", "content": system})
		}
	case []any:
		var parts []string
		for _, block := range system {
			record := wire.AsRecord(block)
			if text, ok := record["text"].(string); ok {
				if text != "" {
					parts = append(parts, text)
				}
			} else if t := wire.TextOf(block); t != "" {
				parts = append(parts, t)
			}
		}
		if text := strings.Join(parts, "\n\n"); text != "" {
			messages = append(messages, wire.Body{"role": "system", "content": text})
		}
	}

	for _, raw := range wire.AsSlice(body["messages"]) {
		message := wire.AsRecord(raw)
		role := message["role"]
		if role != "user" && role != "assistant" {
			continue
		}
		if content, ok := message["content"].(string); ok {
			var value any = content
			if content == "" && role == "assistant" {
				value = nil
			}
			messages = append(messages, wire.Body{"role": role, "content": value})
			continue
		}
		content := wire.AsSlice(message["content"])
		var texts []string
		var images []any
		var toolCalls []any
		for _, rawBlock := range content {
			block := wire.AsRecord(rawBlock)
			switch block["type"] {
			case "text":
				if text := wire.AsString(block["text"]); text != "" {
					texts = append(texts, text)
				}
			case "image":
				if role == "user" {
					if image := imageAsChat(block); image != nil {
						images = append(images, image)
					}
				}
			case "tool_use":
				toolCalls = append(toolCalls, wire.Body{
					"id":   wire.AsString(block["id"]),
					"type": "function",
					"function": wire.Body{
						"name":      wire.AsString(block["name"]),
						"arguments": wire.MarshalJSON(wire.AsRecord(block["input"])),
					},
				})
			case "tool_result":
				messages = append(messages, wire.Body{
					"role":         "tool",
					"tool_call_id": wire.AsString(block["tool_use_id"]),
					"content":      toolResultContent(block["content"]),
				})
			}
		}
		if role == "assistant" {
			entry := wire.Body{"role": "assistant"}
			if len(texts) > 0 {
				entry["content"] = strings.Join(texts, "\n")
			} else {
				entry["content"] = nil
			}
			if len(toolCalls) > 0 {
				entry["tool_calls"] = toolCalls
			}
			messages = append(messages, entry)
			continue
		}
		if len(images) > 0 {
			// Images keep their place beside the text as Chat content parts;
			// a text-only turn stays a plain string, which every Chat host
			// accepts.
			var parts []any
			for _, text := range texts {
				parts = append(parts, wire.Body{"type": "text", "text": text})
			}
			parts = append(parts, images...)
			messages = append(messages, wire.Body{"role": "user", "content": parts})
			continue
		}
		if len(texts) > 0 {
			messages = append(messages, wire.Body{"role": "user", "content": strings.Join(texts, "\n")})
		}
	}

	max := body["max_tokens"]
	maxTokens := 4096
	if wire.IsNumber(max) && wire.Number(max) > 0 {
		maxTokens = int(wire.Number(max))
	}
	out := wire.Body{
		"model":      model,
		"messages":   messages,
		"max_tokens": float64(maxTokens),
	}
	// Without its tools the model cannot call any, and an agent turn silently
	// degrades into a text answer — the tool definitions travel with the
	// conversation.
	for k, v := range ToolsAsChat(body) {
		out[k] = v
	}
	if v, ok := body["temperature"]; ok && wire.IsNumber(v) {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok && wire.IsNumber(v) {
		out["top_p"] = v
	}
	if v, ok := body["stream"]; ok {
		if b, isBool := v.(bool); isBool {
			out["stream"] = b
		}
	}
	return out
}

// imageAsChat converts an Anthropic image block to a Chat `image_url` part:
// inline base64 or a URL source.
func imageAsChat(block wire.Body) wire.Body {
	source := wire.AsRecord(block["source"])
	if source["type"] == "base64" {
		mediaType, mok := source["media_type"].(string)
		data, dok := source["data"].(string)
		if mok && dok {
			return wire.Body{
				"type":      "image_url",
				"image_url": wire.Body{"url": "data:" + mediaType + ";base64," + data},
			}
		}
	}
	if source["type"] == "url" {
		if u, ok := source["url"].(string); ok {
			return wire.Body{
				"type":      "image_url",
				"image_url": wire.Body{"url": u},
			}
		}
	}
	return nil
}

// ToolsAsChat converts Anthropic `tools` to Chat Completions function tools.
// Server tools (no schema) are dropped.
func ToolsAsChat(body wire.Body) wire.Body {
	var tools []any
	for _, raw := range wire.AsSlice(body["tools"]) {
		tool := wire.AsRecord(raw)
		name := wire.AsString(tool["name"])
		schema, isObject := tool["input_schema"].(map[string]any)
		if name == "" || !isObject || schema == nil {
			continue
		}
		fn := wire.Body{"name": name, "parameters": tool["input_schema"]}
		if desc, ok := tool["description"].(string); ok {
			fn["description"] = desc
		}
		tools = append(tools, wire.Body{"type": "function", "function": fn})
	}
	choice := wire.AsRecord(body["tool_choice"])
	var toolChoice any
	switch {
	case choice["type"] == "any":
		toolChoice = "required"
	case choice["type"] == "none":
		toolChoice = "none"
	case choice["type"] == "tool" && wire.AsString(choice["name"]) != "":
		toolChoice = wire.Body{
			"type":     "function",
			"function": wire.Body{"name": wire.AsString(choice["name"])},
		}
	case choice["type"] == "auto":
		toolChoice = "auto"
	}
	out := wire.Body{}
	if len(tools) > 0 {
		out["tools"] = tools
		if toolChoice != nil {
			out["tool_choice"] = toolChoice
		}
	}
	return out
}

var chatStopReasons = map[string]string{
	"stop":           "end_turn",
	"length":         "max_tokens",
	"tool_calls":     "tool_use",
	"content_filter": "refusal",
}

// ChatToMessage folds an OpenAI chat completion back into Anthropic's
// `/messages` response shape.
func ChatToMessage(response wire.Body, model string) wire.Body {
	choices := wire.AsSlice(response["choices"])
	choice := wire.Body{}
	if len(choices) > 0 {
		choice = wire.AsRecord(choices[0])
	}
	message := wire.AsRecord(choice["message"])
	var content []any
	if text := wire.AsString(message["content"]); text != "" {
		content = append(content, wire.Body{"type": "text", "text": text})
	}
	for _, rawCall := range wire.AsSlice(message["tool_calls"]) {
		call := wire.AsRecord(rawCall)
		fn := wire.AsRecord(call["function"])
		content = append(content, wire.Body{
			"type":  "tool_use",
			"id":    wire.AsString(call["id"]),
			"name":  wire.AsString(fn["name"]),
			"input": parseArgs(fn["arguments"]),
		})
	}
	usage := wire.AsRecord(response["usage"])
	finish := wire.AsString(choice["finish_reason"])
	stop, ok := chatStopReasons[finish]
	if !ok {
		stop = "end_turn"
	}
	id := wire.AsString(response["id"])
	if id == "" {
		id = "msg_" + randomHex(24)
	}
	m := wire.AsString(response["model"])
	if m == "" {
		m = model
	}
	return wire.Body{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         m,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": wire.Body{
			"input_tokens":  wire.Number(usage["prompt_tokens"]),
			"output_tokens": wire.Number(usage["completion_tokens"]),
		},
	}
}

// Usage extracts the cross-wire Usage from an Anthropic usage object.
func Usage(raw any) wire.Usage {
	usage := wire.AsRecord(raw)
	return wire.Usage{
		Input:      int(wire.Number(usage["input_tokens"])),
		Output:     int(wire.Number(usage["output_tokens"])),
		CacheRead:  int(wire.Number(usage["cache_read_input_tokens"])),
		CacheWrite: int(wire.Number(usage["cache_creation_input_tokens"])),
	}
}

var stopReasons = map[string]string{
	"end_turn":      "stop",
	"stop_sequence": "stop",
	"max_tokens":    "length",
	"tool_use":      "tool_calls",
	"refusal":       "content_filter",
}

// Call is one tool call folded out of an Anthropic response.
type Call struct {
	ID        string
	Name      string
	Arguments string
}

// Result is the folded outcome of an Anthropic message.
type Result struct {
	Text         string
	Calls        []Call
	FinishReason string
	Usage        wire.Usage
}

// ResultOf folds an Anthropic message response into text, calls and usage.
func ResultOf(response wire.Body) Result {
	var text strings.Builder
	var calls []Call
	for _, raw := range wire.AsSlice(response["content"]) {
		block := wire.AsRecord(raw)
		switch block["type"] {
		case "text":
			if s, ok := block["text"].(string); ok {
				text.WriteString(s)
			}
		case "tool_use":
			input := block["input"]
			if input == nil {
				input = wire.Body{}
			}
			calls = append(calls, Call{
				ID:        wire.AsString(block["id"]),
				Name:      wire.AsString(block["name"]),
				Arguments: wire.MarshalJSON(input),
			})
		}
	}
	stop := wire.AsString(response["stop_reason"])
	finish, ok := stopReasons[stop]
	if !ok {
		finish = "stop"
	}
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	return Result{
		Text:         text.String(),
		Calls:        calls,
		FinishReason: finish,
		Usage:        Usage(response["usage"]),
	}
}

// ToChat folds a non-streaming Anthropic message response into an OpenAI
// chat.completion object.
func ToChat(response wire.Body, model string) wire.Body {
	result := ResultOf(response)
	message := wire.Body{"role": "assistant"}
	if result.Text != "" {
		message["content"] = result.Text
	} else {
		message["content"] = nil
	}
	if len(result.Calls) > 0 {
		var calls []any
		for index, call := range result.Calls {
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", index)
			}
			calls = append(calls, wire.Body{
				"index": float64(index),
				"id":    id,
				"type":  "function",
				"function": wire.Body{
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			})
		}
		message["tool_calls"] = calls
	}
	id := wire.AsString(response["id"])
	if id == "" {
		id = "chatcmpl-" + randomHex(24)
	}
	m := wire.AsString(response["model"])
	if m == "" {
		m = model
	}
	return wire.Body{
		"id":      id,
		"object":  "chat.completion",
		"created": float64(time.Now().Unix()),
		"model":   m,
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

func cloneMap(m wire.Body) wire.Body {
	next := make(wire.Body, len(m))
	for k, v := range m {
		next[k] = v
	}
	return next
}
