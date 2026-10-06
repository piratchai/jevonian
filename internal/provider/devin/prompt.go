package devin

import (
	"regexp"
	"strings"

	"github.com/xinyao27/jevonian/internal/wire"
)

// builtinPromptRewrites are the rival-prompt signatures Devin's content policy
// has been observed to reject, each replaced with wording that keeps the same
// instruction without tripping the filter. Port of BUILTIN_PROMPT_REWRITES in
// src/prompt-policy.ts.
//
// TODO(go-rewrite): move to a shared internal/promptpolicy package once one
// exists; it lives here because the Devin wire is the only built-in caller.
var builtinPromptRewrites = [][2]string{
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

// identityPattern covers the identity line in any casing and with any trailing
// detail ("Cursor IDE", "!").
var identityPattern = regexp.MustCompile(`(?i)You operate in Cursor[^.\n]*[.!]?`)

// SanitizeSystemPrompt applies the built-in rival-prompt rewrites. Idempotent.
// Port of sanitizeBuiltinPrompt / sanitizeDevinSystemPrompt.
func SanitizeSystemPrompt(text string) string {
	out := text
	for _, rule := range builtinPromptRewrites {
		out = strings.ReplaceAll(out, rule[0], rule[1])
	}
	return identityPattern.ReplaceAllLiteralString(out, "You work inside the user's code editor.")
}

// StripAgentSystemMessages drops system and developer messages — the last
// resort for a content_policy refusal. The wire injects its own minimal prompt
// when tools are present, so the request stays valid.
func StripAgentSystemMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		role := wire.AsRecord(raw)["role"]
		if role == "system" || role == "developer" {
			continue
		}
		out = append(out, raw)
	}
	return out
}
