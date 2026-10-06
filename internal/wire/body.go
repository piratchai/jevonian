// Package wire holds the protocol-translation helpers shared by every wire
// family (OpenAI Chat Completions, Anthropic Messages, OpenAI Responses).
//
// Bodies travel as map[string]any — the same loose Record<string, unknown>
// shape the TypeScript sources manipulated — so field names and ordering
// semantics round-trip with real clients (Codex, Claude Code, Cursor).
package wire

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// Body is a JSON object being translated between wire shapes.
type Body = map[string]any

// AsRecord returns value as a JSON object, or an empty (non-nil) map.
func AsRecord(value any) Body {
	if m, ok := value.(map[string]any); ok && m != nil {
		return m
	}
	return Body{}
}

// AsString returns value when it is a string, else "".
func AsString(value any) string {
	s, _ := value.(string)
	return s
}

// AsSlice returns value as a generic slice, or nil.
func AsSlice(value any) []any {
	if s, ok := value.([]any); ok {
		return s
	}
	return nil
}

// Number returns value as a finite float64, else 0 — the TS `number()` helper.
func Number(value any) float64 {
	switch n := value.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0
		}
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

// Bool reports whether value is the boolean true.
func Bool(value any) bool {
	b, _ := value.(bool)
	return b
}

// IsFiniteNumber reports whether value is a JSON number (not bool/string/null).
func IsNumber(value any) bool {
	switch value.(type) {
	case float64, float32, int, int64, json.Number:
		return true
	}
	return false
}

// FirstNonEmpty returns the first non-empty string among candidates —
// unlike a zero-value fallback, an explicit "" still counts as missing.
func FirstNonEmpty(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && len(s) > 0 {
			return s
		}
	}
	return ""
}

// TextOf flattens a message `content` field to text: strings pass through,
// arrays contribute each block's `text` joined by newlines, anything else is
// JSON-encoded. Port of the shared TS `textOf` (anthropic.ts variant).
func TextOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if parts, ok := value.([]any); ok {
		var texts []string
		for _, block := range parts {
			if t, ok := AsRecord(block)["text"].(string); ok {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n")
	}
	if value == nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

// MarshalJSON is json.Marshal that never fails on JSON-derived values and
// returns "null" when it does — matching the TS `JSON.stringify` call sites.
func MarshalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(data)
}

// SortedMap builds an ordered view of a JSON object — used where the TS code
// iterated `Object.entries` (insertion order is not recoverable in Go, so
// callers that depended on first-match semantics get a stable lexical order
// instead; see effort.go for the levels where this matters).
func SortedMap(m Body) []struct {
	Key   string
	Value any
} {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]struct {
		Key   string
		Value any
	}, 0, len(keys))
	for _, k := range keys {
		out = append(out, struct {
			Key   string
			Value any
		}{k, m[k]})
	}
	return out
}
