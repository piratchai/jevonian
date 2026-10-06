package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

// ─── decoder-level tests: script the server frames, watch the events ────────

type fakeWriter struct {
	sent  [][]byte
	blobs map[string][]byte
}

func (w *fakeWriter) Send(frame []byte) { w.sent = append(w.sent, frame) }
func (w *fakeWriter) Blob(id string) []byte {
	if w.blobs == nil {
		return nil
	}
	return w.blobs[id]
}

// interactionFrame wraps an AgentServerMessage's interaction_update (field 1).
func interactionFrame(updates ...[]byte) []byte {
	msg := new(Pb)
	for _, u := range updates {
		msg.Bytes(1, u)
	}
	return msg.Build()
}

func textUpdate(text string) []byte {
	return new(Pb).Bytes(1, new(Pb).Str(1, text).Build()).Build()
}

func thinkingUpdate(text string) []byte {
	return new(Pb).Bytes(4, new(Pb).Str(1, text).Build()).Build()
}

func usageUpdate(input, output, cacheRead, cacheWrite uint64) []byte {
	return new(Pb).Bytes(14, new(Pb).
		Varint(1, input).Varint(2, output).Varint(3, cacheRead).Varint(4, cacheWrite).Build()).Build()
}

func listedUpdate(n uint64) []byte {
	return new(Pb).Bytes(27, new(Pb).Varint(1, n).Build()).Build()
}

// toolCallExec wraps an exec_server_message (field 2) carrying an MCP tool
// call (field 11).
func toolCallExec(id uint64, execID, callID, toolName string, args map[string]any) []byte {
	entry := new(Pb)
	if callID != "" {
		entry.Str(3, callID)
	}
	entry.Str(5, toolName)
	for k, v := range args {
		entry.Bytes(2, new(Pb).Str(1, k).Bytes(2, pbValue(v)).Build())
	}
	return new(Pb).Bytes(2,
		new(Pb).Varint(1, id).Str(15, execID).Bytes(11, entry.Build()).Build()).Build()
}

// kvAsk wraps a kv_server_message (field 4) asking for a blob.
func kvAsk(id uint64, blobID string) []byte {
	return new(Pb).Bytes(4,
		new(Pb).Varint(1, id).Bytes(2, new(Pb).Str(1, blobID).Build()).Build()).Build()
}

func collectEvents(t *testing.T, d *RunDecoder, chunks [][]byte) []Event {
	t.Helper()
	var out []Event
	for _, chunk := range chunks {
		events, err := d.Push(chunk)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, events...)
	}
	return out
}

func TestDecoderTextAndStop(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	frame := interactionFrame(textUpdate("hello "), textUpdate("world"))
	events := collectEvents(t, d, [][]byte{EncodeFrame(frame, 0)})
	if len(events) != 2 || events[0].Type != EventText || events[0].Text != "hello " {
		t.Fatalf("events: %+v", events)
	}
	final := d.Finish()
	if len(final) != 1 || final[0].Type != EventStop || final[0].ToolCalls {
		t.Fatalf("finish: %+v", final)
	}
}

func TestDecoderUsageAndThinking(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	frame := interactionFrame(thinkingUpdate("hmm"), usageUpdate(10, 4, 2, 1))
	events := collectEvents(t, d, [][]byte{EncodeFrame(frame, 0)})
	if len(events) != 2 {
		t.Fatalf("events: %+v", events)
	}
	if events[0].Type != EventThinking || events[0].Text != "hmm" {
		t.Fatalf("thinking: %+v", events[0])
	}
	if events[1].Type != EventUsage {
		t.Fatalf("usage: %+v", events[1])
	}
	u := events[1].Usage
	if u.Input != 10 || u.Output != 4 || u.CacheRead != 2 || u.CacheWrite != 1 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestDecoderEmptyReplyIsError(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	events := d.Finish()
	if len(events) != 1 || events[0].Type != EventError {
		t.Fatalf("events: %+v", events)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Message, "empty reply") {
		t.Fatalf("error: %+v", events[0].Error)
	}
}

func TestDecoderEndFrameTrailerError(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	payload := []byte(`{"code":"unauthenticated","message":"token expired"}`)
	events, err := d.Push(EncodeFrame(payload, 0x02))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventError {
		t.Fatalf("events: %+v", events)
	}
	if events[0].Error.Kind != KindAuth {
		t.Fatalf("kind: %s", events[0].Error.Kind)
	}
}

func TestDecoderToolCallEndsAfterListed(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	frame := interactionFrame(listedUpdate(1))
	if _, err := d.Push(EncodeFrame(frame, 0)); err != nil {
		t.Fatal(err)
	}
	exec := toolCallExec(7, "exec-1", "call_abc", "read-file", map[string]any{"path": "/x"})
	events, err := d.Push(EncodeFrame(exec, 0))
	if err != nil {
		t.Fatal(err)
	}
	var tool, stop bool
	for _, e := range events {
		if e.Type == EventTool {
			tool = true
			if e.Name != "read-file" || e.ID != "call_abc" {
				t.Fatalf("tool: %+v", e)
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(e.Args), &args); err != nil {
				t.Fatal(err)
			}
			if args["path"] != "/x" {
				t.Fatalf("args: %v", args)
			}
		}
		if e.Type == EventStop && e.ToolCalls {
			stop = true
		}
	}
	if !tool || !stop {
		t.Fatalf("events: %+v", events)
	}
}

func TestDecoderServesBlobRequests(t *testing.T) {
	blobBytes := []byte("the message body")
	store := map[string][]byte{}
	store[blobHex(blobBytes)] = blobBytes
	writer := &fakeWriter{blobs: store}
	d := NewRunDecoder(writer)
	if _, err := d.Push(EncodeFrame(kvAsk(3, blobHex(blobBytes)), 0)); err != nil {
		t.Fatal(err)
	}
	if len(writer.sent) != 1 {
		t.Fatalf("sent %d frames", len(writer.sent))
	}
	// The reply is AgentClientMessage field 3 (kv_client_message): id + field 2 result.
	fields := pbFields(writer.sent[0])
	if len(fields) != 1 || fields[0].num != 3 {
		t.Fatalf("reply: %+v", fields)
	}
	inner := pbFields(fields[0].data)
	id, _ := pbNum(inner, 1)
	if id != 3 {
		t.Fatalf("id: %d", id)
	}
	var result *pbField
	for i := range inner {
		if inner[i].num == 2 {
			result = &inner[i]
		}
	}
	if result == nil {
		t.Fatal("no result field")
	}
	resultFields := pbFields(result.data)
	// Field 1 = blob bytes when found.
	var found []byte
	for _, f := range resultFields {
		if f.num == 1 {
			found = f.data
		}
	}
	if string(found) != "the message body" {
		t.Fatalf("blob: %q", found)
	}
}

func TestDecoderMissingBlobRepliesNotFound(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	if _, err := d.Push(EncodeFrame(kvAsk(4, "deadbeef"), 0)); err != nil {
		t.Fatal(err)
	}
	if len(writer.sent) != 1 {
		t.Fatalf("sent %d frames", len(writer.sent))
	}
	inner := pbFields(pbFields(writer.sent[0])[0].data)
	var result *pbField
	for i := range inner {
		if inner[i].num == 2 {
			result = &inner[i]
		}
	}
	errFields := pbFields(pbFields(result.data)[0].data)
	msg, _ := pbStr(errFields, 1)
	if msg != "blob not found" {
		t.Fatalf("message: %q", msg)
	}
}

func blobHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ─── conversation encoding tests ────────────────────────────────────────────

func TestMessagesSystemAndUser(t *testing.T) {
	blobs, _ := Messages("You are helpful.", []Message{
		{Role: "user", Parts: []Part{{Kind: "text", Text: "hi"}}},
	}, nil)
	if len(blobs) != 2 {
		t.Fatalf("blobs: %d", len(blobs))
	}
	var system, user map[string]any
	if err := json.Unmarshal(blobs[0], &system); err != nil {
		t.Fatal(err)
	}
	if system["role"] != "system" || system["content"] != "You are helpful." {
		t.Fatalf("system: %v", system)
	}
	if err := json.Unmarshal(blobs[1], &user); err != nil {
		t.Fatal(err)
	}
	if user["role"] != "user" {
		t.Fatalf("user: %v", user)
	}
}

func TestMessagesToolCatalog(t *testing.T) {
	blobs, _ := Messages("", []Message{
		{Role: "user", Parts: []Part{{Kind: "text", Text: "hi"}}},
	}, []ToolDef{{Name: "read", Description: "Read a file", InputSchema: `{"type":"object"}`}})
	if len(blobs) != 2 {
		t.Fatalf("blobs: %d", len(blobs))
	}
	var system map[string]any
	if err := json.Unmarshal(blobs[0], &system); err != nil {
		t.Fatal(err)
	}
	content, _ := system["content"].(string)
	if !strings.Contains(content, "<dynamic_tool_catalog>") ||
		!strings.Contains(content, `<tool name="read">`) ||
		!strings.Contains(content, "magpie") {
		t.Fatalf("catalog: %q", content)
	}
}

func TestMessagesToolCallAndResultPairing(t *testing.T) {
	blobs, _ := Messages("", []Message{
		{Role: "user", Parts: []Part{{Kind: "text", Text: "go"}}},
		{Role: "assistant", Parts: []Part{
			{Kind: "tool-call", ID: "call_1", Name: "read", Args: `{"path":"/a"}`},
		}},
		{Role: "user", Parts: []Part{
			{Kind: "tool-result", CallID: "call_1", Text: "file contents"},
		}},
	}, nil)
	// user, assistant (tool-call), tool results
	if len(blobs) != 3 {
		t.Fatalf("blobs: %d", len(blobs))
	}
	var toolMsg map[string]any
	if err := json.Unmarshal(blobs[2], &toolMsg); err != nil {
		t.Fatal(err)
	}
	if toolMsg["role"] != "tool" {
		t.Fatalf("tool msg: %v", toolMsg)
	}
	results, _ := toolMsg["content"].([]any)
	if len(results) != 1 {
		t.Fatalf("results: %v", results)
	}
	result, _ := results[0].(map[string]any)
	if result["toolCallId"] != "call_1" || result["result"] != "file contents" {
		t.Fatalf("result: %v", result)
	}
}

func TestMessagesUnansweredToolCallGetsInterrupt(t *testing.T) {
	blobs, _ := Messages("", []Message{
		{Role: "assistant", Parts: []Part{
			{Kind: "tool-call", ID: "call_9", Name: "read", Args: `{}`},
		}},
	}, nil)
	if len(blobs) != 2 {
		t.Fatalf("blobs: %d", len(blobs))
	}
	var toolMsg map[string]any
	if err := json.Unmarshal(blobs[1], &toolMsg); err != nil {
		t.Fatal(err)
	}
	results, _ := toolMsg["content"].([]any)
	result, _ := results[0].(map[string]any)
	if result["result"] != noResult || result["isError"] != true {
		t.Fatalf("result: %v", result)
	}
}

func TestCallIDMapping(t *testing.T) {
	if got := CallID("call_1__fc_2"); got != "call_1\nfc_2" {
		t.Fatalf("got %q", got)
	}
	if got := CallID("other"); got != "other" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildRunBlobStore(t *testing.T) {
	blobs, _ := Messages("sys", []Message{
		{Role: "user", Parts: []Part{{Kind: "text", Text: "hi"}}},
	}, nil)
	run := BuildRun(blobs, "hi", nil, "model-x")
	if run == nil || len(run.Body) == 0 {
		t.Fatal("empty body")
	}
	// Every blob should be stored under its sha256 hex.
	if len(run.Blobs) == 0 {
		t.Fatal("no blobs stored")
	}
	for _, blob := range blobs {
		if _, ok := run.Blobs[blobHex(blob)]; !ok {
			t.Fatalf("blob %q not stored", blobHex(blob))
		}
	}
	// The body decodes as an AgentClientMessage wrapping run_request.
	fields := pbFields(run.Body)
	if len(fields) != 1 || fields[0].num != 1 {
		t.Fatalf("body: %+v", fields)
	}
	runRequest := pbFields(fields[0].data)
	var modelSet bool
	for _, f := range runRequest {
		if f.num == 3 {
			m := pbFields(f.data)
			if s, ok := pbStr(m, 1); ok && s == "model-x" {
				modelSet = true
			}
		}
	}
	if !modelSet {
		t.Fatal("model not in run_request")
	}
}

// ─── chat body → conversation ───────────────────────────────────────────────

func TestConversationBodyPullsSystemAndTools(t *testing.T) {
	body := wire.Body{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello", "tool_calls": []any{
				map[string]any{"id": "call_1", "function": map[string]any{
					"name": "read", "arguments": `{"path":"/a"}`,
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "out"},
		},
		"tools": []any{
			map[string]any{"function": map[string]any{
				"name": "read", "description": "Read a file",
				"parameters": map[string]any{"type": "object"},
			}},
		},
	}
	conversation := ConversationBody(body)
	if conversation.System != "You are helpful." {
		t.Fatalf("system: %q", conversation.System)
	}
	if len(conversation.Tools) != 1 || conversation.Tools[0].Name != "read" {
		t.Fatalf("tools: %+v", conversation.Tools)
	}
	if len(conversation.Messages) != 3 {
		t.Fatalf("messages: %+v", conversation.Messages)
	}
	if LastUser(conversation.Messages) != "hi" {
		t.Fatalf("last user: %q", LastUser(conversation.Messages))
	}
}

// ─── full HTTP round-trip ───────────────────────────────────────────────────

// cursorServer is a httptest.Server that reads the Connect request stream and
// speaks one scripted Run back.
type cursorServer struct {
	*httptest.Server
	mu      sync.Mutex
	gotBody []byte
}

func (s *cursorServer) body() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte{}, s.gotBody...)
}

func newCursorServer(t *testing.T, frames [][]byte) *cursorServer {
	t.Helper()
	server := &cursorServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The request body stays open while the reply streams (Connect bidi over
		// HTTP/1.1 here); without full duplex the server would try to drain the
		// never-ending body before writing headers.
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		requestDone := make(chan struct{})
		go func() {
			defer close(requestDone)
			buf := make([]byte, 4096)
			for {
				n, err := r.Body.Read(buf)
				if n > 0 {
					server.mu.Lock()
					server.gotBody = append(server.gotBody, buf[:n]...)
					server.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		for _, frame := range frames {
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
		// Do not let net/http start reading the next request while our
		// full-duplex request reader still owns the connection.
		<-r.Context().Done()
		<-requestDone
	}))
	return server
}

func runTest(t *testing.T, opts RunOptions, frames [][]byte) (*Turn, *cursorServer) {
	t.Helper()
	server := newCursorServer(t, frames)
	t.Cleanup(server.Close)
	opts.AgentURL = server.URL
	opts.Token = "tok"
	turn, err := Run(context.Background(), server.Client(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return turn, server
}

func drain(t *testing.T, s *EventStream) []Event {
	t.Helper()
	var out []Event
	for {
		events, err := s.Next()
		out = append(out, events...)
		if err != nil {
			return out
		}
	}
}

func TestRunStreamsTextAndStops(t *testing.T) {
	frames := [][]byte{
		EncodeFrame(interactionFrame(textUpdate("hi ")), 0),
		EncodeFrame(interactionFrame(textUpdate("there"), usageUpdate(5, 2, 0, 0)), 0),
		EncodeFrame([]byte("{}"), 0x02),
	}
	turn, _ := runTest(t, RunOptions{Model: "m", LastUser: "."}, frames)
	if turn.Error != nil {
		t.Fatalf("error: %+v", turn.Error)
	}
	events := drain(t, turn.Events)
	var text strings.Builder
	var stopped bool
	for _, e := range events {
		if e.Type == EventText {
			text.WriteString(e.Text)
		}
		if e.Type == EventStop {
			stopped = true
		}
	}
	if text.String() != "hi there" {
		t.Fatalf("text: %q", text.String())
	}
	if !stopped {
		t.Fatal("no stop")
	}
}

func TestRunRefusalOnNonOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reply without waiting on the still-open request stream.
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.Header().Set("Connection", "close")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"token expired"}`))
	}))
	defer server.Close()
	turn, err := Run(context.Background(), server.Client(), RunOptions{
		Token: "tok", AgentURL: server.URL, Model: "m", LastUser: ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Error == nil || turn.Error.Kind != KindAuth {
		t.Fatalf("error: %+v", turn.Error)
	}
}

// ─── OpenAI adaptation ──────────────────────────────────────────────────────

func TestChatCompletionFoldsEvents(t *testing.T) {
	frames := [][]byte{
		EncodeFrame(interactionFrame(textUpdate("answer"), usageUpdate(3, 7, 1, 0)), 0),
		EncodeFrame([]byte("{}"), 0x02),
	}
	turn, _ := runTest(t, RunOptions{Model: "gpt-test", LastUser: "."}, frames)
	completion, finish := ChatCompletion("gpt-test", turn.Events)
	if finish.ToolCalls {
		t.Fatal("unexpected tool calls")
	}
	if finish.Error != nil {
		t.Fatalf("error: %+v", finish.Error)
	}
	choices, _ := completion["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices: %v", completion["choices"])
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "answer" {
		t.Fatalf("content: %v", message["content"])
	}
	usage, _ := completion["usage"].(map[string]any)
	// Exclusive usage folds cache reads back into the prompt count.
	if usage["prompt_tokens"] != 4 || usage["completion_tokens"] != 7 || usage["total_tokens"] != 11 {
		t.Fatalf("usage: %v", usage)
	}
}

func TestToChatStreamEmitsSSE(t *testing.T) {
	frames := [][]byte{
		EncodeFrame(interactionFrame(textUpdate("stream "), textUpdate("text")), 0),
		EncodeFrame([]byte("{}"), 0x02),
	}
	turn, _ := runTest(t, RunOptions{Model: "m", LastUser: "."}, frames)
	var finished Finish
	stream := ToChatStream("m", turn.Events, StreamOptions{OnFinish: func(f Finish) { finished = f }})
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, `"content":"stream "`) || !strings.Contains(text, `"content":"text"`) {
		t.Fatalf("sse: %q", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("sse end: %q", text)
	}
	if !finished.ToolCalls && finished.Error != nil {
		t.Fatalf("finish: %+v", finished)
	}
}

func TestToChatStreamFoldsErrorToSoftText(t *testing.T) {
	writer := &fakeWriter{}
	d := NewRunDecoder(writer)
	stream := &EventStream{events: make(chan eventResult, 4), done: make(chan struct{})}
	go func() {
		defer close(stream.events)
		events, _ := d.Push(EncodeFrame(interactionFrame(textUpdate("partial")), 0))
		stream.events <- eventResult{events: events}
		stream.events <- eventResult{events: []Event{{Type: EventError, Error: &StreamError{
			Status: 503, Kind: KindCapacity, Message: "model overloaded",
		}}}}
	}()
	out, err := io.ReadAll(ToChatStream("m", stream, StreamOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, `"content":"partial"`) {
		t.Fatalf("sse: %q", text)
	}
	if !strings.Contains(text, wire.SoftErrorMessage("model overloaded")) {
		t.Fatalf("soft error missing: %q", text)
	}
}

// ─── provider seam ──────────────────────────────────────────────────────────

func TestProviderChatStreams(t *testing.T) {
	frames := [][]byte{
		EncodeFrame(interactionFrame(textUpdate("hi")), 0),
		EncodeFrame([]byte("{}"), 0x02),
	}
	server := newCursorServer(t, frames)
	defer server.Close()

	p := &Provider{
		HTTP:          server.Client(),
		AgentURL:      server.URL,
		Login:         &config.ProviderLogin{},
		Token:         func(context.Context) (string, error) { return "tok", nil },
		ClientVersion: "cli-test",
	}
	result, err := p.Chat(context.Background(), ChatRequest{
		Body: wire.Body{"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		}},
		Model:  "auto",
		Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil {
		t.Fatalf("refusal: %+v", result.Error)
	}
	out, _ := io.ReadAll(result.Stream)
	if !strings.Contains(string(out), `"content":"hi"`) {
		t.Fatalf("sse: %q", out)
	}
}

func TestProviderChatNonStream(t *testing.T) {
	frames := [][]byte{
		EncodeFrame(interactionFrame(textUpdate("done")), 0),
		EncodeFrame([]byte("{}"), 0x02),
	}
	server := newCursorServer(t, frames)
	defer server.Close()

	p := &Provider{
		HTTP:          server.Client(),
		AgentURL:      server.URL,
		Token:         func(context.Context) (string, error) { return "tok", nil },
		ClientVersion: "cli-test",
	}
	result, err := p.Chat(context.Background(), ChatRequest{
		Body:  wire.Body{"messages": []any{map[string]any{"role": "user", "content": "q"}}},
		Model: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := result.Completion["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("completion: %v", result.Completion)
	}
}

// ─── catalog file ───────────────────────────────────────────────────────────

func TestCatalogSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("JEVONIAN_DATA_DIR", t.TempDir())
	ResetCatalog()
	raw := []Model{
		{ID: "grok-4.7-low", Name: "Grok 4.7 Low", Context: 200_000},
		{ID: "grok-4.7-high", Name: "Grok 4.7 High", Context: 200_000},
		{ID: "auto", Name: "Auto", Context: 200_000},
	}
	file := SaveCatalog(raw)
	defer ResetCatalog()
	if len(file.Raw) != 3 || len(file.Models) != 2 {
		t.Fatalf("file: %+v", file)
	}
	loaded := LoadCatalog()
	if loaded == nil || len(loaded.Models) != 2 {
		t.Fatalf("loaded: %+v", loaded)
	}
}

func TestModelIDPicksEffortVariant(t *testing.T) {
	t.Setenv("JEVONIAN_DATA_DIR", t.TempDir())
	ResetCatalog()
	raw := []Model{
		{ID: "grok-4.7-low", Name: "Grok 4.7 Low", Context: 200_000},
		{ID: "grok-4.7-medium", Name: "Grok 4.7 Medium", Context: 200_000},
		{ID: "grok-4.7-high", Name: "Grok 4.7 High", Context: 200_000},
		{ID: "grok-4.7-low-fast", Name: "Grok 4.7 Low Fast", Context: 200_000},
	}
	SaveCatalog(raw)
	defer ResetCatalog()

	if got := ModelID("grok-4.7", "high", false); got != "grok-4.7-high" {
		t.Fatalf("high: %q", got)
	}
	if got := ModelID("grok-4.7", "low", true); got != "grok-4.7-low-fast" {
		t.Fatalf("low fast: %q", got)
	}
	// Unknown model passes through.
	if got := ModelID("unlisted", "high", false); got != "unlisted" {
		t.Fatalf("unlisted: %q", got)
	}
	// A listed id keeps itself when no effort is asked.
	if got := ModelID("grok-4.7-high", "", false); got != "grok-4.7-high" {
		t.Fatalf("own id: %q", got)
	}
}

func TestRunURLAndHeaders(t *testing.T) {
	if got := RunURL("https://x.example/"); got != "https://x.example"+runPath {
		t.Fatalf("url: %q", got)
	}
	if got := RunURL(""); got != AgentFallback+runPath {
		t.Fatalf("fallback: %q", got)
	}
	h := Headers("tok", "cli-1", "req-1")
	if h.Get("authorization") != "Bearer tok" ||
		h.Get("x-cursor-client-version") != "cli-1" ||
		h.Get("x-cursor-agent-allowed-tools") != "mcp_tool_call,get_mcp_tools_tool_call" {
		t.Fatalf("headers: %v", h)
	}
}
