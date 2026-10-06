package softstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/xinyao27/jevonian/internal/config"
)

// Past this many bytes without a frame boundary, the text is forwarded rather than
// buffered.
const maxCarry = 1 << 20

// Bytes kept when the carry overflows, so a frame split at the boundary is still
// parsed.
const carryTail = 1 << 16

// softStreamState tracks the progress of a streamed turn, used to place the soft
// block and to spot a real finish.
type softStreamState struct {
	// started: the client has seen the wire's stream opener (or any forwarded event).
	started  bool
	finished bool
	failed   bool
	reason   string
	// Anthropic: highest content block index seen, and the block still open (if any).
	maxBlockIndex  int
	openBlockIndex int
	// Responses: highest output index seen, and the item still open (if any).
	maxOutputIndex int
	openItem       *OpenItem
}

func newState() *softStreamState {
	return &softStreamState{maxBlockIndex: -1, openBlockIndex: -1, maxOutputIndex: -1}
}

// parsedEvent is one parsed SSE event: its JSON `data:` payload, a `[DONE]`
// sentinel, or neither.
type parsedEvent struct {
	json map[string]any
	done bool
}

func parseEvent(event string) parsedEvent {
	for _, line := range strings.Split(event, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[len("data:"):])
		if raw == "" {
			continue
		}
		if raw == "[DONE]" {
			return parsedEvent{done: true}
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			if m, ok := parsed.(map[string]any); ok && m != nil {
				return parsedEvent{json: m}
			}
		}
		// A malformed data line is not a terminal signal; keep looking at the others.
	}
	return parsedEvent{}
}

// isErrorEvent reports whether an event is the wire's terminal failure frame, which
// must never reach the client.
func isErrorEvent(kind config.UpstreamWire, json map[string]any) bool {
	typ := asString(json["type"])
	switch kind {
	case config.WireOpenAI:
		errVal, present := json["error"]
		return present && errVal != nil
	case config.WireAnthropic:
		return typ == "error"
	default: // responses
		return typ == "response.failed" || typ == "error"
	}
}

func errorReason(kind config.UpstreamWire, json map[string]any) string {
	raw := json["error"]
	if s, ok := raw.(string); ok && len(s) > 0 {
		return s
	}
	if kind == config.WireOpenAI || kind == config.WireAnthropic {
		if msg := asString(asRecord(raw)["message"]); msg != "" {
			return msg
		}
		return "upstream error"
	}
	nested := asString(asRecord(asRecord(json["response"])["error"])["message"])
	if nested != "" {
		return nested
	}
	if msg := asString(asRecord(raw)["message"]); msg != "" {
		return msg
	}
	return "upstream response failed"
}

// applyEvent folds one parsed event into the stream state.
//
// A real finish is recognised by its parsed `type` or a non-null `finish_reason`,
// never by a substring: a marker buried more than a carry-length into a frame used
// to be missed, which appended a second answer to an already-finished turn.
func applyEvent(kind config.UpstreamWire, json map[string]any, state *softStreamState) {
	typ := asString(json["type"])
	switch kind {
	case config.WireOpenAI:
		// An error frame was never forwarded, so it does not open the stream.
		if errVal, present := json["error"]; present && errVal != nil {
			state.failed = true
			state.reason = errorReason(kind, json)
			return
		}
		state.started = true
		choices, _ := json["choices"].([]any)
		for _, raw := range choices {
			finish := asRecord(raw)["finish_reason"]
			if s, ok := finish.(string); ok && len(s) > 0 {
				state.finished = true
			}
		}
	case config.WireAnthropic:
		if typ == "error" {
			state.failed = true
			state.reason = errorReason(kind, json)
			return
		}
		if typ == "message_start" {
			state.started = true
		}
		switch typ {
		case "content_block_start":
			state.started = true
			index := int(asNumber(json["index"]))
			if index > state.maxBlockIndex {
				state.maxBlockIndex = index
			}
			state.openBlockIndex = index
		case "content_block_stop":
			if state.openBlockIndex == int(asNumber(json["index"])) {
				state.openBlockIndex = -1
			}
		case "message_stop":
			state.finished = true
		}
	default: // responses
		if typ == "response.failed" || typ == "error" {
			state.failed = true
			state.reason = errorReason(kind, json)
			return
		}
		if typ == "response.created" {
			state.started = true
		}
		if typ == "response.output_item.added" {
			state.started = true
			outputIndex := int(asNumber(json["output_index"]))
			if outputIndex > state.maxOutputIndex {
				state.maxOutputIndex = outputIndex
			}
			state.openItem = &OpenItem{OutputIndex: outputIndex, Item: asRecord(json["item"]), Text: ""}
			return
		}
		if state.openItem != nil && int(asNumber(json["output_index"])) == state.openItem.OutputIndex {
			switch typ {
			case "response.output_text.delta", "response.function_call_arguments.delta":
				state.openItem.Text += asString(json["delta"])
			case "response.output_text.done":
				state.openItem.Text = asString(json["text"])
			case "response.output_item.done":
				state.openItem = nil
			}
			return
		}
		if typ == "response.completed" || typ == "response.incomplete" || typ == "response.done" {
			state.finished = true
		}
	}
}

// SoftOptions configures Wrap.
type SoftOptions struct {
	// Message is the assistant text used when the stream ends without an upstream
	// reason (socket drop, truncated read). Defaults to a generic "ended
	// unexpectedly" soft message.
	Message string
	// OnSoft is called once when the soft completion replaces a failure, with the
	// upstream reason (already redacted when Redact is set) or "" for a silent
	// early end. Use it to record the real status in the ledger.
	OnSoft func(reason string)
	// Redact scrubs provider credentials from an upstream error before it is shown
	// to the user or recorded.
	Redact func(string) string
}

// Wrap returns a reader over an upstream SSE body so a mid-stream failure (a
// dropped socket, a proxy that speaks the wrong protocol, a truncated read) ends
// as a normal assistant message instead of an abrupt close the harness can roll
// back.
//
// A clean upstream that never emitted a finish marker is treated the same way:
// the turn is ended with the soft message rather than left dangling. When the
// stream already finished normally, the bytes pass through untouched. A terminal
// error frame — OpenAI `{ error }`, Anthropic `type: "error"`, Responses
// `response.failed` — is swallowed and replaced by the soft completion, so no hard
// failure ever reaches a streaming client.
//
// A nil source answers the turn with the soft message alone (pre-stream failure).
// The returned reader is not safe for concurrent use. ctx cancellation behaves
// like a client hang-up: the turn is dropped quietly (OnSoft is not invoked).
func Wrap(ctx context.Context, source io.ReadCloser, kind config.UpstreamWire, model string, opts SoftOptions) io.ReadCloser {
	if ctx == nil {
		ctx = context.Background()
	}
	message := opts.Message
	if message == "" {
		message = SoftErrorMessage("the upstream stream ended unexpectedly")
	}
	w := &softReader{
		ctx:     ctx,
		source:  source,
		kind:    kind,
		model:   model,
		opts:    opts,
		message: message,
		state:   newState(),
		queue:   make(chan []byte, 64),
		aborted: make(chan struct{}),
	}
	go w.run()
	return w
}

// softReader pumps the upstream through event parsing on a goroutine so a client
// hang-up (ctx cancel / Close) can interrupt a wedged upstream read rather than
// blocking the consumer forever.
type softReader struct {
	ctx     context.Context
	source  io.ReadCloser
	kind    config.UpstreamWire
	model   string
	opts    SoftOptions
	message string
	state   *softStreamState

	queue   chan []byte
	aborted chan struct{}

	abortOnce sync.Once
	mu        sync.Mutex
	pending   []byte
	eof       bool
	closed    bool
}

func (w *softReader) Read(p []byte) (int, error) {
	for len(w.pending) == 0 {
		if w.eof {
			return 0, io.EOF
		}
		select {
		case chunk, ok := <-w.queue:
			if !ok {
				w.eof = true
				return 0, io.EOF
			}
			w.pending = chunk
		case <-w.ctx.Done():
			w.eof = true
			w.abort()
			return 0, io.EOF
		case <-w.aborted:
			w.eof = true
			return 0, io.EOF
		}
	}
	n := copy(p, w.pending)
	w.pending = w.pending[n:]
	return n, nil
}

func (w *softReader) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	w.abort()
	if w.source != nil {
		return w.source.Close()
	}
	return nil
}

// abort signals the producer that the consumer left; pending emits return early.
func (w *softReader) abort() {
	w.abortOnce.Do(func() { close(w.aborted) })
}

// emit hands one chunk to the consumer. Returns false when the consumer went away
// (ctx canceled or Close called).
func (w *softReader) emit(chunk []byte) bool {
	if len(chunk) == 0 {
		return true
	}
	select {
	case w.queue <- chunk:
		return true
	case <-w.ctx.Done():
		return false
	case <-w.aborted:
		return false
	}
}

// run drives the upstream read + event folding loop on its own goroutine.
func (w *softReader) run() {
	defer close(w.queue)

	if w.source == nil {
		// An upstream that produced no body at all: still answer the turn rather
		// than handing the client an empty 200 it will read as broken.
		w.softFinish()
		return
	}
	defer w.source.Close()

	carry := ""
	buf := make([]byte, 32<<10)
	for {
		n, err := w.source.Read(buf)
		if n > 0 {
			decoded := carry + string(buf[:n])
			decoded = strings.ReplaceAll(decoded, "\r\n", "\n")
			events := strings.Split(decoded, "\n\n")
			carry = events[len(events)-1]
			events = events[:len(events)-1]
			for _, event := range events {
				if !w.forwardEvent(event) {
					return
				}
			}
			if len(carry) > maxCarry {
				// A frame that never completes (a mislabeled non-SSE body, a wedged
				// upstream) must not buffer without bound: forward the excess and
				// keep searching for a boundary in the tail.
				if !w.emit([]byte(carry[:len(carry)-carryTail])) {
					return
				}
				carry = carry[len(carry)-carryTail:]
			}
		}
		if err != nil {
			if err == io.EOF {
				// A trailing incomplete frame cannot be parsed, so it is dropped
				// rather than forwarded as an invalid event — unless the upstream
				// already failed, where nothing is forwarded.
				if strings.TrimSpace(carry) != "" && !w.state.failed && !w.state.finished {
					if !w.emit([]byte(carry)) {
						return
					}
				}
				if w.state.finished {
					return
				}
				w.softFinish()
				return
			}
			// The upstream died mid-stream: close the turn softly instead of
			// erroring the client.
			if w.state.finished {
				return
			}
			w.softFinish()
			return
		}
		// A terminal error ends the turn: stop pulling and close it softly now
		// rather than wait for an upstream that may hold the socket open after
		// the frame.
		if w.state.failed && !w.state.finished {
			w.softFinish()
			return
		}
	}
}

// forwardEvent parses one SSE event and either forwards it, folds a failure into
// state, or marks the turn finished. Returns false when the consumer went away.
func (w *softReader) forwardEvent(event string) bool {
	// A terminal failure already decided this turn: nothing after it may be
	// forwarded, not even a `[DONE]` — the soft completion carries its own
	// terminator.
	if w.state.failed {
		return true
	}
	parsed := parseEvent(event)
	if parsed.done {
		w.state.finished = true
		return w.emit(append([]byte(event), '\n', '\n'))
	}
	if parsed.json != nil {
		if isErrorEvent(w.kind, parsed.json) {
			// Never hand a hard failure back. If the turn already finished the
			// frame is dropped outright; otherwise it becomes the reason the soft
			// completion reports.
			if !w.state.finished {
				applyEvent(w.kind, parsed.json, w.state)
			}
			return true
		}
		applyEvent(w.kind, parsed.json, w.state)
	}
	return w.emit(append([]byte(event), '\n', '\n'))
}

// softFinish emits the soft completion chunks and the OnSoft callback, once.
// A client hang-up is not Jevonian's failure to report: when the consumer is
// already gone, the soft body and the callback are both skipped.
func (w *softReader) softFinish() {
	select {
	case <-w.ctx.Done():
		return
	case <-w.aborted:
		return
	default:
	}
	reason := w.state.reason
	text := w.message
	if reason != "" {
		if w.opts.Redact != nil {
			reason = w.opts.Redact(reason)
		}
		text = SoftErrorMessage(reason)
	}
	index := w.state.maxOutputIndex + 1
	if w.kind == config.WireAnthropic {
		index = w.state.maxBlockIndex + 1
	}
	opts := SoftCompletionOptions{
		Started:    w.state.started,
		Index:      index,
		CloseIndex: -1,
	}
	if w.kind == config.WireAnthropic && w.state.openBlockIndex >= 0 {
		opts.CloseIndex = w.state.openBlockIndex
	}
	if w.kind == config.WireResponses && w.state.openItem != nil {
		opts.CloseItem = w.state.openItem
	}
	for _, chunk := range SoftCompletionChunks(w.kind, w.model, text, opts) {
		if !w.emit(chunk) {
			return
		}
	}
	// A client hang-up is not Jevonian's failure to report: skip the callback
	// when the consumer already left, even if every emit won the select race.
	select {
	case <-w.ctx.Done():
		return
	case <-w.aborted:
		return
	default:
	}
	// The reason is redacted here too: the ledger is read by the dashboard and
	// shown to users.
	if w.opts.OnSoft != nil {
		w.opts.OnSoft(reason)
	}
}

// Bytes drains a wrapped stream into one buffer — the soft-completion equivalent
// of reading a whole response body.
func Bytes(ctx context.Context, source io.ReadCloser, kind config.UpstreamWire, model string, opts SoftOptions) ([]byte, error) {
	r := Wrap(ctx, source, kind, model, opts)
	defer r.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}
