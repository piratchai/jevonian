package compaction

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xinyao27/jevonian/internal/routing"
)

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ReencodeMessages writes a compacted transcript back into a request body,
// preserving each message's original wire shape. Only `messages`/`input` is
// replaced; every other field stays as the caller sent it.
func ReencodeMessages(body map[string]any, messages []Message) map[string]any {
	key := "input"
	if _, ok := body["messages"].([]any); ok {
		key = "messages"
	}
	original, _ := body[key].([]any)
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		var source map[string]any
		if m.SourceIndex >= 0 && m.SourceIndex < len(original) {
			source = asRecord(original[m.SourceIndex])
		}
		if len(source) == 0 {
			out = append(out, encodeMessage(m))
			continue
		}
		calls := map[string]bool{}
		for _, t := range m.ToolUses {
			calls[t.ToolUseID] = true
		}
		results := map[string]ToolResult{}
		for _, r := range m.ToolResults {
			results[r.ToolUseID] = r
		}
		out = append(out, reencodeOne(source, calls, results))
	}
	next := copyMap(body)
	next[key] = out
	return next
}

func reencodeOne(source map[string]any, calls map[string]bool, results map[string]ToolResult) any {
	if source["role"] == "tool" {
		r, ok := results[idOf(source["tool_call_id"])]
		if ok && r.Text != textOf(source["content"]) {
			n := copyMap(source)
			n["content"] = r.Text
			return n
		}
		return source
	}
	if source["type"] == "function_call_output" {
		r, ok := results[idOf(source["call_id"])]
		if ok && r.Text != textOf(source["output"]) {
			n := copyMap(source)
			n["output"] = r.Text
			return n
		}
		return source
	}
	if source["type"] == "function_call" {
		return source
	}
	if tcs, ok := source["tool_calls"].([]any); ok {
		remaining := []any{}
		for _, raw := range tcs {
			if calls[idOf(asRecord(raw)["id"])] {
				remaining = append(remaining, raw)
			}
		}
		if len(remaining) == len(tcs) {
			return source
		}
		n := copyMap(source)
		n["tool_calls"] = remaining
		return n
	}
	if content, ok := source["content"].([]any); ok {
		unchanged := true
		for _, raw := range content {
			b := asRecord(raw)
			switch b["type"] {
			case "tool_use":
				if !calls[idOf(b["id"])] {
					unchanged = false
				}
			case "tool_result":
				r, ok := results[idOf(b["tool_use_id"])]
				cur := b["content"]
				if cur == nil {
					cur = b["text"]
				}
				if !ok || r.Text != textOf(cur) {
					unchanged = false
				}
			}
		}
		if unchanged {
			return source
		}
		blocks := []any{}
		for _, raw := range content {
			b := asRecord(raw)
			switch b["type"] {
			case "tool_use":
				if !calls[idOf(b["id"])] {
					continue
				}
			case "tool_result":
				r, ok := results[idOf(b["tool_use_id"])]
				if !ok {
					continue
				}
				nb := copyMap(b)
				if b["text"] != nil {
					nb["text"] = r.Text
				} else {
					nb["content"] = r.Text
				}
				blocks = append(blocks, nb)
				continue
			}
			blocks = append(blocks, raw)
		}
		n := copyMap(source)
		n["content"] = blocks
		return n
	}
	return source
}

func encodeMessage(m Message) map[string]any {
	content := []any{}
	if len(m.Text) > 0 {
		content = append(content, map[string]any{"type": "text", "text": m.Text})
	}
	for _, t := range m.ToolUses {
		content = append(content, map[string]any{"type": "tool_use", "id": t.ToolUseID, "name": t.Tool, "input": t.Input})
	}
	for _, r := range m.ToolResults {
		content = append(content, map[string]any{"type": "tool_result", "tool_use_id": r.ToolUseID, "content": r.Text})
	}
	return map[string]any{"role": m.Role, "content": content}
}

// OverflowResult is the outcome of CompactForOverflow.
type OverflowResult struct {
	OK    bool
	Body  map[string]any
	Stats Stats
	Error string
}

// CompactForOverflow shrinks a request body no model's window could hold. It
// keeps every word of prose verbatim; when the reduction is not worth the churn
// the original body stays untouched and OK is false.
// src/upstream.ts compactForOverflow.
func CompactForOverflow(ctx context.Context, brains int, asker Asker, body map[string]any) OverflowResult {
	if brains == 0 {
		return OverflowResult{Error: "no Jev brain is configured"}
	}
	messages := NormalizeTranscript(body)
	if len(messages) == 0 {
		return OverflowResult{Error: "the request has no messages to compact"}
	}
	r, err := Compact(ctx, messages, asker, Options{PreserveRecentMessages: F(4)})
	if err != nil {
		return OverflowResult{Error: err.Error()}
	}
	if ReductionRatio(r) < 0.05 {
		return OverflowResult{Error: fmt.Sprintf("compaction only reduced the history by %.0f%%", ReductionRatio(r)*100)}
	}
	rewritten := ReencodeMessages(body, r.Messages)
	if float64(routing.CompactionEstimate(rewritten)) > float64(routing.CompactionEstimate(body))*0.9 {
		return OverflowResult{Error: "compaction did not sufficiently reduce the outgoing request"}
	}
	return OverflowResult{OK: true, Body: rewritten, Stats: r.Stats}
}

func roundTrip(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
