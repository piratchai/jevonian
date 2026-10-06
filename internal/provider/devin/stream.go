package devin

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/xinyao27/jevonian/internal/wire"
)

// ─── response frames ────────────────────────────────────────────────────────

type connectFrame struct {
	flags   byte
	payload []byte
}

// frameSplitter is an incremental Connect envelope splitter; it tolerates
// frames split across arbitrary chunks.
type frameSplitter struct {
	buffer []byte
}

func (s *frameSplitter) push(chunk []byte) ([]connectFrame, error) {
	s.buffer = append(s.buffer, chunk...)
	var frames []connectFrame
	offset := 0
	for len(s.buffer)-offset >= 5 {
		flags := s.buffer[offset]
		length := binary.BigEndian.Uint32(s.buffer[offset+1 : offset+5])
		if length > maxFrameBytes {
			return frames, fmt.Errorf("devin: frame of %d bytes exceeds limit", length)
		}
		if len(s.buffer)-offset < 5+int(length) {
			break
		}
		payload := append([]byte(nil), s.buffer[offset+5:offset+5+int(length)]...)
		frames = append(frames, connectFrame{flags: flags, payload: payload})
		offset += 5 + int(length)
	}
	if offset > 0 {
		s.buffer = append([]byte(nil), s.buffer[offset:]...)
	}
	return frames, nil
}

func framePayload(frame connectFrame) ([]byte, error) {
	if frame.flags&flagGzip == 0 {
		return frame.payload, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(frame.payload))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, maxFrameBytes))
}

// trailerError reads the end-stream trailer JSON: `{}` on success,
// `{"error":{...}}` on failure.
func trailerError(payload []byte, token string) *StreamError {
	text := strings.TrimSpace(string(payload))
	if text == "" || text == "{}" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil
	}
	if _, ok := parsed["error"]; !ok {
		return nil
	}
	return ClassifyError(0, text, token)
}

type toolCallDelta struct {
	id   string
	name string
	args []byte
}

type usageDelta struct {
	input, output, cacheWrite, cacheRead *uint64
	model                                string
}

type frameDelta struct {
	text      []byte
	thinking  []byte
	stop      *uint64
	toolCalls []toolCallDelta
	usage     *usageDelta
}

func optInt(fields []pbField, num int) *uint64 {
	if v, ok := pbInt(fields, num); ok {
		return &v
	}
	return nil
}

func decodeFrame(payload []byte) *frameDelta {
	fields := tryDecodePb(payload)
	if fields == nil && len(payload) > 0 {
		return nil
	}
	delta := &frameDelta{}
	for _, f := range fields {
		switch {
		case f.wire == 2 && f.num == 3:
			delta.text = append(delta.text, f.bytes...)
		case f.wire == 2 && f.num == 9:
			delta.thinking = append(delta.thinking, f.bytes...)
		case f.wire == 2 && f.num == 6:
			if call := tryDecodePb(f.bytes); call != nil || len(f.bytes) == 0 {
				id, _ := pbString(call, 1)
				name, _ := pbString(call, 2)
				delta.toolCalls = append(delta.toolCalls, toolCallDelta{id: id, name: name, args: pbBytes(call, 3)})
			}
		case f.wire == 2 && f.num == 7:
			if usage := tryDecodePb(f.bytes); usage != nil || len(f.bytes) == 0 {
				model, _ := pbString(usage, 9)
				delta.usage = &usageDelta{
					input:      optInt(usage, 2),
					output:     optInt(usage, 3),
					cacheWrite: optInt(usage, 4),
					cacheRead:  optInt(usage, 5),
					model:      model,
				}
			}
		case f.wire == 0 && f.num == 5:
			v := f.int
			delta.stop = &v
		}
	}
	return delta
}

// frameHasContent reports whether a data frame commits visible output.
func frameHasContent(payload []byte) bool {
	d := decodeFrame(payload)
	return d != nil && (len(d.text) > 0 || len(d.thinking) > 0 || len(d.toolCalls) > 0)
}

// ─── stream assembly ────────────────────────────────────────────────────────

// Finish is how a Devin stream ended: exclusive usage (Anthropic-style
// accounting: input excludes cache reads/writes), the model the server
// actually ran, and the classified error when it failed.
type Finish struct {
	Usage wire.Usage
	Model string
	Error *StreamError
}

type call struct {
	index   int
	id      string
	name    string
	args    string
	decoder utf8Stream
}

type eventKind int

const (
	evText eventKind = iota
	evThinking
	evCall
)

type event struct {
	kind  eventKind
	text  string
	call  *call
	start bool
	name  *string
	args  string
}

// stopReasons maps top-level #5; only 2/4 (clean stop) and 10 (tool calls)
// are live-calibrated.
var stopReasons = map[uint64]string{2: "stop", 4: "stop", 10: "tool_calls"}

func truncatedError() *StreamError {
	return &StreamError{Status: 502, Kind: KindInternal, Message: "Devin stream ended without an end-of-stream trailer"}
}

// streamState folds Connect frames into text/reasoning/tool-call events plus
// terminal usage and status.
type streamState struct {
	token    string
	calls    []*call
	usage    wire.Usage
	model    string
	stop     *uint64
	err      *StreamError
	ended    bool
	splitter frameSplitter
	text     utf8Stream
	thinking utf8Stream
}

func (s *streamState) push(chunk []byte) []event {
	if s.ended || s.err != nil {
		return nil
	}
	var events []event
	frames, splitErr := s.splitter.push(chunk)
	for _, frame := range frames {
		payload, err := framePayload(frame)
		if err != nil {
			s.err = &StreamError{Status: 502, Kind: KindOther, Message: "Devin stream decode failed"}
			return events
		}
		if frame.flags&flagEndStream != 0 {
			s.ended = true
			s.err = trailerError(payload, s.token)
			break
		}
		if delta := decodeFrame(payload); delta != nil {
			s.apply(delta, &events)
		}
	}
	if splitErr != nil && !s.ended {
		s.err = &StreamError{Status: 502, Kind: KindOther, Message: "Devin stream decode failed"}
	}
	return events
}

// finish flushes split UTF-8 tails and records truncation when no trailer arrived.
func (s *streamState) finish() []event {
	var events []event
	if t := s.thinking.flush(); t != "" {
		events = append(events, event{kind: evThinking, text: t})
	}
	if t := s.text.flush(); t != "" {
		events = append(events, event{kind: evText, text: t})
	}
	for _, c := range s.calls {
		if tail := c.decoder.flush(); tail != "" {
			c.args += tail
			events = append(events, event{kind: evCall, call: c, args: tail})
		}
	}
	if !s.ended && s.err == nil {
		s.err = truncatedError()
	}
	return events
}

func (s *streamState) finishReason() string {
	if len(s.calls) > 0 {
		return "tool_calls"
	}
	if s.stop != nil {
		if reason, ok := stopReasons[*s.stop]; ok {
			return reason
		}
	}
	return "stop"
}

func (s *streamState) result() Finish {
	return Finish{Usage: s.usage, Model: s.model, Error: s.err}
}

func (s *streamState) apply(delta *frameDelta, events *[]event) {
	if delta.thinking != nil {
		if t := s.thinking.decode(delta.thinking); t != "" {
			*events = append(*events, event{kind: evThinking, text: t})
		}
	}
	if delta.text != nil {
		if t := s.text.decode(delta.text); t != "" {
			*events = append(*events, event{kind: evText, text: t})
		}
	}
	for _, part := range delta.toolCalls {
		s.applyCall(part, events)
	}
	if delta.stop != nil {
		s.stop = delta.stop
	}
	if u := delta.usage; u != nil {
		if u.input != nil {
			s.usage.Input = int(*u.input)
		}
		if u.output != nil {
			s.usage.Output = int(*u.output)
		}
		if u.cacheWrite != nil {
			s.usage.CacheWrite = int(*u.cacheWrite)
		}
		if u.cacheRead != nil {
			s.usage.CacheRead = int(*u.cacheRead)
		}
		if u.model != "" {
			s.model = u.model
		}
	}
}

// applyCall: a new id opens a call; an empty (or repeated) id continues the
// matching call.
func (s *streamState) applyCall(part toolCallDelta, events *[]event) {
	var c *call
	if part.id != "" {
		for _, existing := range s.calls {
			if existing.id == part.id {
				c = existing
				break
			}
		}
	} else if n := len(s.calls); n > 0 {
		c = s.calls[n-1]
	}
	start := false
	if c == nil {
		id := part.id
		if id == "" {
			id = newCallID()
		}
		c = &call{index: len(s.calls), id: id, name: part.name}
		s.calls = append(s.calls, c)
		start = true
	}
	var name *string
	if !start && part.name != "" && c.name == "" {
		c.name = part.name
		n := part.name
		name = &n
	}
	args := ""
	if len(part.args) > 0 {
		args = c.decoder.decode(part.args)
	}
	c.args += args
	if start || name != nil || args != "" {
		*events = append(*events, event{kind: evCall, call: c, start: start, name: name, args: args})
	}
}

// OpenAIUsage renders exclusive usage as an OpenAI usage object (inclusive prompt count).
func OpenAIUsage(u wire.Usage) map[string]any {
	prompt := u.Input + u.CacheWrite + u.CacheRead
	return map[string]any{
		"prompt_tokens":         prompt,
		"completion_tokens":     u.Output,
		"total_tokens":          prompt + u.Output,
		"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheRead},
	}
}

func completionID() string {
	return "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
}

func streamedCallDelta(e event) map[string]any {
	if e.start {
		return map[string]any{
			"index":    e.call.index,
			"id":       e.call.id,
			"type":     "function",
			"function": map[string]any{"name": e.call.name, "arguments": e.args},
		}
	}
	fn := map[string]any{"arguments": e.args}
	if e.name != nil {
		fn["name"] = *e.name
	}
	return map[string]any{"index": e.call.index, "function": fn}
}

// StreamOptions tune ToChatStream.
type StreamOptions struct {
	// Token is the local Devin token, scrubbed from any surfaced error text.
	Token string
	// OnFinish runs exactly once: on clean end, error, or cancellation (Close).
	OnFinish func(Finish)
	// OnEvent reports content/usage so the ledger can tell a delivered turn from
	// an abandoned one after a client hang-up.
	OnEvent func(wire.StreamEvent)
}

// ToChatStream translates a Devin Connect body into OpenAI
// `chat.completion.chunk` SSE. It emits the role chunk first, then content /
// reasoning_content / tool_calls deltas, and a final chunk with finish_reason
// and usage, then `[DONE]`. A trailer error (or truncation) closes the turn as
// a readable soft-error assistant message instead of a bare error frame; the
// real error still reaches OnFinish.
//
// The returned reader owns body: Close cancels the upstream read and reports
// "Devin stream aborted" when the stream had not finished.
func ToChatStream(model string, body io.ReadCloser, opts StreamOptions) io.ReadCloser {
	s := &chatStream{
		body:    body,
		model:   model,
		id:      completionID(),
		created: time.Now().Unix(),
		state:   &streamState{token: opts.Token},
		opts:    opts,
	}
	s.emit(s.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	return s
}

type chatStream struct {
	body    io.ReadCloser
	model   string
	id      string
	created int64
	state   *streamState
	opts    StreamOptions

	out      bytes.Buffer
	done     bool
	closed   bool
	reported sync.Once
	readBuf  [32 * 1024]byte
}

func (s *chatStream) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func (s *chatStream) emit(payload map[string]any) {
	s.out.WriteString("data: ")
	s.out.WriteString(jsonString(payload))
	s.out.WriteString("\n\n")
}

func (s *chatStream) emitEvents(events []event) {
	for _, e := range events {
		if s.opts.OnEvent != nil {
			s.opts.OnEvent(wire.StreamEvent{Kind: wire.StreamContent})
		}
		switch e.kind {
		case evText:
			s.emit(s.chunk(map[string]any{"content": e.text}, nil))
		case evThinking:
			s.emit(s.chunk(map[string]any{"reasoning_content": e.text}, nil))
		default:
			s.emit(s.chunk(map[string]any{"tool_calls": []any{streamedCallDelta(e)}}, nil))
		}
	}
}

func (s *chatStream) report() {
	s.reported.Do(func() {
		if s.opts.OnFinish != nil {
			s.opts.OnFinish(s.state.result())
		}
	})
}

func (s *chatStream) flush() {
	s.emitEvents(s.state.finish())
	usage := OpenAIUsage(s.state.usage)
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(wire.StreamEvent{Kind: wire.StreamUsage, Usage: s.state.usage})
	}
	if s.state.err != nil {
		// A mid-stream refusal is Jevonian's problem, not the model's: close the
		// turn with a readable assistant message so the harness keeps the
		// conversation. The real error still reaches the ledger through report().
		s.emit(s.chunk(map[string]any{"content": wire.SoftErrorMessage(s.state.err.Message)}, nil))
		final := s.chunk(map[string]any{}, "stop")
		final["usage"] = usage
		s.emit(final)
	} else {
		final := s.chunk(map[string]any{}, s.state.finishReason())
		final["usage"] = usage
		s.emit(final)
	}
	s.out.Write(wire.SSEDone)
	s.done = true
	s.report()
}

func (s *chatStream) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		if s.done || s.closed {
			return 0, io.EOF
		}
		n, err := s.body.Read(s.readBuf[:])
		if n > 0 {
			s.emitEvents(s.state.push(s.readBuf[:n]))
			// The trailer (or a decode failure) decides the turn; do not wait for
			// an upstream that may hold the socket open after it.
			if s.state.ended || s.state.err != nil {
				s.flush()
				continue
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && s.state.err == nil && !s.state.ended {
				s.state.err = &StreamError{Status: 502, Kind: KindInternal, Message: "Devin stream read failed"}
			}
			s.flush()
		}
	}
	return s.out.Read(p)
}

func (s *chatStream) Close() error {
	if !s.closed {
		s.closed = true
		if !s.done && s.state.err == nil {
			s.state.err = &StreamError{Status: 502, Kind: KindInternal, Message: "Devin stream aborted"}
		}
		s.report()
	}
	return s.body.Close()
}

// ChatCompletion folds a full Devin stream into a non-streaming
// `chat.completion`. A trailer error or truncation is reported on
// Finish.Error; the completion then holds whatever partial output arrived.
// body is read to the end-stream trailer and closed.
func ChatCompletion(body io.ReadCloser, model, token string) (wire.Body, Finish) {
	state := &streamState{token: token}
	var content, reasoning strings.Builder
	collect := func(events []event) {
		for _, e := range events {
			switch e.kind {
			case evText:
				content.WriteString(e.text)
			case evThinking:
				reasoning.WriteString(e.text)
			}
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			collect(state.push(buf[:n]))
			if state.ended || state.err != nil {
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && state.err == nil {
				state.err = &StreamError{Status: 502, Kind: KindInternal, Message: "Devin stream read failed"}
			}
			break
		}
	}
	_ = body.Close()
	collect(state.finish())

	message := map[string]any{"role": "assistant", "content": nil}
	if content.Len() > 0 {
		message["content"] = content.String()
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(state.calls) > 0 {
		calls := make([]any, 0, len(state.calls))
		for _, c := range state.calls {
			calls = append(calls, map[string]any{
				"id":       c.id,
				"type":     "function",
				"function": map[string]any{"name": c.name, "arguments": c.args},
			})
		}
		message["tool_calls"] = calls
	}
	completion := wire.Body{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": state.finishReason()}},
		"usage":   OpenAIUsage(state.usage),
	}
	return completion, state.result()
}

// Peek reads until the first content-bearing data frame (text, reasoning, or
// tool call) or the end-stream trailer. An error trailer, a malformed frame,
// a read failure, or a truncated stream before any content returns the
// classified error and closes body; otherwise it returns a reader that replays
// every byte read so far followed by the rest of body. This is what lets the
// caller fail over on a leading refusal before committing to the client.
func Peek(body io.ReadCloser, token string) (io.ReadCloser, *StreamError) {
	var seen bytes.Buffer
	var splitter frameSplitter
	buf := make([]byte, 32*1024)
	fail := func(e *StreamError) (io.ReadCloser, *StreamError) {
		_ = body.Close()
		return nil, e
	}
	for {
		n, err := body.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			frames, splitErr := splitter.push(buf[:n])
			for _, frame := range frames {
				payload, perr := framePayload(frame)
				if perr != nil {
					return fail(&StreamError{Status: 502, Kind: KindOther, Message: "Devin frame decompression failed"})
				}
				if frame.flags&flagEndStream != 0 {
					if e := trailerError(payload, token); e != nil {
						return fail(e)
					}
					return replay(seen.Bytes(), body), nil
				}
				if frameHasContent(payload) {
					return replay(seen.Bytes(), body), nil
				}
			}
			if splitErr != nil {
				return fail(&StreamError{Status: 502, Kind: KindOther, Message: "Devin stream decode failed"})
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// No content and no trailer is a truncated, unusable stream.
				return fail(truncatedError())
			}
			return fail(&StreamError{Status: 502, Kind: KindOther, Message: "Devin stream read failed"})
		}
	}
}

func replay(prefix []byte, rest io.ReadCloser) io.ReadCloser {
	return &replayReader{prefix: append([]byte(nil), prefix...), rest: rest}
}

type replayReader struct {
	prefix []byte
	rest   io.ReadCloser
}

// errReadFailed hides the upstream read error (which can echo credentials).
var errReadFailed = errors.New("Devin stream read failed")

func (r *replayReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	n, err := r.rest.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, errReadFailed
	}
	return n, err
}

func (r *replayReader) Close() error { return r.rest.Close() }
