package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/upstream"
)

// responseOf normalizes data and returns the response map for assertions.
func responseOf(t *testing.T, kind upstream.ClientKind, stream bool, data string) map[string]any {
	t.Helper()
	return normalizeResponse(kind, stream, []byte(data))
}

func textOfResponse(t *testing.T, response map[string]any) string {
	t.Helper()
	text, ok := response["text"].(string)
	if !ok {
		t.Fatalf("response.text is not a string: %v", response["text"])
	}
	return text
}

func TestNormalizeResponse(t *testing.T) {
	tests := []struct {
		name         string
		kind         upstream.ClientKind
		stream       bool
		data         string
		wantText     string
		wantReason   string
		wantFinish   string
		wantToolID   string
		wantToolName string
		wantToolArgs string
		wantWire     string
		wantStatus   float64
	}{
		{
			name:       "openai chat json",
			kind:       upstream.KindOpenAI,
			data:       `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			wantText:   "hello",
			wantFinish: "stop",
			wantWire:   "openai",
			wantStatus: 200,
		},
		{
			name:   "openai chat sse text tool call split finish",
			kind:   upstream.KindOpenAI,
			stream: true,
			data: "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n" +
				"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n" +
				"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
				"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a.go\\\"}\"}}]}}]}\n\n" +
				"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n",
			wantText:     "Hello",
			wantFinish:   "tool_calls",
			wantToolID:   "call_1",
			wantToolName: "read_file",
			wantToolArgs: `{"path":"a.go"}`,
			wantWire:     "openai",
		},
		{
			name:       "anthropic json text thinking tool_use",
			kind:       upstream.KindAnthropic,
			data:       `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.go"}}],"stop_reason":"tool_use"}`,
			wantText:   "hi",
			wantReason: "hmm",
			wantFinish: "tool_use",
			wantWire:   "anthropic",
			// tool assertions below
			wantToolID:   "toolu_1",
			wantToolName: "read_file",
			wantToolArgs: `{"path":"a.go"}`,
		},
		{
			name:   "anthropic sse deltas",
			kind:   upstream.KindAnthropic,
			stream: true,
			data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read_file\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.go\\\"}\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n",
			wantText:     "answer",
			wantReason:   "think",
			wantFinish:   "tool_use",
			wantToolID:   "toolu_1",
			wantToolName: "read_file",
			wantToolArgs: `{"path":"a.go"}`,
			wantWire:     "anthropic",
		},
		{
			name:       "responses json message function_call reasoning",
			kind:       upstream.KindResponses,
			data:       `{"id":"r1","object":"response","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi there"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"}]}`,
			wantText:   "hi there",
			wantReason: "thought",
			wantFinish: "completed",
			wantToolID: "call_1",
			// tool assertions below
			wantToolName: "read_file",
			wantToolArgs: `{"path":"a.go"}`,
			wantWire:     "responses",
		},
		{
			name:   "responses sse deltas completed",
			kind:   upstream.KindResponses,
			stream: true,
			data: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi \"}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"there\"}\n\n" +
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"read_file\",\"arguments\":\"\"}}\n\n" +
				"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"path\\\":\"}\n\n" +
				"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"\\\"a.go\\\"}\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\n",
			wantText:     "hi there",
			wantFinish:   "completed",
			wantToolID:   "call_1",
			wantToolName: "read_file",
			wantToolArgs: `{"path":"a.go"}`,
			wantWire:     "responses",
		},
		{
			name:       "garbage json",
			kind:       upstream.KindOpenAI,
			data:       `not json at all {{{`,
			wantText:   "",
			wantWire:   "openai",
			wantStatus: 200,
		},
		{
			name:       "garbage sse",
			kind:       upstream.KindAnthropic,
			stream:     true,
			data:       "event: nonsense\ndata: not-json\n\n",
			wantText:   "",
			wantWire:   "anthropic",
			wantStatus: 200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := responseOf(t, tc.kind, tc.stream, tc.data)
			if got := textOfResponse(t, response); got != tc.wantText {
				t.Fatalf("text = %q, want %q (response %v)", got, tc.wantText, response)
			}
			if got, _ := response["wire"].(string); got != tc.wantWire {
				t.Fatalf("wire = %q, want %q", got, tc.wantWire)
			}
			if tc.wantStatus != 0 {
				if got := wireNumber(response["status"]); got != tc.wantStatus {
					t.Fatalf("status = %v, want %v", response["status"], tc.wantStatus)
				}
			}
			if got, _ := response["stream"].(bool); got != tc.stream {
				t.Fatalf("stream = %v, want %v", response["stream"], tc.stream)
			}
			if tc.wantReason == "" {
				if _, present := response["reasoning"]; present {
					t.Fatalf("reasoning should be omitted, got %v", response["reasoning"])
				}
			} else if got, _ := response["reasoning"].(string); got != tc.wantReason {
				t.Fatalf("reasoning = %q, want %q", got, tc.wantReason)
			}
			if tc.wantFinish == "" {
				if _, present := response["finishReason"]; present {
					t.Fatalf("finishReason should be omitted, got %v", response["finishReason"])
				}
			} else if got, _ := response["finishReason"].(string); got != tc.wantFinish {
				t.Fatalf("finishReason = %q, want %q", got, tc.wantFinish)
			}
			if tc.wantToolID == "" {
				if _, present := response["toolCalls"]; present {
					t.Fatalf("toolCalls should be omitted, got %v", response["toolCalls"])
				}
				return
			}
			calls, _ := response["toolCalls"].([]any)
			if len(calls) != 1 {
				t.Fatalf("toolCalls = %v, want exactly 1", response["toolCalls"])
			}
			call := wireRecord(calls[0])
			if got, _ := call["id"].(string); got != tc.wantToolID {
				t.Fatalf("toolCalls[0].id = %q, want %q", got, tc.wantToolID)
			}
			if got, _ := call["name"].(string); got != tc.wantToolName {
				t.Fatalf("toolCalls[0].name = %q, want %q", got, tc.wantToolName)
			}
			if got, _ := call["arguments"].(string); got != tc.wantToolArgs {
				t.Fatalf("toolCalls[0].arguments = %q, want %q", got, tc.wantToolArgs)
			}
		})
	}
}

func wireNumber(value any) float64 {
	n, _ := value.(float64)
	if n == 0 {
		// JSON numbers decoded into any are float64; accept int for safety.
		if i, ok := value.(int); ok {
			return float64(i)
		}
	}
	return n
}

func wireRecord(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func TestNormalizeResponseTruncatesText(t *testing.T) {
	huge := strings.Repeat("a", maxCaptureFieldBytes+4096)
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": huge},
			"finish_reason": "stop",
		}},
	})
	response := normalizeResponse(upstream.KindOpenAI, false, body)
	text := textOfResponse(t, response)
	if len(text) != maxCaptureFieldBytes {
		t.Fatalf("text length = %d, want %d", len(text), maxCaptureFieldBytes)
	}
	if truncated, _ := response["truncated"].(bool); !truncated {
		t.Fatalf("truncated = %v, want true", response["truncated"])
	}
}

func TestNormalizeResponseTruncatesReasoning(t *testing.T) {
	huge := strings.Repeat("t", maxCaptureFieldBytes+1)
	body, _ := json.Marshal(map[string]any{
		"content": []any{
			map[string]any{"type": "thinking", "thinking": huge},
			map[string]any{"type": "text", "text": "answer"},
		},
		"stop_reason": "end_turn",
	})
	response := normalizeResponse(upstream.KindAnthropic, false, body)
	if got := textOfResponse(t, response); got != "answer" {
		t.Fatalf("text = %q", got)
	}
	reason, _ := response["reasoning"].(string)
	if len(reason) != maxCaptureFieldBytes {
		t.Fatalf("reasoning length = %d, want %d", len(reason), maxCaptureFieldBytes)
	}
	if truncated, _ := response["truncated"].(bool); !truncated {
		t.Fatalf("truncated = %v, want true", response["truncated"])
	}
}

func TestStreamCaptureCapAndTee(t *testing.T) {
	capture := &streamCapture{}
	capture.write([]byte("hello "))
	capture.write([]byte("world"))
	if got, truncated := capture.snapshot(); string(got) != "hello world" || truncated {
		t.Fatalf("snapshot = %q truncated=%v", got, truncated)
	}

	// A single write past the cap buffers only the cap and marks truncated.
	big := &streamCapture{}
	big.write([]byte(strings.Repeat("x", maxCaptureStreamBytes+10)))
	got, truncated := big.snapshot()
	if len(got) != maxCaptureStreamBytes || !truncated {
		t.Fatalf("len=%d truncated=%v, want %d/true", len(got), truncated, maxCaptureStreamBytes)
	}
	// Writes after the cap are ignored without growing the buffer.
	big.write([]byte("more"))
	if got2, _ := big.snapshot(); len(got2) != maxCaptureStreamBytes {
		t.Fatalf("buffer grew after cap: %d", len(got2))
	}

	// The tee forwards every byte to the consumer and to the capture.
	tee := &teeReadCloser{source: io.NopCloser(strings.NewReader("payload")), capture: &streamCapture{}}
	data, err := io.ReadAll(tee)
	if err != nil || string(data) != "payload" {
		t.Fatalf("tee read = %q %v", data, err)
	}
	if got, _ := tee.capture.snapshot(); string(got) != "payload" {
		t.Fatalf("tee capture = %q", got)
	}
}

// TestResponseCaptureIntegration proves a proxied request's body file carries
// the model answer, for the stream and non-stream paths.
func TestResponseCaptureIntegration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "")

	const chatOK = `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"streamed\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	cfg := config.Config{
		Listen:          config.ListenConfig{Host: "127.0.0.1", Port: 8787},
		DefaultProvider: "p1",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{{
			Name: "p1", Type: config.ProviderTypeOpenAI, BaseURL: up.URL + "/v1",
			APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "m"}},
		}},
	}
	h := New("", Deps{Config: &cfg}).Handler()

	readCapture := func(t *testing.T, requestID string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "bodies", requestID+".json"))
		if err != nil {
			t.Fatalf("no capture for %s: %v", requestID, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	post := func(t *testing.T, payload string) (string, string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		return rr.Header().Get("x-jevonian-request-id"), rr.Body.String()
	}

	// Non-stream: the final client-wire JSON is parsed into response.text.
	id, body := post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if id == "" {
		t.Fatal("no request id header")
	}
	FlushBodies()
	capture := readCapture(t, id)
	response, _ := capture["response"].(map[string]any)
	if response == nil {
		t.Fatalf("capture has no response: %v", capture)
	}
	if got, _ := response["text"].(string); got != "ok" {
		t.Fatalf("non-stream response.text = %q (body %s)", got, body)
	}
	if got, _ := response["wire"].(string); got != "openai" {
		t.Fatalf("non-stream response.wire = %q", got)
	}
	if got, _ := response["status"].(float64); got != 200 {
		t.Fatalf("non-stream response.status = %v", response["status"])
	}
	if got, _ := response["finishReason"].(string); got != "stop" {
		t.Fatalf("non-stream response.finishReason = %q", got)
	}

	// Stream: the teed client-wire SSE is folded into response.text.
	id, body = post(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	FlushBodies()
	capture = readCapture(t, id)
	response, _ = capture["response"].(map[string]any)
	if response == nil {
		t.Fatalf("stream capture has no response: %v", capture)
	}
	if got, _ := response["text"].(string); got != "streamed" {
		t.Fatalf("stream response.text = %q (body %s)", got, body)
	}
	if stream, _ := response["stream"].(bool); !stream {
		t.Fatalf("stream response.stream = %v", response["stream"])
	}
}

// TestResponseCaptureFailurePath proves a failed turn's body file records the
// error text with an empty text, per the capture contract.
func TestResponseCaptureFailurePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()

	cfg := config.Config{
		Listen:          config.ListenConfig{Host: "127.0.0.1", Port: 8787},
		DefaultProvider: "p1",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{{
			Name: "p1", Type: config.ProviderTypeOpenAI, BaseURL: up.URL + "/v1",
			APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "m"}},
		}},
	}
	zero := 0
	h := New("", Deps{Config: &cfg, SameHostRetries: &zero}).Handler()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 200 {
		t.Fatalf("expected a failure, got %d", rr.Code)
	}
	id := rr.Header().Get("x-jevonian-request-id")
	FlushBodies()

	raw, err := os.ReadFile(filepath.Join(dir, "bodies", id+".json"))
	if err != nil {
		t.Fatalf("no capture for %s: %v", id, err)
	}
	var capture map[string]any
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	response, _ := capture["response"].(map[string]any)
	if response == nil {
		t.Fatalf("capture has no response: %s", raw)
	}
	if text, _ := response["text"].(string); text != "" {
		t.Fatalf("failure response.text = %q, want empty", text)
	}
	if errText, _ := response["error"].(string); errText == "" {
		t.Fatalf("failure response.error is empty: %v", response)
	}
	if stream, _ := response["stream"].(bool); stream {
		t.Fatalf("failure response.stream = true, want false")
	}
}
