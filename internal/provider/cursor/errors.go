package cursor

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ErrorKind is a routing-relevant classification of a Cursor refusal.
type ErrorKind string

const (
	KindAuth      ErrorKind = "auth"
	KindRegion    ErrorKind = "region"
	KindQuota     ErrorKind = "quota"
	KindRateLimit ErrorKind = "rate_limit"
	KindContext   ErrorKind = "context"
	KindInvalid   ErrorKind = "invalid"
	KindCapacity  ErrorKind = "capacity"
	KindOther     ErrorKind = "other"
)

// StreamError is a classified Cursor failure: the status to surface, the
// routing kind, and a message.
type StreamError struct {
	Status  int
	Kind    ErrorKind
	Message string
}

func (e *StreamError) Error() string { return string(e.Kind) + ": " + e.Message }

var kindStatus = map[ErrorKind]int{
	KindAuth:      401,
	KindRegion:    403,
	KindQuota:     429,
	KindRateLimit: 429,
	KindContext:   400,
	KindInvalid:   400,
	KindCapacity:  503,
	KindOther:     502,
}

// ErrorTypes is the client-facing error `type` per kind. Port of
// CURSOR_ERROR_TYPES in src/upstream.ts.
var ErrorTypes = map[ErrorKind]string{
	KindAuth:      "authentication_error",
	KindRegion:    "permission_error",
	KindQuota:     "rate_limit_error",
	KindRateLimit: "rate_limit_error",
	KindContext:   "invalid_request_error",
	KindInvalid:   "invalid_request_error",
	KindCapacity:  "overloaded_error",
	KindOther:     "api_error",
}

// ShouldFailover reports whether a refusal should send the turn to another
// provider. Everything Cursor says about the account or the model right now —
// a regional block, a quota, an overload, a rejected credential — is a fact
// another provider can usually take the turn for. Only a malformed request is
// the client's to fix. Port of cursorShouldFailover in src/upstream.ts.
func ShouldFailover(err *StreamError) bool {
	return err.Kind != KindInvalid && err.Kind != KindContext
}

// SurfacedStatus clamps the status to a 4xx/5xx the client can see.
func SurfacedStatus(err *StreamError) int {
	if err.Status >= 400 && err.Status <= 599 {
		return err.Status
	}
	return 502
}

// CooldownLabel is the spent-provider label a refusal of this kind records
// (markCursorRefusal in src/upstream.ts), or "" when the kind carries no
// verdict about the subscription. For KindAuth the caller should also drop
// its cached token so the next resolve re-reads the keychain/auth.json.
func CooldownLabel(err *StreamError) string {
	switch err.Kind {
	case KindQuota, KindRateLimit:
		return "limit"
	case KindRegion:
		return "region"
	case KindCapacity, KindOther:
		return string(err.Kind)
	case KindAuth:
		return "auth"
	}
	return ""
}

var leadingColon = regexp.MustCompile(`^:\s*`)

// ClassifyError maps a Connect failure (`{code, message, details}` or a stream
// trailer's `{error}`) to a routing-relevant kind and the status to surface.
func ClassifyError(status int, body string) *StreamError {
	parsed := parseFailure(body)
	message := parsed.message
	if parsed.title != "" || parsed.detail != "" {
		prefix := ""
		if parsed.title != "" {
			prefix = parsed.title + ": "
		}
		message = leadingColon.ReplaceAllString(prefix+parsed.detail, "")
	}
	if message == "" || message == "Error" {
		message = parsed.code
	}
	lower := strings.ToLower(message)
	// JS `status / 100 === 2` is only true for exactly 200.
	if message == "" && status == 200 {
		message = "Cursor returned an empty reply"
	}
	if message == "" {
		if status > 0 {
			message = "Cursor upstream error (HTTP " + strconv.Itoa(status) + ")"
		} else {
			message = "Cursor upstream error"
		}
	}
	kind := classifyKind(status, parsed.code, lower)
	return &StreamError{Status: kindStatus[kind], Kind: kind, Message: truncateUTF16(message, 2000)}
}

type failure struct {
	code, message, title, detail string
}

func parseFailure(body string) failure {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return failure{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil || parsed == nil {
		// JSON.parse of a non-object scalar would still "succeed" in TS; for a
		// non-object the field reads all come up empty.
		var scalar any
		if json.Unmarshal([]byte(trimmed), &scalar) == nil {
			return failure{}
		}
		return failure{message: trimmed}
	}
	inner := parsed
	if errObj, ok := parsed["error"].(map[string]any); ok {
		inner = errObj
	}
	var out failure
	if code, ok := inner["code"].(string); ok {
		out.code = strings.ToLower(code)
	}
	out.message, _ = inner["message"].(string)
	details, _ := inner["details"].([]any)
	for _, entry := range details {
		obj, _ := entry.(map[string]any)
		debug, _ := obj["debug"].(map[string]any)
		inner2, _ := debug["details"].(map[string]any)
		if inner2 == nil {
			continue
		}
		if title, ok := inner2["title"].(string); ok {
			out.title = strings.TrimSpace(title)
		}
		if detail, ok := inner2["detail"].(string); ok {
			out.detail = strings.TrimSpace(detail)
		}
	}
	if out.message == "" {
		if text, ok := parsed["error"].(string); ok {
			out.message = text
		}
	}
	return out
}

var (
	signInPattern   = regexp.MustCompile(`sign in|signed out`)
	capacityPattern = regexp.MustCompile(`overloaded|try again later|at capacity|unavailable`)
)

func classifyKind(status int, code, lower string) ErrorKind {
	if strings.Contains(lower, "region") {
		return KindRegion
	}
	if code == "permission_denied" {
		return KindAuth
	}
	if code == "unauthenticated" || status == 401 || strings.Contains(lower, "expired") || signInPattern.MatchString(lower) {
		return KindAuth
	}
	if code == "resource_exhausted" || strings.Contains(lower, "quota") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "usage limit") {
		return KindQuota
	}
	if strings.Contains(lower, "too long") || strings.Contains(lower, "context length") || strings.Contains(lower, "too many tokens") {
		return KindContext
	}
	if code == "invalid_argument" {
		return KindInvalid
	}
	if code == "unavailable" || status == 503 || capacityPattern.MatchString(lower) {
		return KindCapacity
	}
	return KindOther
}

// truncateUTF16 mirrors String.prototype.slice(0, n), which counts UTF-16 units.
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		size := 1
		if r > 0xFFFF {
			size = 2
		}
		if units+size > n {
			return s[:i]
		}
		units += size
	}
	return s
}
