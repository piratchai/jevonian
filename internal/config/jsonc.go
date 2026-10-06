package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// UnmarshalJSONC parses JSONC (JSON with // and /* */ comments plus trailing commas)
// into v, the same way encoding/json.Unmarshal would for strict JSON.
func UnmarshalJSONC(data []byte, v any) error {
	cleaned, err := StripJSONC(data)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(cleaned, v); err != nil {
		return fmt.Errorf("jsonc: %w", err)
	}
	return nil
}

// StripJSONC removes comments and trailing commas, producing strict JSON bytes.
func StripJSONC(src []byte) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(src))
	i := 0
	n := len(src)
	for i < n {
		c := src[i]

		// Line comment.
		if c == '/' && i+1 < n && src[i+1] == '/' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment.
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 >= n {
				return nil, fmt.Errorf("jsonc: unclosed block comment")
			}
			i += 2
			continue
		}

		// String — copy verbatim so // inside strings is preserved.
		if c == '"' {
			start := i
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == '"' {
					i++
					break
				}
				i++
			}
			out.Write(src[start:i])
			continue
		}

		// Trailing comma before } or ].
		if c == ',' {
			j := i + 1
			for j < n {
				r, size := utf8.DecodeRune(src[j:])
				if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
					j += size
					continue
				}
				// Skip comments between comma and closer.
				if src[j] == '/' && j+1 < n && src[j+1] == '/' {
					j += 2
					for j < n && src[j] != '\n' {
						j++
					}
					continue
				}
				if src[j] == '/' && j+1 < n && src[j+1] == '*' {
					j += 2
					for j+1 < n && !(src[j] == '*' && src[j+1] == '/') {
						j++
					}
					if j+1 >= n {
						return nil, fmt.Errorf("jsonc: unclosed block comment")
					}
					j += 2
					continue
				}
				break
			}
			if j < n && (src[j] == '}' || src[j] == ']') {
				i++ // drop the comma
				continue
			}
		}

		out.WriteByte(c)
		i++
	}
	return out.Bytes(), nil
}
