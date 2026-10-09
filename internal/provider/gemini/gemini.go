// Package gemini is the Antigravity (Cloud Code Assist) wire: a Chat
// Completions body is folded into a Gemini `generateContent` envelope, and the
// Gemini reply is folded back into Chat Completions JSON / SSE. Port of
// src/gemini.ts.
package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

const skipThoughtSignature = "skip_thought_signature_validator"
const signatureCacheLimit = 512

var sigs = struct {
	sync.Mutex
	m     map[string]string
	order []string
}{m: map[string]string{}}

func signatureKey(name string, args any) string { return name + ":" + wire.MarshalJSON(args) }

func rememberSignature(name string, args any, signature string) {
	if signature == "" {
		return
	}
	key := signatureKey(name, args)
	sigs.Lock()
	defer sigs.Unlock()
	if _, ok := sigs.m[key]; ok {
		for i, k := range sigs.order {
			if k == key {
				sigs.order = append(sigs.order[:i], sigs.order[i+1:]...)
				break
			}
		}
	}
	sigs.m[key] = signature
	sigs.order = append(sigs.order, key)
	if len(sigs.order) > signatureCacheLimit {
		delete(sigs.m, sigs.order[0])
		sigs.order = sigs.order[1:]
	}
}

// ThoughtSignatureFor is the signature remembered for a function call.
func ThoughtSignatureFor(name string, args any) (string, bool) {
	sigs.Lock()
	defer sigs.Unlock()
	s, ok := sigs.m[signatureKey(name, args)]
	return s, ok
}

// Endpoint is the Cloud Code URL for a base URL.
func Endpoint(baseURL string, stream bool) string {
	base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1internal")
	if stream {
		return base + "/v1internal:streamGenerateContent?alt=sse"
	}
	return base + "/v1internal:generateContent"
}

// Unwrap strips the Cloud Code `response` envelope.
func Unwrap(payload any) wire.Body {
	body := wire.AsRecord(payload)
	if body["response"] == nil {
		return body
	}
	return wire.AsRecord(body["response"])
}

func textOf(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, b := range v {
			if t, ok := wire.AsRecord(b)["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return wire.MarshalJSON(value)
}

func responseObject(value any) wire.Body {
	switch v := value.(type) {
	case string:
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				return m
			}
			if parsed != nil {
				return wire.Body{"result": parsed}
			}
		}
		return wire.Body{"result": v}
	case []any:
		t := textOf(v)
		if t == "" {
			t = wire.MarshalJSON(v)
		}
		return wire.Body{"result": t}
	case map[string]any:
		if len(v) > 0 {
			return v
		}
	}
	return wire.Body{"result": ""}
}

func parseArgs(value any) wire.Body {
	switch v := value.(type) {
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil && m != nil {
			return m
		}
		return wire.Body{}
	case map[string]any:
		return v
	}
	return wire.Body{}
}

var unsupportedSchemaKeys = map[string]bool{"$schema": true, "$id": true, "$defs": true, "definitions": true, "$ref": true, "default": true, "examples": true, "title": true, "const": true}

// SanitizeSchema drops JSON-schema keys Gemini rejects; `const` becomes a
// one-value `enum`.
func SanitizeSchema(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = SanitizeSchema(item)
		}
		return out
	case map[string]any:
		out := wire.Body{}
		for k, item := range v {
			if !unsupportedSchemaKeys[k] {
				out[k] = SanitizeSchema(item)
			}
		}
		if c, ok := v["const"]; ok && c != nil && out["enum"] == nil {
			out["enum"] = []any{c}
		}
		return out
	}
	return value
}

var badToolChars = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)
var toolStart = regexp.MustCompile(`^[a-zA-Z_]`)

// SanitizeToolName makes a tool name Gemini accepts.
func SanitizeToolName(name string) string {
	cleaned := badToolChars.ReplaceAllString(name, "_")
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	if cleaned == "" {
		return "tool"
	}
	if toolStart.MatchString(cleaned) {
		return cleaned
	}
	cleaned = "_" + cleaned
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

func toolsToGemini(raw any) []any {
	var decls []any
	for _, entry := range wire.AsSlice(raw) {
		tool := wire.AsRecord(entry)
		if tool["type"] != "function" {
			continue
		}
		fn := wire.AsRecord(tool["function"])
		name := wire.AsString(fn["name"])
		if name == "" {
			continue
		}
		d := wire.Body{"name": SanitizeToolName(name), "parameters": SanitizeSchema(wire.AsRecord(fn["parameters"]))}
		if desc, ok := fn["description"].(string); ok {
			d["description"] = desc
		}
		decls = append(decls, d)
	}
	if len(decls) == 0 {
		return nil
	}
	return []any{wire.Body{"functionDeclarations": decls}}
}

func toolConfigFor(choice any) wire.Body {
	switch c := choice.(type) {
	case string:
		if c == "none" {
			return wire.Body{"functionCallingConfig": wire.Body{"mode": "NONE"}}
		}
		if c == "required" {
			return wire.Body{"functionCallingConfig": wire.Body{"mode": "ANY"}}
		}
	case map[string]any:
		if name := wire.AsString(wire.AsRecord(c["function"])["name"]); name != "" {
			return wire.Body{"functionCallingConfig": wire.Body{"mode": "ANY", "allowedFunctionNames": []any{SanitizeToolName(name)}}}
		}
	}
	return nil
}

// ChatToGemini folds a Chat Completions body into a Gemini request.
func ChatToGemini(body wire.Body) wire.Body {
	var systemTexts []string
	type content struct {
		role  string
		parts []any
	}
	var contents []*content
	toolNames := map[string]string{}
	push := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 && contents[n-1].role == role {
			contents[n-1].parts = append(contents[n-1].parts, parts...)
			return
		}
		contents = append(contents, &content{role, parts})
	}
	for _, raw := range wire.AsSlice(body["messages"]) {
		m := wire.AsRecord(raw)
		switch m["role"] {
		case "system", "developer":
			if t := textOf(m["content"]); t != "" {
				systemTexts = append(systemTexts, t)
			}
		case "tool", "function":
			callID := wire.AsString(m["tool_call_id"])
			name, ok := toolNames[callID]
			if !ok {
				n := wire.AsString(m["name"])
				if n == "" {
					n = "tool"
				}
				name = SanitizeToolName(n)
			}
			fr := wire.Body{"name": name, "response": responseObject(m["content"])}
			if callID != "" {
				fr["id"] = callID
			}
			push("user", []any{wire.Body{"functionResponse": fr}})
		case "assistant":
			var parts []any
			if t := textOf(m["content"]); t != "" {
				parts = append(parts, wire.Body{"text": t})
			}
			first := true
			for _, rc := range wire.AsSlice(m["tool_calls"]) {
				call := wire.AsRecord(rc)
				fn := wire.AsRecord(call["function"])
				id := wire.AsString(call["id"])
				name := SanitizeToolName(wire.AsString(fn["name"]))
				if id != "" {
					toolNames[id] = name
				}
				args := parseArgs(fn["arguments"])
				fc := wire.Body{"name": name, "args": args}
				if id != "" {
					fc["id"] = id
				}
				part := wire.Body{"functionCall": fc}
				if sig, ok := ThoughtSignatureFor(name, args); ok {
					part["thoughtSignature"] = sig
				} else if first {
					part["thoughtSignature"] = skipThoughtSignature
				}
				first = false
				parts = append(parts, part)
			}
			push("model", parts)
		default:
			if t := textOf(m["content"]); t != "" {
				push("user", []any{wire.Body{"text": t}})
			}
		}
	}
	outContents := make([]any, len(contents))
	for i, c := range contents {
		outContents[i] = wire.Body{"role": c.role, "parts": c.parts}
	}
	req := wire.Body{"contents": outContents}
	if len(systemTexts) > 0 {
		req["systemInstruction"] = wire.Body{"parts": []any{wire.Body{"text": strings.Join(systemTexts, "\n\n")}}}
	}
	if tools := toolsToGemini(body["tools"]); tools != nil {
		req["tools"] = tools
	}
	cfg := wire.Body{}
	if wire.IsNumber(body["temperature"]) {
		cfg["temperature"] = body["temperature"]
	}
	if wire.IsNumber(body["top_p"]) {
		cfg["topP"] = body["top_p"]
	}
	max := body["max_completion_tokens"]
	if max == nil {
		max = body["max_tokens"]
	}
	if wire.IsNumber(max) {
		cfg["maxOutputTokens"] = max
	}
	switch s := body["stop"].(type) {
	case string:
		cfg["stopSequences"] = []any{s}
	case []any:
		var seqs []any
		for _, v := range s {
			if str, ok := v.(string); ok {
				seqs = append(seqs, str)
			}
		}
		if len(seqs) > 0 {
			cfg["stopSequences"] = seqs
		}
	}
	if len(cfg) > 0 {
		req["generationConfig"] = cfg
	}
	if tc := toolConfigFor(body["tool_choice"]); tc != nil {
		req["toolConfig"] = tc
	}
	return req
}

// Envelope wraps a Gemini request in the Cloud Code envelope.
func Envelope(project, model string, request wire.Body) wire.Body {
	if project == "" {
		project = "default-cli-project"
	}
	return wire.Body{"project": project, "model": model, "userAgent": "antigravity", "requestId": newID(16), "request": request}
}

func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Usage maps Gemini usageMetadata onto wire.Usage.
func Usage(raw any) wire.Usage {
	m := wire.AsRecord(raw)
	return wire.Usage{
		Input:     int(wire.Number(m["promptTokenCount"])),
		Output:    int(wire.Number(m["candidatesTokenCount"]) + wire.Number(m["thoughtsTokenCount"])),
		CacheRead: int(wire.Number(m["cachedContentTokenCount"])),
	}
}

var finishReasons = map[string]string{"STOP": "stop", "MAX_TOKENS": "length", "MAX_OUTPUT_TOKENS": "length", "SAFETY": "content_filter", "RECITATION": "content_filter", "BLOCKLIST": "content_filter", "PROHIBITED_CONTENT": "content_filter", "SPII": "content_filter", "OTHER": "stop"}

func partsOf(resp wire.Body) []wire.Body {
	cands := wire.AsSlice(resp["candidates"])
	if len(cands) == 0 {
		return nil
	}
	var out []wire.Body
	for _, p := range wire.AsSlice(wire.AsRecord(wire.AsRecord(cands[0])["content"])["parts"]) {
		out = append(out, wire.AsRecord(p))
	}
	return out
}

func rawFinish(resp wire.Body) string {
	cands := wire.AsSlice(resp["candidates"])
	if len(cands) == 0 {
		return ""
	}
	return wire.AsString(wire.AsRecord(cands[0])["finishReason"])
}

func finishOf(raw string, hasCalls bool) string {
	if hasCalls && (raw == "STOP" || raw == "OTHER" || raw == "") {
		return "tool_calls"
	}
	if r, ok := finishReasons[raw]; ok {
		return r
	}
	if hasCalls {
		return "tool_calls"
	}
	return "stop"
}

// ChatCompletion folds a non-stream Gemini reply into Chat Completions JSON.
func ChatCompletion(resp wire.Body, model string) wire.Body {
	var text string
	var calls []any
	pending := ""
	for _, part := range partsOf(resp) {
		sig := wire.AsString(part["thoughtSignature"])
		if part["thought"] == true {
			if sig != "" {
				pending = sig
			}
			continue
		}
		if t, ok := part["text"].(string); ok {
			text += t
		}
		if fc, ok := part["functionCall"].(map[string]any); ok {
			args := fc["args"]
			if args == nil {
				args = wire.Body{}
			}
			if sig == "" {
				sig = pending
			}
			rememberSignature(wire.AsString(fc["name"]), args, sig)
			pending = ""
			id := wire.AsString(fc["id"])
			if id == "" {
				id = "call_" + itoa(len(calls))
			}
			calls = append(calls, wire.Body{"index": len(calls), "id": id, "type": "function", "function": wire.Body{"name": wire.AsString(fc["name"]), "arguments": wire.MarshalJSON(args)}})
		}
	}
	msg := wire.Body{"role": "assistant", "content": nil}
	if text != "" {
		msg["content"] = text
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	u := Usage(resp["usageMetadata"])
	id := wire.AsString(resp["responseId"])
	if id == "" {
		id = "chatcmpl-" + newID(12)
	}
	if mv := wire.AsString(resp["modelVersion"]); mv != "" {
		model = mv
	}
	usage := wire.Body{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": u.Input + u.Output}
	if u.CacheRead > 0 {
		// Gemini counts cached content inside promptTokenCount (inclusive, like
		// OpenAI), so the folded usage must advertise the reads or the ledger
		// records zero cache hits.
		usage["prompt_tokens_details"] = wire.Body{"cached_tokens": u.CacheRead}
	}
	return wire.Body{
		"id": id, "object": "chat.completion", "created": float64(time.Now().Unix()), "model": model,
		"choices": []any{wire.Body{"index": 0, "message": msg, "finish_reason": finishOf(rawFinish(resp), len(calls) > 0)}},
		"usage":   usage,
	}
}

func itoa(n int) string { return wire.MarshalJSON(n) }

// ToChatStream translates streamed generateContent chunks into Chat
// Completions SSE. It implements wire.Translator.
type ToChatStream struct {
	Model    string
	OnFinish func(wire.Usage)
	OnEvent  func(wire.StreamEvent)

	id, created        string
	createdN           int64
	usage              wire.Usage
	callIndex          int
	roleSent, finished bool
	pending            string
}

// NewToChatStream builds the translator.
func NewToChatStream(model string, onFinish func(wire.Usage), onEvent func(wire.StreamEvent)) *ToChatStream {
	return &ToChatStream{Model: model, OnFinish: onFinish, OnEvent: onEvent, id: "chatcmpl-" + newID(12), createdN: time.Now().Unix()}
}

func (s *ToChatStream) chunk(delta wire.Body, finish any) wire.Body {
	return wire.Body{"id": s.id, "object": "chat.completion.chunk", "created": s.createdN, "model": s.Model,
		"choices": []any{wire.Body{"index": 0, "delta": delta, "finish_reason": finish}}}
}

func (s *ToChatStream) event(e wire.StreamEvent) {
	if s.OnEvent != nil {
		s.OnEvent(e)
	}
}

func (s *ToChatStream) finalChunk(reason string) wire.Body {
	c := s.chunk(wire.Body{}, reason)
	usage := wire.Body{"prompt_tokens": s.usage.Input, "completion_tokens": s.usage.Output, "total_tokens": s.usage.Input + s.usage.Output}
	if s.usage.CacheRead > 0 {
		// Gemini counts cached content inside promptTokenCount (inclusive, like
		// OpenAI); without this the ledger reads every Antigravity turn as a miss.
		usage["prompt_tokens_details"] = wire.Body{"cached_tokens": s.usage.CacheRead}
	}
	c["usage"] = usage
	return c
}

// Handle implements wire.Translator.
func (s *ToChatStream) Handle(event wire.Body, sink wire.EventSink) {
	resp := Unwrap(event)
	if !s.roleSent {
		sink.EmitData(s.chunk(wire.Body{"role": "assistant", "content": ""}, nil))
		s.roleSent = true
	}
	for _, part := range partsOf(resp) {
		sig := wire.AsString(part["thoughtSignature"])
		if part["thought"] == true {
			if sig != "" {
				s.pending = sig
			}
			continue
		}
		if t, ok := part["text"].(string); ok && t != "" {
			s.event(wire.StreamEvent{Kind: wire.StreamContent})
			sink.EmitData(s.chunk(wire.Body{"content": t}, nil))
		}
		if fc, ok := part["functionCall"].(map[string]any); ok {
			s.event(wire.StreamEvent{Kind: wire.StreamContent})
			id := wire.AsString(fc["id"])
			if id == "" {
				id = "call_" + itoa(s.callIndex)
			}
			args := fc["args"]
			if args == nil {
				args = wire.Body{}
			}
			if sig == "" {
				sig = s.pending
			}
			rememberSignature(wire.AsString(fc["name"]), args, sig)
			s.pending = ""
			sink.EmitData(s.chunk(wire.Body{"tool_calls": []any{wire.Body{"index": s.callIndex, "id": id, "type": "function",
				"function": wire.Body{"name": wire.AsString(fc["name"]), "arguments": wire.MarshalJSON(args)}}}}, nil))
			s.callIndex++
		}
	}
	if resp["usageMetadata"] != nil {
		s.usage = Usage(resp["usageMetadata"])
		s.event(wire.StreamEvent{Kind: wire.StreamUsage, Usage: s.usage})
	}
	if raw := rawFinish(resp); raw != "" && !s.finished {
		s.finished = true
		s.event(wire.StreamEvent{Kind: wire.StreamFinish})
		sink.EmitData(s.finalChunk(finishOf(raw, s.callIndex > 0)))
	}
}

// Finish implements wire.Translator.
func (s *ToChatStream) Finish(sink wire.EventSink) {
	if !s.finished {
		reason := "stop"
		if s.callIndex > 0 {
			reason = "tool_calls"
		}
		sink.EmitData(s.finalChunk(reason))
	}
	sink.Emit(wire.SSEDone)
	if s.OnFinish != nil {
		s.OnFinish(s.usage)
	}
}
