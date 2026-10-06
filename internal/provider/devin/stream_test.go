package devin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

func regexpMust(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// chunkedReader yields data n bytes at a time, like a split network stream.
type chunkedReader struct {
	data   []byte
	n      int
	closed bool
	err    error // returned after data is exhausted (nil → io.EOF)
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := r.n
	if n <= 0 || n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func (r *chunkedReader) Close() error { r.closed = true; return nil }

func chunks(data []byte, n int) *chunkedReader { return &chunkedReader{data: data, n: n} }

func TestStreamSplitGzipReasoningToolsUsageFinish(t *testing.T) {
	var finishes []Finish
	stream := ToChatStream("requested-model", chunks(upstreamStream(), 3), StreamOptions{
		OnFinish: func(f Finish) { finishes = append(finishes, f) },
	})
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	lines := sseLines(t, out)
	first := sseJSON(t, lines[0])
	if d := sseDelta(first); d["role"] != "assistant" || d["content"] != "" {
		t.Fatalf("role chunk %v", d)
	}
	var reasoning, contents []string
	var calls []map[string]any
	for _, line := range lines {
		if line == "[DONE]" {
			continue
		}
		d := sseDelta(sseJSON(t, line))
		if r, ok := d["reasoning_content"].(string); ok {
			reasoning = append(reasoning, r)
		}
		if c, ok := d["content"].(string); ok && c != "" {
			contents = append(contents, c)
		}
		if tc, ok := d["tool_calls"].([]any); ok {
			calls = append(calls, tc[0].(map[string]any))
		}
	}
	if strings.Join(reasoning, "") != "think" {
		t.Fatalf("reasoning %v", reasoning)
	}
	if strings.Join(contents, "|") != "Hello |world" {
		t.Fatalf("contents %v", contents)
	}
	if len(calls) != 2 {
		t.Fatalf("calls %v", calls)
	}
	if calls[0]["index"] != float64(0) || calls[0]["id"] != "call_weather" || calls[0]["type"] != "function" {
		t.Fatalf("first call %v", calls[0])
	}
	if fn := calls[0]["function"].(map[string]any); fn["name"] != "weather" || fn["arguments"] != `{"city":` {
		t.Fatalf("first fn %v", fn)
	}
	if fn := calls[1]["function"].(map[string]any); calls[1]["index"] != float64(0) || fn["arguments"] != `"Paris"}` {
		t.Fatalf("second call %v", calls[1])
	}
	last := sseJSON(t, lines[len(lines)-2])
	if sseFinish(last) != "tool_calls" {
		t.Fatalf("finish %v", sseFinish(last))
	}
	usage := last["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(25) || usage["completion_tokens"] != float64(7) || usage["total_tokens"] != float64(32) {
		t.Fatalf("usage %v", usage)
	}
	if usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(9) {
		t.Fatalf("cached %v", usage)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatal("no DONE")
	}
	if len(finishes) != 1 {
		t.Fatalf("finishes %d", len(finishes))
	}
	if f := finishes[0]; f.Usage != (wire.Usage{Input: 11, Output: 7, CacheWrite: 5, CacheRead: 9}) || f.Model != "actual-model" || f.Error != nil {
		t.Fatalf("finish %+v", f)
	}
}

func TestStreamMidStreamTrailerSoftCloses(t *testing.T) {
	data := Concat(
		EncodeFrame(StringField(3, "partial"), 0),
		EncodeFrame([]byte(`{"error":{"code":"unavailable","message":"high demand"}}`), 2),
	)
	var finishes []Finish
	out, _ := io.ReadAll(ToChatStream("model", chunks(data, 0), StreamOptions{
		OnFinish: func(f Finish) { finishes = append(finishes, f) },
	}))
	lines := sseLines(t, out)
	if sseDelta(sseJSON(t, lines[1]))["content"] != "partial" {
		t.Fatal("partial content")
	}
	text := sseText(lines)
	if !strings.Contains(text, "Jevonian hit an internal error") || !strings.Contains(text, "high demand") {
		t.Fatalf("soft text %q", text)
	}
	if sseFinish(sseJSON(t, lines[len(lines)-2])) != "stop" || lines[len(lines)-1] != "[DONE]" {
		t.Fatal("soft close shape")
	}
	if len(finishes) != 1 || finishes[0].Error == nil || finishes[0].Error.Kind != KindCapacity || finishes[0].Error.Status != 503 {
		t.Fatalf("finish %+v", finishes)
	}
}

func TestStreamStopReasonsAndTruncation(t *testing.T) {
	normal := Concat(EncodeFrame(Concat(StringField(3, "ok"), VarintField(5, 4)), 0), EncodeFrame([]byte("{}"), 2))
	out, _ := io.ReadAll(ToChatStream("model", chunks(normal, 0), StreamOptions{}))
	lines := sseLines(t, out)
	if sseFinish(sseJSON(t, lines[len(lines)-2])) != "stop" {
		t.Fatal("normal stop")
	}
	var finish Finish
	out, _ = io.ReadAll(ToChatStream("model", chunks(EncodeFrame(StringField(3, "partial"), 0), 0), StreamOptions{
		OnFinish: func(f Finish) { finish = f },
	}))
	lines = sseLines(t, out)
	if sseFinish(sseJSON(t, lines[len(lines)-2])) != "stop" {
		t.Fatal("truncated stop")
	}
	if finish.Error == nil || finish.Error.Kind != KindInternal {
		t.Fatalf("truncation not reported: %+v", finish)
	}
}

func TestStreamCloseReportsAbortOnce(t *testing.T) {
	var finishes []Finish
	body := &chunkedReader{data: EncodeFrame(StringField(3, "first"), 0), err: errors.New("never")}
	stream := ToChatStream("model", body, StreamOptions{OnFinish: func(f Finish) { finishes = append(finishes, f) }})
	buf := make([]byte, 4096)
	_, _ = stream.Read(buf)
	_ = stream.Close()
	_ = stream.Close()
	if !body.closed {
		t.Fatal("upstream not closed")
	}
	if len(finishes) != 1 || finishes[0].Error == nil || finishes[0].Error.Message != "Devin stream aborted" {
		t.Fatalf("finishes %+v", finishes)
	}
}

func TestPeekReplaysEveryByte(t *testing.T) {
	data := Concat(EncodeFrame(VarintField(4, 2), 0), upstreamStream())
	body := chunks(data, 2)
	stream, perr := Peek(body, "")
	if perr != nil {
		t.Fatal(perr)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("replay mismatch")
	}
	_ = stream.Close()
	if !body.closed {
		t.Fatal("not closed")
	}
}

func TestPeekReadFailureHidesCause(t *testing.T) {
	token := "devin-session-token$read-secret"
	body := &chunkedReader{err: errors.New("failed to read Basic " + token + "-" + token)}
	_, perr := Peek(body, token)
	if perr == nil || perr.Status != 502 || perr.Kind != KindOther || perr.Message != "Devin stream read failed" {
		t.Fatalf("got %+v", perr)
	}
	if !body.closed {
		t.Fatal("body not closed")
	}
}

func TestPeekTruncatedMalformedEarlyErrorClose(t *testing.T) {
	cases := [][]byte{
		nil,
		{0, 255, 255, 255, 255},
		EncodeFrame([]byte(`{"error":{"message":"invalid token"}}`), 2),
	}
	for i, data := range cases {
		body := chunks(data, 0)
		if _, perr := Peek(body, ""); perr == nil {
			t.Fatalf("case %d: expected error", i)
		}
		if !body.closed {
			t.Fatalf("case %d: not closed", i)
		}
	}
}

func TestPeekRedactsTrailerCredentials(t *testing.T) {
	token := "opaque-secret"
	msg, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "echo " + token + " and Basic " + token + "-" + token}})
	_, perr := Peek(chunks(EncodeFrame(msg, 2), 0), token)
	if perr == nil || perr.Message != "Devin upstream error" {
		t.Fatalf("got %+v", perr)
	}
	if strings.Contains(perr.Message, token) {
		t.Fatal("token leaked")
	}
}

func TestPeekSanitizesReadErrorAfterHandoff(t *testing.T) {
	token := "devin-session-token$after-peek"
	body := &chunkedReader{data: EncodeFrame(StringField(3, "ok"), 0), err: errors.New("read failed: Basic " + token + "-" + token)}
	stream, perr := Peek(body, token)
	if perr != nil {
		t.Fatal(perr)
	}
	_, err := io.ReadAll(stream)
	if err == nil || err.Error() != "Devin stream read failed" {
		t.Fatalf("err %v", err)
	}
}

func TestSecretsStayOutOfCompletionAndSSE(t *testing.T) {
	token := "devin-session-token$my-secret"
	msg, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "echo " + token}})
	trailer := EncodeFrame(msg, 2)
	_, finish := ChatCompletion(chunks(trailer, 0), "model", token)
	if finish.Error == nil || finish.Error.Message != "echo [REDACTED]" {
		t.Fatalf("finish %+v", finish.Error)
	}
	out, _ := io.ReadAll(ToChatStream("model", chunks(trailer, 0), StreamOptions{Token: token}))
	if strings.Contains(string(out), token) {
		t.Fatal("token leaked in SSE")
	}
	if !strings.Contains(sseText(sseLines(t, out)), "echo [REDACTED]") {
		t.Fatal("redacted message missing")
	}
	failing := &chunkedReader{err: errors.New("read failed: " + token)}
	_, failed := ChatCompletion(failing, "model", token)
	if failed.Error == nil || failed.Error.Message != "Devin stream read failed" {
		t.Fatalf("read failure %+v", failed.Error)
	}
	if !failing.closed {
		t.Fatal("not closed")
	}
}

func TestPeekClassifiesEarlyRateLimitTrailer(t *testing.T) {
	data := Concat(
		EncodeFrame(VarintField(4, 2), 0),
		EncodeFrame([]byte(`{"error":{"code":"resource_exhausted","message":"Reached message rate limit for this model. Resets in: 3h0m0s"}}`), 2),
	)
	_, perr := Peek(chunks(data, 7), "")
	if perr == nil || perr.Kind != KindRateLimit || perr.Status != 429 {
		t.Fatalf("got %+v", perr)
	}
	if time.Until(perr.ResetsAt) < 2*time.Hour {
		t.Fatalf("resetsAt %v", perr.ResetsAt)
	}
	if !ModelScoped(perr) {
		t.Fatal("expected model-scoped")
	}
}

func TestChatCompletionFoldsAndKeepsExclusiveUsage(t *testing.T) {
	completion, finish := ChatCompletion(chunks(upstreamStream(), 0), "requested-model", "")
	if finish.Usage != (wire.Usage{Input: 11, Output: 7, CacheWrite: 5, CacheRead: 9}) || finish.Model != "actual-model" || finish.Error != nil {
		t.Fatalf("finish %+v", finish)
	}
	if completion["object"] != "chat.completion" || completion["model"] != "requested-model" {
		t.Fatal("envelope")
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatal("finish reason")
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "Hello world" || message["reasoning_content"] != "think" {
		t.Fatalf("message %v", message)
	}
	calls := message["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_weather" || fn["name"] != "weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("call %v", call)
	}
	usage := completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != 25 || usage["completion_tokens"] != 7 || usage["total_tokens"] != 32 {
		t.Fatalf("usage %v", usage)
	}
}

func TestUTF8SplitAcrossFrames(t *testing.T) {
	word := []byte("héllo") // é is two bytes
	data := Concat(
		EncodeFrame(BytesField(3, word[:2]), 0),
		EncodeFrame(BytesField(3, word[2:]), 0),
		EncodeFrame([]byte("{}"), 2),
	)
	completion, _ := ChatCompletion(chunks(data, 0), "m", "")
	msg := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "héllo" {
		t.Fatalf("content %q", msg["content"])
	}
}
