package wire

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// SSEReadResult carries the parsed events plus the unterminated remainder the
// caller must prepend to the next input — the same contract as the TS
// `splitSseEvents` ({ events, rest }).
type SSEReadResult struct {
	Events []Body
	Rest   string
}

// SplitSseEvents parses complete SSE frames (separated by a blank line) out of
// `text`, decoding each `data:` line as JSON. `[DONE]` markers and blank or
// non-JSON data lines are skipped, matching splitSseEvents in src/responses.ts.
func SplitSseEvents(text string) SSEReadResult {
	var events []Body
	rest := strings.ReplaceAll(text, "\r\n", "\n")
	for {
		index := strings.Index(rest, "\n\n")
		if index < 0 {
			break
		}
		block := rest[:index]
		rest = rest[index+2:]
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[5:])
			if len(data) == 0 || data == "[DONE]" {
				continue
			}
			var event Body
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			events = append(events, event)
		}
	}
	return SSEReadResult{Events: events, Rest: rest}
}

// SplitSseFrames is SplitSseEvents that keeps the raw `data:` payloads (no JSON
// decode) so callers can re-serialize or inspect non-object frames like
// `[DONE]`. Each entry is the trimmed data text of one line.
func SplitSseFrames(text string) (datas []string, rest string) {
	rest = strings.ReplaceAll(text, "\r\n", "\n")
	for {
		index := strings.Index(rest, "\n\n")
		if index < 0 {
			break
		}
		block := rest[:index]
		rest = rest[index+2:]
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			datas = append(datas, strings.TrimSpace(line[5:]))
		}
	}
	return datas, rest
}

// SSEData renders a `data:`-only frame — the OpenAI Chat dialect.
func SSEData(payload any) []byte {
	return []byte("data: " + MarshalJSON(payload) + "\n\n")
}

// SSEDone is the OpenAI Chat stream terminator.
var SSEDone = []byte("data: [DONE]\n\n")

// SSEEvent renders an `event: <type>\ndata:` frame — the Anthropic and
// Responses dialects. A payload without a string `type` falls back to
// "message", as the TS emitters do.
func SSEEvent(payload any) []byte {
	typ := "message"
	if m, ok := payload.(map[string]any); ok {
		if t, ok := m["type"].(string); ok && t != "" {
			typ = t
		}
	}
	return []byte("event: " + typ + "\ndata: " + MarshalJSON(payload) + "\n\n")
}

// EventSink is where a Translator writes the SSE frames it produces.
type EventSink interface {
	// Emit appends one serialized SSE frame.
	Emit(frame []byte)
	// EmitData serializes payload as a `data:`-only frame (Chat dialect).
	EmitData(payload any)
	// EmitEvent serializes payload as an `event: <type>` frame
	// (Anthropic / Responses dialect).
	EmitEvent(payload any)
}

// Collector is the in-memory EventSink tests and non-stream callers use.
type Collector struct {
	frames [][]byte
}

var _ EventSink = (*Collector)(nil)

// Emit implements EventSink.
func (c *Collector) Emit(frame []byte) { c.frames = append(c.frames, frame) }

// EmitData implements EventSink.
func (c *Collector) EmitData(payload any) { c.Emit(SSEData(payload)) }

// EmitEvent implements EventSink.
func (c *Collector) EmitEvent(payload any) { c.Emit(SSEEvent(payload)) }

// Bytes concatenates every emitted frame.
func (c *Collector) Bytes() []byte { return bytes.Join(c.frames, nil) }

// String is Bytes as text.
func (c *Collector) String() string { return string(bytes.Join(c.frames, nil)) }

// Translator is a stream bridge: it ingests upstream SSE events (already
// decoded into objects) and emits client-wire frames through the EventSink,
// then flushes any terminal state on Finish. Ported from the TransformStream
// bridge pattern used across the TS wire modules.
type Translator interface {
	// Handle consumes one decoded upstream SSE event.
	Handle(event Body, sink EventSink)
	// Finish flushes terminal frames (finish chunks, [DONE], message_stop).
	Finish(sink EventSink)
}

// TeeTranslator observes events without emitting: for stream side-effects such
// as reasoning passback capture.
type TeeTranslator interface {
	Ingest(event Body)
	Flush()
}

// StreamEvent is the relay vocabulary a bridged stream surfaces so the ledger
// layer can tell "client saw content" from "request went unanswered" after a
// cancel — the port of relay.ts's StreamEvent union.
type StreamEvent struct {
	Kind    StreamEventKind
	Usage   Usage
	Message string
}

// StreamEventKind enumerates what a bridged stream decided matters.
type StreamEventKind int

const (
	// StreamContent is a token the client can render: text, thinking, tool call.
	StreamContent StreamEventKind = iota
	// StreamUsage is a usage report, in whatever framing the upstream wire uses.
	StreamUsage
	// StreamFinish is a clean turn end (finish_reason, message_stop, [DONE]).
	StreamFinish
	// StreamError is an upstream error folded into the stream instead of a status.
	StreamError
)

// StreamTracker accumulates StreamEvents so a caller (ledger) can ask whether
// anything was delivered, what usage was seen, and whether the turn finished —
// the port of relay.ts's trackEvents + cancelOutcome.
type StreamTracker struct {
	mu        sync.Mutex
	delivered bool
	finished  bool
	failure   string
	usage     Usage
}

// Feed records one event.
func (t *StreamTracker) Feed(event StreamEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch event.Kind {
	case StreamContent:
		t.delivered = true
	case StreamUsage:
		t.usage = event.Usage
	case StreamFinish:
		t.delivered = true
		t.finished = true
	case StreamError:
		t.failure = event.Message
	}
}

// Delivered reports whether a cancel now means "finished turn" rather than
// "abandoned request".
func (t *StreamTracker) Delivered() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.delivered }

// Finished reports whether the turn ended cleanly.
func (t *StreamTracker) Finished() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.finished }

// UsageSeen is the latest accounting the stream surfaced.
func (t *StreamTracker) UsageSeen() Usage { t.mu.Lock(); defer t.mu.Unlock(); return t.usage }

// Failure is the upstream error message, when the stream carried one.
func (t *StreamTracker) Failure() string { t.mu.Lock(); defer t.mu.Unlock(); return t.failure }

// CancelOutcome is what the ledger row should say when the client hung up:
// 200 when something was delivered (the turn is already on their screen), 499
// when the request truly went unanswered.
func (t *StreamTracker) CancelOutcome() (status int, usage Usage, errText string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.delivered {
		return 200, t.usage, ""
	}
	return 499, Usage{}, "client canceled"
}

// Usage is the cross-wire token accounting every bridge reports — the same
// shape pricing.ts defines (input/output plus cache reads and writes).
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// translatorSink queues emitted frames into the stream's buffered channel.
type translatorSink struct{ s *TranslatorStream }

func (q translatorSink) Emit(frame []byte)     { q.s.enqueue(frame) }
func (q translatorSink) EmitData(payload any)  { q.s.enqueue(SSEData(payload)) }
func (q translatorSink) EmitEvent(payload any) { q.s.enqueue(SSEEvent(payload)) }

// TranslatorStream adapts a Translator to io.Reader/io.Writer plumbing — the
// Go analogue of `source.pipeThrough(transform)`: upstream bytes go in via
// Write, translated SSE frames come out via Read, and Close runs the
// Translator's terminal flush.
type TranslatorStream struct {
	translator Translator
	tee        TeeTranslator

	buf   strings.Builder
	queue chan []byte
	done  chan struct{}
	once  sync.Once
}

// NewTranslatorStream wires a Translator to a byte stream.
func NewTranslatorStream(t Translator) *TranslatorStream {
	return &TranslatorStream{
		translator: t,
		queue:      make(chan []byte, 256),
		done:       make(chan struct{}),
	}
}

// WithTee attaches an observer that sees every ingested event (no output).
func (s *TranslatorStream) WithTee(tee TeeTranslator) *TranslatorStream {
	s.tee = tee
	return s
}

func (s *TranslatorStream) enqueue(frame []byte) {
	select {
	case s.queue <- frame:
	case <-s.done:
	}
}

// Write ingests upstream bytes, splits complete SSE frames, and feeds each
// decoded event to the Translator. Implements io.Writer.
func (s *TranslatorStream) Write(p []byte) (int, error) {
	select {
	case <-s.done:
		return 0, ErrStreamClosed
	default:
	}
	s.buf.Write(p)
	result := SplitSseEvents(s.buf.String())
	s.buf.Reset()
	s.buf.WriteString(result.Rest)
	s.feed(result.Events)
	return len(p), nil
}

func (s *TranslatorStream) feed(events []Body) {
	sink := translatorSink{s: s}
	for _, event := range events {
		if s.tee != nil {
			s.tee.Ingest(event)
		}
		s.translator.Handle(event, sink)
	}
}

// Close flushes the tail buffer, runs the Translator's Finish, and signals EOF
// to readers.
func (s *TranslatorStream) Close() error {
	s.once.Do(func() {
		if s.buf.Len() > 0 {
			s.buf.WriteString("\n\n")
			result := SplitSseEvents(s.buf.String())
			s.feed(result.Events)
			s.buf.Reset()
		}
		if s.tee != nil {
			s.tee.Flush()
		}
		s.translator.Finish(translatorSink{s: s})
		close(s.done)
		close(s.queue)
	})
	return nil
}

// Read implements io.Reader: drains emitted frames in order, returning io.EOF
// after Close. Each Read returns at most one emitted frame; a frame larger
// than p is split across reads.
type TranslatorStreamReader struct {
	s       *TranslatorStream
	pending []byte
}

// Reader exposes the stream as a plain io.Reader, buffering partial frames.
func (s *TranslatorStream) Reader() io.Reader {
	return &TranslatorStreamReader{s: s}
}

func (r *TranslatorStreamReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		frame, ok := <-r.s.queue
		if !ok {
			return 0, io.EOF
		}
		r.pending = frame
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Read implements io.Reader. Frames larger than p are dropped beyond the first
// p bytes — use Reader when the consumer's buffer size is unknown.
func (s *TranslatorStream) Read(p []byte) (int, error) {
	frame, ok := <-s.queue
	if !ok {
		return 0, io.EOF
	}
	return copy(p, frame), nil
}

// Pipe runs an entire upstream body through a Translator synchronously and
// returns the emitted bytes — the test-friendly form of io.Copy(dst, stream).
func Pipe(t Translator, upstream []byte) []byte {
	s := NewTranslatorStream(t)
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&out, s.Reader())
		close(done)
	}()
	_, _ = s.Write(upstream)
	_ = s.Close()
	<-done
	return out.Bytes()
}
