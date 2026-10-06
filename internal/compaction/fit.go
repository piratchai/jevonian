package compaction

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Options tune compaction. Nil/NaN fields take the defaults.
type Options struct {
	Goal                   string
	KeepThreshold          *float64
	PreserveRecentMessages *float64
	MaxStateTokens         *float64
	MaxRequestTokens       *float64
	TruncateHeadChars      *float64
}

// Resolved is Options with defaults applied.
type Resolved struct {
	Goal                   string
	KeepThreshold          float64
	PreserveRecentMessages int
	MaxStateTokens         int
	MaxRequestTokens       int
	TruncateHeadChars      int
}

// F returns a pointer to v, for building Options literals.
func F(v float64) *float64 { return &v }

func finite(v *float64, fallback float64) float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return fallback
	}
	return *v
}

// ResolveOptions fills in defaults and ignores non-finite values.
func ResolveOptions(o Options) Resolved {
	return Resolved{
		Goal:                   o.Goal,
		KeepThreshold:          finite(o.KeepThreshold, 0.5),
		PreserveRecentMessages: int(math.Max(0, math.Floor(finite(o.PreserveRecentMessages, 6)))),
		MaxStateTokens:         int(math.Max(1, finite(o.MaxStateTokens, 25_000))),
		MaxRequestTokens:       int(math.Max(1, finite(o.MaxRequestTokens, 30_000))),
		TruncateHeadChars:      int(math.Max(0, math.Floor(finite(o.TruncateHeadChars, 300)))),
	}
}

const requestOverheadTokens = 20

// StateContext is the fixed `context` field of the Jev state.
const StateContext = "A coding assistant conversation is being compacted to free context. `history` is the whole conversation so far, oldest first; tool outputs are replaced by a short `result` note and long texts may be abridged. Each question asks whether one tool call, or the full output of that call, still needs to stay in the history verbatim. Whatever is not kept is deleted permanently, but the assistant can always re-run a tool or re-read a file."

var inputChars = []int{1000, 200, 60}

const (
	textHead = 400
	textTail = 150
)

// ToolCall is a tool call paired with its result.
type ToolCall struct {
	ID          string
	ToolUseID   string
	Tool        string
	Input       map[string]any
	Keys        []string
	CallIndex   int
	ResultIndex int
	ResultChars int
	IsError     bool
	Pinned      bool
}

// HistoryToolCall is the structured per-call form in the state.
type HistoryToolCall struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Result string `json:"result"`
}

// HistoryEntry is one state entry. ToolCalls is []HistoryToolCall or []string.
type HistoryEntry struct {
	I         int    `json:"i"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	ToolCalls any    `json:"tool_calls,omitempty"`
}

// State is what Jev sees.
type State struct {
	Context string         `json:"context"`
	Goal    string         `json:"goal"`
	History []HistoryEntry `json:"history"`
}

func truncate(text string, limit int) string {
	if ulen(text) <= limit {
		return text
	}
	n := limit - 1
	if n < 0 {
		n = 0
	}
	return usub(text, 0, n) + "…"
}

func abridge(text string, head, tail int) string {
	n := ulen(text)
	if n <= head+tail+40 {
		return text
	}
	return fmt.Sprintf("%s\n[… %d chars omitted …]\n%s", usub(text, 0, head), n-head-tail, usub(text, n-tail, n))
}

func isPinned(index, total, preserve int) bool { return index == 0 || index >= total-preserve }

// CollectToolCalls pairs every tool_use with its result by id. Calls without a
// result are not candidates.
func CollectToolCalls(messages []Message, preserve int) []ToolCall {
	type found struct {
		index  int
		result ToolResult
	}
	results := map[string]found{}
	for i, m := range messages {
		for _, r := range m.ToolResults {
			results[r.ToolUseID] = found{i, r}
		}
	}
	calls := []ToolCall{}
	for callIndex, m := range messages {
		for _, tool := range m.ToolUses {
			f, ok := results[tool.ToolUseID]
			if !ok {
				continue
			}
			calls = append(calls, ToolCall{
				ID: fmt.Sprintf("t%d", len(calls)+1), ToolUseID: tool.ToolUseID, Tool: tool.Tool,
				Input: tool.Input, Keys: tool.Keys, CallIndex: callIndex, ResultIndex: f.index,
				ResultChars: ulen(f.result.Text), IsError: f.result.IsError,
				Pinned: isPinned(callIndex, len(messages), preserve) || isPinned(f.index, len(messages), preserve),
			})
		}
	}
	return calls
}

func callInputText(c ToolCall, limit int) string {
	return truncate(orderedObject(c.Input, c.Keys), limit)
}

func resultNote(c ToolCall) string {
	kind := "ok"
	if c.IsError {
		kind = "error"
	}
	return fmt.Sprintf("%s, %d chars (omitted)", kind, c.ResultChars)
}

var wsRun = regexp.MustCompile(`[\s\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)

func compactCall(c ToolCall) string {
	keys := c.Keys
	if len(keys) != len(c.Input) {
		keys = sortedKeys(c.Input)
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := c.Input[k]
		var text string
		if s, ok := v.(string); ok {
			text = s
		} else {
			text = truncate(jsJSON(map[string]any{k: v}), 200)
		}
		parts = append(parts, k+"="+wsRun.ReplaceAllString(text, " "))
	}
	kind := "ok"
	if c.IsError {
		kind = "error"
	}
	return fmt.Sprintf("%s %s %s → %s %dch", c.ID, c.Tool, truncate(strings.Join(parts, " "), inputChars[2]), kind, c.ResultChars)
}

func callsByMessage(calls []ToolCall) map[int][]ToolCall {
	by := map[int][]ToolCall{}
	for _, c := range calls {
		by[c.CallIndex] = append(by[c.CallIndex], c)
	}
	return by
}

func historyEntries(messages []Message, calls []ToolCall, chars int) []HistoryEntry {
	by := callsByMessage(calls)
	entries := []HistoryEntry{}
	for i, m := range messages {
		var tcs []HistoryToolCall
		for _, c := range by[i] {
			tcs = append(tcs, HistoryToolCall{ID: c.ID, Tool: c.Tool, Input: callInputText(c, chars), Result: resultNote(c)})
		}
		if strings.TrimSpace(m.Text) == "" && len(tcs) == 0 {
			continue
		}
		e := HistoryEntry{I: i, Role: m.Role, Text: base64Image.ReplaceAllString(m.Text, "[image data omitted]")}
		if len(tcs) > 0 {
			e.ToolCalls = tcs
		}
		entries = append(entries, e)
	}
	return entries
}

var base64Image = regexp.MustCompile(`data:image/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]{100,}|"data"\s*:\s*"[A-Za-z0-9+/=]{100,}"`)

// GoalFromMessages is the last three user prompts.
func GoalFromMessages(messages []Message) string {
	var texts []string
	for _, m := range messages {
		if m.Role == "user" && strings.TrimSpace(m.Text) != "" && len(m.ToolResults) == 0 {
			texts = append(texts, m.Text)
		}
	}
	if len(texts) > 3 {
		texts = texts[len(texts)-3:]
	}
	for i, t := range texts {
		texts[i] = truncate(base64Image.ReplaceAllString(t, "[image]"), 500)
	}
	return strings.Join(texts, "\n")
}

// Fitted is a state that fits the ceiling.
type Fitted struct {
	State  State
	Tokens int
	Stage  string
}

func entryTokens(e HistoryEntry) int { return EstimateTokens(jsJSON(e)) + 1 }

func mergeCallRuns(history []HistoryEntry, pinned func(HistoryEntry) bool) []HistoryEntry {
	foldable := func(e HistoryEntry) bool {
		if pinned(e) || len(e.Text) != 0 {
			return false
		}
		s, ok := e.ToolCalls.([]string)
		return ok && len(s) > 0
	}
	merged := []HistoryEntry{}
	for _, e := range history {
		if n := len(merged); n > 0 && foldable(merged[n-1]) && foldable(e) && merged[n-1].Role == e.Role {
			prev := merged[n-1].ToolCalls.([]string)
			merged[n-1].ToolCalls = append(append([]string{}, prev...), e.ToolCalls.([]string)...)
			continue
		}
		merged = append(merged, e)
	}
	return merged
}

// FitState builds the Jev state and shrinks it in stages until it fits
// MaxStateTokens. It returns an error when even the newest entry cannot fit.
func FitState(messages []Message, calls []ToolCall, o Resolved) (Fitted, error) {
	goal := o.Goal
	if goal == "" {
		goal = GoalFromMessages(messages)
	}
	stateOf := func(h []HistoryEntry) State {
		if h == nil {
			h = []HistoryEntry{}
		}
		return State{Context: StateContext, Goal: goal, History: h}
	}
	baseTokens := EstimateTokens(jsJSON(stateOf(nil)))
	var history []HistoryEntry
	var perEntry []int
	tokens := 0
	rebuild := func(chars int) {
		history = historyEntries(messages, calls, chars)
		perEntry = make([]int, len(history))
		tokens = baseTokens
		for i, e := range history {
			perEntry[i] = entryTokens(e)
			tokens += perEntry[i]
		}
	}
	fits := func() bool { return tokens <= o.MaxStateTokens }
	shrink := func(i int, change func(*HistoryEntry)) {
		change(&history[i])
		now := entryTokens(history[i])
		tokens += now - perEntry[i]
		perEntry[i] = now
	}
	done := func(h []HistoryEntry, n int, stage string) (Fitted, error) {
		return Fitted{State: stateOf(h), Tokens: n, Stage: stage}, nil
	}

	rebuild(inputChars[0])
	if fits() {
		return done(history, tokens, "full")
	}
	for _, limit := range inputChars[1:] {
		rebuild(limit)
		if fits() {
			return done(history, tokens, fmt.Sprintf("inputs<=%d", limit))
		}
	}
	pinned := func(e HistoryEntry) bool { return isPinned(e.I, len(messages), o.PreserveRecentMessages) }
	order := []int{}
	for i := range history {
		if !pinned(history[i]) {
			order = append(order, i)
		}
	}
	for i := range history {
		if pinned(history[i]) {
			order = append(order, i)
		}
	}
	for _, i := range order {
		if ulen(history[i].Text) <= textHead+textTail+40 {
			continue
		}
		shrink(i, func(e *HistoryEntry) { e.Text = abridge(e.Text, textHead, textTail) })
		if fits() {
			return done(history, tokens, "texts abridged")
		}
	}
	for _, i := range order {
		if pinned(history[i]) || len(history[i].Text) == 0 {
			continue
		}
		original := ulen(history[i].Text)
		if history[i].I < len(messages) {
			original = ulen(messages[history[i].I].Text)
		}
		shrink(i, func(e *HistoryEntry) { e.Text = fmt.Sprintf("[… %d chars omitted …]", original) })
		if fits() {
			return done(history, tokens, "old messages collapsed")
		}
	}
	by := callsByMessage(calls)
	for _, i := range order {
		own, ok := by[history[i].I]
		if pinned(history[i]) || !ok {
			continue
		}
		shrink(i, func(e *HistoryEntry) {
			lines := make([]string, len(own))
			for k, c := range own {
				lines[k] = compactCall(c)
			}
			e.ToolCalls = lines
		})
		if fits() {
			return done(history, tokens, "old calls compacted")
		}
	}
	left := map[int]bool{}
	for _, i := range order {
		if pinned(history[i]) || history[i].ToolCalls != nil {
			continue
		}
		left[i] = true
		tokens -= perEntry[i]
		if fits() {
			kept := []HistoryEntry{}
			for k, e := range history {
				if !left[k] {
					kept = append(kept, e)
				}
			}
			return done(kept, tokens, "old messages left out")
		}
	}
	recent := []HistoryEntry{}
	for k, e := range history {
		if !left[k] {
			recent = append(recent, e)
		}
	}
	history = mergeCallRuns(recent, pinned)
	tokens = baseTokens
	for _, e := range history {
		tokens += entryTokens(e)
	}
	if fits() {
		return done(history, tokens, "old calls merged")
	}
	history = recent
	perEntry = make([]int, len(history))
	for i, e := range history {
		perEntry[i] = entryTokens(e)
	}
	tokens = baseTokens
	first := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		if tokens+perEntry[i] > o.MaxStateTokens {
			break
		}
		tokens += perEntry[i]
		first = i
	}
	if first < len(history) {
		return done(history[first:], tokens, "recent context only")
	}
	last := 0
	if n := len(perEntry); n > 0 {
		last = perEntry[n-1]
	}
	return Fitted{}, fmt.Errorf("latest message too large for Jev (~%d tokens, limit %d)", baseTokens+last, o.MaxStateTokens)
}
