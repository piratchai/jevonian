// Package responses translates between the canonical OpenAI Chat Completions
// shape and the OpenAI Responses (`/v1/responses`) wire, in both directions,
// plus the SSE grammars that carry them. Port of src/responses.ts.
package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/xinyao27/jevonian/internal/wire"
)

// OpenAI Responses rejects `call_id` longer than this (string_above_max_length).
const maxCallIDLength = 64

func syntheticCallID() string {
	return "call_" + randomHex(16)
}

// clampCallID maps oversized / corrupted ids to a stable short form so
// function_call / function_call_output pairs still match after clamp.
// Bridged clients can emit ids past the Responses 64-char max, or concatenate
// two ids with a newline (still rejected).
func clampCallID(id string) string {
	// Prefer a single short line when the client glued two ids together.
	var lines []string
	for _, part := range strings.FieldsFunc(id, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	var candidate string
	if len(lines) > 1 {
		candidate = lines[0]
		for _, part := range lines {
			if len(part) <= maxCallIDLength {
				candidate = part
				break
			}
		}
	} else if len(lines) == 1 {
		candidate = lines[0]
	} else {
		candidate = strings.TrimSpace(id)
	}
	if candidate != "" && len(candidate) <= maxCallIDLength {
		return candidate
	}
	sum := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(sum[:])[:24]
}

// callIDOf prefers real ids; never returns "" — OpenAI Responses rejects an
// empty `call_id`.
func callIDOf(values ...any) string {
	id := wire.FirstNonEmpty(values...)
	if id == "" {
		return syntheticCallID()
	}
	return clampCallID(id)
}

// toolNameOf never returns "" — OpenAI Responses rejects empty `name` on
// function_call items (minLength 1).
func toolNameOf(values ...any) string {
	if name := wire.FirstNonEmpty(values...); name != "" {
		return name
	}
	return "tool"
}

// EnsureCallIDs fills empty / oversized `call_id` and empty `name` on
// Responses `input` items before egress, and pairs orphan
// `function_call_output` items with preceding unpaired `function_call`s.
// Bridged history can leave "" on both fields after broken stream merges, or
// grow `call_id` past 64 characters.
func EnsureCallIDs(body wire.Body) wire.Body {
	input := wire.AsSlice(body["input"])
	if len(input) == 0 {
		return body
	}
	var unpaired []string
	changed := false
	lastName := ""
	next := make([]any, 0, len(input))
	for _, raw := range input {
		item := wire.AsRecord(raw)
		switch item["type"] {
		case "function_call":
			callID := callIDOf(item["call_id"])
			name := toolNameOf(item["name"], lastName)
			if wire.FirstNonEmpty(item["name"]) != "" {
				lastName = wire.AsString(item["name"])
			}
			unpaired = append(unpaired, callID)
			if callID == item["call_id"] && name == item["name"] {
				next = append(next, raw)
				continue
			}
			changed = true
			out := cloneMap(item)
			out["call_id"] = callID
			out["name"] = name
			next = append(next, out)
		case "function_call_output":
			existing := wire.FirstNonEmpty(item["call_id"])
			var callID string
			if existing != "" {
				callID = clampCallID(existing)
				for i, id := range unpaired {
					if id == callID {
						unpaired = append(unpaired[:i], unpaired[i+1:]...)
						break
					}
				}
			} else if len(unpaired) > 0 {
				callID = unpaired[0]
				unpaired = unpaired[1:]
			} else {
				callID = syntheticCallID()
			}
			if callID == item["call_id"] {
				next = append(next, raw)
				continue
			}
			changed = true
			out := cloneMap(item)
			out["call_id"] = callID
			next = append(next, out)
		default:
			next = append(next, raw)
		}
	}
	if !changed {
		return body
	}
	out := cloneMap(body)
	out["input"] = next
	return out
}

// Usage extracts the cross-wire Usage from a Responses usage object.
func Usage(raw any) wire.Usage {
	usage := wire.AsRecord(raw)
	details := wire.AsRecord(usage["input_tokens_details"])
	return wire.Usage{
		Input:     int(wire.Number(usage["input_tokens"])),
		Output:    int(wire.Number(usage["output_tokens"])),
		CacheRead: int(wire.Number(details["cached_tokens"])),
	}
}

// compactionTriggerTypes are the input markers Codex uses for remote
// compaction v2 (`POST /v1/responses`).
var compactionTriggerTypes = map[string]bool{
	"compaction_trigger": true,
	"context_compaction": true,
}

// compactionOutputTypes are the output item types Codex accepts as the
// compaction result.
var compactionOutputTypes = map[string]bool{
	"compaction":         true,
	"compaction_summary": true,
	"context_compaction": true,
}

// IsRemoteCompactionV2 reports whether the Responses body is a Codex
// remote-compaction v2 turn. Those requests must stay on a native Responses
// upstream — bridging them to Chat Completions yields an ordinary message
// item and Codex fails with "expected exactly one compaction output item".
func IsRemoteCompactionV2(body wire.Body) bool {
	for _, raw := range wire.AsSlice(body["input"]) {
		if compactionTriggerTypes[wire.AsString(wire.AsRecord(raw)["type"])] {
			return true
		}
	}
	return false
}

// IsCompactionOutputItem reports whether an output item is a compaction item.
func IsCompactionOutputItem(item any) bool {
	return compactionOutputTypes[wire.AsString(wire.AsRecord(item)["type"])]
}

// CollectOutputItems pulls output items from SSE `output_item.*` events.
// Upstream often puts the real compaction payload on `response.output_item.done`
// while `response.completed` still has `output: []`. `.done` wins over `.added`
// for the same index.
func CollectOutputItems(events []wire.Body) []any {
	byIndex := map[int]any{}
	var unordered []any
	for _, event := range events {
		typ := wire.AsString(event["type"])
		if typ != "response.output_item.done" && typ != "response.output_item.added" {
			continue
		}
		item, present := event["item"]
		if !present || item == nil {
			continue
		}
		if raw, present := event["output_index"]; present && wire.IsNumber(raw) {
			index := int(wire.Number(raw))
			if typ == "response.output_item.done" {
				byIndex[index] = item
			} else if _, exists := byIndex[index]; !exists {
				byIndex[index] = item
			}
		} else {
			unordered = append(unordered, item)
		}
	}
	indexes := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}
	// Sort by output_index.
	for i := 0; i < len(indexes); i++ {
		for j := i + 1; j < len(indexes); j++ {
			if indexes[j] < indexes[i] {
				indexes[i], indexes[j] = indexes[j], indexes[i]
			}
		}
	}
	var ordered []any
	for _, index := range indexes {
		ordered = append(ordered, byIndex[index])
	}
	return append(ordered, unordered...)
}

// RepairOutput rebuilds a terminal Responses object's empty `output` from SSE
// `output_item.*` events so Codex still sees the compaction item.
func RepairOutput(response wire.Body, events []wire.Body) wire.Body {
	if existing := wire.AsSlice(response["output"]); len(existing) > 0 {
		return response
	}
	collected := CollectOutputItems(events)
	if len(collected) == 0 {
		return response
	}
	next := cloneMap(response)
	next["output"] = collected
	return next
}

func textOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if parts, ok := value.([]any); ok {
		var texts []string
		for _, block := range parts {
			record := wire.AsRecord(block)
			if t, ok := record["text"].(string); ok {
				texts = append(texts, t)
				continue
			}
			if record["type"] == "image_url" || record["type"] == "input_image" {
				texts = append(texts, "[image]")
				continue
			}
			texts = append(texts, wire.MarshalJSON(record))
		}
		return strings.Join(texts, "\n")
	}
	if value == nil {
		return ""
	}
	return wire.MarshalJSON(value)
}

func chatToolToResponses(raw any) []any {
	tool := wire.AsRecord(raw)
	if tool["type"] != "function" {
		return nil
	}
	fn := wire.AsRecord(tool["function"])
	name, ok := fn["name"].(string)
	if !ok || name == "" {
		return nil
	}
	out := wire.Body{
		"type":       "function",
		"name":       name,
		"parameters": wire.AsRecord(fn["parameters"]),
		"strict":     false,
	}
	if desc, ok := fn["description"].(string); ok {
		out["description"] = desc
	}
	return []any{out}
}

func responsesToolToChat(raw any) wire.Body {
	tool := wire.AsRecord(raw)
	if tool["type"] != "function" {
		return nil
	}
	name := wire.AsString(tool["name"])
	if name == "" {
		return nil
	}
	fn := wire.Body{
		"name":       name,
		"parameters": wire.AsRecord(tool["parameters"]),
	}
	if desc, ok := tool["description"].(string); ok {
		fn["description"] = desc
	}
	return wire.Body{"type": "function", "function": fn}
}

func contentText(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	parts, ok := value.([]any)
	if !ok {
		if value == nil {
			return ""
		}
		return textOf(value)
	}
	var texts []string
	for _, block := range parts {
		record := wire.AsRecord(block)
		if t, ok := record["text"].(string); ok {
			texts = append(texts, t)
			continue
		}
		switch record["type"] {
		case "input_text", "output_text", "text":
			texts = append(texts, wire.AsString(record["text"]))
		case "input_image", "image_url":
			texts = append(texts, "[image]")
		}
	}
	var filtered []string
	for _, part := range texts {
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "\n")
}

// ToChatRequest is the inverse of ChatToResponses: a Responses `/responses`
// body as OpenAI chat. Needed when ChatGPT Desktop / Codex is routed off the
// ChatGPT subscription onto an OpenAI Chat Completions host.
func ToChatRequest(body wire.Body, model string) wire.Body {
	messages := []any{}
	if instructions, ok := body["instructions"].(string); ok && instructions != "" {
		messages = append(messages, wire.Body{"role": "system", "content": instructions})
	}

	var input []any
	if s, ok := body["input"].(string); ok {
		input = []any{wire.Body{"role": "user", "content": s}}
	} else {
		input = wire.AsSlice(body["input"])
	}
	for _, raw := range input {
		item := wire.AsRecord(raw)
		// Compaction markers are request-only; they must never become chat
		// messages.
		if compactionTriggerTypes[wire.AsString(item["type"])] || IsCompactionOutputItem(item) {
			continue
		}
		if item["type"] == "function_call" || item["type"] == "function_call_output" {
			if item["type"] == "function_call" {
				var arguments string
				if s, ok := item["arguments"].(string); ok {
					arguments = s
				} else if item["arguments"] != nil {
					arguments = wire.MarshalJSON(item["arguments"])
				} else {
					arguments = "{}"
				}
				messages = append(messages, wire.Body{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{wire.Body{
						"id":   callIDOf(item["call_id"], item["id"]),
						"type": "function",
						"function": wire.Body{
							"name":      wire.AsString(item["name"]),
							"arguments": arguments,
						},
					}},
				})
			} else {
				content := item["output"]
				if content == nil {
					content = item["content"]
				}
				messages = append(messages, wire.Body{
					"role":         "tool",
					"tool_call_id": callIDOf(item["call_id"], item["id"]),
					"content":      contentText(content),
				})
			}
			continue
		}

		role := "user"
		if item["role"] == "assistant" || item["role"] == "system" {
			role = wire.AsString(item["role"])
		}
		if role == "assistant" {
			var toolCalls []any
			// Rare Responses shape: assistant message with embedded function
			// calls in content.
			for _, rawPart := range wire.AsSlice(item["content"]) {
				part := wire.AsRecord(rawPart)
				if part["type"] == "function_call" || part["type"] == "tool_use" {
					var arguments string
					switch {
					case wire.AsString(part["arguments"]) != "":
						arguments = wire.AsString(part["arguments"])
					case part["arguments"] != nil:
						arguments = wire.MarshalJSON(part["arguments"])
					case part["input"] != nil:
						arguments = wire.MarshalJSON(part["input"])
					default:
						arguments = "{}"
					}
					toolCalls = append(toolCalls, wire.Body{
						"id":   callIDOf(part["call_id"], part["id"]),
						"type": "function",
						"function": wire.Body{
							"name":      wire.AsString(part["name"]),
							"arguments": arguments,
						},
					})
				}
			}
			text := contentText(item["content"])
			entry := wire.Body{"role": "assistant"}
			if text != "" {
				entry["content"] = text
			} else {
				entry["content"] = nil
			}
			if len(toolCalls) > 0 {
				entry["tool_calls"] = toolCalls
			}
			messages = append(messages, entry)
			continue
		}
		messages = append(messages, wire.Body{"role": role, "content": contentText(item["content"])})
	}

	var tools []any
	for _, raw := range wire.AsSlice(body["tools"]) {
		if converted := responsesToolToChat(raw); converted != nil {
			tools = append(tools, converted)
		}
	}
	reasoning := wire.AsRecord(body["reasoning"])
	max := body["max_output_tokens"]
	if max == nil {
		max = body["max_tokens"]
	}

	out := wire.Body{"model": model, "messages": messages}
	if s, ok := body["stream"].(bool); ok {
		out["stream"] = s
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if v, present := body["tool_choice"]; present {
		out["tool_choice"] = v
	}
	if v, ok := body["temperature"]; ok && wire.IsNumber(v) {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok && wire.IsNumber(v) {
		out["top_p"] = v
	}
	if wire.IsNumber(max) {
		out["max_tokens"] = max
	}
	if effort, ok := reasoning["effort"].(string); ok {
		out["reasoning_effort"] = effort
	}
	if key, ok := body["prompt_cache_key"].(string); ok {
		out["prompt_cache_key"] = key
	}
	return out
}

// ChatToResponses folds an OpenAI Chat Completions body into a Responses
// `/responses` body. System/developer messages become `instructions`,
// tool results become `function_call_output` items, and the reply is always
// requested as a stream with `store: false`.
func ChatToResponses(body wire.Body, model string) wire.Body {
	messages := wire.AsSlice(body["messages"])
	var instructions []string
	input := []any{}
	// Track assistant tool call ids so orphan tool results can pair by order.
	var unpairedCallIDs []string

	for _, raw := range messages {
		message := wire.AsRecord(raw)
		role := message["role"]
		if role == "system" || role == "developer" {
			if text := textOf(message["content"]); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if role == "tool" || role == "function" {
			existing := wire.FirstNonEmpty(message["tool_call_id"])
			var callID string
			if existing != "" {
				callID = clampCallID(existing)
				for i, id := range unpairedCallIDs {
					if id == callID {
						unpairedCallIDs = append(unpairedCallIDs[:i], unpairedCallIDs[i+1:]...)
						break
					}
				}
			} else if len(unpairedCallIDs) > 0 {
				callID = unpairedCallIDs[0]
				unpairedCallIDs = unpairedCallIDs[1:]
			} else {
				callID = syntheticCallID()
			}
			input = append(input, wire.Body{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  textOf(message["content"]),
			})
			continue
		}
		if role == "assistant" {
			text := textOf(message["content"])
			if text != "" {
				input = append(input, wire.Body{
					"type":    "message",
					"role":    "assistant",
					"content": []any{wire.Body{"type": "output_text", "text": text}},
				})
			}
			// Streaming merges sometimes leave a sibling with name+empty args
			// and another with args+empty name in the same assistant turn —
			// reuse the last real name.
			lastToolName := ""
			for _, rawCall := range wire.AsSlice(message["tool_calls"]) {
				call := wire.AsRecord(rawCall)
				fn := wire.AsRecord(call["function"])
				callID := callIDOf(call["id"])
				name := toolNameOf(fn["name"], lastToolName)
				if wire.FirstNonEmpty(fn["name"]) != "" {
					lastToolName = wire.AsString(fn["name"])
				}
				unpairedCallIDs = append(unpairedCallIDs, callID)
				var arguments string
				if s, ok := fn["arguments"].(string); ok {
					arguments = s
				} else if fn["arguments"] != nil {
					arguments = wire.MarshalJSON(fn["arguments"])
				} else {
					arguments = "{}"
				}
				input = append(input, wire.Body{
					"type":      "function_call",
					"call_id":   callID,
					"name":      name,
					"arguments": arguments,
				})
			}
			continue
		}
		input = append(input, wire.Body{
			"type":    "message",
			"role":    "user",
			"content": []any{wire.Body{"type": "input_text", "text": textOf(message["content"])}},
		})
	}

	var tools []any
	for _, raw := range wire.AsSlice(body["tools"]) {
		tools = append(tools, chatToolToResponses(raw)...)
	}
	max := body["max_completion_tokens"]
	if max == nil {
		max = body["max_tokens"]
	}

	out := wire.Body{
		"model":  model,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if v, present := body["tool_choice"]; present {
		out["tool_choice"] = v
	}
	if v, ok := body["temperature"]; ok && wire.IsNumber(v) {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok && wire.IsNumber(v) {
		out["top_p"] = v
	}
	if wire.IsNumber(max) {
		out["max_output_tokens"] = max
	}
	if effort, ok := body["reasoning_effort"].(string); ok {
		out["reasoning"] = wire.Body{"effort": effort}
	}
	if key, ok := body["prompt_cache_key"].(string); ok {
		out["prompt_cache_key"] = key
	}
	return out
}

func cloneMap(m wire.Body) wire.Body {
	next := make(wire.Body, len(m))
	for k, v := range m {
		next[k] = v
	}
	return next
}
