// Package jsoncedit is surgical JSONC editing (port of src/jsonc.ts).
package jsoncedit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Surgical JSONC editing: change only the keys a config edit owns, leaving every
// other byte — comments, blank lines, key order, 2-vs-4-space indent — untouched.
//
// A whole-file json.Unmarshal + MarshalIndent round-trip silently reformats the
// user's settings.json. Claude Code (and most agent CLIs) permit `//` comments and
// trailing commas, so the file is JSONC, not strict JSON. The surgical rule: locate
// the managed member's value span in the text and replace just that span, or insert
// one property in the object's existing indent style. Nothing else is ever
// reordered, deleted, or reprinted.
//
// The surface is intentionally narrow — set or remove an object member, including
// one nested inside an object member like `env`. Keys that cannot be resolved are
// skipped rather than risk a corrupt file.
//
// Port of src/jsonc.ts.

// Edit is one member change in a JSONC document.
type Edit struct {
	// Path is the member path, e.g. []string{"env", "ANTHROPIC_BASE_URL"}.
	Path []string
	// Remove deletes the member instead of writing Value.
	Remove bool
	// Value is the JSON value to write.
	Value any
}

type tokKind byte

const (
	tokStr tokKind = iota
	tokLit
	tokLBrace
	tokRBrace
	tokLBrack
	tokRBrack
	tokColon
	tokComma
)

type tok struct {
	kind       tokKind
	start, end int
	text       string // decoded text for strings, raw for literals/punct
}

// scan tokenizes JSONC, dropping whitespace and both comment forms.
func scanJSONC(text string) []tok {
	var tokens []tok
	n := len(text)
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	for i := 0; i < n; {
		c := text[i]
		if isSpace(c) {
			i++
			continue
		}
		if c == '/' && i+1 < n && text[i+1] == '/' {
			for i < n && text[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && text[i+1] == '*' {
			i += 2
			for i < n && !(text[i] == '*' && i+1 < n && text[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		switch c {
		case '{':
			tokens = append(tokens, tok{tokLBrace, i, i + 1, "{"})
			i++
			continue
		case '}':
			tokens = append(tokens, tok{tokRBrace, i, i + 1, "}"})
			i++
			continue
		case '[':
			tokens = append(tokens, tok{tokLBrack, i, i + 1, "["})
			i++
			continue
		case ']':
			tokens = append(tokens, tok{tokRBrack, i, i + 1, "]"})
			i++
			continue
		case ':':
			tokens = append(tokens, tok{tokColon, i, i + 1, ":"})
			i++
			continue
		case ',':
			tokens = append(tokens, tok{tokComma, i, i + 1, ","})
			i++
			continue
		}
		if c == '"' {
			start := i
			i++
			var value strings.Builder
			for i < n && text[i] != '"' {
				if text[i] == '\\' && i+1 < n {
					esc := text[i+1]
					if esc == 'u' {
						cp := 0
						if i+6 <= n {
							_, _ = fmt.Sscanf(text[i+2:i+6], "%04x", &cp)
						}
						if cp > 0 {
							value.WriteRune(rune(cp))
						}
						i += 6
					} else {
						switch esc {
						case 'n':
							value.WriteByte('\n')
						case 't':
							value.WriteByte('\t')
						case 'r':
							value.WriteByte('\r')
						case 'b':
							value.WriteByte('\b')
						case 'f':
							value.WriteByte('\f')
						default:
							value.WriteByte(esc)
						}
						i += 2
					}
					continue
				}
				value.WriteByte(text[i])
				i++
			}
			i++ // closing quote
			tokens = append(tokens, tok{tokStr, start, i, value.String()})
			continue
		}
		// Literal: number, true, false, null.
		start := i
		for i < n {
			b := text[i]
			if isSpace(b) || b == '{' || b == '}' || b == '[' || b == ']' ||
				b == ':' || b == ',' || b == '"' {
				break
			}
			if b == '/' && i+1 < n && (text[i+1] == '/' || text[i+1] == '*') {
				break
			}
			i++
		}
		tokens = append(tokens, tok{tokLit, start, i, text[start:i]})
	}
	return tokens
}

// member is one object member at the current depth.
type member struct {
	keyTok   tok
	valIdx   int // index of the token the value starts at
	valueEnd int // end offset of the member's value in the source text
}

// valueEndIndex returns the index of the token after the value starting at vi
// (skips nested blocks).
func valueEndIndex(tokens []tok, vi int) int {
	if vi < 0 || vi >= len(tokens) {
		return vi
	}
	t := tokens[vi]
	if t.kind == tokLBrace {
		depth := 1
		j := vi + 1
		for j < len(tokens) && depth > 0 {
			if tokens[j].kind == tokLBrace {
				depth++
			} else if tokens[j].kind == tokRBrace {
				depth--
			}
			j++
		}
		return j
	}
	if t.kind == tokLBrack {
		depth := 1
		j := vi + 1
		for j < len(tokens) && depth > 0 {
			if tokens[j].kind == tokLBrack {
				depth++
			} else if tokens[j].kind == tokRBrack {
				depth--
			}
			j++
		}
		return j
	}
	return vi + 1
}

// membersOf iterates the top-level members of the object whose `{` is at
// tokens[startIdx], or the document root when startIdx is -1.
func membersOf(tokens []tok, startIdx int) (members []member, closeIdx int) {
	i := startIdx + 1
	closeIdx = len(tokens)
	for i < len(tokens) {
		t := tokens[i]
		if t.kind == tokRBrace {
			closeIdx = i
			break
		}
		if t.kind == tokStr && i+1 < len(tokens) && tokens[i+1].kind == tokColon {
			valueIndex := i + 2
			endIdx := valueEndIndex(tokens, valueIndex)
			end := t.end
			if endIdx-1 >= 0 && endIdx-1 < len(tokens) {
				end = tokens[endIdx-1].end
			} else if valueIndex < len(tokens) {
				end = tokens[valueIndex].end
			}
			members = append(members, member{keyTok: t, valIdx: valueIndex, valueEnd: end})
			i = endIdx
			continue
		}
		i++
	}
	return members, closeIdx
}

type resolvedPath struct {
	found    bool
	vs, ve   int // value span when found
	keyStart int
	insertAt int
	indent   string
}

// resolvePath resolves path to a concrete edit site: the member's value span when
// found, or the insertion point (the closing `}` of the containing object) when
// the leaf is missing. nil when a non-final path step is absent or not an object.
func resolvePath(tokens []tok, text string, path []string) *resolvedPath {
	scopeStart := -1 // -1 → root object
	for depth := 0; depth < len(path); depth++ {
		startIdx := scopeStart
		members, closeIdx := membersOf(tokens, startIdx)
		key := path[depth]
		var found *member
		for i := range members {
			if members[i].keyTok.text == key {
				found = &members[i]
				break
			}
		}
		last := depth == len(path)-1
		if found != nil && last {
			return &resolvedPath{
				found:    true,
				vs:       tokens[found.valIdx].start,
				ve:       found.valueEnd,
				keyStart: found.keyTok.start,
			}
		}
		if found != nil {
			valueTok := tokens[found.valIdx]
			if valueTok.kind != tokLBrace {
				return nil // not an object we can descend into
			}
			scopeStart = found.valIdx
			continue
		}
		if !last {
			return nil // can't create intermediate objects surgically
		}
		insertAt := len(text)
		if closeIdx < len(tokens) {
			insertAt = tokens[closeIdx].start
		}
		return &resolvedPath{
			found:    false,
			insertAt: insertAt,
			indent:   detectIndent(text, startIdx, tokens, closeIdx),
		}
	}
	return nil
}

// lineIndent is the leading whitespace of the line containing offset.
func lineIndent(text string, offset int) string {
	start := strings.LastIndex(text[:max(offset, 0)], "\n") + 1
	i := start
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return text[start:i]
}

// detectIndent is the indent used by members of the object whose `{` is
// tokens[startIdx] (or root if -1).
func detectIndent(text string, startIdx int, tokens []tok, closeIdx int) string {
	var openTok *tok
	if startIdx >= 0 && startIdx < len(tokens) {
		openTok = &tokens[startIdx]
	}
	var first *tok
	if startIdx+1 < len(tokens) {
		first = &tokens[startIdx+1]
	}
	if openTok != nil && first != nil && first.kind != tokRBrace && closeIdx < len(tokens) {
		ind := lineIndent(text, first.start)
		if ind != "" {
			return ind
		}
	}
	base := ""
	if openTok != nil {
		base = lineIndent(text, openTok.start)
	}
	return base + "  "
}

// removeMember removes path's member, swallowing one adjacent comma so the
// object stays valid.
func removeMember(text string, tokens []tok, path []string) string {
	resolved := resolvePath(tokens, text, path)
	if resolved == nil || !resolved.found {
		return text
	}
	keyStart, valueEnd := resolved.keyStart, resolved.ve

	keyIdx := -1
	for i := range tokens {
		if tokens[i].start == keyStart {
			keyIdx = i
			break
		}
	}
	if keyIdx < 0 {
		return text
	}

	afterIdx := valueEndIndex(tokens, keyIdx+2) // token index just after the value
	hasFollowingComma := afterIdx < len(tokens) && tokens[afterIdx].kind == tokComma
	hasPrecedingComma := keyIdx-1 >= 0 && tokens[keyIdx-1].kind == tokComma

	start := keyStart
	end := valueEnd
	if hasFollowingComma {
		// Removing a middle/first member: swallow the comma and the
		// whitespace+newline after it, and back up over the member's leading line
		// indent so no blank or over-indented line is left behind.
		end = tokens[afterIdx].end
		for end < len(text) && (text[end] == ' ' || text[end] == '\t') {
			end++
		}
		if end < len(text) && text[end] == '\n' {
			end++
		}
		s := start - 1
		for s >= 0 && (text[s] == ' ' || text[s] == '\t') {
			s--
		}
		if s >= 0 && text[s] == '\n' {
			start = s + 1 // keep the newline; drop the indent
		}
	} else if hasPrecedingComma {
		// Removing the last member: take the preceding comma plus the whitespace
		// and newline that separated it from the previous member, so the close
		// brace lands on the previous member's own line ending.
		start = tokens[keyIdx-1].start
		s := start - 1
		for s >= 0 && (text[s] == ' ' || text[s] == '\t') {
			s--
		}
		if s >= 0 && text[s] == '\n' {
			start = s + 1
		}
		for end < len(text) && (text[end] == ' ' || text[end] == '\t') {
			end++
		}
		if end < len(text) && text[end] == '\n' {
			end++
			for end < len(text) && (text[end] == ' ' || text[end] == '\t') {
				end++
			}
			return text[:start] + "\n" + lineIndent(text, end) + text[end:]
		}
	}
	return text[:start] + text[end:]
}

// insertMember inserts `key: serialized` at insertAt (the containing object's `}`).
func insertMember(text, serialized string, insertAt int, key, indent string) string {
	prev := insertAt - 1
	for prev >= 0 && isJSONSpace(text[prev]) {
		prev--
	}
	emptyObject := prev >= 0 && text[prev] == '{'
	needsComma := !emptyObject && prev >= 0 && text[prev] != ','
	closingIndent := lineIndent(text, insertAt)

	head := text[:prev+1]
	tail := text[insertAt:]
	if emptyObject {
		return head + "\n" + indent + "\"" + key + "\": " + serialized + "\n" + closingIndent + tail
	}
	comma := ""
	if needsComma {
		comma = ","
	}
	return head + comma + "\n" + indent + "\"" + key + "\": " + serialized + "\n" + closingIndent + tail
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// applyOneEdit applies a single edit, returning updated text (or the original
// when unresolvable).
func applyOneEdit(text string, edit Edit) string {
	if len(edit.Path) == 0 {
		return text
	}
	tokens := scanJSONC(text)
	resolved := resolvePath(tokens, text, edit.Path)
	if resolved == nil {
		return text
	}

	if edit.Remove {
		if resolved.found {
			return removeMember(text, tokens, edit.Path)
		}
		return text
	}

	serialized, err := json.Marshal(edit.Value)
	if err != nil {
		return text
	}
	if resolved.found {
		return text[:resolved.vs] + string(serialized) + text[resolved.ve:]
	}
	return insertMember(text, string(serialized), resolved.insertAt, edit.Path[len(edit.Path)-1], resolved.indent)
}

// ApplyEdits applies edits to JSONC text. Each edit is resolved against the
// fresh text so offsets never drift. Unresolvable paths are skipped; the file is
// never corrupted.
func ApplyEdits(text string, edits []Edit) string {
	for _, edit := range edits {
		text = applyOneEdit(text, edit)
	}
	return text
}

// PathExists reports whether path resolves to an existing member value in
// the JSONC text.
func PathExists(text string, path []string) bool {
	r := resolvePath(scanJSONC(text), text, path)
	return r != nil && r.found
}

// ObjectKeys returns the immediate member keys of the object at path, or
// nil when the path is absent or is not an object. Lets a caller decide whether
// a block is now empty without a strict unmarshal that would choke on comments.
func ObjectKeys(text string, path []string) []string {
	tokens := scanJSONC(text)
	resolved := resolvePath(tokens, text, path)
	if resolved == nil || !resolved.found {
		return nil
	}
	valueIdx := -1
	for i := range tokens {
		if tokens[i].start == resolved.vs {
			valueIdx = i
			break
		}
	}
	if valueIdx < 0 || tokens[valueIdx].kind != tokLBrace {
		return nil
	}
	members, _ := membersOf(tokens, valueIdx)
	keys := make([]string, 0, len(members))
	for _, m := range members {
		keys = append(keys, m.keyTok.text)
	}
	return keys
}
