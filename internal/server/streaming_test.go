package server_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/server"
	"github.com/xinyao27/jevonian/internal/softstream"
)

// droppedBody delivers one delta then fails like a dropped upstream socket.
type droppedBody struct {
	sent bool
}

func (d *droppedBody) Read(p []byte) (int, error) {
	if !d.sent {
		d.sent = true
		return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), nil
	}
	return 0, errors.New("socket hang up")
}

func (d *droppedBody) Close() error { return nil }

func TestClientKindOfPath(t *testing.T) {
	cases := map[string]config.UpstreamWire{
		"/messages":            config.WireAnthropic,
		"/v1/messages":         config.WireAnthropic,
		"/responses":           config.WireResponses,
		"/v1/responses":        config.WireResponses,
		"/chat/completions":    config.WireOpenAI,
		"/v1/chat/completions": config.WireOpenAI,
	}
	for path, want := range cases {
		if got := server.ClientKindOfPath(path); got != want {
			t.Fatalf("ClientKindOfPath(%q) = %q want %q", path, got, want)
		}
	}
}

// A streamed Chat request whose upstream failed before the stream answers with a
// soft assistant turn instead of a JSON 5xx (soft-fallback.test.ts case 1).
func TestWriteSoftErrorAnswersStreamedRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	server.WriteSoftError(rec, config.WireOpenAI, "gpt-test", "upstream is on fire", map[string]string{
		"x-jevonian-provider": "p1",
	})
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("content-type"), "text/event-stream") {
		t.Fatalf("content-type = %q", rec.Header().Get("content-type"))
	}
	if rec.Header().Get("x-jevonian-soft-error") != "1" || rec.Header().Get("x-jevonian-provider") != "p1" {
		t.Fatalf("headers = %v", rec.Header())
	}
	text := rec.Body.String()
	if !strings.Contains(text, softstream.SoftErrorPrefix) || !strings.Contains(text, "upstream is on fire") {
		t.Fatalf("body = %q", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish: %q", text)
	}
	if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
		t.Fatalf("missing DONE: %q", text)
	}
}

// Redaction happens before the reason reaches the client (soft-fallback case 5).
func TestWriteSoftErrorRedactsEchoedCredential(t *testing.T) {
	key := "sk-live-super-secret-credential"
	rec := httptest.NewRecorder()
	reason := softstream.RedactSecrets("Invalid key "+key, key)
	server.WriteSoftError(rec, config.WireOpenAI, "gpt-test", reason, nil)
	if strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// A dropped stream is closed as a soft assistant turn, end to end through an
// httptest server, and the real failure is reported via OnSoft (soft-fallback
// cases 2 + 4).
func TestWriteStreamClosesDroppedStreamSoftly(t *testing.T) {
	var (
		mu     sync.Mutex
		soft   []string
		cancel int
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.WriteStream(w, r, &droppedBody{}, server.StreamOptions{
			Kind:    config.WireOpenAI,
			Model:   "gpt-test",
			Headers: map[string]string{"x-jevonian-provider": "p1"},
			OnSoft: func(reason string) {
				mu.Lock()
				soft = append(soft, reason)
				mu.Unlock()
			},
			OnClientCancel: func() {
				mu.Lock()
				cancel++
				mu.Unlock()
			},
		})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if resp.StatusCode != 200 || resp.Header.Get("x-jevonian-provider") != "p1" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(text, "partial") || !strings.Contains(text, softstream.SoftErrorPrefix) {
		t.Fatalf("body = %q", text)
	}
	if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
		t.Fatalf("missing DONE: %q", text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(soft) != 1 || soft[0] != "" {
		t.Fatalf("onSoft = %v", soft)
	}
}

// A mid-stream `{ error }` frame is replaced, never forwarded (soft-fallback case 3).
func TestWriteStreamReplacesErrorFrame(t *testing.T) {
	var reason string
	body := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
			"data: {\"error\":{\"message\":\"upstream refused\",\"type\":\"server_error\"}}\n\n"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	server.WriteStream(rec, req, body, server.StreamOptions{
		Kind: config.WireOpenAI, Model: "gpt-test",
		OnSoft: func(r string) { reason = r },
	})
	text := rec.Body.String()
	if strings.Contains(text, `"error"`) {
		t.Fatalf("error frame leaked: %q", text)
	}
	if !strings.Contains(text, softstream.SoftErrorPrefix) || !strings.Contains(text, "upstream refused") {
		t.Fatalf("body = %q", text)
	}
	if reason != "upstream refused" {
		t.Fatalf("reason = %q", reason)
	}
}

// A native Responses response.failed becomes a soft completion (soft-fallback last case).
func TestWriteStreamResponsesFailedBecomesCompleted(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"message\":\"backend exploded\"}}}\n\n"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	server.WriteStream(rec, req, body, server.StreamOptions{
		Kind: server.ClientKindOfPath("/v1/responses"), Model: "gpt-6-astra",
	})
	text := rec.Body.String()
	if strings.Contains(text, "response.failed") {
		t.Fatalf("failed frame leaked: %q", text)
	}
	for _, want := range []string{"response.completed", softstream.SoftErrorPrefix, "backend exploded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %q", want, text)
		}
	}
}

// A non-SSE passthrough is forwarded untouched — no soft wrapping.
func TestWriteStreamNonSSEPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	server.WriteStream(rec, req, io.NopCloser(strings.NewReader(`{"ok":true}`)), server.StreamOptions{
		Kind: config.WireOpenAI, Model: "m", ContentType: "application/json",
	})
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// A silent upstream gets keepalive comments, flushed to the client.
func TestWriteStreamEmitsKeepalive(t *testing.T) {
	pr, pw := io.Pipe()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.WriteStream(w, r, pr, server.StreamOptions{
			Kind: config.WireOpenAI, Model: "m", KeepaliveInterval: 20 * time.Millisecond,
		})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	defer pw.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	deadline := time.Now().Add(3 * time.Second)
	var got strings.Builder
	for time.Now().Before(deadline) && !strings.Contains(got.String(), ": keepalive") {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got.String(), ": keepalive\n\n") {
		t.Fatalf("no keepalive: %q", got.String())
	}
}
