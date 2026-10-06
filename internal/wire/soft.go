package wire

import (
	"strings"
	"unicode"
)

// SoftErrorPrefix kept stable so users recognise a Jevonian-generated message
// at a glance. Mirrors src/soft-error.ts.
const SoftErrorPrefix = "Jevonian hit an internal error"

// SoftErrorMessage renders a one-line assistant message describing a failure,
// safe to show to the user. A bare SSE `{ error }` frame makes harnesses roll
// the user message back, so bridged streams close the turn with this text
// instead.
func SoftErrorMessage(reason string) string {
	detail := TruncateRunes(strings.Join(strings.FieldsFunc(reason, unicode.IsSpace), " "), 300)
	suffix := ""
	if len(detail) > 0 {
		suffix = ": " + detail
	}
	return SoftErrorPrefix + suffix + ". The turn was stopped safely — please retry."
}

// TruncateRunes cuts s to at most n runes without splitting a UTF-8 sequence.
// JS String.slice counts UTF-16 code units; byte slicing in Go would emit
// invalid UTF-8 for CJK/emoji error text, which upstreams and clients reject.
// A non-BMP rune counts as the two UTF-16 units JS would see, so a truncation
// limit matched against TS keeps the same visible cut for emoji and rare CJK.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	units := 0
	for i, r := range s {
		if units >= n {
			return s[:i]
		}
		if r > 0xffff {
			// A surrogate pair straddling the cut cannot be represented in Go;
			// stop before it so the result stays valid UTF-8.
			if units+2 > n {
				return s[:i]
			}
			units += 2
			continue
		}
		units++
	}
	return s
}
