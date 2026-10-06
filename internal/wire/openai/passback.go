package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

// ReasoningContent passback — port of src/reasoning-passback.ts.
//
// DeepSeek (and Moonshot/Kimi) thinking mode requires `reasoning_content` on
// assistant turns to be replayed whenever the request carries `tools`.
// Clients like Cursor drop that field after the first tool call, so the next
// upstream request fails with "The reasoning_content in the thinking mode
// must be passed back to the API." Jevonian sits between the client and the
// provider: capture the field from upstream responses, then reinject it into
// later outbound chat histories.

const (
	passbackMaxEntries = 4000
	passbackMaxAge     = 6 * time.Hour
)

// NeedsReasoningPassback reports whether this upstream is known to reject
// missing `reasoning_content` on tool-bearing turns (DeepSeek thinking mode,
// Moonshot/Kimi thinking).
func NeedsReasoningPassback(providerName, model, baseURL string) bool {
	haystack := strings.ToLower(providerName + " " + model + " " + baseURL)
	if strings.Contains(haystack, "deepseek") || strings.Contains(haystack, "moonshot") {
		return true
	}
	// Match "kimi" as a whole word.
	for i := 0; i+4 <= len(haystack); i++ {
		if haystack[i:i+4] == "kimi" {
			before := i == 0 || !isLowerLetter(haystack[i-1])
			after := i+4 == len(haystack) || !isLowerLetter(haystack[i+4])
			if before && after {
				return true
			}
		}
	}
	return false
}

func isLowerLetter(b byte) bool { return b >= 'a' && b <= 'z' }

type passbackEntry struct {
	reasoning string
	at        time.Time
}

// PassbackCache is the in-process store mapping message fingerprints to their
// captured reasoning. The zero value is ready to use; the shared Default is
// what the pipeline uses.
type PassbackCache struct {
	mu      sync.Mutex
	entries map[string]passbackEntry
	order   []string // oldest first; entries moved to back on touch
	pos     map[string]int
}

// Default is the process-wide cache, mirroring the TS module-level Map.
var Default = &PassbackCache{entries: map[string]passbackEntry{}, pos: map[string]int{}}

func (c *PassbackCache) ensure() {
	if c.entries == nil {
		c.entries = map[string]passbackEntry{}
		c.pos = map[string]int{}
	}
}

func (c *PassbackCache) put(key, reasoning string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	c.removeLocked(key)
	c.entries[key] = passbackEntry{reasoning: reasoning, at: time.Now()}
	c.pos[key] = len(c.order)
	c.order = append(c.order, key)
	c.pruneLocked()
}

func (c *PassbackCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Since(entry.at) > passbackMaxAge {
		c.removeLocked(key)
		return "", false
	}
	// Refresh LRU order: move the key to the back.
	c.removeLocked(key)
	c.entries[key] = entry
	c.pos[key] = len(c.order)
	c.order = append(c.order, key)
	return entry.reasoning, true
}

func (c *PassbackCache) removeLocked(key string) {
	if _, ok := c.entries[key]; !ok {
		return
	}
	delete(c.entries, key)
	if i, ok := c.pos[key]; ok {
		c.order = append(c.order[:i], c.order[i+1:]...)
		delete(c.pos, key)
		for j := i; j < len(c.order); j++ {
			c.pos[c.order[j]] = j
		}
	}
}

func (c *PassbackCache) pruneLocked() {
	now := time.Now()
	for _, key := range append([]string(nil), c.order...) {
		if now.Sub(c.entries[key].at) > passbackMaxAge {
			c.removeLocked(key)
		}
	}
	for len(c.order) > passbackMaxEntries {
		oldest := c.order[0]
		c.removeLocked(oldest)
	}
}

// Size reports the number of cached entries — test instrumentation.
func (c *PassbackCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Clear empties the cache — tests only.
func (c *PassbackCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]passbackEntry{}
	c.order = nil
	c.pos = map[string]int{}
}

func sha256Hex(payload any) string {
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func contentFingerprint(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	if parts, ok := content.([]any); ok {
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			block := wire.AsRecord(part)
			if text, ok := block["text"].(string); ok {
				typ := block["type"]
				if typ == nil {
					typ = "text"
				}
				out = append(out, wire.Body{"type": typ, "text": text})
			} else {
				out = append(out, block)
			}
		}
		return out
	}
	if content == nil {
		return ""
	}
	return content
}

func normalizeToolCall(raw any) wire.Body {
	call := wire.AsRecord(raw)
	fn := wire.AsRecord(call["function"])
	var arguments string
	switch v := fn["arguments"].(type) {
	case string:
		arguments = v
	case nil:
		arguments = ""
	default:
		arguments = wire.MarshalJSON(v)
	}
	var id any
	if s, ok := call["id"].(string); ok {
		id = s
	}
	typ := "function"
	if s, ok := call["type"].(string); ok {
		typ = s
	}
	return wire.Body{
		"id":   id,
		"type": typ,
		"function": wire.Body{
			"name":      wire.AsString(fn["name"]),
			"arguments": arguments,
		},
	}
}

func toolCallIDs(message wire.Body) []string {
	var ids []string
	for _, raw := range wire.AsSlice(message["tool_calls"]) {
		if id, ok := wire.AsRecord(raw)["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func toolCallSignatures(message wire.Body) []string {
	var sigs []string
	for _, raw := range wire.AsSlice(message["tool_calls"]) {
		normalized := normalizeToolCall(raw)
		rest := wire.Body{
			"type":     normalized["type"],
			"function": normalized["function"],
		}
		sigs = append(sigs, sha256Hex(rest))
	}
	return sigs
}

// MessageSignature is a stable fingerprint of an assistant message, ignoring
// reasoning_content.
func MessageSignature(message wire.Body) string {
	var calls []any
	for _, raw := range wire.AsSlice(message["tool_calls"]) {
		calls = append(calls, normalizeToolCall(raw))
	}
	return sha256Hex(wire.Body{
		"content":    contentFingerprint(message["content"]),
		"tool_calls": calls,
	})
}

func canonicalScopeMessage(message wire.Body) wire.Body {
	canonical := wire.Body{"role": message["role"]}
	if _, present := message["content"]; present {
		canonical["content"] = contentFingerprint(message["content"])
	}
	if s, ok := message["name"].(string); ok {
		canonical["name"] = s
	}
	if s, ok := message["tool_call_id"].(string); ok {
		canonical["tool_call_id"] = s
	}
	if calls := wire.AsSlice(message["tool_calls"]); calls != nil {
		var normalized []any
		for _, raw := range calls {
			normalized = append(normalized, normalizeToolCall(raw))
		}
		canonical["tool_calls"] = normalized
	}
	return canonical
}

// ConversationScope hashes the conversation prefix used to isolate
// concurrent chats.
func ConversationScope(messages []wire.Body, namespace string) string {
	scope := make([]any, 0, len(messages))
	for _, m := range messages {
		scope = append(scope, canonicalScopeMessage(m))
	}
	if namespace != "" {
		return sha256Hex(wire.Body{"namespace": namespace, "messages": scope})
	}
	return sha256Hex(scope)
}

func (c *PassbackCache) lookupKeys(message wire.Body, scope, namespace string) []string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	add("scope:" + scope + ":signature:" + MessageSignature(message))
	for _, id := range toolCallIDs(message) {
		add("scope:" + scope + ":tool_call:" + id)
	}
	for _, sig := range toolCallSignatures(message) {
		add("scope:" + scope + ":tool_call_signature:" + sig)
	}
	if namespace != "" {
		// Portable fallbacks when the prefix hash drifts (compaction, soft
		// edits) but tool-call ids or the message body still match within
		// this session.
		add("ns:" + namespace + ":signature:" + MessageSignature(message))
		for _, id := range toolCallIDs(message) {
			add("ns:" + namespace + ":tool_call:" + id)
		}
		for _, sig := range toolCallSignatures(message) {
			add("ns:" + namespace + ":tool_call_signature:" + sig)
		}
	}
	return keys
}

// RememberAssistantReasoning persists an assistant message's reasoning under
// every lookup key later turns might use to find it again. Returns how many
// keys were stored.
func (c *PassbackCache) RememberAssistantReasoning(message wire.Body, priorMessages []wire.Body, namespace string) int {
	if message["role"] != "assistant" {
		return 0
	}
	reasoning, ok := message["reasoning_content"].(string)
	if !ok {
		return 0
	}
	scope := ConversationScope(priorMessages, namespace)
	keys := c.lookupKeys(message, scope, namespace)
	for _, key := range keys {
		c.put(key, reasoning)
	}
	return len(keys)
}

func requestHasTools(body wire.Body) bool {
	return len(wire.AsSlice(body["tools"])) > 0
}

func assistantNeedsReasoning(message wire.Body, prior []wire.Body, hasTools bool) bool {
	if !hasTools {
		// Without tools, DeepSeek ignores reasoning on later turns — no
		// repair needed.
		return false
	}
	// With tools present, every assistant turn must carry reasoning_content
	// — including turns that never called a tool.
	if message["role"] != "assistant" {
		return false
	}
	if len(wire.AsSlice(message["tool_calls"])) > 0 {
		return true
	}
	for i := len(prior) - 1; i >= 0; i-- {
		switch prior[i]["role"] {
		case "tool":
			return true
		case "user", "system":
			return true
		}
	}
	// Final assistant reply after a tool loop, or a plain assistant turn
	// while tools are advertised — DeepSeek still requires the field.
	return true
}

// RepairStats reports what RepairReasoningContent did.
type RepairStats struct {
	Patched        int
	EmptyFilled    int
	AlreadyPresent int
}

// RepairReasoningContent reinjects cached `reasoning_content` into a
// chat-completions body before it goes upstream. When a required field is
// still missing, fills "" so the provider accepts the request (the same
// contract DeepSeek documents for turns that produced no reasoning text).
func (c *PassbackCache) RepairReasoningContent(body wire.Body, namespace string) (wire.Body, RepairStats) {
	var stats RepairStats
	if !requestHasTools(body) {
		return body, stats
	}
	messages := wire.AsSlice(body["messages"])
	if messages == nil {
		return body, stats
	}
	next := make([]wire.Body, 0, len(messages))
	for _, raw := range messages {
		next = append(next, wire.AsRecord(raw))
	}
	changed := false

	for i := 0; i < len(next); i++ {
		message := next[i]
		if !assistantNeedsReasoning(message, next[:i], true) {
			continue
		}
		if _, ok := message["reasoning_content"].(string); ok {
			stats.AlreadyPresent++
			// Keep whatever the client already sent, and refresh the cache
			// from it.
			c.RememberAssistantReasoning(message, next[:i], namespace)
			continue
		}
		scope := ConversationScope(next[:i], namespace)
		var restored string
		found := false
		for _, key := range c.lookupKeys(message, scope, namespace) {
			if value, ok := c.get(key); ok {
				restored = value
				found = true
				break
			}
		}
		reasoning := restored
		patched := cloneBody(message)
		patched["reasoning_content"] = reasoning
		next[i] = patched
		changed = true
		if found {
			stats.Patched++
		} else {
			stats.EmptyFilled++
		}
	}

	if !changed {
		return body, stats
	}
	out := cloneBody(body)
	converted := make([]any, 0, len(next))
	for _, m := range next {
		converted = append(converted, m)
	}
	out["messages"] = converted
	return out, stats
}

// RememberFromChatCompletion captures reasoning from a non-streaming chat
// completion response.
func (c *PassbackCache) RememberFromChatCompletion(jsonBody wire.Body, priorMessages []wire.Body, namespace string) int {
	stored := 0
	for _, raw := range wire.AsSlice(jsonBody["choices"]) {
		message := wire.AsRecord(wire.AsRecord(raw)["message"])
		stored += c.RememberAssistantReasoning(message, priorMessages, namespace)
	}
	return stored
}

// streamChoiceState accumulates one streamed choice.
type streamChoiceState struct {
	content      strings.Builder
	reasoning    strings.Builder
	hasReasoning bool
	toolCalls    []wire.Body
	finishReason string
}

// ReasoningStreamAccumulator assembles OpenAI chat-completion SSE deltas so
// reasoning can be stored once tool-call ids (or the finish reason) are
// known — the port of ReasoningStreamAccumulator.
type ReasoningStreamAccumulator struct {
	cache   *PassbackCache
	choices map[int]*streamChoiceState
	order   []int
}

// NewReasoningStreamAccumulator builds an accumulator bound to a cache (nil
// uses Default).
func NewReasoningStreamAccumulator(cache *PassbackCache) *ReasoningStreamAccumulator {
	if cache == nil {
		cache = Default
	}
	return &ReasoningStreamAccumulator{cache: cache, choices: map[int]*streamChoiceState{}}
}

// Ingest consumes one decoded chat.completion.chunk.
func (a *ReasoningStreamAccumulator) Ingest(chunk wire.Body) {
	for _, raw := range wire.AsSlice(chunk["choices"]) {
		choice := wire.AsRecord(raw)
		index := int(wire.Number(choice["index"]))
		state, ok := a.choices[index]
		if !ok {
			state = &streamChoiceState{}
			a.choices[index] = state
			a.order = append(a.order, index)
		}
		if reason, ok := choice["finish_reason"].(string); ok {
			state.finishReason = reason
		}
		delta := wire.AsRecord(choice["delta"])
		if s, ok := delta["content"].(string); ok {
			state.content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			state.hasReasoning = true
			state.reasoning.WriteString(s)
		} else if s, ok := delta["reasoning"].(string); ok {
			// OpenRouter-style alias.
			state.hasReasoning = true
			state.reasoning.WriteString(s)
		}
		a.mergeToolCalls(state, delta["tool_calls"])
	}
}

// Store persists any choice that has reasoning and either finished or
// already carries identified tool calls. Returns how many keys were stored.
func (a *ReasoningStreamAccumulator) Store(priorMessages []wire.Body, namespace string) int {
	stored := 0
	for _, index := range a.order {
		state := a.choices[index]
		if !state.hasReasoning {
			continue
		}
		ready := state.finishReason != "" ||
			(len(state.toolCalls) > 0 && allIDsPresent(state.toolCalls))
		if !ready {
			continue
		}
		message := wire.Body{
			"role":              "assistant",
			"content":           state.content.String(),
			"reasoning_content": state.reasoning.String(),
		}
		if len(state.toolCalls) > 0 {
			calls := make([]any, 0, len(state.toolCalls))
			for _, call := range state.toolCalls {
				calls = append(calls, call)
			}
			message["tool_calls"] = calls
		}
		stored += a.cache.RememberAssistantReasoning(message, priorMessages, namespace)
	}
	return stored
}

func allIDsPresent(calls []wire.Body) bool {
	for _, call := range calls {
		id, ok := call["id"].(string)
		if !ok || id == "" {
			return false
		}
	}
	return true
}

func (a *ReasoningStreamAccumulator) mergeToolCalls(state *streamChoiceState, deltas any) {
	for _, raw := range wire.AsSlice(deltas) {
		delta := wire.AsRecord(raw)
		index := int(wire.Number(delta["index"]))
		if !wire.IsNumber(delta["index"]) {
			index = len(state.toolCalls)
		}
		for len(state.toolCalls) <= index {
			state.toolCalls = append(state.toolCalls, wire.Body{
				"type":     "function",
				"function": wire.Body{"name": "", "arguments": ""},
			})
		}
		call := state.toolCalls[index]
		if id, ok := delta["id"].(string); ok && id != "" {
			call["id"] = id
		}
		if typ, ok := delta["type"].(string); ok {
			call["type"] = typ
		}
		fnDelta := wire.AsRecord(delta["function"])
		fn := wire.AsRecord(call["function"])
		if name, ok := fnDelta["name"].(string); ok && name != "" {
			if existing, ok := fn["name"].(string); ok {
				fn["name"] = existing + name
			} else {
				fn["name"] = name
			}
		}
		if args, ok := fnDelta["arguments"].(string); ok {
			if existing, ok := fn["arguments"].(string); ok {
				fn["arguments"] = existing + args
			} else {
				fn["arguments"] = args
			}
		}
		call["function"] = fn
	}
}

// ReasoningCaptureTranslator is a wire.TeeTranslator that tees an OpenAI
// chat SSE stream, remembering reasoning without altering bytes sent to the
// client — the port of reasoningCaptureTransform. Attach with
// TranslatorStream.WithTee.
type ReasoningCaptureTranslator struct {
	accumulator   *ReasoningStreamAccumulator
	priorMessages []wire.Body
	namespace     string
}

// NewReasoningCapture builds the tee: it observes decoded events and stores
// assembled reasoning into `cache` (nil uses Default).
func NewReasoningCapture(priorMessages []wire.Body, namespace string, cache *PassbackCache) *ReasoningCaptureTranslator {
	return &ReasoningCaptureTranslator{
		accumulator:   NewReasoningStreamAccumulator(cache),
		priorMessages: priorMessages,
		namespace:     namespace,
	}
}

// Ingest implements wire.TeeTranslator.
func (t *ReasoningCaptureTranslator) Ingest(event wire.Body) {
	t.accumulator.Ingest(event)
	t.accumulator.Store(t.priorMessages, t.namespace)
}

// Flush implements wire.TeeTranslator.
func (t *ReasoningCaptureTranslator) Flush() {
	t.accumulator.Store(t.priorMessages, t.namespace)
}

func cloneBody(m wire.Body) wire.Body {
	next := make(wire.Body, len(m))
	for k, v := range m {
		next[k] = v
	}
	return next
}
