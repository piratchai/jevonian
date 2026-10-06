package cursor

import (
	"bytes"
	"io"
	"sync"

	"github.com/xinyao27/jevonian/internal/wire"
)

// StreamOptions carries the stream callbacks; see wire.StreamEvent for the
// vocabulary OnEvent receives.
type StreamOptions struct {
	OnFinish func(Finish)
	OnEvent  func(wire.StreamEvent)
}

// ToChatStream adapts a Run's events to OpenAI `chat.completion.chunk` SSE.
// Emits the role chunk first, then incremental content / reasoning_content /
// tool_calls deltas, and a final chunk carrying finish_reason and usage.
// Late refusals become safe assistant text; Run detects leading refusals before
// returning a live stream. Port of cursorToChatStream.
func ToChatStream(model string, stream *EventStream, opts StreamOptions) io.ReadCloser {
	return &chatStream{model: model, stream: stream, opts: opts}
}

type chatStream struct {
	model  string
	stream *EventStream
	opts   StreamOptions

	// Only readers take readMu. Close must never wait behind a blocked Next.
	readMu    sync.Mutex
	mu        sync.Mutex
	buf       bytes.Buffer
	started   bool
	done      bool
	closed    bool
	id        string
	created   int64
	usage     wire.Usage
	toolCalls bool
	toolIndex int
	streamErr *StreamError
	reported  bool
}

// chunk, emit, and finishLocked are called with s.mu held.
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
	s.buf.Write(wire.SSEData(payload))
}

// finishLocked snapshots the final state exactly once. Callbacks run outside
// s.mu so they may close the stream without re-entering the same lock.
func (s *chatStream) finishLocked() *Finish {
	if s.reported {
		return nil
	}
	s.reported = true
	return &Finish{Usage: s.usage, ToolCalls: s.toolCalls, Error: s.streamErr}
}

func (s *chatStream) report(finish *Finish) {
	if finish != nil && s.opts.OnFinish != nil {
		s.opts.OnFinish(*finish)
	}
}

// fill advances only one batch, rather than draining the entire Run. Waiting
// for upstream bytes never holds the state mutex needed by Close.
func (s *chatStream) fill() {
	events, err := s.stream.Next()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	var callbacks []wire.StreamEvent
	terminal := err != nil
process:
	for _, event := range events {
		switch event.Type {
		case EventText, EventThinking, EventTool:
			callbacks = append(callbacks, wire.StreamEvent{Kind: wire.StreamContent})
		}
		switch event.Type {
		case EventText:
			s.emit(s.chunk(map[string]any{"content": event.Text}, nil))
		case EventThinking:
			s.emit(s.chunk(map[string]any{"reasoning_content": event.Text}, nil))
		case EventTool:
			s.toolCalls = true
			s.emit(s.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    s.toolIndex,
					"id":       event.ID,
					"type":     "function",
					"function": map[string]any{"name": event.Name, "arguments": event.Args},
				}},
			}, nil))
			s.toolIndex++
		case EventUsage:
			s.usage = event.Usage
			callbacks = append(callbacks, wire.StreamEvent{Kind: wire.StreamUsage, Usage: event.Usage})
		case EventError:
			s.streamErr = event.Error
			if s.streamErr != nil {
				// Preserve partial output and let the harness keep the turn.
				s.emit(s.chunk(map[string]any{"content": wire.SoftErrorMessage(s.streamErr.Message)}, nil))
			}
			terminal = true
			break process
		case EventStop:
			s.toolCalls = s.toolCalls || event.ToolCalls
			terminal = true
			break process
		}
	}
	if err != nil && err != io.EOF && s.streamErr == nil {
		s.streamErr = &StreamError{Status: 502, Kind: KindOther, Message: err.Error()}
		s.emit(s.chunk(map[string]any{"content": wire.SoftErrorMessage(s.streamErr.Message)}, nil))
	}
	var finish *Finish
	if terminal {
		s.done = true
		finishReason := "stop"
		if s.toolCalls {
			finishReason = "tool_calls"
		}
		final := s.chunk(map[string]any{}, finishReason)
		final["usage"] = OpenAIUsage(s.usage)
		s.emit(final)
		s.buf.Write(wire.SSEDone)
		finish = s.finishLocked()
	}
	s.mu.Unlock()
	if terminal {
		s.stream.Close()
	}
	for _, event := range callbacks {
		if s.opts.OnEvent != nil {
			s.opts.OnEvent(event)
		}
	}
	s.report(finish)
}

func (s *chatStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if !s.started {
			s.started = true
			s.id = completionID()
			s.created = nowSeconds()
			s.emit(s.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		}
		if s.buf.Len() > 0 {
			n, _ := s.buf.Read(p)
			s.mu.Unlock()
			return n, nil
		}
		if s.done {
			s.mu.Unlock()
			return 0, io.EOF
		}
		s.mu.Unlock()
		s.fill()
	}
}

func (s *chatStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.done = true
	s.buf.Reset()
	finish := s.finishLocked()
	s.mu.Unlock()
	if s.stream != nil {
		s.stream.Close()
	}
	s.report(finish)
	return nil
}
