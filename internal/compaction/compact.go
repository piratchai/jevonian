package compaction

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Question is one `noul` question for Jev.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

// Answer is one Jev answer. Noul is nil when absent.
type Answer struct{ Noul *float64 }

// Asker answers Jev questions: the router's brain client, or a test double.
type Asker interface {
	Ask(ctx context.Context, state State, questions map[string]Question) (map[string]Answer, error)
}

// QuestionsFor asks two things about one call: keep the result verbatim, and
// (unless the call is outside Jev's recent context) keep the call itself.
func QuestionsFor(c ToolCall, unseen bool) map[string]Question {
	in := callInputText(c, 160)
	q := map[string]Question{
		"result_" + c.ID: {Type: "noul", Instructions: fmt.Sprintf("The full output of tool call %s (%s, input: %s, %d chars) should stay in the history verbatim: the assistant still needs its contents and re-running the tool would not do", c.ID, c.Tool, in, c.ResultChars)},
	}
	if !unseen {
		q["call_"+c.ID] = Question{Type: "noul", Instructions: fmt.Sprintf("Tool call %s (%s, input: %s) should stay in the history: knowing this call was made, with its input, still matters for what the assistant does next", c.ID, c.Tool, in)}
	}
	return q
}

// questionsJSON serialises like JS: `result_` first, then `call_`.
func questionsJSON(c ToolCall, unseen bool) string {
	q := QuestionsFor(c, unseen)
	var b strings.Builder
	b.WriteByte('{')
	b.WriteString(jsJSON("result_" + c.ID))
	b.WriteByte(':')
	b.WriteString(jsJSON(q["result_"+c.ID]))
	if cq, ok := q["call_"+c.ID]; ok {
		b.WriteByte(',')
		b.WriteString(jsJSON("call_" + c.ID))
		b.WriteByte(':')
		b.WriteString(jsJSON(cq))
	}
	b.WriteByte('}')
	return b.String()
}

// BatchCalls splits candidates into batches whose questions plus the state fit
// one request.
func BatchCalls(calls []ToolCall, stateTokens int, maxRequestTokens int, unseen map[string]bool) ([][]ToolCall, error) {
	budget := maxRequestTokens - stateTokens - requestOverheadTokens
	batches := [][]ToolCall{}
	current := []ToolCall{}
	currentTokens := 0
	for _, c := range calls {
		tokens := EstimateTokens(questionsJSON(c, unseen[c.ID]))
		if len(current) > 0 && currentTokens+tokens > budget {
			batches = append(batches, current)
			current, currentTokens = []ToolCall{}, 0
		}
		if len(current) == 0 && tokens > budget {
			return nil, fmt.Errorf("state leaves no room for questions (~%d of %d tokens)", stateTokens, maxRequestTokens)
		}
		current = append(current, c)
		currentTokens += tokens
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches, nil
}

// CallAnswer is the keep probabilities for one call.
type CallAnswer struct{ KeepCall, KeepResult float64 }

// Decision is the verdict for one call.
type Decision struct {
	ID         string
	Tool       string
	KeepCall   float64
	KeepResult float64
	Action     string // keep | drop_result | drop_call
	Reason     string // pinned | kept | result_dropped | call_dropped
}

// DecideCall turns keep probabilities into an action.
func DecideCall(c ToolCall, a CallAnswer, keepThreshold float64) Decision {
	d := Decision{ID: c.ID, Tool: c.Tool, KeepCall: a.KeepCall, KeepResult: a.KeepResult}
	switch {
	case c.Pinned:
		d.Action, d.Reason = "keep", "pinned"
	case a.KeepResult >= keepThreshold:
		d.Action, d.Reason = "keep", "kept"
	case a.KeepCall >= keepThreshold:
		d.Action, d.Reason = "drop_result", "result_dropped"
	default:
		d.Action, d.Reason = "drop_call", "call_dropped"
	}
	return d
}

func noulAnswer(answers map[string]Answer, name string) (float64, error) {
	a, ok := answers[name]
	if !ok || a.Noul == nil || math.IsNaN(*a.Noul) || math.IsInf(*a.Noul, 0) {
		return 0, fmt.Errorf("Invalid Jev answer for %s", name)
	}
	return *a.Noul, nil
}

func askBatch(ctx context.Context, asker Asker, state State, batch []ToolCall, unseen map[string]bool) (map[string]CallAnswer, error) {
	questions := map[string]Question{}
	for _, c := range batch {
		for k, v := range QuestionsFor(c, unseen[c.ID]) {
			questions[k] = v
		}
	}
	answers, err := asker.Ask(ctx, state, questions)
	if err != nil {
		return nil, err
	}
	out := map[string]CallAnswer{}
	for _, c := range batch {
		keepCall := 1.0
		if !unseen[c.ID] {
			v, err := noulAnswer(answers, "call_"+c.ID)
			if err != nil {
				return nil, err
			}
			keepCall = v
		}
		keepResult, err := noulAnswer(answers, "result_"+c.ID)
		if err != nil {
			return nil, err
		}
		out[c.ID] = CallAnswer{keepCall, keepResult}
	}
	return out, nil
}

func truncatedResultText(text string, isError bool, head int) string {
	n := ulen(text)
	if n <= head+120 {
		return text
	}
	h := ""
	if head > 0 {
		h = usub(text, 0, head) + "\n"
	}
	suffix := ""
	if isError {
		suffix = " (error)"
	}
	return fmt.Sprintf("%s[jevonian truncated %d chars of this tool result%s; re-run the tool if needed]", h, n-head, suffix)
}

// ApplyDecisions rebuilds the conversation from the decisions. A dropped call
// disappears with its result; a dropped result keeps a bounded head and a note.
// Messages that lose all content are removed.
func ApplyDecisions(messages []Message, decisions []Decision, calls []ToolCall, head int) []Message {
	byID := map[string]ToolCall{}
	for _, c := range calls {
		byID[c.ID] = c
	}
	actions := map[string]string{}
	for _, d := range decisions {
		if c, ok := byID[d.ID]; ok && d.Action != "keep" {
			actions[c.ToolUseID] = d.Action
		}
	}
	kept := []Message{}
	for _, m := range messages {
		touched := false
		for _, t := range m.ToolUses {
			if _, ok := actions[t.ToolUseID]; ok {
				touched = true
			}
		}
		for _, r := range m.ToolResults {
			if _, ok := actions[r.ToolUseID]; ok {
				touched = true
			}
		}
		if !touched {
			kept = append(kept, m)
			continue
		}
		var uses []ToolUse
		for _, t := range m.ToolUses {
			if actions[t.ToolUseID] != "drop_call" {
				uses = append(uses, t)
			}
		}
		var results []ToolResult
		for _, r := range m.ToolResults {
			if actions[r.ToolUseID] == "drop_call" {
				continue
			}
			if actions[r.ToolUseID] == "drop_result" {
				r.Text = truncatedResultText(r.Text, r.IsError, head)
			}
			results = append(results, r)
		}
		if strings.TrimSpace(m.Text) == "" && len(uses) == 0 && len(results) == 0 && !m.HasUnmodeledContent {
			continue
		}
		rebuilt := m
		rebuilt.ToolUses, rebuilt.ToolResults = uses, results
		kept = append(kept, rebuilt)
	}
	return kept
}

// Stats summarises one compaction.
type Stats struct {
	MessagesBefore, MessagesAfter int
	CharsBefore, CharsAfter       int
	Calls, Kept                   int
	ResultsDropped, CallsDropped  int
	Pinned                        int
	StateTokens                   int
	StateStage                    string
	Requests                      int
	Ms                            int64
}

// Result is the outcome of Compact.
type Result struct {
	Messages  []Message
	Decisions []Decision
	Stats     Stats
}

// ReductionRatio is the share of characters removed.
func ReductionRatio(r Result) float64 {
	if r.Stats.CharsBefore == 0 {
		return 0
	}
	return float64(r.Stats.CharsBefore-r.Stats.CharsAfter) / float64(r.Stats.CharsBefore)
}

func count(ds []Decision, reason string) int {
	n := 0
	for _, d := range ds {
		if d.Reason == reason {
			n++
		}
	}
	return n
}

// Compact asks Jev about tool calls outside the pinned first and newest
// messages and deletes what it judges stale. It returns an error when Jev fails
// or the history cannot be fitted; the caller decides whether to fall back and
// should check ReductionRatio before accepting the result.
func Compact(ctx context.Context, messages []Message, asker Asker, options Options) (Result, error) {
	started := time.Now()
	o := ResolveOptions(options)
	calls := CollectToolCalls(messages, o.PreserveRecentMessages)
	candidates := []ToolCall{}
	for _, c := range calls {
		if !c.Pinned {
			candidates = append(candidates, c)
		}
	}
	charsBefore := 0
	for _, m := range messages {
		charsBefore += MessageChars(m)
	}
	fitted := Fitted{}
	batches := [][]ToolCall{}
	answers := map[string]CallAnswer{}
	if len(candidates) > 0 {
		var err error
		fitted, err = FitState(messages, calls, o)
		if err != nil {
			return Result{}, err
		}
		visible := map[int]bool{}
		for _, e := range fitted.State.History {
			visible[e.I] = true
		}
		unseen := map[string]bool{}
		if fitted.Stage == "recent context only" {
			for _, c := range candidates {
				if !visible[c.CallIndex] {
					unseen[c.ID] = true
				}
			}
		}
		batches, err = BatchCalls(candidates, fitted.Tokens, o.MaxRequestTokens, unseen)
		if err != nil {
			return Result{}, err
		}
		// Bound concurrent requests so a long session does not burst the
		// brain's rate limit.
		for start := 0; start < len(batches); start += 3 {
			end := start + 3
			if end > len(batches) {
				end = len(batches)
			}
			group := batches[start:end]
			maps := make([]map[string]CallAnswer, len(group))
			errs := make([]error, len(group))
			var wg sync.WaitGroup
			for i, b := range group {
				wg.Add(1)
				go func(i int, b []ToolCall) {
					defer wg.Done()
					maps[i], errs[i] = askBatch(ctx, asker, fitted.State, b, unseen)
				}(i, b)
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				return Result{}, firstErr(errs)
			}
			for _, m := range maps {
				for id, a := range m {
					answers[id] = a
				}
			}
		}
	}
	decisions := make([]Decision, len(calls))
	for i, c := range calls {
		a, ok := answers[c.ID]
		if !ok {
			a = CallAnswer{1, 1}
		}
		decisions[i] = DecideCall(c, a, o.KeepThreshold)
	}
	kept := ApplyDecisions(messages, decisions, calls, o.TruncateHeadChars)
	charsAfter := 0
	for _, m := range kept {
		charsAfter += MessageChars(m)
	}
	return Result{Messages: kept, Decisions: decisions, Stats: Stats{
		MessagesBefore: len(messages), MessagesAfter: len(kept), CharsBefore: charsBefore, CharsAfter: charsAfter,
		Calls: len(calls), Kept: count(decisions, "kept"), ResultsDropped: count(decisions, "result_dropped"),
		CallsDropped: count(decisions, "call_dropped"), Pinned: count(decisions, "pinned"),
		StateTokens: fitted.Tokens, StateStage: fitted.Stage, Requests: len(batches), Ms: time.Since(started).Milliseconds(),
	}}, nil
}

func firstErr(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
