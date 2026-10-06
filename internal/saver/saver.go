// Package saver compresses prior tool results with the external rtk pipe command.
// Failures leave the original body intact. It never invokes a shell.
package saver

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
	"time"
)

const minPipeChars = 200
const maxConcurrency = 8
const cacheEntries = 512
const cacheMaxText = 1_000_000

type Stats struct {
	ResultsCompressed int
	SavedTokens       int
	CharsBefore       int
	CharsAfter        int
	Failures          int
	Unavailable       bool
}
type Result struct {
	Body  wire.Body
	Stats Stats
}
type outcome struct {
	text            string
	failed, missing bool
}
type memo struct{ key, value string }

var cache = struct {
	sync.Mutex
	order   *list.List
	entries map[string]*list.Element
}{order: list.New(), entries: map[string]*list.Element{}}
var warned = struct {
	sync.Mutex
	commands map[string]bool
}{commands: map[string]bool{}}

func ClearCache() {
	cache.Lock()
	defer cache.Unlock()
	cache.order.Init()
	cache.entries = map[string]*list.Element{}
}
func cacheGet(key string) (string, bool) {
	cache.Lock()
	defer cache.Unlock()
	e, ok := cache.entries[key]
	if !ok {
		return "", false
	}
	cache.order.MoveToBack(e)
	return e.Value.(memo).value, true
}
func cacheSet(key, value string) {
	cache.Lock()
	defer cache.Unlock()
	if e := cache.entries[key]; e != nil {
		e.Value = memo{key, value}
		cache.order.MoveToBack(e)
	} else {
		cache.entries[key] = cache.order.PushBack(memo{key, value})
	}
	for cache.order.Len() > cacheEntries {
		e := cache.order.Front()
		delete(cache.entries, e.Value.(memo).key)
		cache.order.Remove(e)
	}
}

// ParseCommand supports quoted binary paths and leading flags without shell expansion.
func ParseCommand(command string) (string, []string) {
	var parts []string
	var current strings.Builder
	var quote rune
	for _, c := range strings.TrimSpace(command) {
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				current.WriteRune(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if unicode.IsSpace(c) {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(c)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	if len(parts) == 0 {
		return "rtk", nil
	}
	return parts[0], parts[1:]
}

// WarnUnavailable logs once per configured command. A runtime failure is not an install warning.
func WarnUnavailable(cfg config.TokenSaverConfig, unavailable bool) {
	if !unavailable {
		return
	}
	warned.Lock()
	if warned.commands[cfg.Command] {
		warned.Unlock()
		return
	}
	warned.commands[cfg.Command] = true
	warned.Unlock()
	log.Printf("[token-saver] cannot run %q — tool results are passing through unchanged. Install it (`brew install rtk`) or point tokenSaver.command at the binary.", cfg.Command)
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64*1024*1024 {
		return 0, errors.New("rtk output exceeds 64 MiB")
	}
	return b.Buffer.Write(p)
}
func spawn(ctx context.Context, text string, cfg config.TokenSaverConfig, filter string) outcome {
	bin, args := ParseCommand(cfg.Command)
	args = append(args, "pipe")
	if filter != "" {
		args = append(args, "-f", filter)
	}
	ms := cfg.TimeoutMs
	if ms <= 0 {
		ms = 3000
	}
	childCtx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
	defer cancel()
	child := exec.CommandContext(childCtx, bin, args...)
	child.Stdin = strings.NewReader(text)
	var out, stderr limitedBuffer
	child.Stdout = &out
	child.Stderr = &stderr
	// Descendants that hold inherited pipes cannot keep a timed-out call blocked.
	child.WaitDelay = 50 * time.Millisecond
	err := child.Run()
	if err != nil || out.Len() == 0 {
		return outcome{text: text, failed: true, missing: errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, exec.ErrNotFound)}
	}
	result := out.String()
	if chars(result) >= chars(text) {
		result = text
	}
	return outcome{text: result}
}
func pipe(ctx context.Context, text, command string, cfg config.TokenSaverConfig) outcome {
	key := ""
	if chars(text) <= cacheMaxText {
		key = cfg.Command + "\x00" + command + "\x00" + text
		if hit, ok := cacheGet(key); ok {
			return outcome{text: hit}
		}
	}
	final := spawn(ctx, text, cfg, "")
	if !final.failed && final.text == text {
		if filter := FilterForCommand(command); filter != "" {
			retry := spawn(ctx, text, cfg, filter)
			if !retry.failed && chars(retry.text) < chars(text) {
				final = retry
			}
		}
	}
	if key != "" && !final.failed {
		cacheSet(key, final.text)
	}
	return final
}

var filterPrefixes = [][2]string{{"cargo test", "cargo-test"}, {"git status", "git-status"}, {"git diff", "git-diff"}, {"git log", "git-log"}, {"go test", "go-test"}, {"go build", "go-build"}, {"python -m pytest", "pytest"}, {"python -m mypy", "mypy"}}
var runners = map[string]bool{"npx": true, "pnpm": true, "pnpx": true, "yarn": true, "bunx": true, "bun": true, "uvx": true}
var singleFilters = map[string]bool{"tsc": true, "mypy": true, "pytest": true, "vitest": true, "grep": true, "rg": true, "find": true, "fd": true}
var sudoRE = regexp.MustCompile(`^\s*sudo\s+`)
var envRE = regexp.MustCompile(`^\s*env\s+(?:\w+=\S+\s+)*`)
var cdRE = regexp.MustCompile(`^\s*cd\s+\S+\s*&&\s*`)

// FilterForCommand matches the TS hint rules, including their first-segment behavior.
func FilterForCommand(command string) string {
	if i := strings.IndexAny(command, "|;&"); i >= 0 {
		command = command[:i]
	}
	command = sudoRE.ReplaceAllString(command, "")
	command = envRE.ReplaceAllString(command, "")
	command = cdRE.ReplaceAllString(command, "")
	tokens := strings.Fields(command)
	for i, t := range tokens {
		if n := strings.LastIndex(t, "/"); n >= 0 {
			tokens[i] = t[n+1:]
		}
	}
	for len(tokens) > 0 && (strings.Contains(tokens[0], "=") || runners[tokens[0]]) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return ""
	}
	for _, pair := range filterPrefixes {
		words := strings.Fields(pair[0])
		if len(words) > len(tokens) {
			continue
		}
		match := true
		for i, w := range words {
			if tokens[i] != w {
				match = false
				break
			}
		}
		if match {
			return pair[1]
		}
	}
	if singleFilters[tokens[0]] {
		return tokens[0]
	}
	return ""
}

var lettersRE = regexp.MustCompile(`[a-zA-Z]`)

func extractCommand(raw any, depth int) string {
	if depth > 64 {
		return ""
	}
	if s, ok := raw.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(s), &decoded) == nil {
			return extractCommand(decoded, depth+1)
		}
		if lettersRE.MatchString(s) {
			return s
		}
		return ""
	}
	m := wire.AsRecord(raw)
	for _, key := range []string{"command", "cmd", "program", "script", "shell", "bash"} {
		if v, ok := m[key]; ok && v != nil {
			s, _ := v.(string)
			return s
		}
	}
	return ""
}
func collectCommands(m wire.Body, into map[string]string) {
	for _, raw := range wire.AsSlice(m["tool_calls"]) {
		call := wire.AsRecord(raw)
		id, ok := call["id"].(string)
		if !ok {
			continue
		}
		if command := extractCommand(wire.AsRecord(call["function"])["arguments"], 0); command != "" {
			into[id] = command
		}
	}
	for _, raw := range wire.AsSlice(m["content"]) {
		block := wire.AsRecord(raw)
		if block["type"] != "tool_use" {
			continue
		}
		id, ok := block["id"].(string)
		if ok {
			if command := extractCommand(block["input"], 0); command != "" {
				into[id] = command
			}
		}
	}
	if m["type"] == "function_call" {
		if id, ok := m["call_id"].(string); ok {
			if command := extractCommand(m["arguments"], 0); command != "" {
				into[id] = command
			}
		}
	}
}
func extractText(part wire.Body, field string) (string, bool) {
	if text, ok := part[field].(string); ok {
		return text, true
	}
	for _, raw := range wire.AsSlice(part[field]) {
		if text, ok := wire.AsRecord(raw)["text"].(string); ok {
			return text, true
		}
	}
	return "", false
}
func copyMap(m wire.Body) wire.Body {
	out := wire.Body{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func injectText(part wire.Body, field, text string) wire.Body {
	out := copyMap(part)
	if _, ok := part[field].(string); ok {
		out[field] = text
		return out
	}
	if blocks, ok := part[field].([]any); ok {
		next := append([]any{}, blocks...)
		for i, b := range next {
			m := wire.AsRecord(b)
			if _, ok := m["text"].(string); ok {
				c := copyMap(m)
				c["text"] = text
				next[i] = c
				break
			}
		}
		out[field] = next
	}
	return out
}

type slot struct {
	message, block       int
	part                 wire.Body
	field, text, command string
}

func slotsFor(m wire.Body, index int, commands map[string]string) []slot {
	var slots []slot
	add := func(part wire.Body, block int, field, id string) {
		if text, ok := extractText(part, field); ok {
			slots = append(slots, slot{index, block, part, field, text, commands[id]})
		}
	}
	for i, raw := range wire.AsSlice(m["content"]) {
		block := wire.AsRecord(raw)
		if block["type"] == "tool_result" {
			add(block, i, "content", wire.AsString(block["tool_use_id"]))
		}
	}
	if len(slots) > 0 {
		return slots
	}
	if m["role"] == "tool" {
		if id, ok := m["tool_call_id"].(string); ok {
			add(m, -1, "content", id)
			return slots
		}
	}
	if m["type"] == "function_call_output" {
		add(m, -1, "output", wire.AsString(m["call_id"]))
	}
	return slots
}

// SaveTokens preserves all non-tool fields and text blocks after the first one.
// It shares eight worker slots per request and a bounded, concurrent LRU cache.
func SaveTokens(ctx context.Context, body wire.Body, cfg config.TokenSaverConfig) Result {
	result := Result{Body: body}
	if !cfg.Enabled {
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := "messages"
	raw, ok := body[key].([]any)
	if !ok {
		key = "input"
		raw, ok = body[key].([]any)
	}
	if !ok {
		return result
	}
	commands := map[string]string{}
	for _, item := range raw {
		collectCommands(wire.AsRecord(item), commands)
	}
	var work []slot
	for i, item := range raw {
		for _, s := range slotsFor(wire.AsRecord(item), i, commands) {
			if chars(s.text) >= minPipeChars {
				work = append(work, s)
			}
		}
	}
	if len(work) == 0 {
		return result
	}
	outcomes := make([]outcome, len(work))
	jobs := make(chan int)
	var wg sync.WaitGroup
	n := min(maxConcurrency, len(work))
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := work[i]
				outcomes[i] = pipe(ctx, s.text, s.command, cfg)
			}
		}()
	}
	for i := range work {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	next := append([]any{}, raw...)
	changed := map[int]wire.Body{}
	for i, o := range outcomes {
		s := work[i]
		if o.failed {
			result.Stats.Failures++
		}
		if o.missing {
			result.Stats.Unavailable = true
		}
		if o.text == s.text {
			continue
		}
		result.Stats.ResultsCompressed++
		result.Stats.CharsBefore += chars(s.text)
		result.Stats.CharsAfter += chars(o.text)
		result.Stats.SavedTokens += max(0, estimateTokens(s.text)-estimateTokens(o.text))
		m := changed[s.message]
		if m == nil {
			m = copyMap(wire.AsRecord(raw[s.message]))
			changed[s.message] = m
		}
		if s.block < 0 {
			m = injectText(m, s.field, o.text)
			changed[s.message] = m
		} else {
			blocks := append([]any{}, wire.AsSlice(m["content"])...)
			blocks[s.block] = injectText(s.part, s.field, o.text)
			m["content"] = blocks
		}
		next[s.message] = m
	}
	if result.Stats.ResultsCompressed > 0 {
		result.Body = copyMap(body)
		result.Body[key] = next
	}
	return result
}
func chars(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// Keep this leaf package independent of routing (routing imports upstream).
// These are the calibrated estimateTokens rules in src/compaction.ts.
var imageRE = regexp.MustCompile(`data:image/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]{100,}|"data"\s*:\s*"[A-Za-z0-9+/=]{100,}"`)

func estimateTokens(text string) int {
	tokens := 0.0
	text = imageRE.ReplaceAllStringFunc(text, func(string) string { tokens += 1200; return "" })
	runes := []rune(text)
	for i := 0; i < len(runes); {
		r := runes[i]
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			j := i + 1
			for j < len(runes) && ((runes[j] >= 'a' && runes[j] <= 'z') || (runes[j] >= 'A' && runes[j] <= 'Z')) {
				j++
			}
			tokens += float64(1 + (j-i-1)/6)
			i = j
		} else if r >= '0' && r <= '9' {
			j := i + 1
			for j < len(runes) && runes[j] >= '0' && runes[j] <= '9' {
				j++
			}
			tokens += float64(j-i) / 2
			i = j
		} else if (unicode.IsSpace(r) && r != 0x85) || r == 0xfeff {
			i++
		} else {
			tokens += 0.9
			if r >= 0x10000 {
				tokens += 0.9
			}
			i++
		}
	}
	return int(math.Ceil(tokens))
}

var _ io.Writer = (*limitedBuffer)(nil)
