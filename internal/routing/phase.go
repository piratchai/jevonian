package routing

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

// PhaseSignals is what the heuristic classifier reads off a request.
// src/routing.ts PhaseSignals.
type PhaseSignals struct {
	Phase               string
	ConsecutiveFailures int
	HasToolResults      bool
	HasTools            bool
	RecentToolResults   []string
	// WithinTurn: the agent is handing tool results back rather than the user
	// speaking again — a mid-turn request.
	WithinTurn bool
}

// failurePattern recognises a failing tool result. src/routing.ts FAILURE_PATTERN.
// Go RE2 has no case-insensitive per-group flag mixing, so `\bFAIL\b` is matched
// case-sensitively via a second pattern, preserving the TS /i semantics where
// the whole expression is case-insensitive (FAIL ⊂ failure match anyway).
var failurePattern = regexp.MustCompile(
	`(?i)(\berror\b|\berrors\b|\bfailed\b|\bfailure\b|exception|traceback|assertionerror|npm err|exit code [1-9]|\bFAIL\b|✗)`,
)

// base64ImageData matches inline base64 images. src/compaction.ts BASE64_IMAGE_DATA.
var base64ImageData = regexp.MustCompile(
	`data:image/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]{100,}|"data"\s*:\s*"[A-Za-z0-9+/=]{100,}"`,
)

// imageTokenEstimate is the per-image token charge. src/compaction.ts IMAGE_TOKEN_ESTIMATE.
const imageTokenEstimate = 1200

// jsSpace mirrors JavaScript's `\s` (Unicode-aware), which RE2's `\s` is not.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func isASCIILetter(r rune) bool { return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') }
func isASCIIDigit(r rune) bool  { return r >= '0' && r <= '9' }

// utf16Len is JavaScript's String.length for s.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// EstimateTokens is the calibrated compaction estimator: a word costs one
// token per six letters, a digit half a token, any other symbol nine tenths,
// images a fixed tile charge. src/compaction.ts estimateTokens. The TS regex
// has no `u` flag, so every non-BMP rune is two UTF-16 units and each unit
// counts as its own symbol — mirrored here.
func EstimateTokens(text string) int {
	imageTokens := 0
	stripped := base64ImageData.ReplaceAllStringFunc(text, func(string) string {
		imageTokens += imageTokenEstimate
		return ""
	})
	tokens := float64(imageTokens)
	runes := []rune(stripped)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case isASCIILetter(r):
			j := i
			for j < len(runes) && isASCIILetter(runes[j]) {
				j++
			}
			tokens += float64(1 + (j-i-1)/6)
			i = j
		case isASCIIDigit(r):
			j := i
			for j < len(runes) && isASCIIDigit(runes[j]) {
				j++
			}
			tokens += float64(j-i) / 2
			i = j
		case jsSpace(r):
			i++
		default:
			if r >= 0x10000 {
				// Two UTF-16 units, each charged on its own: one 1.8 addition
				// rounds differently from two 0.9 additions, and ceil() can
				// turn that into a whole-token difference from the TS estimate.
				tokens += 0.9
				tokens += 0.9
			} else {
				tokens += 0.9
			}
			i++
		}
	}
	return int(math.Ceil(tokens))
}

// CompactionEstimate is the conversation's size in tokens for the context
// filter: messages + tools + system, times a 1.5 safety margin for bridged
// wires. src/routing.ts compactionEstimate.
func CompactionEstimate(body map[string]any) int {
	messages := body["messages"]
	if messages == nil {
		messages = body["input"]
	}
	if messages == nil {
		messages = ""
	}
	tools := body["tools"]
	if tools == nil {
		tools = []any{}
	}
	system := body["system"]
	if system == nil {
		system = body["instructions"]
	}
	if system == nil {
		system = ""
	}
	// JS JSON.stringify key order: messages, tools, system.
	envelope := orderedJSON([]kv{{"messages", messages}, {"tools", tools}, {"system", system}})
	return int(math.Ceil(float64(EstimateTokens(envelope)) * 1.5))
}

type kv struct {
	k string
	v any
}

func orderedJSON(pairs []kv) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(p.k)
		b.Write(key)
		b.WriteByte(':')
		b.WriteString(jsStringify(p.v))
	}
	b.WriteByte('}')
	return b.String()
}

// jsStringify marshals like JSON.stringify for decoded JSON values (no HTML
// escaping). Map key order differs from JS insertion order, which only
// perturbs the token estimate by key placement, not by count.
func jsStringify(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	out := strings.TrimSuffix(b.String(), "\n")
	// Go escapes U+2028/2029 even without HTML escaping; JSON.stringify does not.
	out = strings.ReplaceAll(out, `\u2028`, "\u2028")
	return strings.ReplaceAll(out, `\u2029`, "\u2029")
}

func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// stringifyContent flattens message content to text, replacing inline image
// data. src/routing.ts stringifyContent.
func stringifyContent(value any) string {
	switch v := value.(type) {
	case string:
		return base64ImageData.ReplaceAllString(v, "[image]")
	case []any:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			record := asRecord(block)
			if text, ok := record["text"].(string); ok {
				parts = append(parts, base64ImageData.ReplaceAllString(text, "[image]"))
				continue
			}
			if record["type"] == "image_url" || record["type"] == "image" {
				parts = append(parts, "[image]")
				continue
			}
			parts = append(parts, base64ImageData.ReplaceAllString(jsStringify(record), "[image]"))
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		return base64ImageData.ReplaceAllString(jsStringify(v), "[image]")
	}
}

var (
	trailingSpaceNL = regexp.MustCompile(`[ \t]+\n`)
	manyNL          = regexp.MustCompile(`\n{3,}`)
)

// ClassifyPhase reads plan/execute signals from message and tool shapes.
// src/routing.ts classifyPhase.
func ClassifyPhase(body map[string]any, kind RequestKind) PhaseSignals {
	messages := asArray(body["messages"])
	input := asArray(body["input"])
	tools := asArray(body["tools"])
	toolResults := []string{}
	latestUserMessage := -1
	for i, raw := range messages {
		message := asRecord(raw)
		if message["role"] != "user" {
			continue
		}
		if kind == KindAnthropic {
			for _, rawBlock := range asArray(message["content"]) {
				block := asRecord(rawBlock)
				if block["type"] == "text" && strings.TrimSpace(stringField(block, "text")) != "" {
					latestUserMessage = i
					break
				}
			}
		} else {
			latestUserMessage = i
		}
	}

	if kind == KindResponses {
		for _, raw := range input {
			item := asRecord(raw)
			if item["type"] == "function_call_output" {
				toolResults = append(toolResults, stringifyContent(item["output"]))
			}
			if item["type"] == "message" && item["role"] == "tool" {
				toolResults = append(toolResults, stringifyContent(item["content"]))
			}
		}
	}
	for _, raw := range messages {
		message := asRecord(raw)
		if kind == KindOpenAI {
			if message["role"] == "tool" {
				toolResults = append(toolResults, stringifyContent(message["content"]))
			}
			continue
		}
		if kind == KindResponses {
			continue
		}
		for _, block := range asArray(message["content"]) {
			record := asRecord(block)
			if record["type"] == "tool_result" {
				toolResults = append(toolResults, stringifyContent(record["content"]))
			}
		}
	}

	consecutive := 0
	if kind == KindResponses {
		latestUserMessage = -1
		for i, raw := range input {
			if item := asRecord(raw); item["type"] == "message" && item["role"] == "user" {
				latestUserMessage = i
			}
		}
	}
	activeToolResults := []string{}
	if latestUserMessage < 0 {
		activeToolResults = toolResults
	} else if kind == KindResponses {
		for i, raw := range input {
			item := asRecord(raw)
			if i > latestUserMessage && item["type"] == "function_call_output" {
				activeToolResults = append(activeToolResults, stringifyContent(item["output"]))
			}
		}
	} else {
		for i := latestUserMessage + 1; i < len(messages); i++ {
			message := asRecord(messages[i])
			if kind == KindOpenAI && message["role"] == "tool" {
				activeToolResults = append(activeToolResults, stringifyContent(message["content"]))
			} else if kind == KindAnthropic {
				for _, rawBlock := range asArray(message["content"]) {
					block := asRecord(rawBlock)
					if block["type"] == "tool_result" {
						activeToolResults = append(activeToolResults, stringifyContent(block["content"]))
					}
				}
			}
		}
	}
	// Phase, hasToolResults and withinTurn are turn-scoped: a fresh user
	// message after tool results reopens the plan phase. Failure streaks and
	// the recent-results window stay conversation-wide — work that was just
	// failing is still struggling work for the brain's board focus.
	hasToolResults := len(activeToolResults) > 0
	phase := "plan"
	if hasToolResults {
		phase = "execute"
	}
	for i := len(toolResults) - 1; i >= 0; i-- {
		if failurePattern.MatchString(toolResults[i]) {
			consecutive++
		} else {
			break
		}
	}
	// Keep signal, not harness boilerplate: when the tail is failing, drop the
	// trailing successes instead of the failures. src/routing.ts.
	tail := lastN(toolResults, 4)
	failed := []string{}
	for _, r := range tail {
		if failurePattern.MatchString(r) {
			failed = append(failed, r)
		}
	}
	source := toolResults
	if len(failed) > 0 {
		source = failed
	}
	recent := lastN(source, 2)
	recentOut := make([]string, 0, len(recent))
	for _, r := range recent {
		r = trailingSpaceNL.ReplaceAllString(r, "\n")
		r = manyNL.ReplaceAllString(r, "\n\n")
		recentOut = append(recentOut, truncateRunes(strings.TrimSpace(r), 300))
	}
	return PhaseSignals{
		Phase:               phase,
		ConsecutiveFailures: consecutive,
		HasToolResults:      hasToolResults,
		HasTools:            len(tools) > 0,
		RecentToolResults:   recentOut,
		WithinTurn:          hasToolResults,
	}
}

func lastN(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[len(list)-n:]
}

// truncateRunes cuts s to at most n UTF-16 code units like JS .slice(0, n);
// a surrogate pair that would straddle the cut is dropped whole.
func truncateRunes(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// ---- user-intent extraction (src/routing.ts §3.4 context envelopes) ----

var userQueryRe = regexp.MustCompile(`(?is)<user_query>(.*?)</user_query>`)

// ExtractUserQuery returns the content of a <user_query> wrapper, else the
// trimmed text. src/routing.ts extractUserQuery.
func ExtractUserQuery(text string) string {
	if m := userQueryRe.FindStringSubmatch(text); m != nil && m[1] != "" {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(text)
}

// htmlElements are markup the user is talking about, never an agent's context
// envelope. src/routing.ts HTML_ELEMENTS.
var htmlElements = func() map[string]bool {
	names := []string{
		"html", "head", "body", "title", "meta", "link", "script", "style", "div", "span", "p",
		"a", "img", "br", "hr", "em", "strong", "b", "i", "u", "s", "small", "h1", "h2", "h3",
		"h4", "h5", "h6", "blockquote", "pre", "code", "kbd", "samp", "var", "ul", "ol", "li",
		"dl", "dt", "dd", "table", "thead", "tbody", "tfoot", "tr", "td", "th", "section",
		"article", "aside", "nav", "header", "footer", "main", "figure", "figcaption", "form",
		"input", "textarea", "button", "select", "option", "label", "fieldset", "legend",
		"iframe", "video", "audio", "source", "track", "canvas", "svg", "path", "g", "defs",
		"use", "details", "summary", "dialog", "template", "slot", "picture", "map", "area",
		"object",
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}()

var openTagRe = regexp.MustCompile(`(?i)<([a-z][a-z0-9_:-]*)\b[^>]*>`)

// stripContextBlocks removes paired non-HTML tags (agent envelopes) and then
// truncates at a dangling non-HTML open tag. src/routing.ts stripContextBlocks.
// RE2 has no backreferences, so PAIRED_TAG (`<(name)…>…</\1>`) is emulated by
// scanning open tags and finding the nearest matching close tag.
func stripContextBlocks(text string) string {
	var b strings.Builder
	pos := 0
	for pos < len(text) {
		loc := openTagRe.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		end := pos + loc[1]
		name := text[pos+loc[2] : pos+loc[3]]
		closeRe := regexp.MustCompile(`(?i)</` + regexp.QuoteMeta(name) + `\s*>`)
		closeLoc := closeRe.FindStringIndex(text[end:])
		if closeLoc == nil {
			// Unpaired: leave as-is for the dangling-tag pass. The JS regex
			// retries from the next character, so a tag nested inside this
			// one's `[^>]*` span can still pair up — advance one byte only.
			b.WriteString(text[pos : start+1])
			pos = start + 1
			continue
		}
		closeEnd := end + closeLoc[1]
		b.WriteString(text[pos:start])
		if htmlElements[strings.ToLower(name)] {
			b.WriteString(text[start:closeEnd])
		} else {
			b.WriteString(" ")
		}
		pos = closeEnd
	}
	b.WriteString(text[pos:])
	stripped := b.String()
	for _, m := range openTagRe.FindAllStringSubmatchIndex(stripped, -1) {
		name := stripped[m[2]:m[3]]
		if !htmlElements[strings.ToLower(name)] {
			return stripped[:m[0]]
		}
	}
	return stripped
}

// jsWS is JavaScript's `\s` class; RE2's `\s` is ASCII-only.
const jsWS = `\s\x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var whitespaceRe = regexp.MustCompile(`[` + jsWS + `]+`)

// isContextOnly is true when nothing but injected context remains.
// src/routing.ts isContextOnly.
func isContextOnly(text string) bool {
	return whitespaceRe.ReplaceAllString(stripContextBlocks(text), "") == ""
}

// resolveAsk is the user's actual ask: an explicit <user_query> wins;
// otherwise the message, or "" when it carries only injected context.
// src/routing.ts resolveAsk.
func resolveAsk(text string) string {
	explicit := ExtractUserQuery(text)
	if explicit != strings.TrimSpace(text) {
		return explicit
	}
	if isContextOnly(text) {
		return ""
	}
	return explicit
}

// greetingRe matches openers that carry no task. src/routing.ts GREETING.
var greetingRe = regexp.MustCompile(
	`(?i)^(hi|hey|hello|yo|ok|okay|thanks|thank you|你好|嗨|哈喽|在吗|谢谢|继续|好的|嗯)[` + jsWS + `!,.。！？?~]*$`,
)

func messageText(message map[string]any, kind RequestKind) string {
	if kind == KindAnthropic {
		if s, ok := message["content"].(string); ok {
			return s
		}
		parts := []string{}
		for _, block := range asArray(message["content"]) {
			record := asRecord(block)
			if record["type"] == "text" {
				if t, ok := record["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return stringifyContent(message["content"])
}

// userMessages lists user texts across the three wire shapes.
// src/routing.ts userMessages.
func userMessages(body map[string]any, kind RequestKind) []string {
	texts := []string{}
	for _, raw := range asArray(body["messages"]) {
		message := asRecord(raw)
		if message["role"] != "user" {
			continue
		}
		if kind == KindAnthropic {
			if s, ok := message["content"].(string); ok {
				texts = append(texts, s)
				continue
			}
			for _, block := range asArray(message["content"]) {
				record := asRecord(block)
				if record["type"] == "text" {
					if t, ok := record["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
			continue
		}
		texts = append(texts, stringifyContent(message["content"]))
	}
	for _, raw := range asArray(body["input"]) {
		item := asRecord(raw)
		if item["type"] == "message" && item["role"] == "user" {
			texts = append(texts, stringifyContent(item["content"]))
		}
	}
	if s, ok := body["input"].(string); ok {
		texts = append(texts, s)
	}
	out := texts[:0]
	for _, t := range texts {
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return out
}

func assistantMessages(body map[string]any, kind RequestKind) []string {
	texts := []string{}
	for _, raw := range asArray(body["messages"]) {
		message := asRecord(raw)
		if message["role"] != "assistant" {
			continue
		}
		if t := strings.TrimSpace(messageText(message, kind)); t != "" {
			texts = append(texts, t)
		}
	}
	for _, raw := range asArray(body["input"]) {
		item := asRecord(raw)
		if item["type"] == "message" && item["role"] == "assistant" {
			if t := strings.TrimSpace(stringifyContent(item["content"])); t != "" {
				texts = append(texts, t)
			}
		}
	}
	return texts
}

// RecentMessage is one compact role/text pair for the brain state.
type RecentMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func recentMessages(body map[string]any, kind RequestKind, limit int) []RecentMessage {
	source := asArray(body["messages"])
	if len(source) == 0 {
		source = asArray(body["input"])
	}
	out := []RecentMessage{}
	for _, raw := range source {
		message := asRecord(raw)
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		text := whitespaceRe.ReplaceAllString(resolveAsk(messageText(message, kind)), " ")
		text = truncateRunes(strings.TrimSpace(text), 200)
		if text == "" {
			continue
		}
		out = append(out, RecentMessage{Role: role, Text: text})
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// shellToolName matches shell-like tools whose args trip Cloudflare WAF in
// front of TypeSafe. src/routing.ts SHELL_TOOL_NAME.
var shellToolName = regexp.MustCompile(`(?i)^(shell|bash|local_shell)$`)

func formatToolCallForBrain(name, args string) string {
	if shellToolName.MatchString(name) {
		return name + "(<command redacted>)"
	}
	return name + "(" + truncateRunes(args, 80) + ")"
}

func recentToolCalls(body map[string]any, kind RequestKind, limit int) []string {
	calls := []string{}
	for _, raw := range asArray(body["messages"]) {
		message := asRecord(raw)
		if message["role"] != "assistant" {
			continue
		}
		if kind == KindAnthropic {
			for _, block := range asArray(message["content"]) {
				record := asRecord(block)
				if record["type"] != "tool_use" {
					continue
				}
				name, _ := record["name"].(string)
				if name == "" {
					name = "tool"
				}
				input := record["input"]
				if input == nil {
					input = map[string]any{}
				}
				calls = append(calls, formatToolCallForBrain(name, jsStringify(input)))
			}
			continue
		}
		for _, rawCall := range asArray(message["tool_calls"]) {
			fn := asRecord(asRecord(rawCall)["function"])
			name, _ := fn["name"].(string)
			if name == "" {
				name = "tool"
			}
			args, ok := fn["arguments"].(string)
			if !ok {
				a := fn["arguments"]
				if a == nil {
					a = map[string]any{}
				}
				args = jsStringify(a)
			}
			calls = append(calls, formatToolCallForBrain(name, args))
		}
	}
	// Responses shape: tool calls are top-level items. src/routing.ts.
	for _, raw := range asArray(body["input"]) {
		item := asRecord(raw)
		switch item["type"] {
		case "function_call":
			name, _ := item["name"].(string)
			if name == "" {
				name = "tool"
			}
			args, ok := item["arguments"].(string)
			if !ok {
				args = "{}"
			}
			calls = append(calls, formatToolCallForBrain(name, args))
		case "local_shell_call":
			cmd := asRecord(item["action"])["command"]
			if cmd == nil {
				cmd = "command"
			}
			calls = append(calls, formatToolCallForBrain("shell", jsStringify(cmd)))
		}
	}
	return lastN(calls, limit)
}

func sessionGoal(body map[string]any, kind RequestKind) string {
	fallback := ""
	for _, text := range userMessages(body, kind) {
		query := resolveAsk(text)
		if query == "" {
			continue
		}
		if fallback == "" {
			fallback = query
		}
		compact := strings.TrimSpace(whitespaceRe.ReplaceAllString(query, " "))
		if utf16Len(compact) < 12 || greetingRe.MatchString(compact) {
			continue
		}
		return truncateRunes(compact, 400)
	}
	return truncateRunes(fallback, 400)
}

// LastUserMessage walks back past trailing injected blocks to the real ask.
// src/routing.ts lastUserMessage.
func LastUserMessage(body map[string]any, kind RequestKind) string {
	texts := userMessages(body, kind)
	for i := len(texts) - 1; i >= 0; i-- {
		if q := resolveAsk(texts[i]); q != "" {
			return truncateRunes(q, 3_000)
		}
	}
	return ""
}

// FullTranscript is the ≤400k-char transcript for fullPrompt brains.
// src/routing.ts fullTranscript.
func FullTranscript(body map[string]any, kind RequestKind) string {
	messages := asArray(body["messages"])
	if messages == nil {
		messages = asArray(body["input"])
	}
	lines := []string{}
	for _, raw := range messages {
		message := asRecord(raw)
		role, _ := message["role"].(string)
		if role == "" {
			role, _ = message["type"].(string)
		}
		if role == "" {
			role = "item"
		}
		if t := strings.TrimSpace(messageText(message, kind)); t != "" {
			lines = append(lines, "["+role+"] "+truncateRunes(t, 64_000))
		}
	}
	prefix := ""
	system, ok := body["system"].(string)
	if !ok {
		system, _ = body["instructions"].(string)
	}
	if system != "" {
		prefix = "[system] " + system + "\n"
	}
	return truncateRunes(prefix+strings.Join(lines, "\n"), 400_000)
}

func messageCount(body map[string]any) int {
	if a, ok := body["messages"].([]any); ok {
		return len(a)
	}
	if a, ok := body["input"].([]any); ok {
		return len(a)
	}
	if _, ok := body["input"].(string); ok {
		return 1
	}
	return 0
}

// ---- session key / headers ----

func firstHeader(headers map[string]string, names ...string) string {
	for _, n := range names {
		if v := headers[n]; v != "" {
			return v
		}
	}
	return ""
}

func stringField(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

// ResolveSessionKey picks the session id: explicit headers / body fields
// first, else a fingerprint of the conversation head.
// src/routing.ts resolveSessionKey.
func ResolveSessionKey(body map[string]any, headers map[string]string) string {
	if v := firstHeader(headers, "x-session-id", "x-jevonian-session", "x-opencode-session"); v != "" {
		return v
	}
	for _, key := range []string{"previous_response_id", "prompt_cache_key"} {
		if v := stringField(body, key); v != "" {
			return v
		}
	}
	if uid, _ := asRecord(body["metadata"])["user_id"].(string); uid != "" {
		return uid
	}
	if v := stringField(body, "user"); v != "" {
		return v
	}
	system := body["system"]
	if system == nil {
		system = body["instructions"]
	}
	messages := body["messages"]
	if messages == nil {
		messages = body["input"]
	}
	return SessionFingerprint(system, body["tools"], messages)
}

// headerEffort is the thinking level the caller demanded.
// src/routing.ts headerEffort.
func headerEffort(headers map[string]string) string {
	raw := firstHeader(headers, RequestHeaderEffort, RequestHeaderReasoningEffort)
	if IsReasoningEffort(raw) {
		return raw
	}
	return ""
}
