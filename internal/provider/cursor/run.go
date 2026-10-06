package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/provider/devin"
	"github.com/xinyao27/jevonian/internal/wire"
)

// DefaultBaseURL is the main Cursor API root; the agent API it reports is
// resolved through ServerConfigService.
const DefaultBaseURL = "https://api2.cursor.sh"

// AgentFallback is the agent API used when the server config cannot name a
// region-specific one.
const AgentFallback = "https://agentn.global.api5.cursor.sh"

const (
	serverConfigPath = "/aiserver.v1.ServerConfigService/GetServerConfig"
	runPath          = "/agent.v1.AgentService/Run"
)

// The pseudo-tool the model calls; Cursor hands each call back to the client.
const (
	callDynamicTool = "CallDynamicTool"
	mcpNamespace    = "magpie"
)

const (
	maxFrameBytes = 64 * 1024 * 1024
	// Heartbeat is how often the client must tell the server it is still there.
	Heartbeat = 5 * time.Second
)

// EnvFields reports the platform strings the Run's env message carries; tests
// swap it to pin the fields.
var EnvFields = func() (osName, tmpdir, tz string) {
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	return runtime.GOOS, tmp, "UTC"
}

// ─── Connect framing ────────────────────────────────────────────────────────

// EncodeFrame wraps a payload in one Connect envelope: flags byte + 4-byte
// big-endian length + payload.
func EncodeFrame(payload []byte, flags byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// heartbeatFrame is the keep-alive frame the client sends while a Run is open.
func heartbeatFrame() []byte { return new(Pb).Bytes(7, []byte{}).Build() }

func blobID(bytes []byte) []byte {
	sum := sha256.Sum256(bytes)
	return sum[:]
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexed := make([]byte, 32)
	hex.Encode(hexed, b[:])
	h := string(hexed)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ─── conversation encoding ──────────────────────────────────────────────────

// ToolDef is one caller tool, listed for the model as an MCP tool.
type ToolDef struct {
	Name        string
	Description string
	InputSchema string // JSON source
}

// Part is one part of a Message (AI SDK-style parts).
type Part struct {
	Kind      string // "text" | "tool-call" | "tool-result" | "image" | "file"
	Text      string
	ID        string
	Name      string
	Args      string // JSON source
	CallID    string
	IsError   bool
	Data      string // base64, for image
	MediaType string
}

// Message is one AI SDK message: a role with its parts.
type Message struct {
	Role  string
	Parts []Part
}

const noResult = "Tool use was interrupted and did not produce a result."

// Messages renders the conversation as AI SDK message JSON blobs, the way the
// Run stores them (each blob is named by its sha256 and served back on demand).
// Port of cursorMessages.
func Messages(system string, messages []Message, tools []ToolDef) (blobs [][]byte, out []ToolDef) {
	var add func(map[string]any)
	add = func(message map[string]any) {
		data, err := json.Marshal(message)
		if err != nil {
			return
		}
		blobs = append(blobs, data)
	}
	fullSystem := system + toolCatalog(tools)
	if fullSystem != "" {
		add(map[string]any{"role": "system", "content": fullSystem})
	}

	var pending []string
	answer := func(results []any) {
		list := append([]any{}, results...)
		for _, id := range pending {
			list = append(list, toolResult(id, noResult, true))
		}
		pending = nil
		if len(list) > 0 {
			add(map[string]any{"role": "tool", "content": list})
		}
	}

	for _, message := range messages {
		if message.Role == "assistant" {
			answer(nil)
			var content []any
			for _, part := range message.Parts {
				switch {
				case part.Kind == "text" && part.Text != "":
					content = append(content, map[string]any{"type": "text", "text": part.Text})
				case part.Kind == "tool-call":
					args := any(map[string]any{})
					if part.Args != "" {
						var parsed any
						if err := json.Unmarshal([]byte(part.Args), &parsed); err == nil {
							args = parsed
						}
					}
					content = append(content, map[string]any{
						"type":       "tool-call",
						"toolCallId": CallID(part.ID),
						"toolName":   callDynamicTool,
						"args": map[string]any{
							"namespace": mcpNamespace,
							"toolName":  part.Name,
							"arguments": args,
						},
					})
					pending = append(pending, part.ID)
				}
			}
			if len(content) > 0 {
				add(map[string]any{"role": "assistant", "content": content})
			}
			continue
		}
		var results []any
		var content []any
		for _, part := range message.Parts {
			switch {
			case part.Kind == "tool-result":
				if at := indexOf(pending, part.CallID); at >= 0 {
					pending = append(append([]string{}, pending[:at]...), pending[at+1:]...)
					results = append(results, toolResult(part.CallID, part.Text, part.IsError))
				}
			case part.Kind == "text" && part.Text != "":
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			case part.Kind == "image" && part.Data != "":
				mediaType := part.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				content = append(content, map[string]any{
					"type":     "image",
					"mimeType": mediaType,
					"image":    map[string]any{"__type": "Uint8Array", "hex": part.Data},
				})
			case part.Kind == "file" && part.Text != "":
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			}
		}
		answer(results)
		if len(content) > 0 {
			add(map[string]any{"role": "user", "content": content})
		}
	}
	answer(nil)
	return blobs, tools
}

func toolResult(id, text string, isError bool) map[string]any {
	var result any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		result = text
	}
	out := map[string]any{
		"type":                 "tool-result",
		"toolCallId":           CallID(id),
		"toolName":             callDynamicTool,
		"result":               result,
		"experimental_content": []any{map[string]any{"type": "text", "text": text}},
	}
	if isError {
		out["isError"] = true
	}
	return out
}

// toolCatalog lists the caller's tools for the model, which only sees MCP tools.
func toolCatalog(tools []ToolDef) string {
	if len(tools) == 0 {
		return ""
	}
	var lines []string
	for _, tool := range tools {
		lines = append(lines, "<tool name=\""+tool.Name+"\">\n"+tool.Description+"\ninput schema: "+tool.InputSchema+"\n</tool>")
	}
	return "\n\n<dynamic_tool_catalog>\nThe tools below are available in the MCP namespace \"" + mcpNamespace +
		"\". Call one with `" + callDynamicTool + "` (namespace \"" + mcpNamespace +
		"\", toolName, arguments). Their schemas are given here, so there is no need to call `GetDynamicTools` first.\n" +
		strings.Join(lines, "\n") + "\n</dynamic_tool_catalog>"
}

// CallID maps an OpenAI call id onto the form Cursor expects.
func CallID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return strings.ReplaceAll(id, "__fc_", "\nfc_")
	}
	return id
}

// RunURL is the URL one Run is POSTed to.
func RunURL(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		root = AgentFallback
	}
	return root + runPath
}

func cursorToolDef(tool ToolDef) []byte {
	def := new(Pb).Str(1, tool.Name)
	if tool.Description != "" {
		def.Str(2, tool.Description)
	}
	var schema any
	if err := json.Unmarshal([]byte(tool.InputSchema), &schema); err == nil {
		def.Bytes(3, pbValue(schema))
	}
	// A schema that is not JSON is passed as the raw description string only.
	return def.Str(4, mcpNamespace).Str(5, tool.Name).Str(6, tool.InputSchema).Build()
}

// RunRequest is the Run's opening message and the blobs the server may ask
// back for.
type RunRequest struct {
	Body  []byte
	Blobs map[string][]byte
}

// BuildRun is the Run's opening message, an AgentClientMessage with its
// run_request, plus the store of sha256-named blobs it refers to. Port of
// buildCursorRun.
func BuildRun(blobs [][]byte, lastUser string, tools []ToolDef, model string) *RunRequest {
	store := map[string][]byte{}
	put := func(bytes []byte) []byte {
		id := blobID(bytes)
		store[hex.EncodeToString(id)] = bytes
		return id
	}

	state := new(Pb)
	for _, blob := range blobs {
		state.Bytes(1, put(blob))
	}
	messageID := uuid()
	user := new(Pb).Str(1, lastUser).Str(2, messageID).Varint(4, 1)
	turn := new(Pb).Bytes(1, new(Pb).Bytes(1, put(user.Build())).Str(10, messageID).Build())
	state = new(Pb).Bytes(1, state.Build()).Bytes(8, put(turn.Build())).Varint(10, 1).Str(22, "cli")

	osName, tmpdir, tz := EnvFields()
	env := new(Pb).Str(1, osName).Str(2, tmpdir).Str(10, tz)
	requestContext := new(Pb).Bytes(4, env.Build())
	mcp := new(Pb)
	for _, tool := range tools {
		def := cursorToolDef(tool)
		requestContext.Bytes(7, def)
		mcp.Bytes(1, def)
	}
	action := new(Pb).Bytes(2, new(Pb).Bytes(2, requestContext.Build()).Build())
	runRequest := new(Pb).
		Bytes(1, state.Build()).
		Bytes(2, action.Build()).
		Bytes(3, new(Pb).Str(1, model).Str(3, model).Str(4, model).Build()).
		Bytes(4, mcp.Build()).
		Str(5, uuid()).
		Bytes(9, new(Pb).Str(1, model).Build()).
		Varint(19, 1)

	return &RunRequest{Body: new(Pb).Bytes(1, runRequest.Build()).Build(), Blobs: store}
}

// Headers are the Connect headers one Run is POSTed with.
func Headers(token, version, requestID string) http.Header {
	return http.Header{
		"Content-Type":                 {"application/connect+proto"},
		"Connect-Protocol-Version":     {"1"},
		"Authorization":                {"Bearer " + token},
		"X-Cursor-Client-Version":      {version},
		"X-Cursor-Client-Type":         {"cli"},
		"X-Ghost-Mode":                 {"true"},
		"X-Request-Id":                 {requestID},
		"X-Cursor-Agent-Allowed-Tools": {"mcp_tool_call,get_mcp_tools_tool_call"},
	}
}

// ResolveAgentURL is the agent API to run on, as Cursor's server config names
// it, or the global fallback.
func ResolveAgentURL(ctx context.Context, client *http.Client, token, baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		root = DefaultBaseURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+serverConfigPath, strings.NewReader("{}"))
	if err == nil {
		req.Header = http.Header{
			"Content-Type":             {"application/json"},
			"Connect-Protocol-Version": {"1"},
			"Authorization":            {"Bearer " + token},
			"X-Cursor-Client-Version":  {ClientVersion()},
			"X-Cursor-Client-Type":     {"cli"},
			"X-Ghost-Mode":             {"true"},
		}
		if resp, err := client.Do(req); err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			var cfg struct {
				AgentURLConfig struct {
					AgentURL  string `json:"agentUrl"`
					AgentNURL string `json:"agentnUrl"`
				} `json:"agentUrlConfig"`
			}
			if json.Unmarshal(data, &cfg) == nil {
				for _, raw := range []string{cfg.AgentURLConfig.AgentURL, cfg.AgentURLConfig.AgentNURL} {
					if raw == "" {
						continue
					}
					if u, err := url.Parse(raw); err == nil &&
						(u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
						return strings.TrimRight(raw, "/")
					}
				}
			}
		} else if resp != nil {
			_ = resp.Body.Close()
		}
	}
	return AgentFallback
}

// ─── stream decoding ────────────────────────────────────────────────────────

// Event is one thing the Run surfaced.
type Event struct {
	Type  EventType
	Text  string
	ID    string
	Name  string
	Args  string
	Usage wire.Usage
	Error *StreamError
	// ToolCalls marks a stop that follows at least one tool call.
	ToolCalls bool
}

// EventType enumerates what a decoded frame or transport close means.
type EventType int

const (
	EventText EventType = iota
	EventThinking
	EventTool
	EventUsage
	EventError
	EventStop
)

// EventWriter is what the Run needs to answer while the server reads the
// conversation: Send a reply frame, Blob one of ours by hex id.
type EventWriter interface {
	Send(frame []byte)
	Blob(id string) []byte
}

// RunDecoder decodes one Run's server frames. The server may ask the client
// for a blob (kv_server_message) or hand it an MCP tool call to run
// (exec_server_message); both are answered through the writer. Port of
// CursorRunDecoder.
type RunDecoder struct {
	splitter frameSplitter
	calls    int
	listed   int
	said     int
	stopped  bool
	writer   EventWriter
}

// NewRunDecoder starts a decoder whose replies go through writer.
func NewRunDecoder(writer EventWriter) *RunDecoder {
	return &RunDecoder{writer: writer}
}

// Push feeds one upstream chunk; the events it decodes are returned.
func (d *RunDecoder) Push(chunk []byte) ([]Event, error) {
	var events []Event
	frames, err := d.splitter.push(chunk)
	if err != nil {
		return nil, err
	}
	for _, frame := range frames {
		if d.stopped {
			return events, nil
		}
		if frame.end {
			d.finish(&events, frame.payload)
			return events, nil
		}
		d.applyFrame(frame.payload, &events)
		if d.stopped {
			return events, nil
		}
	}
	return events, nil
}

// Finish emits the terminal stop event when the transport closed without an
// end-stream frame.
func (d *RunDecoder) Finish() []Event {
	var events []Event
	d.finish(&events, nil)
	return events
}

func (d *RunDecoder) finish(events *[]Event, payload []byte) {
	if d.stopped {
		return
	}
	if len(payload) > 0 {
		text := strings.TrimSpace(string(payload))
		if text != "" && text != "{}" {
			if err := ClassifyError(200, text); err.Status/100 != 2 {
				*events = append(*events, Event{Type: EventError, Error: err})
				d.stopped = true
				return
			}
		}
	}
	if d.said == 0 && d.calls == 0 {
		*events = append(*events, Event{Type: EventError, Error: &StreamError{
			Status: 502, Kind: KindOther, Message: "Cursor returned an empty reply",
		}})
		d.stopped = true
		return
	}
	*events = append(*events, Event{Type: EventStop, ToolCalls: d.calls > 0})
	d.stopped = true
}

func (d *RunDecoder) applyFrame(payload []byte, events *[]Event) {
	for _, field := range pbFields(payload) {
		switch {
		case field.num == 1 && field.data != nil:
			d.applyInteraction(field.data, events)
		case field.num == 2 && field.data != nil:
			d.applyExec(field.data, events)
		case field.num == 4 && field.data != nil:
			d.applyKV(field.data)
		}
		if d.stopped {
			return
		}
	}
}

func (d *RunDecoder) applyInteraction(payload []byte, events *[]Event) {
	for _, update := range pbFields(payload) {
		fields := pbFields(update.data)
		switch update.num {
		case 1, 4: // text / thinking
			if text, ok := pbStr(fields, 1); ok && text != "" {
				d.said += len(text)
				kind := EventText
				if update.num == 4 {
					kind = EventThinking
				}
				*events = append(*events, Event{Type: kind, Text: text})
			}
		case 14: // usage
			input, _ := pbNum(fields, 1)
			output, _ := pbNum(fields, 2)
			cacheRead, _ := pbNum(fields, 3)
			cacheWrite, _ := pbNum(fields, 4)
			*events = append(*events, Event{Type: EventUsage, Usage: wire.Usage{
				Input: int(input), Output: int(output), CacheRead: int(cacheRead), CacheWrite: int(cacheWrite),
			}})
		case 27:
			listed, _ := pbNum(fields, 1)
			d.listed = int(listed)
		}
	}
}

func (d *RunDecoder) applyExec(payload []byte, events *[]Event) {
	fields := pbFields(payload)
	id, _ := pbNum(fields, 1)
	execID, _ := pbStr(fields, 15)
	answer := func(num int, result []byte) {
		d.writer.Send(new(Pb).Bytes(2,
			new(Pb).Varint(1, id).Str(15, execID).Bytes(num, result).Build()).Build())
		d.closeExec(id)
	}
	for _, field := range fields {
		switch {
		case field.num == 11 && field.data != nil: // MCP tool call
			entryFields := pbFields(field.data)
			argMap := map[string]any{}
			for _, kv := range entryFields {
				if kv.num != 2 || kv.data == nil {
					continue
				}
				entry := pbFields(kv.data)
				key, _ := pbStr(entry, 1)
				var value *pbField
				for i := range entry {
					if entry[i].num == 2 {
						value = &entry[i]
						break
					}
				}
				if value != nil && value.data != nil {
					argMap[key] = pbAny(value.data)
				} else {
					argMap[key] = nil
				}
			}
			name, _ := pbStr(entryFields, 5)
			if name == "" {
				raw, _ := pbStr(entryFields, 1)
				name = strings.TrimPrefix(raw, "magpie-")
			}
			// An OpenAI model's id is its call's and its item's, a line apart,
			// which no caller would take as an id: the line goes as __.
			callID, _ := pbStr(entryFields, 3)
			callID = strings.ReplaceAll(callID, "\n", "__")
			if callID == "" {
				var b [12]byte
				_, _ = rand.Read(b[:])
				callID = "call_" + hex.EncodeToString(b[:])
			}
			args, _ := json.Marshal(argMap)
			d.calls++
			d.said += len(args)
			*events = append(*events, Event{Type: EventTool, ID: callID, Name: name, Args: string(args)})
			if d.listed > 0 && d.calls >= d.listed {
				d.finish(events, nil)
			}
			return
		case field.num == 36: // MCP server handshake
			server := new(Pb).Str(1, mcpNamespace).Str(2, mcpNamespace).Str(7, "connected")
			answer(36, new(Pb).Bytes(1, new(Pb).Bytes(1, server.Build()).Build()).Build())
			return
		case field.num == 10: // environment request
			osName, tmpdir, tz := EnvFields()
			env := new(Pb).Str(1, osName).Str(2, tmpdir).Str(10, tz)
			answer(10, new(Pb).Bytes(1,
				new(Pb).Bytes(1, new(Pb).Bytes(4, env.Build()).Build()).Build()).Build())
			return
		}
	}
	// Anything else the server asks of the client (a shell, a file read) is refused.
	d.writer.Send(new(Pb).Bytes(5,
		new(Pb).Bytes(2, new(Pb).Varint(1, id).Str(2, "not available").Build()).Build()).Build())
	d.closeExec(id)
}

func (d *RunDecoder) applyKV(payload []byte) {
	fields := pbFields(payload)
	id, _ := pbNum(fields, 1)
	for _, field := range fields {
		switch {
		case field.num == 2 && field.data != nil:
			asked, _ := pbStr(pbFields(field.data), 1)
			blob := d.writer.Blob(asked)
			var result []byte
			if blob != nil {
				result = new(Pb).Bytes(1, blob).Build()
			} else {
				result = new(Pb).Bytes(2, new(Pb).Str(1, "blob not found").Build()).Build()
			}
			d.writer.Send(new(Pb).Bytes(3,
				new(Pb).Varint(1, id).Bytes(2, result).Build()).Build())
		case field.num == 3:
			d.writer.Send(new(Pb).Bytes(3,
				new(Pb).Varint(1, id).Bytes(3, []byte{}).Build()).Build())
		}
	}
}

func (d *RunDecoder) closeExec(id uint64) {
	d.writer.Send(new(Pb).Bytes(5,
		new(Pb).Bytes(1, new(Pb).Varint(1, id).Build()).Build()).Build())
}

// connFrame is one decoded Connect envelope.
type connFrame struct {
	end     bool
	payload []byte
}

// frameSplitter is the incremental Connect envelope splitter.
type frameSplitter struct {
	buffer []byte
}

func (s *frameSplitter) push(chunk []byte) ([]connFrame, error) {
	if len(s.buffer) == 0 {
		s.buffer = chunk
	} else {
		s.buffer = append(s.buffer, chunk...)
	}
	var frames []connFrame
	offset := 0
	for len(s.buffer)-offset >= 5 {
		flags := s.buffer[offset]
		length := int(binary.BigEndian.Uint32(s.buffer[offset+1:]))
		if length > maxFrameBytes {
			return nil, errors.New("Cursor frame exceeds the size limit")
		}
		if len(s.buffer)-offset < 5+length {
			break
		}
		payload := make([]byte, length)
		copy(payload, s.buffer[offset+5:offset+5+length])
		frames = append(frames, connFrame{end: flags&0x02 != 0, payload: payload})
		offset += 5 + length
	}
	if offset > 0 {
		s.buffer = append([]byte{}, s.buffer[offset:]...)
	}
	return frames, nil
}

// ─── run ────────────────────────────────────────────────────────────────────

// RunOptions is one Run against Cursor's agent API.
type RunOptions struct {
	Token string
	// AgentURL is the agent API to run on, as ResolveAgentURL resolved it.
	AgentURL     string
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDef
	Model        string
	LastUser     string
	RequestID    string
	// ClientVersion overrides the version header; empty picks ClientVersion().
	ClientVersion string
}

// streamBody is the half-duplex request body: Run frames are written while the
// response is read.
type streamBody struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
	wake   chan struct{}
}

func newStreamBody() *streamBody { return &streamBody{wake: make(chan struct{}, 1)} }

func (b *streamBody) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, http.ErrBodyNotAllowed
	}
	b.buf = append(b.buf, p...)
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (b *streamBody) Read(p []byte) (int, error) {
	for {
		b.mu.Lock()
		if len(b.buf) > 0 {
			n := copy(p, b.buf)
			b.buf = b.buf[n:]
			b.mu.Unlock()
			return n, nil
		}
		if b.closed {
			b.mu.Unlock()
			return 0, io.EOF
		}
		b.mu.Unlock()
		<-b.wake
	}
}

func (b *streamBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return nil
}

// wireWriter feeds decoder replies and heartbeats into the request body.
type wireWriter struct {
	body  *streamBody
	blobs map[string][]byte
}

func (w *wireWriter) Send(frame []byte) {
	// Frames go into the request body exactly as the CLI writes them (the TS
	// wire enqueues raw payloads into the Connect stream).
	_, _ = w.body.Write(frame)
}

func (w *wireWriter) Blob(id string) []byte { return w.blobs[id] }

// Turn is one Run's outcome: either an immediate refusal (Error, no events)
// or a live stream the caller drains.
type Turn struct {
	Events *EventStream
	Error  *StreamError
}

// EventStream yields a Run's events until Stop.
type EventStream struct {
	events chan eventResult
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	// readMu serializes consumers, not cancellation. A leading batch inspected
	// by Run is replayed before reading the bounded queue.
	readMu sync.Mutex
	peeked *eventResult
}

type eventResult struct {
	events []Event
	err    error
}

// Next returns the next batch of events, or io.EOF at the terminal one.
func (s *EventStream) Next() ([]Event, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	select {
	case <-s.done:
		return nil, io.EOF
	default:
	}
	var canceled <-chan struct{}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		canceled = s.ctx.Done()
	}
	if s.peeked != nil {
		result := s.peeked
		s.peeked = nil
		return result.events, result.err
	}
	select {
	case <-s.done:
		return nil, io.EOF
	case <-canceled:
		return nil, s.ctx.Err()
	case result, ok := <-s.events:
		if !ok {
			return nil, io.EOF
		}
		return result.events, result.err
	}
}

// Close stops the run and its heartbeat.
func (s *EventStream) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		close(s.done)
	})
}

// Run performs one Run against Cursor's agent API: the request is a Connect
// stream that stays open while the server reads the conversation (blobs are
// served on demand, MCP tool calls are handed back, a heartbeat keeps it
// alive). Returns when the first answer event arrives, replaying that batch
// through Events; a refusal before text, reasoning, or tools is Turn.Error.
// Terminal events release the transport without waiting for HTTP EOF. Port of
// runCursor.
func Run(ctx context.Context, client *http.Client, opts RunOptions) (*Turn, error) {
	if client == nil {
		client = http.DefaultClient
	}
	blobs, _ := Messages(opts.SystemPrompt, opts.Messages, opts.Tools)
	request := BuildRun(blobs, opts.LastUser, opts.Tools, opts.Model)

	body := newStreamBody()
	writer := &wireWriter{body: body, blobs: request.Blobs}
	decoder := NewRunDecoder(writer)

	version := opts.ClientVersion
	if version == "" {
		version = ClientVersion()
	}

	runCtx, cancel := context.WithCancel(ctx)
	stream := &EventStream{events: make(chan eventResult, 16), done: make(chan struct{}), ctx: ctx, cancel: cancel}

	// Canceling a request also closes our custom request body: a transport may
	// be waiting in Read instead of watching the request context itself.
	go func() {
		<-runCtx.Done()
		_ = body.Close()
	}()
	cleanup := func() {
		cancel()
		_ = body.Close()
	}

	// The opening message goes first, before any heartbeat.
	if _, err := body.Write(request.Body); err != nil {
		cleanup()
		return nil, err
	}

	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, RunURL(opts.AgentURL), body)
	if err != nil {
		cleanup()
		return nil, err
	}
	req.Header = Headers(opts.Token, version, opts.RequestID)
	go func() {
		ticker := time.NewTicker(Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = body.Write(heartbeatFrame())
			case <-runCtx.Done():
				return
			}
		}
	}()

	resp, err := client.Do(req)
	if err != nil {
		cleanup()
		return nil, err
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		cleanup()
		return &Turn{Error: &StreamError{Status: 502, Kind: KindOther,
			Message: "Cursor returned an empty response body"}}, nil
	}
	// Close must interrupt even a custom response body's blocked Read. Merely
	// canceling the HTTP request is not sufficient for every RoundTripper.
	var responseClose sync.Once
	closeResponse := func() { responseClose.Do(func() { _ = resp.Body.Close() }) }
	go func() {
		<-runCtx.Done()
		closeResponse()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		cleanup()
		closeResponse()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil {
			return nil, readErr
		}
		return &Turn{Error: ClassifyError(resp.StatusCode, string(text))}, nil
	}

	go func() {
		defer func() {
			cleanup()
			closeResponse()
			close(stream.events)
		}()
		// A fixed queue applies back-pressure. Every send also watches
		// cancellation, including errors and the terminal batch.
		send := func(result eventResult) bool {
			select {
			case <-runCtx.Done():
				return false
			case <-stream.done:
				return false
			case stream.events <- result:
				return true
			}
		}
		buf := make([]byte, 32*1024)
		for {
			if runCtx.Err() != nil {
				return
			}
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				events, perr := decoder.Push(buf[:n])
				if perr != nil {
					send(eventResult{err: perr})
					return
				}
				if len(events) > 0 && !send(eventResult{events: events}) {
					return
				}
				// A listed-tool stop or Connect trailer ends the turn even if
				// the upstream leaves its HTTP response open indefinitely.
				if decoder.stopped {
					return
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					send(eventResult{err: readErr})
					return
				}
				if last := decoder.Finish(); len(last) > 0 {
					send(eventResult{events: last})
				}
				return
			}
		}
	}()

	// Do not commit synthetic assistant SSE before discovering a leading
	// trailer refusal. Ignore control-only frames and retain only the latest
	// usage snapshot until text, reasoning, or a tool starts the answer.
	var leadingUsage *Event
	for {
		events, nextErr := stream.Next()
		for i, event := range events {
			switch event.Type {
			case EventUsage:
				usage := event
				leadingUsage = &usage
			case EventText, EventThinking, EventTool:
				first := events[i:]
				if leadingUsage != nil {
					first = append([]Event{*leadingUsage}, first...)
				}
				stream.peeked = &eventResult{events: first, err: nextErr}
				return &Turn{Events: stream}, nil
			case EventError:
				stream.Close()
				return &Turn{Error: event.Error}, nil
			}
		}
		if nextErr != nil {
			stream.Close()
			if nextErr != io.EOF {
				return nil, nextErr
			}
			return &Turn{Error: &StreamError{Status: 502, Kind: KindOther,
				Message: "Cursor returned an empty reply"}}, nil
		}
	}
}

// ─── OpenAI translation ─────────────────────────────────────────────────────

// LastUser is the last user message's text, which the Run is named by.
func LastUser(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Role != "user" {
			continue
		}
		for _, part := range message.Parts {
			if part.Kind == "text" && part.Text != "" {
				return part.Text
			}
		}
	}
	return "."
}

// Finish is what a Run left behind: usage, whether it called tools, and a
// refusal when one ended it.
type Finish struct {
	Usage     wire.Usage
	ToolCalls bool
	Error     *StreamError
}

// OpenAIUsage renders exclusive usage as an OpenAI usage object (inclusive
// prompt count).
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
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

func nowSeconds() int64 { return time.Now().Unix() }

// Conversation is the Chat Completions body as the conversation a Run expects.
type Conversation struct {
	System   string
	Messages []Message
	Tools    []ToolDef
}

// ConversationBody reads an OpenAI Chat Completions body into the conversation
// Cursor's Run expects. Port of chatBodyToCursor / cursorConversation.
func ConversationBody(body wire.Body) Conversation {
	system, parts := chatParts(wire.AsSlice(body["messages"]))
	var tools []ToolDef
	for _, raw := range wire.AsSlice(body["tools"]) {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		description, _ := fn["description"].(string)
		schema := wire.MarshalJSON(fn["parameters"])
		if schema == "" {
			schema = `{"type":"object","properties":{}}`
		}
		tools = append(tools, ToolDef{Name: name, Description: description, InputSchema: schema})
	}
	return Conversation{System: system, Messages: parts, Tools: tools}
}

var dataURLPattern = regexp.MustCompile(`(?is)^data:([a-z0-9.+-]+/[a-z0-9.+-]+);base64,(.+)$`)

func chatParts(messages []any) (string, []Message) {
	var out []Message
	var system []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role == "" {
			role = "user"
		}
		if role == "system" || role == "developer" {
			if text := chatText(message["content"]); text != "" {
				system = append(system, text)
			}
			continue
		}
		var parts []Part
		if role == "tool" || role == "function" {
			callID, _ := message["tool_call_id"].(string)
			text := chatText(message["content"])
			if text == "" {
				text = "(no output)"
			}
			isError := false
			if v, ok := message["is_error"].(bool); ok {
				isError = v
			}
			parts = append(parts, Part{Kind: "tool-result", CallID: callID, Text: text, IsError: isError})
			out = append(out, Message{Role: "user", Parts: parts})
			continue
		}
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, entry := range calls {
					call, ok := entry.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := call["function"].(map[string]any)
					name, _ := fn["name"].(string)
					if name == "" {
						continue
					}
					id, _ := call["id"].(string)
					args := ""
					switch a := fn["arguments"].(type) {
					case string:
						args = a
					default:
						if a == nil {
							args = "{}"
						} else {
							args = wire.MarshalJSON(a)
						}
					}
					parts = append(parts, Part{Kind: "tool-call", ID: id, Name: name, Args: args})
				}
			}
		}
		switch content := message["content"].(type) {
		case string:
			if content != "" {
				parts = append(parts, Part{Kind: "text", Text: content})
			}
		case []any:
			for _, rawPart := range content {
				switch part := rawPart.(type) {
				case string:
					if part != "" {
						parts = append(parts, Part{Kind: "text", Text: part})
					}
				case map[string]any:
					typ, _ := part["type"].(string)
					if typ == "text" {
						if text, ok := part["text"].(string); ok {
							parts = append(parts, Part{Kind: "text", Text: text})
						}
					} else if typ == "image_url" {
						var urlText string
						switch u := part["image_url"].(type) {
						case string:
							urlText = u
						case map[string]any:
							urlText, _ = u["url"].(string)
						}
						if urlText != "" {
							stripped := whitespacePattern.ReplaceAllString(urlText, "")
							if m := dataURLPattern.FindStringSubmatch(stripped); m != nil {
								parts = append(parts, Part{Kind: "image", Data: m[2],
									MediaType: strings.ToLower(m[1])})
							}
						}
					}
				}
			}
		}
		if len(parts) > 0 {
			roleOut := "user"
			if role == "assistant" {
				roleOut = "assistant"
			}
			out = append(out, Message{Role: roleOut, Parts: parts})
		}
	}
	return strings.Join(system, "\n\n"), out
}

var whitespacePattern = regexp.MustCompile(`\s`)

func chatText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var texts []string
		for _, part := range v {
			switch p := part.(type) {
			case string:
				texts = append(texts, p)
			case map[string]any:
				if text, ok := p["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		var out []string
		for _, t := range texts {
			if t != "" {
				out = append(out, t)
			}
		}
		return strings.Join(out, "\n")
	default:
		return wire.MarshalJSON(content)
	}
}

// SanitizeSystemPrompt keeps the caller's system prompt clean of signatures
// upstream may refuse. Port of sanitizeCursorSystemPrompt — the same built-in
// rule list devin.SanitizeSystemPrompt applies.
func SanitizeSystemPrompt(text string) string {
	return devin.SanitizeSystemPrompt(text)
}

// ChatCompletion collects a Run's events into a non-streaming
// chat.completion. An error event lands on Finish.Error; the completion then
// holds whatever partial output arrived. Port of cursorChatCompletion.
func ChatCompletion(model string, stream *EventStream) (wire.Body, Finish) {
	var content, reasoning strings.Builder
	usage := wire.Usage{}
	var streamErr *StreamError
	var calls []Event
	for {
		events, err := stream.Next()
		for _, event := range events {
			switch event.Type {
			case EventText:
				content.WriteString(event.Text)
			case EventThinking:
				reasoning.WriteString(event.Text)
			case EventTool:
				calls = append(calls, event)
			case EventUsage:
				usage = event.Usage
			case EventError:
				streamErr = event.Error
			}
		}
		if err != nil {
			if err != io.EOF && streamErr == nil {
				streamErr = &StreamError{Status: 502, Kind: KindOther, Message: err.Error()}
			}
			break
		}
	}
	message := map[string]any{
		"role":    "assistant",
		"content": nil,
	}
	if content.Len() > 0 {
		message["content"] = content.String()
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		var toolCalls []any
		for _, call := range calls {
			toolCalls = append(toolCalls, map[string]any{
				"id":       call.ID,
				"type":     "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Args},
			})
		}
		message["tool_calls"] = toolCalls
	}
	finishReason := "stop"
	if len(calls) > 0 {
		finishReason = "tool_calls"
	}
	completion := wire.Body{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": nowSeconds(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   OpenAIUsage(usage),
	}
	finish := Finish{Usage: usage, ToolCalls: len(calls) > 0, Error: streamErr}
	return completion, finish
}
