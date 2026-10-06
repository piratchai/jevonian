package freebuff

import (
	"strings"

	"github.com/xinyao27/jevonian/internal/wire"
)

// upstreamKeys are the Chat Completions fields the upstream accepts. Anything
// else a client sends (Anthropic-bridge leftovers, vendor extensions) is
// dropped rather than risk a 400 from the strict gateway.
var upstreamKeys = []string{
	"frequency_penalty", "logit_bias", "logprobs", "max_completion_tokens", "max_tokens",
	"metadata", "modalities", "parallel_tool_calls", "presence_penalty", "reasoning_effort",
	"response_format", "seed", "service_tier", "store", "stream_options",
	"temperature", "tool_choice", "tools", "top_logprobs", "top_p", "top_k", "user",
}

const endTurnTool = "end_turn"

// Prepare turns a Chat Completions body into the static part of a Freebuff
// request: the model, the Buffy system opening, the field whitelist, forced
// streaming and the CLI's stop/provider flags. The per-call codebuff_metadata
// is added by WithMetadata once a session and run exist.
func Prepare(in wire.Body, model Model) wire.Body {
	out := wire.Body{}
	for _, key := range upstreamKeys {
		if v, ok := in[key]; ok && v != nil {
			out[key] = v
		}
	}
	out["model"] = model.ID
	out["messages"] = normalizeMessages(wire.AsSlice(in["messages"]))
	out["stream"] = true
	out["stop"] = stopFor(in["stop"])
	out["provider"] = map[string]any{"data_collection": "deny"}
	if tools := wire.AsSlice(out["tools"]); len(tools) > 0 {
		out["tools"] = withToolsetSignature(tools)
	}
	return out
}

// stopFor keeps the client's stop sequences and always appends the sentinel.
func stopFor(raw any) []any {
	out := []any{}
	switch v := raw.(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		out = append(out, v...)
	}
	for _, s := range out {
		if s == StopSentinel {
			return out
		}
	}
	return append(out, StopSentinel)
}

// withToolsetSignature adds end_turn when the client's tools carry none of the
// CLI's own tool names. The server rejects a "foreign toolset" for free mode;
// end_turn is on the CLI's harmless allow-list and is never actually called.
func withToolsetSignature(tools []any) []any {
	for _, raw := range tools {
		fn := wire.AsRecord(wire.AsRecord(raw)["function"])
		if wire.AsString(fn["name"]) == endTurnTool {
			return tools
		}
	}
	out := append([]any(nil), tools...)
	return append(out, map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        endTurnTool,
			"description": "Signal the end of the current task.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	})
}

// normalizeMessages maps developer→system and makes the first message a system
// message that opens with the Buffy marker, byte for byte.
func normalizeMessages(messages []any) []any {
	out := make([]any, 0, len(messages)+1)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		clone := make(map[string]any, len(msg))
		for k, v := range msg {
			clone[k] = v
		}
		if clone["role"] == "developer" {
			clone["role"] = "system"
		}
		out = append(out, clone)
	}
	if len(out) > 0 {
		if first, ok := out[0].(map[string]any); ok && first["role"] == "system" {
			first["content"] = withBuffyOpening(first["content"])
			return out
		}
	}
	return append([]any{map[string]any{"role": "system", "content": BuffyMarker}}, out...)
}

// withBuffyOpening prefixes the marker to string or text-part content.
func withBuffyOpening(content any) any {
	switch c := content.(type) {
	case string:
		if strings.HasPrefix(c, BuffyMarker) {
			return c
		}
		if c == "" {
			return BuffyMarker
		}
		return BuffyMarker + "\n\n" + c
	case []any:
		parts := make([]any, len(c))
		copy(parts, c)
		for i, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			text := wire.AsString(part["text"])
			if strings.HasPrefix(text, BuffyMarker) {
				return parts
			}
			clone := make(map[string]any, len(part))
			for k, v := range part {
				clone[k] = v
			}
			clone["text"] = BuffyMarker + "\n\n" + text
			parts[i] = clone
			return parts
		}
		return append([]any{map[string]any{"type": "text", "text": BuffyMarker}}, parts...)
	}
	return BuffyMarker
}

// WithMetadata returns a copy of body carrying the CLI envelope for one call.
// A fresh client_id every time: a constant one is fingerprinted as a proxy.
func WithMetadata(body wire.Body, runID, instanceID string) wire.Body {
	out := make(wire.Body, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	meta := map[string]any{
		"run_id":    runID,
		"client_id": ClientID(),
		// Omitting cost_mode sends the request down the paid path, which a
		// zero-balance free account answers with 402 "Out of credits".
		"cost_mode": "free",
	}
	if instanceID != "" {
		meta["freebuff_instance_id"] = instanceID
	}
	out["codebuff_metadata"] = meta
	return out
}
