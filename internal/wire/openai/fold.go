package workbuddy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// FoldStream folds an OpenAI Chat Completions SSE body into one non-streaming
// completion. WorkBuddy refuses non-stream requests, so non-stream clients get
// the stream assembled here.
//
// Port of foldOpenAIChatStream in src/workbuddy.ts.
func FoldStream(body io.Reader, model string) map[string]any {
	result := map[string]any{
		"id":      "chatcmpl-workbuddy",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}

	if body == nil {
		result["choices"] = []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": ""},
			"finish_reason": "stop",
		}}
		return result
	}

	var content strings.Builder
	var reasoning strings.Builder
	finishReason := "stop"
	finishSet := false
	var usage any
	type toolCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	toolCalls := map[int]*toolCall{}
	var order []int

	consume := func(chunk []byte, buffered *strings.Builder) {
		buffered.Write(chunk)
		for {
			text := buffered.String()
			index := strings.Index(text, "\n\n")
			if index < 0 {
				return
			}
			event := text[:index]
			*buffered = strings.Builder{}
			buffered.WriteString(text[index+2:])
			for _, line := range strings.Split(event, "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := strings.TrimSpace(line[5:])
				if data == "" || data == "[DONE]" {
					continue
				}
				var parsed any
				if err := json.Unmarshal([]byte(data), &parsed); err != nil {
					continue
				}
				rec := asRecord(parsed)
				if id, ok := rec["id"].(string); ok && id != "" {
					result["id"] = id
				}
				if u, ok := rec["usage"]; ok && u != nil {
					usage = u
				}
				choices, _ := rec["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				choice := asRecord(choices[0])
				if fr, ok := choice["finish_reason"].(string); ok {
					finishReason = fr
					finishSet = true
				}
				delta := asRecord(choice["delta"])
				if s, ok := delta["content"].(string); ok {
					content.WriteString(s)
				}
				if s, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(s)
				}
				if calls, ok := delta["tool_calls"].([]any); ok {
					for _, raw := range calls {
						call := asRecord(raw)
						idx := 0
						if v, ok := number(call["index"]); ok {
							idx = int(v)
						}
						existing, ok := toolCalls[idx]
						if !ok {
							existing = &toolCall{Type: "function"}
							toolCalls[idx] = existing
							order = append(order, idx)
						}
						if s, ok := call["id"].(string); ok && s != "" {
							existing.ID = s
						}
						if s, ok := call["type"].(string); ok && s != "" {
							existing.Type = s
						}
						fn := asRecord(call["function"])
						if s, ok := fn["name"].(string); ok && s != "" {
							existing.Function.Name = s
						}
						if s, ok := fn["arguments"].(string); ok {
							existing.Function.Arguments += s
						}
					}
				}
			}
		}
	}

	reader := bufio.NewReader(body)
	var buffer strings.Builder
	chunk := make([]byte, 16<<10)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			consume(chunk[:n], &buffer)
		}
		if err != nil {
			break
		}
	}
	// Flush the trailing partial event — a final line without the \n\n close.
	if buffer.Len() > 0 {
		consume([]byte("\n\n"), &buffer)
	}

	message := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		sort.Ints(order)
		calls := make([]any, 0, len(order))
		for _, idx := range order {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if !finishSet {
		finishReason = "stop"
	}
	result["choices"] = []any{map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}}
	if usage != nil {
		result["usage"] = usage
	}
	return result
}

// FoldBytes is FoldStream over an in-memory SSE transcript.
func FoldBytes(data []byte, model string) map[string]any {
	return FoldStream(bytes.NewReader(data), model)
}
