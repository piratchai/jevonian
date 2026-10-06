package devin

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

func testCtx() context.Context       { return context.Background() }
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

const routedModel = "swe-1-6-slow"

// responseFrames mirrors devin-routing.test.ts responseFrames.
func responseFrames(tool, thinking bool) []byte {
	var frames [][]byte
	if thinking {
		frames = append(frames, EncodeFrame(StringField(9, "Checking the weather"), 0))
	}
	text := "pong"
	if tool {
		text = "Calling tool"
	}
	frames = append(frames, EncodeFrame(StringField(3, text), 0))
	if tool {
		frames = append(frames,
			EncodeFrame(BytesField(6, Concat(StringField(1, "call_abc"), StringField(2, "get_weather"), StringField(3, `{"city":`))), 0),
			EncodeFrame(BytesField(6, StringField(3, `"Paris"}`)), 0),
		)
	}
	stop := uint64(2)
	if tool {
		stop = 10
	}
	frames = append(frames,
		EncodeFrame(Concat(
			VarintField(5, stop),
			BytesField(7, Concat(VarintField(2, 4), VarintField(3, 3), VarintField(4, 2), VarintField(5, 6), StringField(9, routedModel))),
		), 0),
		EncodeFrame([]byte("{}"), 2),
	)
	return Concat(frames...)
}

type capture struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
	paths   []string
}

func (c *capture) record(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.Header.Clone())
	c.bodies = append(c.bodies, body)
	c.paths = append(c.paths, r.URL.Path)
	return body
}

// writeChunked flushes the body in 7-byte pieces, like chunkedStream in the TS test.
func writeChunked(w http.ResponseWriter, data []byte) {
	w.Header().Set("content-type", "application/connect+proto")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	for i := 0; i < len(data); i += 7 {
		end := i + 7
		if end > len(data) {
			end = len(data)
		}
		_, _ = w.Write(data[i:end])
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func newProvider(t *testing.T, handler http.HandlerFunc) (*Provider, *capture) {
	t.Helper()
	c := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(context.WithValue(r.Context(), captureKey{}, c)))
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(credentials, []byte("windsurf_api_key = \"test-devin-token\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", credentials)
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	ResetModelMeta()
	t.Cleanup(ResetModelMeta)
	p := NewProvider(config.Provider{Name: "devin-subscription", Type: config.ProviderTypeDevin, BaseURL: server.URL}, server.Client())
	return p, c
}

type captureKey struct{}

func cap(r *http.Request) *capture { return r.Context().Value(captureKey{}).(*capture) }

// postedTurns decodes the Devin turns posted in a Connect request body.
func postedTurns(t *testing.T, body []byte) (system string, turns []map[string]any) {
	t.Helper()
	if body[0] != 0 || binary.BigEndian.Uint32(body[1:5]) != uint32(len(body)-5) {
		t.Fatal("bad envelope")
	}
	request := body[5:]
	system = tStr(t, request, 2)
	for _, f := range tFields(t, request, 3) {
		raw := f.value.([]byte)
		var images []string
		for _, img := range tFields(t, raw, 10) {
			b := img.value.([]byte)
			images = append(images, tStr(t, b, 2)+":"+tStr(t, b, 1))
		}
		turn := map[string]any{"role": tNum(t, raw, 2), "text": tStr(t, raw, 3), "images": images}
		if len(tFields(t, raw, 7)) > 0 {
			turn["toolCallId"] = tStr(t, raw, 7)
		}
		if call := tSub(t, raw, 6); call != nil {
			turn["toolName"] = tStr(t, call, 2)
		}
		turns = append(turns, turn)
	}
	return system, turns
}

func TestProviderStreamsChatDeltasWithUsage(t *testing.T) {
	p, c := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		cap(r).record(r)
		writeChunked(w, responseFrames(false, true))
	})
	var finish Finish
	result, err := p.Chat(testCtx(), ChatRequest{
		Body:     wire.Body{"messages": []any{wire.Body{"role": "user", "content": "say pong"}}},
		Model:    routedModel,
		Stream:   true,
		OnFinish: func(f Finish) { finish = f },
	})
	if err != nil || result.Error != nil {
		t.Fatalf("chat %v %+v", err, result.Error)
	}
	out, _ := io.ReadAll(result.Stream)
	_ = result.Stream.Close()
	text := string(out)
	for _, want := range []string{`"reasoning_content":"Checking the weather"`, `"content":"pong"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	if c.paths[0] != "/exa.api_server_pb.ApiServerService/GetChatMessage" {
		t.Fatal(c.paths[0])
	}
	if c.headers[0].Get("authorization") != "Basic test-devin-token-test-devin-token" || c.headers[0].Get("content-type") != "application/connect+proto" {
		t.Fatal("headers")
	}
	if finish.Usage != (wire.Usage{Input: 4, Output: 3, CacheWrite: 2, CacheRead: 6}) {
		t.Fatalf("usage %+v", finish.Usage)
	}
}

func TestProviderFoldsNonStreamWithTools(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeChunked(w, responseFrames(true, true))
	})
	result, err := p.Chat(testCtx(), ChatRequest{
		Body: wire.Body{
			"messages": []any{wire.Body{"role": "user", "content": "weather?"}},
			"tools":    []any{wire.Body{"type": "function", "function": wire.Body{"name": "get_weather", "parameters": wire.Body{"type": "object", "properties": wire.Body{}}}}},
		},
		Model: routedModel,
	})
	if err != nil || result.Error != nil || result.Finish.Error != nil {
		t.Fatalf("chat %v %+v %+v", err, result.Error, result.Finish.Error)
	}
	choice := result.Completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatal(choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["reasoning_content"] != "Checking the weather" {
		t.Fatal(msg)
	}
	call := msg["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_abc" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Fatal(call)
	}
	usage := result.Completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != 12 || usage["completion_tokens"] != 3 ||
		usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != 6 {
		t.Fatal(usage)
	}
}

func TestProviderEncodesImagesInOrderedTurns(t *testing.T) {
	p, c := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		cap(r).record(r)
		writeChunked(w, responseFrames(false, false))
	})
	// The server folds an Anthropic body into this Chat shape (one image per message).
	body := wire.Body{"messages": []any{
		wire.Body{"role": "system", "content": "Describe precisely"},
		wire.Body{"role": "assistant", "content": "", "tool_calls": []any{
			wire.Body{"id": "call_1", "type": "function", "function": wire.Body{"name": "inspect", "arguments": "{}"}},
		}},
		wire.Body{"role": "tool", "tool_call_id": "call_1", "content": "ready"},
		wire.Body{"role": "user", "content": []any{wire.Body{"type": "text", "text": "Before"}}},
		wire.Body{"role": "user", "content": []any{wire.Body{"type": "image_url", "image_url": wire.Body{"url": "data:image/png;base64,YWJj"}}}},
		wire.Body{"role": "user", "content": []any{wire.Body{"type": "text", "text": "After"}}},
	}}
	result, err := p.Chat(testCtx(), ChatRequest{Body: body, Model: routedModel})
	if err != nil || result.Error != nil {
		t.Fatal(err, result.Error)
	}
	system, turns := postedTurns(t, c.bodies[0])
	if system != "Describe precisely" {
		t.Fatalf("system %q", system)
	}
	want := []struct {
		role   uint64
		text   string
		images int
	}{{2, "", 0}, {4, "ready", 0}, {1, "Before", 0}, {1, "", 1}, {1, "After", 0}}
	if len(turns) != len(want) {
		t.Fatalf("turns %v", turns)
	}
	for i, w := range want {
		if turns[i]["role"] != w.role || turns[i]["text"] != w.text || len(turns[i]["images"].([]string)) != w.images {
			t.Fatalf("turn %d = %v", i, turns[i])
		}
	}
	if turns[3]["images"].([]string)[0] != "image/png:YWJj" {
		t.Fatal(turns[3])
	}
	if turns[0]["toolName"] != "inspect" || turns[1]["toolCallId"] != "call_1" {
		t.Fatal(turns[:2])
	}
}

func TestProviderLeadingTrailerIsClassifiedRefusal(t *testing.T) {
	trailer := EncodeFrame([]byte(`{"error":{"code":"resource_exhausted","message":"Rate limit. Resets in: 1h0m0s"}}`), 2)
	hits := 0
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeChunked(w, trailer)
	})
	result, err := p.Chat(testCtx(), ChatRequest{
		Body:  wire.Body{"messages": []any{wire.Body{"role": "user", "content": "hi"}}},
		Model: routedModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Kind != KindRateLimit || result.Error.Status != 429 || result.Error.ResetsAt.IsZero() {
		t.Fatalf("refusal %+v", result.Error)
	}
	if !ShouldFailover(result.Error) || hits != 1 {
		t.Fatal("failover / hits")
	}
	if ErrorTypes[result.Error.Kind] != "rate_limit_error" {
		t.Fatal("error type")
	}
}

func TestProviderContentPolicyRetriesWithoutSystemPrompt(t *testing.T) {
	p, c := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		cap(r).record(r)
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code":"permission_denied","message":"blocked by our content policy"}`))
	})
	result, err := p.Chat(testCtx(), ChatRequest{
		Body: wire.Body{"messages": []any{
			wire.Body{"role": "system", "content": "You operate in Cursor. CLIENT-SYSTEM-MARKER"},
			wire.Body{"role": "user", "content": "hi"},
		}},
		Model: routedModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Kind != KindContentPolicy || result.Error.Status != 400 || !result.PolicyRetried {
		t.Fatalf("result %+v", result)
	}
	if ShouldFailover(result.Error) {
		t.Fatal("content policy must not fail over")
	}
	if len(c.bodies) != 2 {
		t.Fatalf("attempts %d", len(c.bodies))
	}
	first, second := string(c.bodies[0]), string(c.bodies[1])
	if strings.Contains(first, "You operate in Cursor.") || !strings.Contains(first, "CLIENT-SYSTEM-MARKER") {
		t.Fatal("first attempt prompt")
	}
	if strings.Contains(second, "CLIENT-SYSTEM-MARKER") {
		t.Fatal("retry kept client system prompt")
	}
}

func TestProviderFreeModelLimitIsModelScoped(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeChunked(w, EncodeFrame([]byte(`{"error":{"code":"unavailable","message":"Reached free model rate limit. Upgrade to Max for higher limits, or switch to a different model. Your limit will reset in 2 hours 37 minutes."}}`), 2))
	})
	result, _ := p.Chat(testCtx(), ChatRequest{Body: wire.Body{"messages": []any{wire.Body{"role": "user", "content": "hi"}}}, Model: routedModel})
	if result.Error == nil || result.Error.Kind != KindRateLimit || !ModelScoped(result.Error) {
		t.Fatalf("refusal %+v", result.Error)
	}
}

func TestProviderModelsAndQuota(t *testing.T) {
	row := func(id string, disabled bool) []byte {
		d := uint64(0)
		if disabled {
			d = 1
		}
		return BytesField(1, Concat(StringField(1, id), StringField(22, id), VarintField(4, d),
			BytesField(23, Concat(VarintField(4, 200_000), VarintField(13, 32_000)))))
	}
	p, c := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		cap(r).record(r)
		if strings.Contains(r.URL.Path, "GetCliModelConfigs") {
			_, _ = w.Write(Concat(row(routedModel, false), row("claude-opus-4-8-medium", false), row("fusion-combo", false),
				row("adaptive", false), row("arena-blind", false), row("disabled", true)))
			return
		}
		info := BytesField(1, StringField(2, "Pro"))
		plan := BytesField(13, Concat(info, VarintField(14, 1), VarintField(15, 75), VarintField(17, 1_900_000_000), VarintField(18, 1_900_259_200)))
		_, _ = w.Write(BytesField(1, plan))
	})
	models, err := p.Models(testCtx())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != routedModel || models[1].ID != "claude-opus-4-8-medium" {
		t.Fatalf("models %v", models)
	}
	if c.headers[0].Get("content-type") != "application/proto" {
		t.Fatal("unary content-type")
	}
	if meta, ok := LookupModelMeta(routedModel); !ok || meta.ContextWindow != 200_000 || meta.MaxOutput != 32_000 {
		t.Fatalf("meta %+v", meta)
	}
	status, err := p.Quota(testCtx())
	if err != nil || status.Plan != "Pro" {
		t.Fatal(status, err)
	}
	windows := status.Windows()
	if len(windows) != 2 || windows[0].UsedPercent != 99 || windows[1].UsedPercent != 25 ||
		windows[0].ResetsAt.Unix() != 1_900_000_000 || windows[1].ResetsAt.Unix() != 1_900_259_200 {
		t.Fatalf("windows %+v", windows)
	}
}

func TestProviderTokenFromCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.toml")
	_ = os.WriteFile(path, []byte("# comment\nwindsurf_api_key = \"tok\\\"en\"\napi_server_url = 'https://alt.example/'\n"), 0o600)
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	token, err := ReadToken(nil)
	if err != nil || token != `tok"en` {
		t.Fatalf("token %q %v", token, err)
	}
	if ServerURL(nil) != "https://alt.example" {
		t.Fatal(ServerURL(nil))
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", filepath.Join(dir, "missing.toml"))
	if _, err := ReadToken(nil); err == nil || HasCredential(nil) {
		t.Fatal("missing credentials should fail")
	}
}
