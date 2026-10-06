// Package softstream ports src/soft-error.ts + src/stream-keepalive.ts: last-resort
// "soft" completions for streaming turns.
//
// A coding agent's harness treats a hard failure badly: an abrupt socket close, a JSON
// 5xx, or an SSE `{ error }` mid-agent loop can make it roll the whole turn back —
// sometimes wiping the visible user message and scrambling the conversation context.
// The failure is real, but it is Jevonian's, not the model's, so it must not cost the
// user their turn.
//
// When Jevonian still owns the HTTP response, it answers with a normal assistant
// message that explains the failure and ends the stream cleanly (`finish_reason: stop`
// / `message_stop` / `response.completed`). The turn survives, the user can retry, and
// the ledger still records the real error. This is deliberately the last resort: it
// only runs when there is nothing better to say.
package softstream

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

// SoftErrorPrefix is re-exported from internal/wire, the single source of the
// soft-error wording (bridged streams in wire/* and this package must agree).
const SoftErrorPrefix = wire.SoftErrorPrefix

// SoftErrorHeader marks a response whose body is (or contains) a soft completion.
const SoftErrorHeader = "x-jevonian-soft-error"

// SoftErrorMessage is the one-line assistant message describing a failure, safe to
// show the user. Delegates to wire.SoftErrorMessage.
func SoftErrorMessage(reason string) string { return wire.SoftErrorMessage(reason) }

// RedactSecrets replaces every occurrence of a known credential with `[REDACTED]`.
//
// An upstream error body can echo the `Authorization` header it just rejected, and
// that body is shown to the user inside the soft message. Values shorter than a
// plausible credential are skipped so a config mistake cannot turn a common substring
// into `[REDACTED]`.
func RedactSecrets(text string, secrets ...string) string {
	safe := text
	for _, secret := range secrets {
		if len(secret) < 8 {
			continue
		}
		// Cover the JSON-escaped echo too, not just the raw credential.
		variants := []string{secret}
		if encoded, err := json.Marshal(secret); err == nil && len(encoded) > 2 {
			variants = append(variants, string(encoded[1:len(encoded)-1]))
		}
		for _, variant := range variants {
			if variant != "" {
				safe = replaceAll(safe, variant, "[REDACTED]")
			}
		}
	}
	return safe
}

func replaceAll(s, old, new string) string {
	return bytes.NewBuffer(bytes.ReplaceAll([]byte(s), []byte(old), []byte(new))).String()
}

func sseFrame(payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte("null")
	}
	return []byte("data: " + string(data) + "\n\n")
}

func sseEventFrame(eventType string, payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte("null")
	}
	return []byte("event: " + eventType + "\ndata: " + string(data) + "\n\n")
}

func asRecord(value any) map[string]any {
	if m, ok := value.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func asNumber(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is not a reason to fail the soft completion; fall
		// back to a time-seeded value so the id still looks right.
		return fmt.Sprintf("%0*x", n, time.Now().UnixNano())[:n]
	}
	return hex.EncodeToString(buf)[:n]
}

func unixNow() int64 { return time.Now().Unix() }

func chatChunk(model string, delta map[string]any, finishReason any) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-soft-" + randomHex(16),
		"object":  "chat.completion.chunk",
		"created": unixNow(),
		"model":   model,
		"choices": []any{
			map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason},
		},
	}
}

// OpenItem is a Responses output item left open by a mid-stream failure, closed
// before the soft block opens so the wire stays well-formed.
type OpenItem struct {
	OutputIndex int
	Item        map[string]any
	Text        string
}

// SoftCompletionOptions places the soft block on a wire that may already have
// streamed content.
//
// A failure that lands after the upstream opened a content block / output item
// cannot reuse index 0: strict Anthropic and Responses clients reject a duplicate
// index. `Index` is the next free slot, and `CloseIndex` / `CloseItem` terminate
// whatever was still open before the soft block opens.
type SoftCompletionOptions struct {
	// Started: the wire already emitted its stream opener (`message_start` /
	// `response.created` / role).
	Started bool
	// Index: Anthropic content-block index / Responses output index for the soft
	// block. Defaults to 0.
	Index int
	// CloseIndex: Anthropic open content block to stop before the soft block
	// opens. -1 means none.
	CloseIndex int
	// CloseItem: Responses open output item to finish before the soft item opens.
	CloseItem *OpenItem
}

// SoftChatCompletion returns chunks that end a Chat Completions stream as a normal
// assistant turn carrying `message`. Emits the role opener only when the wire has
// not opened yet; a stream that already sent content continues without it.
func SoftChatCompletion(model, message string, opts SoftCompletionOptions) [][]byte {
	chunks := [][]byte{}
	if !opts.Started {
		chunks = append(chunks, sseFrame(chatChunk(model, map[string]any{"role": "assistant", "content": ""}, nil)))
	}
	chunks = append(chunks, sseFrame(chatChunk(model, map[string]any{"content": message}, nil)))
	chunks = append(chunks, sseFrame(chatChunk(model, map[string]any{}, "stop")))
	chunks = append(chunks, []byte("data: [DONE]\n\n"))
	return chunks
}

// SoftAnthropicCompletion returns Anthropic Messages events that end the stream
// cleanly with a text block.
func SoftAnthropicCompletion(model, message string, opts SoftCompletionOptions) [][]byte {
	chunks := [][]byte{}
	index := opts.Index
	id := "msg_soft_" + randomHex(24)
	if opts.CloseIndex >= 0 {
		chunks = append(chunks, sseEventFrame("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": opts.CloseIndex,
		}))
	}
	if !opts.Started {
		chunks = append(chunks, sseEventFrame("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            id,
				"type":          "message",
				"role":          "assistant",
				"model":         model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
	}
	chunks = append(chunks,
		sseEventFrame("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}),
		sseEventFrame("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{"type": "text_delta", "text": message},
		}),
		sseEventFrame("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		}),
		sseEventFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		}),
		sseEventFrame("message_stop", map[string]any{"type": "message_stop"}),
	)
	return chunks
}

// closeResponsesItem terminates an output item left open by a mid-stream failure,
// so the wire stays well-formed.
func closeResponsesItem(close OpenItem, chunks [][]byte) [][]byte {
	item := close.Item
	itemID := asString(item["id"])
	if asString(item["type"]) == "function_call" {
		done := map[string]any{}
		for k, v := range item {
			done[k] = v
		}
		done["arguments"] = close.Text
		done["status"] = "completed"
		return append(chunks,
			sseEventFrame("response.function_call_arguments.done", map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      itemID,
				"output_index": close.OutputIndex,
				"arguments":    close.Text,
			}),
			sseEventFrame("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": close.OutputIndex,
				"item":         done,
			}),
		)
	}
	if asString(item["type"]) == "message" {
		done := map[string]any{}
		for k, v := range item {
			done[k] = v
		}
		done["status"] = "completed"
		done["content"] = []any{map[string]any{"type": "output_text", "text": close.Text}}
		return append(chunks,
			sseEventFrame("response.output_text.done", map[string]any{
				"type":          "response.output_text.done",
				"item_id":       itemID,
				"output_index":  close.OutputIndex,
				"content_index": 0,
				"text":          close.Text,
			}),
			sseEventFrame("response.content_part.done", map[string]any{
				"type":          "response.content_part.done",
				"item_id":       itemID,
				"output_index":  close.OutputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": close.Text},
			}),
			sseEventFrame("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": close.OutputIndex,
				"item":         done,
			}),
		)
	}
	// A reasoning item or anything else: close it as it stands, without inventing content.
	done := map[string]any{}
	for k, v := range item {
		done[k] = v
	}
	done["status"] = "completed"
	return append(chunks, sseEventFrame("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": close.OutputIndex,
		"item":         done,
	}))
}

// SoftResponsesCompletion returns OpenAI Responses events that end the stream as a
// completed assistant message.
func SoftResponsesCompletion(model, message string, opts SoftCompletionOptions) [][]byte {
	chunks := [][]byte{}
	outputIndex := opts.Index
	id := "resp_soft_" + randomHex(16)
	itemID := "msg_" + id
	skeleton := func(status string, output []any) map[string]any {
		return map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": unixNow(),
			"status":     status,
			"model":      model,
			"output":     output,
			"usage":      map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		}
	}
	if !opts.Started {
		chunks = append(chunks,
			sseEventFrame("response.created", map[string]any{
				"type":     "response.created",
				"response": skeleton("in_progress", []any{}),
			}),
			sseEventFrame("response.in_progress", map[string]any{
				"type":     "response.in_progress",
				"response": skeleton("in_progress", []any{}),
			}),
		)
	}
	if opts.CloseItem != nil {
		chunks = closeResponsesItem(*opts.CloseItem, chunks)
	}
	// The item and its content part are opened even when the stream already started:
	// a delta without a preceding `.added` is dropped by Codex, and reusing a closed
	// index is rejected.
	chunks = append(chunks,
		sseEventFrame("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": outputIndex,
			"item": map[string]any{
				"type":    "message",
				"id":      itemID,
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
		}),
		sseEventFrame("response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       itemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		}),
		sseEventFrame("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       itemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"delta":         message,
		}),
		sseEventFrame("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       itemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"text":          message,
		}),
		sseEventFrame("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       itemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": message},
		}),
		sseEventFrame("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item": map[string]any{
				"type":    "message",
				"id":      itemID,
				"role":    "assistant",
				"status":  "completed",
				"content": []any{map[string]any{"type": "output_text", "text": message}},
			},
		}),
		sseEventFrame("response.completed", map[string]any{
			"type": "response.completed",
			"response": skeleton("completed", []any{
				map[string]any{
					"type":    "message",
					"id":      itemID,
					"role":    "assistant",
					"status":  "completed",
					"content": []any{map[string]any{"type": "output_text", "text": message}},
				},
			}),
		}),
	)
	return chunks
}

// SoftCompletionChunks returns the completion chunks for a client wire.
func SoftCompletionChunks(kind config.UpstreamWire, model, message string, opts SoftCompletionOptions) [][]byte {
	switch kind {
	case config.WireAnthropic:
		return SoftAnthropicCompletion(model, message, opts)
	case config.WireResponses:
		return SoftResponsesCompletion(model, message, opts)
	default:
		return SoftChatCompletion(model, message, opts)
	}
}

// SoftCompletionBytes is one SSE body containing only a soft completion, for a
// pre-stream failure.
func SoftCompletionBytes(kind config.UpstreamWire, model, message string) []byte {
	var out bytes.Buffer
	for _, chunk := range SoftCompletionChunks(kind, model, message, SoftCompletionOptions{CloseIndex: -1}) {
		out.Write(chunk)
	}
	return out.Bytes()
}
