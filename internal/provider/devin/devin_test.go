package devin

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

// ─── wire helpers (port of devin.test.ts parse/field helpers) ───────────────

type tField struct {
	num   int
	wire  int
	value any // uint64 (wire 0) or []byte
}

// parse is an independent one-level protobuf decoder for wire assertions.
func parse(t *testing.T, raw []byte) []tField {
	t.Helper()
	var out []tField
	readVarint := func() (uint64, []byte, error) {
		var v uint64
		var shift uint
		for i := 0; i < 10 && len(raw) > 0; i++ {
			b := raw[0]
			raw = raw[1:]
			v |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				return v, raw, nil
			}
			shift += 7
		}
		return 0, nil, errors.New("truncated varint")
	}
	for len(raw) > 0 {
		tagv, rest, err := readVarint()
		if err != nil {
			t.Fatal(err)
		}
		raw = rest
		num := int(tagv >> 3)
		w := int(tagv & 7)
		switch w {
		case 0:
			v, rest, err := readVarint()
			if err != nil {
				t.Fatal(err)
			}
			raw = rest
			out = append(out, tField{num, w, v})
		case 1, 2, 5:
			var length uint64 = 8
			if w == 5 {
				length = 4
			}
			if w == 2 {
				l, rest, err := readVarint()
				if err != nil {
					t.Fatal(err)
				}
				raw = rest
				length = l
			}
			if uint64(len(raw)) < length {
				t.Fatal("truncated field")
			}
			out = append(out, tField{num, w, append([]byte(nil), raw[:length]...)})
			raw = raw[length:]
		default:
			t.Fatalf("unknown wire type %d", w)
		}
	}
	return out
}

func tFields(t *testing.T, raw []byte, num int) []tField {
	var out []tField
	for _, f := range parse(t, raw) {
		if f.num == num {
			out = append(out, f)
		}
	}
	return out
}

func tValue(t *testing.T, raw []byte, num int) any {
	fs := tFields(t, raw, num)
	if len(fs) == 0 {
		return nil
	}
	return fs[0].value
}

func tSub(t *testing.T, raw []byte, num int) []byte {
	v, _ := tValue(t, raw, num).([]byte)
	return v
}

func tStr(t *testing.T, raw []byte, num int) string {
	return string(tSub(t, raw, num))
}

func tNum(t *testing.T, raw []byte, num int) uint64 {
	v, _ := tValue(t, raw, num).(uint64)
	return v
}

func tDouble(t *testing.T, raw []byte, num int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(tSub(t, raw, num)))
}

func floatField(field int, value float32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], math.Float32bits(value))
	return Concat(tag(field, 5), buf[:])
}

func unframe(t *testing.T, frame []byte) []byte {
	t.Helper()
	if len(frame) < 5 || frame[0] != 0 {
		t.Fatal("expected flag byte 0")
	}
	length := binary.BigEndian.Uint32(frame[1:5])
	if uint64(len(frame)) != uint64(length)+5 {
		t.Fatalf("frame length %d != %d", len(frame), length+5)
	}
	return frame[5:]
}

func gzipped(t *testing.T, payload []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(payload)
	_ = w.Close()
	return buf.Bytes()
}

func sseLines(t *testing.T, data []byte) []string {
	var out []string
	for _, block := range strings.Split(string(data), "\n\n") {
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data: ") {
			t.Fatalf("bad SSE line %q", block)
		}
		out = append(out, strings.TrimPrefix(block, "data: "))
	}
	return out
}

func sseJSON(t *testing.T, line string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("bad SSE JSON %q: %v", line, err)
	}
	return m
}

func sseDelta(m map[string]any) map[string]any {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}
	}
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	return delta
}

func sseFinish(m map[string]any) any {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	return choices[0].(map[string]any)["finish_reason"]
}

func sseText(lines []string) string {
	var b strings.Builder
	for _, line := range lines {
		if line == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if c, ok := sseDelta(m)["content"].(string); ok {
			b.WriteString(c)
		}
	}
	return b.String()
}

func upstreamStream() []byte {
	usage := Concat(VarintField(2, 11), VarintField(3, 7), VarintField(4, 5), VarintField(5, 9), StringField(9, "actual-model"))
	toolStart := Concat(StringField(1, "call_weather"), StringField(2, "weather"), StringField(3, `{"city":`))
	toolEnd := StringField(3, `"Paris"}`)
	var buf bytes.Buffer
	buf.Write(EncodeFrame(StringField(9, "think"), 0))
	buf.Write(EncodeFrame(gzippedN(StringField(3, "Hello ")), 1))
	buf.Write(EncodeFrame(Concat(StringField(3, "world"), BytesField(6, toolStart)), 0))
	buf.Write(EncodeFrame(Concat(BytesField(6, toolEnd), VarintField(5, 10), BytesField(7, usage)), 0))
	buf.Write(EncodeFrame([]byte("{}"), 2))
	return buf.Bytes()
}

func gzippedN(payload []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(payload)
	_ = w.Close()
	return buf.Bytes()
}

// ─── request wire ────────────────────────────────────────────────────────────

func TestChatURLHeadersAndRequest(t *testing.T) {
	if got := ChatURL(DefaultBaseURL + "///"); got != DefaultBaseURL+"/exa.api_server_pb.ApiServerService/GetChatMessage" {
		t.Fatal(got)
	}
	headers := Headers("secret", HeadersStream)
	if headers.Get("authorization") != "Basic secret-secret" {
		t.Fatal(headers.Get("authorization"))
	}
	if headers.Get("content-type") != "application/connect+proto" {
		t.Fatal(headers.Get("content-type"))
	}
	if headers.Get("connect-accept-encoding") != "gzip" {
		t.Fatal("missing connect-accept-encoding")
	}
	if m := headers.Get("sentry-trace"); len(m) != 0 && !regexp32Hex.MatchString(m) {
		t.Fatalf("sentry-trace %q", m)
	}
	if Headers("secret", HeadersUnary).Get("content-type") != "application/proto" {
		t.Fatal("unary content-type")
	}

	request := unframe(t, BuildChatRequest("secret", wire.Body{"messages": []any{}}, "swe-1-6-slow", ChatOptions{}))
	meta := tSub(t, request, 1)
	if len(tFields(t, meta, 3)) != 1 {
		t.Fatal("token field count")
	}
	for num, want := range map[int]string{1: "chisel", 2: clientVersion(), 3: "secret", 4: "en", 5: runtime.GOOS, 7: clientVersion(), 12: "chisel"} {
		if got := tStr(t, meta, num); got != want {
			t.Fatalf("meta #%d = %q, want %q", num, got, want)
		}
	}
	fingerprint := tStr(t, meta, 31)
	if len(fingerprint) != 732 {
		t.Fatalf("fingerprint len %d", len(fingerprint))
	}
	// Stable across calls within one process.
	other := unframe(t, BuildChatRequest("secret", wire.Body{}, "swe-1-6-slow", ChatOptions{}))
	if tStr(t, tSub(t, other, 1), 31) != fingerprint {
		t.Fatal("fingerprint not stable")
	}
	if tNum(t, request, 7) != 5 || tNum(t, request, 20) != 1 {
		t.Fatal("fixed fields")
	}
	if tStr(t, request, 21) != "swe-1-6-slow" {
		t.Fatal("model")
	}
	config := tSub(t, request, 8)
	if tNum(t, config, 1) != 1 || tNum(t, config, 2) != 16384 || tNum(t, config, 3) != 128000 ||
		tDouble(t, config, 5) != 1 || tNum(t, config, 7) != 40 || tDouble(t, config, 8) != 0.95 {
		t.Fatal("completion config")
	}
}

var regexp32Hex = regexpMust(`^[a-f0-9]{32}-[a-f0-9]{16}-1$`)

func TestRequestMapsRolesImagesToolsLimitsSession(t *testing.T) {
	body := wire.Body{
		"messages": []any{
			wire.Body{"role": "system", "content": "Be concise"},
			wire.Body{"role": "developer", "content": "No markup"},
			wire.Body{"role": "user", "content": "First"},
			wire.Body{"role": "user", "content": []any{wire.Body{"type": "text", "text": "Second"}}},
			wire.Body{"role": "user", "content": "Third"},
			wire.Body{"role": "user", "content": []any{
				wire.Body{"type": "text", "text": "See picture"},
				wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,YWJj"}},
				wire.Body{"type": "image_url", "image_url": wire.Body{"url": "https://example.test/image.jpg"}},
			}},
			wire.Body{"role": "assistant", "content": "Calling", "tool_calls": []any{
				wire.Body{"id": "call1", "type": "function", "function": wire.Body{"name": "weather", "arguments": `{"city":"Paris"}`}},
			}},
			wire.Body{"role": "tool", "content": "sunny", "tool_call_id": "call1"},
		},
		"tools": []any{wire.Body{
			"type":     "function",
			"function": wire.Body{"name": "weather", "description": "Look up weather", "parameters": wire.Body{"type": "object"}},
		}},
		"max_tokens":            float64(100),
		"max_completion_tokens": float64(128),
		"temperature":           float64(0),
	}
	request := unframe(t, BuildChatRequest("secret", body, "model", ChatOptions{SessionID: "conversation-1", MaxOutput: 500}))
	system := tStr(t, request, 2)
	if !strings.Contains(system, "Be concise\n\nNo markup") || !strings.Contains(system, "weather: Look up weather") {
		t.Fatalf("system %q", system)
	}
	var turns [][]byte
	for _, f := range tFields(t, request, 3) {
		turns = append(turns, f.value.([]byte))
	}
	sources := []uint64{tNum(t, turns[0], 2), tNum(t, turns[1], 2), tNum(t, turns[2], 2), tNum(t, turns[3], 2)}
	if sources[0] != 1 || sources[1] != 1 || sources[2] != 2 || sources[3] != 4 {
		t.Fatalf("sources %v", sources)
	}
	if tStr(t, turns[0], 3) != "First\n\nSecond\n\nThird" {
		t.Fatal("merged user turn")
	}
	if tStr(t, turns[1], 3) != "See picture" {
		t.Fatal("image turn text")
	}
	images := tFields(t, turns[1], 10)
	if len(images) != 1 {
		t.Fatal("image count")
	}
	if tStr(t, images[0].value.([]byte), 1) != "YWJj" || tStr(t, images[0].value.([]byte), 2) != "image/png" {
		t.Fatal("image fields")
	}
	call := tSub(t, turns[2], 6)
	if tStr(t, call, 1) != "call1" || tStr(t, call, 2) != "weather" || tStr(t, call, 3) != `{"city":"Paris"}` {
		t.Fatal("tool call")
	}
	if tStr(t, turns[3], 7) != "call1" {
		t.Fatal("tool result id")
	}
	tool := tSub(t, request, 10)
	if tStr(t, tool, 1) != "weather" || tStr(t, tool, 2) != "weather" {
		t.Fatal("tool fields")
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(tStr(t, tool, 3)), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema %v", schema)
	}
	if tNum(t, tSub(t, request, 8), 2) != 128 {
		t.Fatal("max tokens")
	}
	if tDouble(t, tSub(t, request, 8), 5) != 0.001 {
		t.Fatal("clamped temperature")
	}
	session := tStr(t, request, 16)
	if !regexpMust(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(session) {
		t.Fatalf("session %q", session)
	}
	if tStr(t, tSub(t, request, 15), 1) != session || tNum(t, tSub(t, request, 15), 3) != 4 || tNum(t, tSub(t, request, 15), 4) != 14 {
		t.Fatal("session embed")
	}
	again := unframe(t, BuildChatRequest("secret", body, "model", ChatOptions{SessionID: "conversation-1"}))
	if tStr(t, again, 16) != session {
		t.Fatal("session not stable")
	}
}

func TestToolDescriptionsMoveToSystemPrompt(t *testing.T) {
	request := unframe(t, BuildChatRequest("secret", wire.Body{
		"messages": []any{
			wire.Body{"role": "system", "content": "Keep existing instructions"},
			wire.Body{"role": "user", "content": "hi"},
		},
		"tools": []any{wire.Body{
			"type": "function",
			"function": wire.Body{
				"name":        "Shell",
				"description": "Run a shell command safely",
				"parameters": wire.Body{
					"type":        "object",
					"description": "Top-level instructions",
					"properties": wire.Body{
						"command":     wire.Body{"type": "string", "description": "The command to run"},
						"description": wire.Body{"type": "string", "description": "A parameter literally named description"},
					},
					"required": []any{"command"},
				},
			},
		}},
	}, "swe-2-max", ChatOptions{}))
	system := tStr(t, request, 2)
	if !strings.Contains(system, "Keep existing instructions") || !strings.Contains(system, "Shell: Run a shell command safely") {
		t.Fatalf("system %q", system)
	}
	tool := tSub(t, request, 10)
	var schema map[string]any
	if err := json.Unmarshal([]byte(tStr(t, tool, 3)), &schema); err != nil {
		t.Fatal(err)
	}
	if _, has := schema["description"]; has {
		t.Fatal("top description kept")
	}
	props, _ := schema["properties"].(map[string]any)
	if got, _ := props["command"].(map[string]any)["description"]; got != nil {
		t.Fatal("property description kept")
	}
	if _, has := props["description"]; !has {
		t.Fatal("property named description dropped")
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "command" {
		t.Fatalf("required %v", schema["required"])
	}
}

func TestToolsWithoutSystemGetOne(t *testing.T) {
	request := unframe(t, BuildChatRequest("secret", wire.Body{
		"messages": []any{wire.Body{"role": "user", "content": "hi"}},
		"tools":    []any{wire.Body{"type": "function", "function": wire.Body{"name": "search"}}},
	}, "model", ChatOptions{MaxOutput: 1000}))
	if !strings.Contains(tStr(t, request, 2), "You are a helpful assistant. Use the available tools when appropriate.") {
		t.Fatal("missing injected system prompt")
	}
	if tNum(t, tSub(t, request, 8), 2) != 1000 {
		t.Fatal("maxOutput option")
	}
}

func TestPromptSignatureRewrites(t *testing.T) {
	identity := "You operate in Cursor."
	toolCalling := "Use specialized tools instead of terminal commands when possible, as this provides a " +
		"better user experience. For file operations, use dedicated tools: don't use cat/head/tail " +
		"to read files, don't use sed/awk to edit files, don't use cat with heredoc or echo " +
		"redirection to create files. Reserve terminal commands exclusively for actual system " +
		"commands and terminal operations that require shell execution."
	system := strings.Join([]string{
		identity, toolCalling,
		"## METHOD 2: MARKDOWN CODE BLOCKS - Proposing or Displaying Code NOT already in Codebase",
		"There is one text file for each terminal the user has running.",
		"Unrelated sentence that must survive verbatim.",
	}, "\n\n")
	request := unframe(t, BuildChatRequest("secret", wire.Body{
		"messages": []any{
			wire.Body{"role": "system", "content": system},
			wire.Body{"role": "user", "content": "hi"},
		},
	}, "model", ChatOptions{}))
	sent := tStr(t, request, 2)
	for _, gone := range []string{identity, toolCalling, "MARKDOWN CODE BLOCKS - Proposing or Displaying", "one text file for each terminal the user has running"} {
		if strings.Contains(sent, gone) {
			t.Fatalf("signature survived: %q", gone)
		}
	}
	if !strings.Contains(sent, "Unrelated sentence that must survive verbatim.") {
		t.Fatal("unrelated text rewritten")
	}
	// The identity line is rewritten wherever it appears, including suffixed forms.
	if got := SanitizeSystemPrompt("You operate in Cursor IDE\nrest"); got != "You work inside the user's code editor.\nrest" {
		t.Fatalf("identity rewrite %q", got)
	}
	// builtins off sends the prompt untouched.
	raw := unframe(t, BuildChatRequest("secret", wire.Body{
		"messages": []any{wire.Body{"role": "system", "content": system}},
	}, "model", ChatOptions{NoBuiltins: true}))
	if !strings.Contains(tStr(t, raw, 2), "You operate in Cursor.") {
		t.Fatal("NoBuiltins still rewrote")
	}
}

func TestStripAgentSystemMessages(t *testing.T) {
	messages := []any{
		wire.Body{"role": "system", "content": "a"},
		wire.Body{"role": "developer", "content": "b"},
		wire.Body{"role": "user", "content": "c"},
		wire.Body{"role": "assistant", "content": "d"},
	}
	got := StripAgentSystemMessages(messages)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if wire.AsRecord(got[0])["role"] != "user" || wire.AsRecord(got[1])["role"] != "assistant" {
		t.Fatalf("kept %v", got)
	}
}
