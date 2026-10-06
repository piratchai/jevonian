package wire

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/xinyao27/jevonian/internal/config"
)

// Outgoing prompt hygiene. Port of src/prompt-policy.ts.
//
// Some upstreams refuse a request when the system prompt carries verbatim wording
// from a rival coding agent's prompt. The built-in signatures below are replaced
// with neutral wording; operators add their own rules under promptPolicy.rewrites.
//
// RE2 (Go) has no look-around or back-references, so a rule that needs them is
// dropped when the config is parsed (internal/config) instead of being applied.
// `\s` and `.` also follow RE2 rules (ASCII whitespace; `.` excludes only "\n").

// BuiltinPromptRewrites are the signatures the upstream blocklist rejects, each
// with wording that keeps the instruction. Same list as src/prompt-policy.ts.
var BuiltinPromptRewrites = [][2]string{
	{"You operate in Cursor.", "You work inside the user's code editor."},
	{
		"Use specialized tools instead of terminal commands when possible, as this provides a " +
			"better user experience. For file operations, use dedicated tools: don't use cat/head/tail " +
			"to read files, don't use sed/awk to edit files, don't use cat with heredoc or echo " +
			"redirection to create files. Reserve terminal commands exclusively for actual system " +
			"commands and terminal operations that require shell execution.",
		"Prefer the dedicated file tools over shell text utilities: read files with the file-reading " +
			"tool rather than cat/head/tail, edit with the edit tool rather than sed/awk, and create " +
			"files with the write tool rather than shell redirection. Keep the shell for real system " +
			"commands and terminal operations.",
	},
	{
		"## METHOD 2: MARKDOWN CODE BLOCKS - Proposing or Displaying Code NOT already in Codebase",
		"## METHOD 2: FENCED CODE BLOCKS - For code that is not yet present in the repository",
	},
	{
		"There is one text file for each terminal the user has running.",
		"Each open terminal has its own file.",
	},
}

// identity line in any casing and with any trailing detail ("Cursor IDE", "!").
var identityPattern = regexp.MustCompile(`(?i)You operate in Cursor[^.\n]*[.!]?`)

// SanitizeBuiltinPrompt applies the built-in rival-prompt signatures. Idempotent.
func SanitizeBuiltinPrompt(text string) string {
	out := text
	for _, rule := range BuiltinPromptRewrites {
		out = strings.ReplaceAll(out, rule[0], rule[1])
	}
	return identityPattern.ReplaceAllLiteralString(out, "You work inside the user's code editor.")
}

type compiledRule struct {
	re *regexp.Regexp
}

var ruleCache sync.Map // key: flags + "\x00" + match -> *compiledRule

// compileRule mirrors compile() in src/prompt-policy.ts: `g` is always on, `i`,
// `m` and `s` map to RE2 flags, `u` and `y` carry no meaning here. A pattern
// that does not compile yields nil and is skipped.
func compileRule(rule config.PromptRewriteRule) *regexp.Regexp {
	key := rule.Flags + "\x00" + rule.Match
	if cached, ok := ruleCache.Load(key); ok {
		return cached.(*compiledRule).re
	}
	prefix := ""
	for _, flag := range rule.Flags {
		switch flag {
		case 'i', 'm', 's':
			prefix += string(flag)
		}
	}
	pattern := rule.Match
	if prefix != "" {
		pattern = "(?" + prefix + ")" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	ruleCache.Store(key, &compiledRule{re: re})
	return re
}

// expandJSReplacement expands a JavaScript String.replace template: `$$`, `$&`,
// "$`", `$'`, `$n`, `$nn` and `$<name>`. A reference to a group that does not
// exist stays literal, as in JS. An unmatched group expands to "".
func expandJSReplacement(re *regexp.Regexp, template, src string, m []int) string {
	if !strings.Contains(template, "$") {
		return template
	}
	groups := (len(m) / 2) - 1
	group := func(n int) string {
		if m[2*n] < 0 {
			return ""
		}
		return src[m[2*n]:m[2*n+1]]
	}
	var out strings.Builder
	for i := 0; i < len(template); i++ {
		c := template[i]
		if c != '$' || i+1 >= len(template) {
			out.WriteByte(c)
			continue
		}
		next := template[i+1]
		switch {
		case next == '$':
			out.WriteByte('$')
			i++
		case next == '&':
			out.WriteString(src[m[0]:m[1]])
			i++
		case next == '`':
			out.WriteString(src[:m[0]])
			i++
		case next == '\'':
			out.WriteString(src[m[1]:])
			i++
		case next >= '0' && next <= '9':
			// Two digits win when that group exists, else one digit.
			if i+2 < len(template) && template[i+2] >= '0' && template[i+2] <= '9' {
				if n, _ := strconv.Atoi(template[i+1 : i+3]); n >= 1 && n <= groups {
					out.WriteString(group(n))
					i += 2
					continue
				}
			}
			if n := int(next - '0'); n >= 1 && n <= groups {
				out.WriteString(group(n))
				i++
				continue
			}
			out.WriteByte(c)
		case next == '<':
			end := strings.IndexByte(template[i+2:], '>')
			names := re.SubexpNames()
			hasNamed := false
			for _, name := range names {
				if name != "" {
					hasNamed = true
					break
				}
			}
			if end < 0 || !hasNamed {
				out.WriteByte(c)
				continue
			}
			name := template[i+2 : i+2+end]
			if idx := re.SubexpIndex(name); idx > 0 {
				out.WriteString(group(idx))
			}
			i += end + 2
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

func replaceAllJS(re *regexp.Regexp, src, template string) string {
	matches := re.FindAllStringSubmatchIndex(src, -1)
	if matches == nil {
		return src
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		out.WriteString(src[last:m[0]])
		out.WriteString(expandJSReplacement(re, template, src, m))
		last = m[1]
	}
	out.WriteString(src[last:])
	return out.String()
}

// SanitizePromptWithPolicy applies the built-ins (when enabled) and then the
// operator rules. An invalid pattern is skipped rather than failing the turn.
func SanitizePromptWithPolicy(text string, policy config.PromptPolicyConfig) string {
	out := text
	if policy.Builtins {
		out = SanitizeBuiltinPrompt(out)
	}
	for _, rule := range policy.Rewrites {
		if re := compileRule(rule); re != nil {
			out = replaceAllJS(re, out, rule.Replace)
		}
	}
	return out
}

// rewriteField rewrites one prompt field, which may be a plain string or a list
// of text blocks. changed reports whether anything differed.
func rewriteField(value any, apply func(string) string) (result any, changed bool) {
	switch v := value.(type) {
	case string:
		next := apply(v)
		return next, next != v
	case []any:
		out := make([]any, len(v))
		for i, part := range v {
			out[i] = part
			rec, ok := part.(map[string]any)
			if !ok {
				continue
			}
			text, ok := rec["text"].(string)
			if !ok {
				continue
			}
			next := apply(text)
			if next == text {
				continue
			}
			copied := make(map[string]any, len(rec))
			for k, val := range rec {
				copied[k] = val
			}
			copied["text"] = next
			out[i] = copied
			changed = true
		}
		if !changed {
			return value, false
		}
		return out, true
	}
	return value, false
}

// RewritePromptBodies applies the policy to every prompt field of an outgoing
// wire body: Chat `messages` (system and developer roles), Anthropic `system`,
// Responses `instructions`. It returns a copy and never mutates the caller's
// body; when nothing matches it returns the same map.
func RewritePromptBodies(body Body, policy config.PromptPolicyConfig) Body {
	if body == nil {
		return body
	}
	apply := func(text string) string { return SanitizePromptWithPolicy(text, policy) }
	next := body
	copiedBody := false
	mutable := func() Body {
		if !copiedBody {
			copied := make(Body, len(body))
			for k, v := range body {
				copied[k] = v
			}
			next = copied
			copiedBody = true
		}
		return next
	}

	if messages, ok := body["messages"].([]any); ok {
		changed := false
		out := make([]any, len(messages))
		for i, raw := range messages {
			out[i] = raw
			rec, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := rec["role"].(string)
			role = strings.ToLower(role)
			if role != "system" && role != "developer" {
				continue
			}
			content, did := rewriteField(rec["content"], apply)
			if !did {
				continue
			}
			copied := make(map[string]any, len(rec))
			for k, v := range rec {
				copied[k] = v
			}
			copied["content"] = content
			out[i] = copied
			changed = true
		}
		if changed {
			mutable()["messages"] = out
		}
	}
	if system, present := body["system"]; present && system != nil {
		if rewritten, did := rewriteField(system, apply); did {
			mutable()["system"] = rewritten
		}
	}
	if instructions, ok := body["instructions"].(string); ok {
		mutable()["instructions"] = apply(instructions)
	}
	// The Antigravity envelope keeps its system prompt inside request.systemInstruction.
	if request, ok := body["request"].(map[string]any); ok {
		if si, present := request["systemInstruction"]; present && si != nil {
			if rewritten, did := rewriteField(si, apply); did {
				copied := make(map[string]any, len(request))
				for k, v := range request {
					copied[k] = v
				}
				copied["systemInstruction"] = rewritten
				mutable()["request"] = copied
			}
		}
	}
	return next
}
