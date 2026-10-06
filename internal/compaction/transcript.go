// Package compaction ports src/compaction.ts: compaction by deletion. Text the
// user and assistant produced stays verbatim; only tool calls and results that
// the Jev brain judges stale are removed. The package normalises the three wire
// formats into one transcript, fits a state into a token ceiling, asks Jev two
// `noul` questions per candidate call, and re-encodes the result.
package compaction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/xinyao27/jevonian/internal/routing"
)

// ToolUse is a tool call. Keys keeps the input's key order when it is known
// (JS objects keep insertion order; Go maps do not).
type ToolUse struct {
	ToolUseID string
	Tool      string
	Input     map[string]any
	Keys      []string
}

// ToolResult is a tool result paired to its call by id.
type ToolResult struct {
	ToolUseID string
	Text      string
	IsError   bool
}

// Message is one transcript message; all wire formats normalise into it.
type Message struct {
	Role        string // "user" | "assistant"
	Text        string
	ToolUses    []ToolUse
	ToolResults []ToolResult
	// SourceIndex is the position in the caller's input; -1 when none.
	SourceIndex         int
	HasUnmodeledContent bool
}

var errorPrefix = regexp.MustCompile(`(?i)^\s*(error|err!?|fatal|traceback|exception)\b`)

func looksLikeError(text string) bool { return errorPrefix.MatchString(text) }

func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func idOf(v any) string {
	s, _ := v.(string)
	return s
}

func textOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	parts := []string{}
	for _, p := range arr {
		if s, ok := p.(string); ok {
			parts = append(parts, s)
			continue
		}
		if t, ok := asRecord(p)["text"].(string); ok {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// u16 helpers: JS string indexes are UTF-16 code units.
func ulen(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= loneSurrogateBase+0xd800 && r <= loneSurrogateBase+0xdfff:
			n++ // a marked lone surrogate is one UTF-16 unit
		case r >= 0x10000:
			n += 2
		default:
			n++
		}
	}
	return n
}

// loneSurrogateBase marks a surrogate half left by a mid-pair slice. Go strings
// cannot hold one, so it becomes the private-use rune loneSurrogateBase+unit and
// is expanded to `\uXXXX` (what JSON.stringify emits) when tokens are counted.
const loneSurrogateBase = 0xF0000

// usub slices by UTF-16 code unit like JS String.slice.
func usub(s string, from, to int) string {
	u := utf16.Encode([]rune(s))
	if from < 0 {
		from = 0
	}
	if to > len(u) {
		to = len(u)
	}
	if from >= to {
		return ""
	}
	var b strings.Builder
	for i := from; i < to; i++ {
		c := rune(u[i])
		if c >= 0xd800 && c <= 0xdbff && i+1 < to && u[i+1] >= 0xdc00 && u[i+1] <= 0xdfff {
			b.WriteRune(utf16.DecodeRune(c, rune(u[i+1])))
			i++
			continue
		}
		if c >= 0xd800 && c <= 0xdfff {
			b.WriteRune(loneSurrogateBase + c)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// orderedKeys returns the top-level keys of a JSON object in source order.
func orderedKeys(raw string) []string {
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil
	}
	keys := []string{}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return keys
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}

// parseArgs mirrors `asRecord(JSON.parse(args))` with `{raw: args}` on failure.
func parseArgs(raw string) (map[string]any, []string) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return map[string]any{"raw": raw}, []string{"raw"}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return m, orderedKeys(raw)
}

// NormalizeTranscript turns a request body from any wire format into one
// transcript. Shape detection is by structure, not by endpoint kind.
func NormalizeTranscript(body map[string]any) []Message {
	var raw []any
	if a, ok := body["messages"].([]any); ok {
		raw = a
	} else if a, ok := body["input"].([]any); ok {
		raw = a
	}
	out := []Message{}
	for sourceIndex, entry := range raw {
		message := asRecord(entry)
		role := "user"
		if message["role"] == "assistant" {
			role = "assistant"
		}
		var uses []ToolUse
		var results []ToolResult
		content, isArr := message["content"].([]any)
		unmodeled := false
		if isArr {
			for _, r := range content {
				t := asRecord(r)["type"]
				if t != "text" && t != "tool_use" && t != "tool_result" {
					unmodeled = true
				}
			}
			for _, br := range content {
				block := asRecord(br)
				switch block["type"] {
				case "tool_use":
					input := asRecord(block["input"])
					uses = append(uses, ToolUse{ToolUseID: idOf(block["id"]), Tool: idOf(block["name"]), Input: input, Keys: sortedKeys(input)})
				case "tool_result":
					text := textOf(block["content"])
					if text == "" {
						text = textOf(block["text"])
					}
					results = append(results, ToolResult{ToolUseID: idOf(block["tool_use_id"]), Text: text, IsError: block["is_error"] == true})
				}
			}
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			for _, cr := range calls {
				call := asRecord(cr)
				fn := asRecord(call["function"])
				var input map[string]any
				var keys []string
				if s, ok := fn["arguments"].(string); ok {
					input, keys = parseArgs(s)
				} else if fn["arguments"] != nil {
					input = asRecord(fn["arguments"])
					keys = sortedKeys(input)
				} else {
					input = asRecord(call["input"])
					keys = sortedKeys(input)
				}
				name := idOf(fn["name"])
				if name == "" {
					name = idOf(call["name"])
				}
				uses = append(uses, ToolUse{ToolUseID: idOf(call["id"]), Tool: name, Input: input, Keys: keys})
			}
		}
		if id, ok := message["tool_call_id"].(string); ok && message["role"] == "tool" {
			text := textOf(message["content"])
			results = append(results, ToolResult{ToolUseID: id, Text: text, IsError: looksLikeError(text)})
		}
		if message["type"] == "function_call" {
			input := map[string]any{}
			var keys []string
			if s, ok := message["arguments"].(string); ok {
				input, keys = parseArgs(s)
			}
			id := idOf(message["call_id"])
			if id == "" {
				id = idOf(message["id"])
			}
			uses = append(uses, ToolUse{ToolUseID: id, Tool: idOf(message["name"]), Input: input, Keys: keys})
		}
		if message["type"] == "function_call_output" {
			results = append(results, ToolResult{ToolUseID: idOf(message["call_id"]), Text: textOf(message["output"])})
		}
		text := ""
		if message["role"] != "tool" && message["type"] != "function_call_output" {
			text = textOf(message["content"])
		}
		if strings.TrimSpace(text) == "" && len(uses) == 0 && len(results) == 0 && !unmodeled {
			continue
		}
		out = append(out, Message{Role: role, Text: text, ToolUses: uses, ToolResults: results,
			SourceIndex: sourceIndex, HasUnmodeledContent: unmodeled})
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Deterministic fallback; the source order is unrecoverable from a map.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// jsJSON is JSON.stringify for decoded values with ordered top-level keys.
func jsJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	s := strings.TrimSuffix(b.String(), "\n")
	s = strings.ReplaceAll(s, `\u2028`, "\u2028")
	return strings.ReplaceAll(s, `\u2029`, "\u2029")
}

func inputJSON(use ToolUse) string { return orderedObject(use.Input, use.Keys) }

func orderedObject(m map[string]any, keys []string) string {
	if len(keys) != len(m) {
		keys = sortedKeys(m)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsJSON(k))
		b.WriteByte(':')
		b.WriteString(jsJSON(m[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// MessageChars is the characters of text, tool input and tool output.
func MessageChars(m Message) int {
	total := ulen(m.Text)
	for _, t := range m.ToolUses {
		total += ulen(inputJSON(t))
	}
	for _, r := range m.ToolResults {
		total += ulen(r.Text)
	}
	return total
}

// EstimateTokens is the calibrated estimator shared with routing. Marked lone
// surrogates (see loneSurrogateBase) count as the `\uXXXX` JS would emit.
func EstimateTokens(text string) int {
	if strings.ContainsFunc(text, func(r rune) bool { return r >= loneSurrogateBase+0xd800 && r <= loneSurrogateBase+0xdfff }) {
		var b strings.Builder
		for _, r := range text {
			if r >= loneSurrogateBase+0xd800 && r <= loneSurrogateBase+0xdfff {
				fmt.Fprintf(&b, "\\u%04x", r-loneSurrogateBase)
				continue
			}
			b.WriteRune(r)
		}
		text = b.String()
	}
	return routing.EstimateTokens(text)
}

func ceilInt(f float64) int { return int(math.Ceil(f)) }
