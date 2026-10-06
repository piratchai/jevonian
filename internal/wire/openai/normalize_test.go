package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func TestNormalizeMessagesLeavesStandardUntouched(t *testing.T) {
	input := []any{
		wire.Body{"role": "system", "content": "You are helpful."},
		wire.Body{"role": "user", "content": "Hello world"},
		wire.Body{"role": "assistant", "content": "Hi there!"},
	}
	out := NormalizeMessages(input)
	if len(out) != 3 {
		t.Fatalf("out = %v", out)
	}
	for i, m := range out {
		if m["content"] != input[i].(wire.Body)["content"] {
			t.Fatalf("m%d = %v", i, m)
		}
	}
}

func TestNormalizeMessagesJoinsTextArrays(t *testing.T) {
	input := []any{
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "text", "text": "Line 1"},
			wire.Body{"type": "text", "text": "Line 2"},
		}},
	}
	out := NormalizeMessages(input)
	if out[0]["content"] != "Line 1\nLine 2" {
		t.Fatalf("content = %v", out[0]["content"])
	}
}

func TestNormalizeMessagesTranslatesToolUse(t *testing.T) {
	input := []any{
		wire.Body{"role": "assistant", "content": []any{
			wire.Body{"type": "text", "text": "Checking files..."},
			wire.Body{"type": "tool_use", "id": "call_abc123", "name": "read_file",
				"input": wire.Body{"path": "app.ts"}},
		}},
	}
	out := NormalizeMessages(input)
	m := out[0]
	if m["role"] != "assistant" || m["content"] != "Checking files..." {
		t.Fatalf("m = %v", m)
	}
	calls := wire.AsSlice(m["tool_calls"])
	call := wire.AsRecord(calls[0])
	if call["id"] != "call_abc123" || call["type"] != "function" {
		t.Fatalf("call = %v", call)
	}
	fn := wire.AsRecord(call["function"])
	if fn["name"] != "read_file" || fn["arguments"] != `{"path":"app.ts"}` {
		t.Fatalf("fn = %v", fn)
	}
}

func TestNormalizeMessagesTranslatesToolResult(t *testing.T) {
	input := []any{
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "tool_result", "tool_use_id": "call_abc123", "content": "file content here"},
		}},
	}
	out := NormalizeMessages(input)
	if len(out) != 1 {
		t.Fatalf("out = %v", out)
	}
	m := out[0]
	if m["role"] != "tool" || m["tool_call_id"] != "call_abc123" || m["content"] != "file content here" {
		t.Fatalf("m = %v", m)
	}
}

func TestNormalizeMessagesPreservesImageURL(t *testing.T) {
	input := []any{
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "text", "text": "Look at this image"},
			wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,abc"}},
		}},
	}
	out := NormalizeMessages(input)
	parts := wire.AsSlice(out[0]["content"])
	if len(parts) != 2 {
		t.Fatalf("parts = %v", parts)
	}
	if wire.AsRecord(parts[0])["type"] != "text" || wire.AsRecord(parts[1])["type"] != "image_url" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestNormalizeMessagesPrunesOldImages(t *testing.T) {
	input := []any{
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,old1"}},
		}},
		wire.Body{"role": "assistant", "content": "I see screenshot 1."},
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,old2"}},
		}},
		wire.Body{"role": "assistant", "content": "I see screenshot 2."},
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "text", "text": "Look at latest screenshot"},
			wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,latest"}},
		}},
	}
	out := NormalizeMessages(input)
	if out[0]["content"] != "[Previous screenshot omitted to prevent multimodal timeout]" {
		t.Fatalf("first = %v", out[0])
	}
	part2 := wire.AsSlice(out[2]["content"])
	if wire.AsRecord(part2[0])["type"] != "image_url" {
		t.Fatalf("second = %v", out[2])
	}
	last := wire.AsSlice(out[4]["content"])
	if len(last) != 2 {
		t.Fatalf("last = %v", out[4])
	}
}

func TestNormalizeMessagesAnthropicImageBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  wire.Body
		wantURL string
	}{
		{"base64", wire.Body{"type": "base64", "media_type": "image/jpeg", "data": "jpegdata123"}, "data:image/jpeg;base64,jpegdata123"},
		{"url", wire.Body{"type": "url", "url": "https://example.com/screenshot.png"}, "https://example.com/screenshot.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []any{
				wire.Body{"role": "user", "content": []any{
					wire.Body{"type": "image", "source": tc.source},
				}},
			}
			out := NormalizeMessages(input)
			parts := wire.AsSlice(out[0]["content"])
			part := wire.AsRecord(parts[0])
			if part["type"] != "image_url" {
				t.Fatalf("part = %v", part)
			}
			if wire.AsRecord(part["image_url"])["url"] != tc.wantURL {
				t.Fatalf("url = %v", part["image_url"])
			}
		})
	}
}

func TestNormalizeMessagesStripsEmptyToolCalls(t *testing.T) {
	input := []any{
		wire.Body{"role": "assistant", "content": "Both files are truncated; let me continue reading.", "tool_calls": []any{}},
		wire.Body{"role": "user", "content": "Have you done?"},
		wire.Body{"role": "assistant", "content": []any{wire.Body{"type": "text", "text": "Checking next file..."}}, "tool_calls": []any{}},
	}
	out := NormalizeMessages(input)
	if _, present := out[0]["tool_calls"]; present {
		t.Fatalf("tool_calls present: %v", out[0])
	}
	if out[0]["content"] != "Both files are truncated; let me continue reading." {
		t.Fatalf("content = %v", out[0])
	}
	if _, present := out[2]["tool_calls"]; present {
		t.Fatalf("tool_calls present: %v", out[2])
	}
	if out[2]["content"] != "Checking next file..." {
		t.Fatalf("content = %v", out[2])
	}
}

func TestNormalizeMessagesToolBeforeUser(t *testing.T) {
	input := []any{
		wire.Body{"role": "user", "content": []any{wire.Body{"type": "text", "text": "go"}}},
		wire.Body{"role": "assistant", "content": []any{
			wire.Body{"type": "tool_use", "id": "tu9", "name": "shot", "input": wire.Body{}},
		}},
		wire.Body{"role": "user", "content": []any{
			wire.Body{"type": "tool_result", "tool_use_id": "tu9", "content": "captured"},
			wire.Body{"type": "text", "text": "what do you see?"},
			wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,AFTER_TOOL"}},
		}},
	}
	out := NormalizeMessages(input)
	var roles []string
	for _, m := range out {
		roles = append(roles, wire.AsString(m["role"]))
	}
	if len(roles) != 4 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "tool" || roles[3] != "user" {
		t.Fatalf("roles = %v", roles)
	}
	if out[2]["tool_call_id"] != "tu9" || out[2]["content"] != "captured" {
		t.Fatalf("tool = %v", out[2])
	}
	parts := wire.AsSlice(out[3]["content"])
	if wire.AsRecord(parts[0])["type"] != "text" || wire.AsRecord(parts[1])["type"] != "image_url" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestSanitizeStreamStripsEmptyIDName(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_123","type":"function","index":0,"function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"","type":"function","index":0,"function":{"arguments":"{\"city\": "}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"","type":"function","index":0,"function":{"arguments":"\"Tokyo\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":""},"index":0,"type":"function","id":""}]}}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	sanitizer := NewSanitizeStream()
	out := string(sanitizer.Write([]byte(raw))) + string(sanitizer.Flush())

	if !strings.Contains(out, `"id":"call_123"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("first chunk damaged: %s", out)
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("lines = %d: %s", len(lines), out)
	}
	for i, line := range lines[1:] {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &chunk); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		delta := wire.AsRecord(wire.AsSlice(chunk["choices"])[0].(map[string]any)["delta"])
		call := wire.AsRecord(wire.AsSlice(delta["tool_calls"])[0])
		if _, present := call["id"]; present {
			t.Fatalf("chunk %d still has id: %v", i+1, call)
		}
	}
	// Chunk 2 keeps its arguments.
	var chunk2 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[1][5:])), &chunk2); err != nil {
		t.Fatal(err)
	}
	delta := wire.AsRecord(wire.AsSlice(chunk2["choices"])[0].(map[string]any)["delta"])
	call := wire.AsRecord(wire.AsSlice(delta["tool_calls"])[0])
	if wire.AsRecord(call["function"])["arguments"] != `{"city": ` {
		t.Fatalf("args = %v", call)
	}
}

func TestSanitizeResponseStripsEmpty(t *testing.T) {
	body := wire.Body{
		"choices": []any{wire.Body{
			"message": wire.Body{
				"role": "assistant",
				"tool_calls": []any{wire.Body{
					"id": "", "type": "function",
					"function": wire.Body{"name": "", "arguments": "{}"},
				}},
			},
		}},
	}
	out := SanitizeResponse(body)
	msg := wire.AsRecord(wire.AsSlice(out["choices"])[0].(map[string]any)["message"])
	tc := wire.AsRecord(wire.AsSlice(msg["tool_calls"])[0])
	if _, present := tc["id"]; present {
		t.Fatalf("id still present: %v", tc)
	}
	if _, present := wire.AsRecord(tc["function"])["name"]; present {
		t.Fatalf("name still present: %v", tc)
	}
}
