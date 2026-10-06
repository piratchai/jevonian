package cursor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

const cursorTestTimeout = 3 * time.Second

// liveCursorServer keeps the HTTP response and request open after the script
// finishes. The client must release both sides of the Connect exchange.
func liveCursorServer(t *testing.T, script func(http.ResponseWriter, *http.Request)) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	requestDone := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		go func() {
			_, _ = io.Copy(io.Discard, r.Body)
			close(requestDone)
		}()
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		script(w, r)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		// The connection is canceled before handler return, so the request
		// reader cannot race the HTTP server's own read of the next request.
		select {
		case <-requestDone:
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server, requestDone
}

func writeCursorFrames(w http.ResponseWriter, frames ...[]byte) {
	for _, frame := range frames {
		if _, err := w.Write(frame); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	}
}

func waitCursorSignal(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(cursorTestTimeout):
		t.Fatal(message)
	}
}

func localCursorProvider(server *httptest.Server) *Provider {
	return &Provider{
		HTTP: server.Client(), AgentURL: server.URL, ClientVersion: "cli-test",
		Token: func(context.Context) (string, error) { return "local-test-token", nil },
	}
}

func readCursorChunk(t *testing.T, stream io.Reader) string {
	t.Helper()
	type result struct {
		text string
		err  error
	}
	out := make(chan result, 1)
	go func() {
		buf := make([]byte, 32*1024)
		n, err := stream.Read(buf)
		out <- result{string(buf[:n]), err}
	}()
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.text
	case <-time.After(cursorTestTimeout):
		t.Fatal("Read waited for the entire Connect response instead of returning a chunk")
		return ""
	}
}

func TestChatStreamIncrementalPreservesEvents(t *testing.T) {
	next := make(chan struct{})
	server, requestDone := liveCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeCursorFrames(w, EncodeFrame(interactionFrame(textUpdate("first")), 0))
		select {
		case <-next:
		case <-r.Context().Done():
			return
		}
		writeCursorFrames(w,
			EncodeFrame(interactionFrame(thinkingUpdate("consider"), usageUpdate(10, 4, 2, 1), listedUpdate(2)), 0),
			EncodeFrame(toolCallExec(1, "exec-1", "call_a\nfc_a", "read", map[string]any{"path": "/a"}), 0),
			EncodeFrame(toolCallExec(2, "exec-2", "call_b", "write", map[string]any{"text": "b"}), 0),
		)
	})
	finished := make(chan Finish, 4)
	var callbackMu sync.Mutex
	var callbacks []wire.StreamEvent
	result, err := localCursorProvider(server).Chat(context.Background(), ChatRequest{
		Model: "m", Stream: true,
		OnFinish: func(f Finish) { finished <- f },
		OnEvent: func(e wire.StreamEvent) {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			callbacks = append(callbacks, e)
		},
	})
	if err != nil || result.Error != nil || result.Stream == nil {
		t.Fatalf("Chat: %+v, %v", result, err)
	}
	t.Cleanup(func() { _ = result.Stream.Close() })
	first := readCursorChunk(t, result.Stream)
	if !strings.Contains(first, `"role":"assistant"`) {
		t.Fatalf("missing leading role chunk: %s", first)
	}
	for !strings.Contains(first, `"content":"first"`) {
		first += readCursorChunk(t, result.Stream)
	}
	select {
	case <-finished:
		t.Fatal("OnFinish fired before the server completed the turn")
	default:
	}
	close(next)
	end := make(chan struct{})
	var rest []byte
	go func() {
		rest, err = io.ReadAll(result.Stream)
		close(end)
	}()
	waitCursorSignal(t, end, "listed tools did not terminate the open Connect response")
	if err != nil {
		t.Fatal(err)
	}
	text := first + string(rest)
	frames := wire.SplitSseEvents(text).Events
	var tools []map[string]any
	for _, frame := range frames {
		choices := frame["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		for _, tool := range wire.AsSlice(delta["tool_calls"]) {
			tools = append(tools, tool.(map[string]any))
		}
	}
	if len(tools) != 2 || tools[0]["index"] != float64(0) || tools[1]["index"] != float64(1) || tools[0]["id"] != "call_a__fc_a" {
		t.Fatalf("tool deltas: %+v", tools)
	}
	if !strings.Contains(text, `"reasoning_content":"consider"`) || !strings.Contains(text, `"finish_reason":"tool_calls"`) || strings.Count(text, "data: [DONE]") != 1 {
		t.Fatalf("SSE: %s", text)
	}
	select {
	case f := <-finished:
		if f.Error != nil || !f.ToolCalls || f.Usage != (wire.Usage{Input: 10, Output: 4, CacheRead: 2, CacheWrite: 1}) {
			t.Fatalf("Finish: %+v", f)
		}
	case <-time.After(cursorTestTimeout):
		t.Fatal("missing OnFinish")
	}
	_ = result.Stream.Close()
	select {
	case f := <-finished:
		t.Fatalf("duplicate OnFinish: %+v", f)
	default:
	}
	callbackMu.Lock()
	defer callbackMu.Unlock()
	content, usage := 0, 0
	for _, e := range callbacks {
		if e.Kind == wire.StreamContent {
			content++
		}
		if e.Kind == wire.StreamUsage {
			usage++
		}
	}
	if content != 4 || usage != 1 {
		t.Fatalf("callbacks: %+v", callbacks)
	}
	waitCursorSignal(t, requestDone, "request body remained open after the tool stop")
}

func TestEventStreamCloseReleasesNext(t *testing.T) {
	stream := &EventStream{events: make(chan eventResult), done: make(chan struct{})}
	for _, alreadyClosed := range []bool{false, true} {
		if alreadyClosed {
			stream.Close()
		}
		done := make(chan struct{})
		go func() {
			_, err := stream.Next()
			if !errors.Is(err, io.EOF) {
				t.Errorf("Next after Close: %v", err)
			}
			close(done)
		}()
		stream.Close()
		waitCursorSignal(t, done, "Next blocked after Close with an open event channel")
	}
}

func TestChatStreamCloseCancelsBlockedRead(t *testing.T) {
	server, requestDone := liveCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeCursorFrames(w, EncodeFrame(interactionFrame(textUpdate("partial")), 0))
	})
	finished := make(chan Finish, 4)
	result, err := localCursorProvider(server).Chat(context.Background(), ChatRequest{
		Model: "m", Stream: true, OnFinish: func(f Finish) { finished <- f },
	})
	if err != nil || result.Error != nil {
		t.Fatalf("Chat: %+v, %v", result, err)
	}
	text := readCursorChunk(t, result.Stream)
	for !strings.Contains(text, `"content":"partial"`) {
		text += readCursorChunk(t, result.Stream)
	}
	reading := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		close(reading)
		_, readErr := result.Stream.Read(make([]byte, 1024))
		if !errors.Is(readErr, io.EOF) {
			t.Errorf("closed Read: %v", readErr)
		}
		close(readDone)
	}()
	waitCursorSignal(t, reading, "Read did not start")
	closeDone := make(chan struct{})
	go func() {
		_ = result.Stream.Close()
		close(closeDone)
	}()
	waitCursorSignal(t, closeDone, "Close deadlocked behind Read")
	waitCursorSignal(t, readDone, "Close did not release Read")
	waitCursorSignal(t, requestDone, "Close did not release Connect request body")
	_ = result.Stream.Close()
	if len(finished) != 1 {
		t.Fatalf("OnFinish called %d times", len(finished))
	}
}

func TestRunContextCancelBeforeContent(t *testing.T) {
	ready := make(chan struct{})
	server, requestDone := liveCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(ready)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	complete := make(chan struct{})
	go func() {
		_, err := Run(ctx, server.Client(), RunOptions{Token: "tok", AgentURL: server.URL, ClientVersion: "cli-test"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run cancel before content: %v", err)
		}
		close(complete)
	}()
	waitCursorSignal(t, ready, "Connect request did not arrive")
	cancel()
	waitCursorSignal(t, complete, "Run peek did not honor request cancellation")
	waitCursorSignal(t, requestDone, "context cancel left request body open")
}

func TestRunLateTrailerRefusalPreservesPartial(t *testing.T) {
	server, requestDone := liveCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeCursorFrames(w,
			EncodeFrame(interactionFrame(textUpdate("partial"), usageUpdate(3, 2, 1, 0)), 0),
			EncodeFrame([]byte(`{"error":{"code":"unavailable","message":"model overloaded"}}`), 0x02),
		)
	})
	finished := make(chan Finish, 1)
	result, err := localCursorProvider(server).Chat(context.Background(), ChatRequest{
		Model: "m", Stream: true, OnFinish: func(f Finish) { finished <- f },
	})
	if err != nil || result.Error != nil || result.Stream == nil {
		t.Fatalf("late refusal became a pre-commit failure: %+v, %v", result, err)
	}
	complete := make(chan struct{})
	var data []byte
	go func() {
		data, err = io.ReadAll(result.Stream)
		close(complete)
	}()
	waitCursorSignal(t, complete, "trailer refusal waited for HTTP EOF")
	if err != nil || !strings.Contains(string(data), `"content":"partial"`) || !strings.Contains(string(data), wire.SoftErrorMessage("model overloaded")) {
		t.Fatalf("late refusal SSE: %s, %v", data, err)
	}
	select {
	case f := <-finished:
		if f.Error == nil || f.Error.Kind != KindCapacity || f.Usage != (wire.Usage{Input: 3, Output: 2, CacheRead: 1}) {
			t.Fatalf("late refusal Finish: %+v", f)
		}
	default:
		t.Fatal("no Finish on late refusal")
	}
	waitCursorSignal(t, requestDone, "late refusal left request body open")
}

type cursorRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cursorRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Each read yields exactly one Connect frame, making pump saturation
// deterministic without depending on network packet coalescing.
type endlessCursorBody struct {
	reads  chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (b *endlessCursorBody) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.EOF
	default:
	}
	b.reads <- struct{}{}
	return copy(p, EncodeFrame(interactionFrame(textUpdate("token")), 0)), nil
}

func (b *endlessCursorBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestRunCloseReleasesBackpressuredPump(t *testing.T) {
	response := &endlessCursorBody{reads: make(chan struct{}, 128), closed: make(chan struct{})}
	requestDone := make(chan struct{})
	client := &http.Client{Transport: cursorRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		go func() {
			_, _ = io.Copy(io.Discard, r.Body)
			close(requestDone)
		}()
		return &http.Response{StatusCode: 200, Body: response, Header: make(http.Header)}, nil
	})}
	turn, err := Run(context.Background(), client, RunOptions{AgentURL: "http://local.invalid", ClientVersion: "cli-test"})
	if err != nil || turn.Error != nil {
		t.Fatalf("Run: %+v, %v", turn, err)
	}
	// Run consumed one leading batch. Sixteen further batches fill the queue;
	// the eighteenth body read blocks on the next send until canceled.
	for i := 0; i < 18; i++ {
		waitCursorSignal(t, response.reads, "pump did not reach backpressure")
	}
	turn.Events.Close()
	pumpDone := make(chan struct{})
	go func() {
		for range turn.Events.events {
		}
		close(pumpDone)
	}()
	waitCursorSignal(t, pumpDone, "canceled pump did not close its bounded event queue")
	waitCursorSignal(t, response.closed, "Close did not release response body")
	waitCursorSignal(t, requestDone, "Close did not release custom request body")
}

func TestRunConnectStopClosesOpenResponse(t *testing.T) {
	server, requestDone := liveCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeCursorFrames(w,
			EncodeFrame(interactionFrame(textUpdate("done")), 0),
			EncodeFrame([]byte("{}"), 0x02),
		)
	})
	turn, err := Run(context.Background(), server.Client(), RunOptions{AgentURL: server.URL, ClientVersion: "cli-test"})
	if err != nil || turn.Error != nil {
		t.Fatalf("Run: %+v, %v", turn, err)
	}
	complete := make(chan struct{})
	go func() {
		completion, finish := ChatCompletion("m", turn.Events)
		if completion == nil || finish.Error != nil {
			t.Errorf("Completion: %+v, %+v", completion, finish)
		}
		close(complete)
	}()
	waitCursorSignal(t, complete, "Connect stop waited for response EOF")
	waitCursorSignal(t, requestDone, "Connect stop left request body open")
}

func TestRunLeadingTrailerRefusal(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "stream"}[streaming], func(t *testing.T) {
			server, requestDone := liveCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeCursorFrames(w,
					EncodeFrame(interactionFrame(usageUpdate(1, 0, 0, 0)), 0),
					EncodeFrame([]byte(`{"error":{"code":"unauthenticated","message":"token expired"}}`), 0x02),
				)
			})
			result, err := localCursorProvider(server).Chat(context.Background(), ChatRequest{Model: "m", Stream: streaming})
			if err != nil || result.Error == nil || result.Error.Kind != KindAuth || result.Status != 401 || result.Stream != nil || result.Completion != nil {
				t.Fatalf("leading trailer must be a pre-commit refusal: %+v, %v", result, err)
			}
			waitCursorSignal(t, requestDone, "request body remained open after trailer refusal")
		})
	}
}
