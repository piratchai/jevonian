package softstream_test

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/softstream"
)

// chunkSource yields chunks, then optionally fails the way a dropped socket does.
type chunkSource struct {
	chunks    [][]byte
	index     int
	failAfter int // -1 = never
}

func source(chunks [][]byte, failAfter int) io.ReadCloser {
	return &chunkSource{chunks: chunks, failAfter: failAfter}
}

func (s *chunkSource) Read(p []byte) (int, error) {
	if s.failAfter >= 0 && s.index == s.failAfter {
		return 0, errors.New("socket hang up")
	}
	if s.index >= len(s.chunks) {
		return 0, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return copy(p, chunk), nil
}

func (s *chunkSource) Close() error { return nil }

// wedgeSource delivers chunk once, then blocks on Read until Close is called —
// a wedged upstream socket.
type wedgeSource struct {
	chunk   []byte
	sent    bool
	release chan struct{}
	once    bool
}

func (s *wedgeSource) Read(p []byte) (int, error) {
	if s.release == nil {
		s.release = make(chan struct{})
	}
	if !s.sent {
		s.sent = true
		return copy(p, s.chunk), nil
	}
	<-s.release
	return 0, io.EOF
}

func (s *wedgeSource) Close() error {
	if s.release != nil && !s.once {
		s.once = true
		close(s.release)
	}
	return nil
}

// blockingSource hangs on Read until released (for cancel tests).
type blockingSource struct {
	release  chan struct{}
	released bool
}

func (s *blockingSource) Read(p []byte) (int, error) {
	if s.released {
		return 0, io.EOF
	}
	<-s.release
	s.released = true
	return 0, io.EOF
}

func (s *blockingSource) Close() error { return nil }

func readText(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

func drainWithDeadline(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(r)
		ch <- result{string(raw), err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read: %v", res.err)
		}
		return res.s
	case <-time.After(5 * time.Second):
		t.Fatal("drain timed out")
		return ""
	}
}

func TestSoftErrorMessage(t *testing.T) {
	t.Run("names Jevonian and asks for a retry", func(t *testing.T) {
		msg := softstream.SoftErrorMessage("upstream refused")
		if !strings.HasPrefix(msg, softstream.SoftErrorPrefix) {
			t.Fatalf("message = %q", msg)
		}
		if !strings.Contains(msg, "upstream refused") || !strings.Contains(msg, "retry") {
			t.Fatalf("message = %q", msg)
		}
	})

	t.Run("collapses whitespace and caps the reason", func(t *testing.T) {
		msg := softstream.SoftErrorMessage("a\n\n  b" + strings.Repeat("x", 500))
		if !strings.Contains(msg, "a b") {
			t.Fatalf("message = %q", msg)
		}
		if len(msg) >= len(softstream.SoftErrorPrefix)+350 {
			t.Fatalf("message too long: %d", len(msg))
		}
	})

	t.Run("handles an empty reason without a dangling colon", func(t *testing.T) {
		want := softstream.SoftErrorPrefix + ". The turn was stopped safely — please retry."
		if got := softstream.SoftErrorMessage("   "); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

func TestRedactSecrets(t *testing.T) {
	key := "sk-live-super-secret-credential"
	text := "Invalid key " + key + " provided"
	out := softstream.RedactSecrets(text, key)
	if strings.Contains(out, key) {
		t.Fatalf("secret echoed: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %q", out)
	}

	// JSON-escaped variant is covered too.
	escaped := `Invalid key sk-live-super-secret-credential \"quoted\"`
	_ = escaped
	raw := `Invalid key sk-live-super\"quoted\"-credential`
	out = softstream.RedactSecrets(raw, `sk-live-super"quoted"-credential`)
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("escaped variant not redacted: %q", out)
	}

	// Short values are ignored so a common substring cannot become [REDACTED].
	if got := softstream.RedactSecrets("the cat sat", "cat"); got != "the cat sat" {
		t.Fatalf("short secret redacted: %q", got)
	}
}

func TestSoftCompletionStream(t *testing.T) {
	t.Run("closes a chat wire with a normal assistant turn", func(t *testing.T) {
		text := string(softstream.SoftCompletionBytes(config.WireOpenAI, "m", "boom"))
		for _, want := range []string{`"role":"assistant"`, `"content":"boom"`, `"finish_reason":"stop"`} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %q", want, text)
			}
		}
		if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
			t.Fatalf("missing DONE terminator: %q", text)
		}
	})

	t.Run("closes an anthropic wire with message_stop", func(t *testing.T) {
		text := string(softstream.SoftCompletionBytes(config.WireAnthropic, "m", "boom"))
		for _, want := range []string{"event: message_start", "event: message_stop", `"stop_reason":"end_turn"`} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %q", want, text)
			}
		}
		if strings.Contains(text, "event: error") {
			t.Fatalf("error event leaked: %q", text)
		}
	})

	t.Run("closes a responses wire with response.completed", func(t *testing.T) {
		text := string(softstream.SoftCompletionBytes(config.WireResponses, "m", "boom"))
		for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %q", want, text)
			}
		}
		if strings.Contains(text, "event: response.failed") {
			t.Fatalf("failed event leaked: %q", text)
		}
	})
}

func TestWrap(t *testing.T) {
	wrap := func(src io.ReadCloser, kind config.UpstreamWire, opts softstream.SoftOptions) io.ReadCloser {
		return softstream.Wrap(context.Background(), src, kind, "m", opts)
	}

	t.Run("appends a soft close when the upstream dies mid-stream", func(t *testing.T) {
		softCalls := 0
		stream := wrap(
			source([][]byte{[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")}, 1),
			config.WireOpenAI,
			softstream.SoftOptions{
				Message: softstream.SoftErrorMessage("the upstream stream ended unexpectedly"),
				OnSoft:  func(string) { softCalls++ },
			},
		)
		text := readText(t, stream)
		if !strings.Contains(text, `"content":"hi"`) {
			t.Fatalf("missing forwarded delta: %q", text)
		}
		if !strings.Contains(text, softstream.SoftErrorPrefix) {
			t.Fatalf("missing soft message: %q", text)
		}
		if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
			t.Fatalf("missing DONE: %q", text)
		}
		if softCalls != 1 {
			t.Fatalf("onSoft calls = %d", softCalls)
		}
	})

	t.Run("appends a soft close when the upstream ends without a finish marker", func(t *testing.T) {
		stream := wrap(
			source([][]byte{[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")}, -1),
			config.WireOpenAI,
			softstream.SoftOptions{Message: softstream.SoftErrorMessage("ended early")},
		)
		text := readText(t, stream)
		if !strings.Contains(text, `"content":"hi"`) || !strings.Contains(text, softstream.SoftErrorPrefix) {
			t.Fatalf("unexpected output: %q", text)
		}
	})

	t.Run("passes a normally finished stream through untouched", func(t *testing.T) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"
		softCalls := 0
		stream := wrap(source([][]byte{[]byte(body)}, -1), config.WireOpenAI,
			softstream.SoftOptions{Message: "unused", OnSoft: func(string) { softCalls++ }})
		text := readText(t, stream)
		if text != body {
			t.Fatalf("body mutated:\ngot  %q\nwant %q", text, body)
		}
		if softCalls != 0 {
			t.Fatalf("onSoft called %d times", softCalls)
		}
	})

	t.Run("recognises a finish marker split across two chunks", func(t *testing.T) {
		softCalls := 0
		stream := wrap(
			source([][]byte{
				[]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"st"),
				[]byte("op\"}]}\n\ndata: [DONE]\n\n"),
			}, -1),
			config.WireOpenAI,
			softstream.SoftOptions{Message: "unused", OnSoft: func(string) { softCalls++ }},
		)
		_ = readText(t, stream)
		if softCalls != 0 {
			t.Fatalf("onSoft called %d times", softCalls)
		}
	})

	t.Run("stops answering after the client hangs up", func(t *testing.T) {
		softCalls := 0
		ctx, cancel := context.WithCancel(context.Background())
		// A source that delivers one event then wedges: the producer is parked in
		// Read when the hang-up lands, so the cancel must be what ends the turn.
		src := &wedgeSource{chunk: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")}
		stream := softstream.Wrap(ctx, src,
			config.WireOpenAI, "m",
			softstream.SoftOptions{Message: "unused", OnSoft: func(string) { softCalls++ }})
		buf := make([]byte, 64)
		_, _ = stream.Read(buf)
		cancel()
		_ = stream.Close()
		// Drain attempt must not panic and must end quickly.
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatal("reads after cancel did not terminate")
			default:
			}
			_, err := stream.Read(buf)
			if err == io.EOF {
				break
			}
		}
		if softCalls != 0 {
			t.Fatalf("onSoft called %d times after cancel", softCalls)
		}
	})

	t.Run("swallows an openai error frame and closes softly with its reason", func(t *testing.T) {
		var reason string
		stream := wrap(
			source([][]byte{
				[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
				[]byte("data: {\"error\":{\"message\":\"upstream refused\",\"type\":\"server_error\"}}\n\n"),
			}, -1),
			config.WireOpenAI,
			softstream.SoftOptions{
				Message: softstream.SoftErrorMessage("stream ended unexpectedly"),
				OnSoft:  func(r string) { reason = r },
			},
		)
		text := readText(t, stream)
		if !strings.Contains(text, "partial") {
			t.Fatalf("missing partial: %q", text)
		}
		if strings.Contains(text, `"error"`) {
			t.Fatalf("error frame leaked: %q", text)
		}
		if !strings.Contains(text, softstream.SoftErrorPrefix) || !strings.Contains(text, "upstream refused") {
			t.Fatalf("soft close missing reason: %q", text)
		}
		if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
			t.Fatalf("missing DONE: %q", text)
		}
		if reason != "upstream refused" {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("swallows an anthropic error event and closes softly", func(t *testing.T) {
		var reason string
		stream := wrap(
			source([][]byte{
				[]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"),
			}, -1),
			config.WireAnthropic,
			softstream.SoftOptions{
				Message: softstream.SoftErrorMessage("stream ended unexpectedly"),
				OnSoft:  func(r string) { reason = r },
			},
		)
		text := readText(t, stream)
		if strings.Contains(text, `"type":"error"`) {
			t.Fatalf("error event leaked: %q", text)
		}
		if !strings.Contains(text, "event: message_stop") || !strings.Contains(text, "Overloaded") {
			t.Fatalf("bad soft close: %q", text)
		}
		if reason != "Overloaded" {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("swallows a responses response.failed event and completes the turn", func(t *testing.T) {
		var reason string
		stream := wrap(
			source([][]byte{
				[]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"nope\"}}}\n\n"),
			}, -1),
			config.WireResponses,
			softstream.SoftOptions{
				Message: softstream.SoftErrorMessage("stream ended unexpectedly"),
				OnSoft:  func(r string) { reason = r },
			},
		)
		text := readText(t, stream)
		if strings.Contains(text, "response.failed") {
			t.Fatalf("failed frame leaked: %q", text)
		}
		if !strings.Contains(text, "event: response.completed") || !strings.Contains(text, "nope") {
			t.Fatalf("bad soft close: %q", text)
		}
		if reason != "nope" {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("continues an anthropic stream at the next free block index", func(t *testing.T) {
		stream := wrap(
			source([][]byte{
				[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
				[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"),
			}, -1),
			config.WireAnthropic,
			softstream.SoftOptions{Message: "SOFT"},
		)
		text := readText(t, stream)
		re := regexp.MustCompile(`"type":"content_block_start".*?"index":(\d+)|"index":(\d+),"type":"content_block_start"`)
		matches := re.FindAllStringSubmatch(text, -1)
		got := make([]string, 0, len(matches))
		for _, m := range matches {
			if m[1] != "" {
				got = append(got, m[1])
			} else {
				got = append(got, m[2])
			}
		}
		if len(got) != 2 || got[0] != "0" || got[1] != "1" {
			t.Fatalf("content_block_start indexes = %v", got)
		}
		// The block left open by the failure is closed before the soft block opens.
		stopIdx := strings.Index(text, `"type":"content_block_stop"`)
		secondStart := strings.LastIndex(text, `"type":"content_block_start"`)
		if stopIdx < 0 || secondStart < 0 || secondStart < stopIdx {
			t.Fatalf("open block not closed first: %q", text)
		}
		if !strings.Contains(text, "event: message_stop") {
			t.Fatalf("missing message_stop: %q", text)
		}
	})

	t.Run("opens a fresh responses item at the next output index", func(t *testing.T) {
		stream := wrap(
			source([][]byte{
				[]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"c1\",\"name\":\"read\",\"arguments\":\"\"}}\n\n"),
				[]byte("event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"{}\"}\n\n"),
			}, -1),
			config.WireResponses,
			softstream.SoftOptions{Message: "SOFT"},
		)
		text := readText(t, stream)
		re := regexp.MustCompile(`"type":"response\.output_item\.added".*?"output_index":(\d+)|"output_index":(\d+).*?"type":"response\.output_item\.added"`)
		matches := re.FindAllStringSubmatch(text, -1)
		got := make([]string, 0, len(matches))
		for _, m := range matches {
			if m[1] != "" {
				got = append(got, m[1])
			} else {
				got = append(got, m[2])
			}
		}
		if len(got) != 2 || got[0] != "0" || got[1] != "1" {
			t.Fatalf("output_item.added indexes = %v", got)
		}
		if !strings.Contains(text, `"type":"response.function_call_arguments.done"`) {
			t.Fatalf("open function_call not terminated: %q", text)
		}
		if !strings.Contains(text, `"type":"response.completed"`) {
			t.Fatalf("missing response.completed: %q", text)
		}
	})

	t.Run("does not append a second answer when a finish sits past a 64-byte carry", func(t *testing.T) {
		softCalls := 0
		stream := wrap(
			source([][]byte{
				[]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"),
				[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 200) + "\"}}]}\n\n"),
			}, -1),
			config.WireOpenAI,
			softstream.SoftOptions{Message: "unused", OnSoft: func(string) { softCalls++ }},
		)
		text := readText(t, stream)
		if strings.Contains(text, softstream.SoftErrorPrefix) {
			t.Fatalf("second answer appended: %q", text)
		}
		if softCalls != 0 {
			t.Fatalf("onSoft called %d times", softCalls)
		}
	})

	t.Run("does not append a second answer after a [DONE] sentinel", func(t *testing.T) {
		softCalls := 0
		stream := wrap(
			source([][]byte{
				[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"),
				[]byte("data: [DONE]\n\n"),
			}, -1),
			config.WireOpenAI,
			softstream.SoftOptions{Message: "unused", OnSoft: func(string) { softCalls++ }},
		)
		text := readText(t, stream)
		if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
			t.Fatalf("missing DONE: %q", text)
		}
		if softCalls != 0 {
			t.Fatalf("onSoft called %d times", softCalls)
		}
	})

	t.Run("does not enqueue after a cancel races an in-flight read", func(t *testing.T) {
		release := make(chan struct{})
		src := &blockingSource{release: release}
		softCalls := 0
		ctx, cancel := context.WithCancel(context.Background())
		stream := softstream.Wrap(ctx, src, config.WireOpenAI, "m",
			softstream.SoftOptions{Message: "unused", OnSoft: func(string) { softCalls++ }})
		// The cancel happens while the upstream read is still pending.
		cancel()
		_ = stream.Close()
		close(release)
		if softCalls != 0 {
			t.Fatalf("onSoft called %d times after cancel", softCalls)
		}
	})

	t.Run("nil source still answers the turn", func(t *testing.T) {
		stream := wrap(nil, config.WireOpenAI,
			softstream.SoftOptions{Message: softstream.SoftErrorMessage("no upstream")})
		text := readText(t, stream)
		if !strings.Contains(text, softstream.SoftErrorPrefix) {
			t.Fatalf("missing soft body: %q", text)
		}
	})
}
