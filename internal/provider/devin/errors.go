package devin

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/wire"
)

// ErrorKind is a routing-relevant classification of a Devin refusal.
type ErrorKind string

const (
	KindQuota         ErrorKind = "quota"
	KindRateLimit     ErrorKind = "rate_limit"
	KindCapacity      ErrorKind = "capacity"
	KindInternal      ErrorKind = "internal"
	KindContentPolicy ErrorKind = "content_policy"
	KindModelBlocked  ErrorKind = "model_blocked"
	KindAuth          ErrorKind = "auth"
	KindOther         ErrorKind = "other"
)

// StreamError is a classified Devin failure: the HTTP status to surface, the
// routing kind, a credential-free message, and (for limits) the reset time.
type StreamError struct {
	Status   int
	Kind     ErrorKind
	Message  string
	ResetsAt time.Time // zero when the upstream named no reset
}

func (e *StreamError) Error() string { return string(e.Kind) + ": " + e.Message }

var kindStatus = map[ErrorKind]int{
	KindQuota:         429,
	KindRateLimit:     429,
	KindCapacity:      503,
	KindInternal:      502,
	KindContentPolicy: 400,
	KindModelBlocked:  403,
	KindAuth:          401,
	KindOther:         502,
}

var kindDefaultMessage = map[ErrorKind]string{
	KindQuota:         "Devin account is out of credit or quota",
	KindRateLimit:     "Devin rate limit reached",
	KindCapacity:      "Devin model is temporarily at capacity",
	KindInternal:      "Devin upstream internal error",
	KindContentPolicy: "Request blocked by Devin content policy",
	KindModelBlocked:  "Model requires a paid Devin plan",
	KindAuth:          "Devin authentication failed",
	KindOther:         "Devin upstream error",
}

// ErrorTypes is the client-facing error `type` per kind (OpenAI/Anthropic
// envelope vocabulary). Port of DEVIN_ERROR_TYPES in src/upstream.ts.
var ErrorTypes = map[ErrorKind]string{
	KindQuota:         "rate_limit_error",
	KindRateLimit:     "rate_limit_error",
	KindCapacity:      "overloaded_error",
	KindInternal:      "api_error",
	KindContentPolicy: "invalid_request_error",
	KindModelBlocked:  "invalid_request_error",
	KindAuth:          "authentication_error",
	KindOther:         "api_error",
}

// ShouldFailover reports whether a refusal should send the turn to another
// provider. Only content_policy — a judgment about the request text — is kept.
func ShouldFailover(err *StreamError) bool { return err.Kind != KindContentPolicy }

// SurfacedStatus clamps the status to a 4xx/5xx the client can see.
func SurfacedStatus(err *StreamError) int {
	if err.Status >= 400 && err.Status <= 599 {
		return err.Status
	}
	return 502
}

// maxReset caps a reset window honoured from a "Resets in: 3h0m0s" hint.
const maxReset = 7 * 24 * time.Hour

var (
	resetCompact = regexp.MustCompile(`(?i)resets? in[:\s]+((?:\d+(?:\.\d+)?(?:h|ms|m|s))+)`)
	compactUnit  = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)(h|ms|m|s)`)
	resetProse   = regexp.MustCompile(`(?i)resets? in[:\s]+((?:\d+\s*(?:days?|hours?|hrs?|minutes?|mins?|seconds?|secs?)[\s,]*(?:and\s+)?)+)`)
	proseDays    = regexp.MustCompile(`(?i)(\d+)\s*days?`)
	proseHours   = regexp.MustCompile(`(?i)(\d+)\s*(?:hours?|hrs?)`)
	proseMinutes = regexp.MustCompile(`(?i)(\d+)\s*(?:minutes?|mins?)`)
	proseSeconds = regexp.MustCompile(`(?i)(\d+)\s*(?:seconds?|secs?)`)
)

func firstNumber(re *regexp.Regexp, text string) float64 {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

// resetDuration parses Go time.Duration spelling ("Resets in: 3h0m0s",
// "resets in 45s") and the prose the free-model limit uses ("Your limit will
// reset in 2 hours 37 minutes"). RE2 has no lookahead, so the compact form is
// tokenized into number+unit pairs instead of the TS `m(?!s)` pattern.
func resetDuration(text string) time.Duration {
	compact := ""
	if m := resetCompact.FindStringSubmatch(text); m != nil {
		compact = m[1]
	}
	var d time.Duration
	if compact != "" {
		var ms float64
		for _, m := range compactUnit.FindAllStringSubmatch(compact, -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			switch strings.ToLower(m[2]) {
			case "h":
				ms += v * 3_600_000
			case "m":
				ms += v * 60_000
			case "s":
				ms += v * 1000
			}
			// "ms" is not a unit the TS parser reads; it contributes nothing.
		}
		d = time.Duration(ms * float64(time.Millisecond))
	} else if m := resetProse.FindStringSubmatch(text); m != nil {
		prose := m[1]
		ms := firstNumber(proseDays, prose)*86_400_000 +
			firstNumber(proseHours, prose)*3_600_000 +
			firstNumber(proseMinutes, prose)*60_000 +
			firstNumber(proseSeconds, prose)*1000
		d = time.Duration(ms * float64(time.Millisecond))
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(math.Min(float64(d), float64(maxReset)))
}

// errorParts pulls {code,message} out of unary ({code,message}) or trailer
// ({error:{...}}) JSON.
func errorParts(text string) (code, message string) {
	trimmed := strings.TrimSpace(text)
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return "", trimmed
	}
	record := wire.AsRecord(parsed)
	inner := wire.AsRecord(record["error"])
	source := record
	if len(inner) > 0 {
		source = inner
	}
	message = wire.AsString(source["message"])
	if message == "" {
		message = wire.AsString(record["error"])
	}
	if message == "" {
		message = trimmed
	}
	return strings.ToLower(wire.AsString(source["code"])), message
}

var (
	reRateLimitWord    = regexp.MustCompile(`rate limit`)
	reResetsIn         = regexp.MustCompile(`resets? in[:\s]`)
	reQuota            = regexp.MustCompile(`insufficient.*(credit|quota|balance|funds)|out of (credits?|quota)|quota.*exceeded|exceeded.*quota|(credit|quota|balance|funds).*(exhausted|depleted|spent|used up)|exhausted.*(credit|quota|balance|funds)|quota has been|usage quota has been|daily usage|usage (cap|allowance|limit).*(reached|hit|exceeded)`)
	reCapacity         = regexp.MustCompile(`high demand|try again later|currently (busy|overloaded|at capacity)|overloaded|temporarily (busy|unavailable)|server is busy|(service|backend|model|server) (is )?(temporarily )?unavailable|at capacity`)
	reInternal         = regexp.MustCompile(`internal error occurred`)
	reContentPolicy    = regexp.MustCompile(`blocked by (our |the )?content policy|remove (sensitive|unsafe) content|content[_ ]policy`)
	reModelBlocked     = regexp.MustCompile(`/upgrade|upgrade to (access|pro|a paid)|insufficient.*entitlement|requires? (a )?(paid|pro|team|teams|enterprise)`)
	reMCPConfig        = regexp.MustCompile(`mcp configuration issue`)
	reAuth             = regexp.MustCompile(`permission_denied|unauthenticated|invalid.*token|token.*(expired|invalid|revoked)`)
	reRateLimitLoose   = regexp.MustCompile(`rate.?limit|too many requests|resource_exhausted`)
	reCredentialLabel  = regexp.MustCompile(`(?i)devin-session-token\$|\bBasic\s+`)
	modelScopedPattern = regexp.MustCompile(`(?i)\b(?:for this model|for the model|free model rate limit)\b`)
)

func classifyKind(status int, code, lc string) ErrorKind {
	// A hard per-model limit carries its reset window; it beats the "try again later" capacity arm.
	if reRateLimitWord.MatchString(lc) && reResetsIn.MatchString(lc) {
		return KindRateLimit
	}
	if reQuota.MatchString(lc) {
		return KindQuota
	}
	if status == 429 || code == "resource_exhausted" {
		return KindRateLimit
	}
	// Transient faults often arrive in a 401/403 shell, so text patterns win over status.
	if code == "unavailable" || reCapacity.MatchString(lc) {
		return KindCapacity
	}
	if reInternal.MatchString(lc) {
		return KindInternal
	}
	if reContentPolicy.MatchString(lc) {
		return KindContentPolicy
	}
	if reModelBlocked.MatchString(lc) {
		return KindModelBlocked
	}
	// A tool-schema gate the server reports as permission_denied; the token is fine.
	if reMCPConfig.MatchString(lc) {
		return KindOther
	}
	if status == 401 || status == 403 || code == "permission_denied" || code == "unauthenticated" || reAuth.MatchString(lc) {
		return KindAuth
	}
	if reRateLimitLoose.MatchString(lc) {
		return KindRateLimit
	}
	return KindOther
}

// redactCredentials scrubs credentials before upstream text can reach an
// error, client, or ledger.
func redactCredentials(message string, kind ErrorKind, token string) string {
	safe := message
	if token != "" {
		escaped := jsonString(token)
		variants := []string{token}
		if len(escaped) >= 2 {
			variants = append(variants, escaped[1:len(escaped)-1])
		}
		for _, secret := range variants {
			if secret != "" {
				safe = strings.ReplaceAll(safe, secret, "[REDACTED]")
			}
		}
	}
	// A label may precede an unknown credential with arbitrary punctuation or
	// escapes; rather than guess its boundary, discard the whole message.
	if reCredentialLabel.MatchString(safe) {
		return kindDefaultMessage[kind]
	}
	return safe
}

// ClassifyError maps an upstream failure (HTTP status plus body or trailer
// text) to a routing kind and the status to surface. Pass status 0 for
// in-stream trailer errors. Supply the local token so even nonstandard
// credential formats are scrubbed.
func ClassifyError(status int, text, token string) *StreamError {
	return classifyAt(status, text, token, time.Now())
}

func classifyAt(status int, text, token string, now time.Time) *StreamError {
	code, message := errorParts(text)
	lc := strings.ToLower(code + " " + message)
	kind := classifyKind(status, code, lc)
	if message == "" {
		message = kindDefaultMessage[kind]
	}
	message = redactCredentials(message, kind, token)
	message = wire.TruncateRunes(message, 2000)
	out := &StreamError{Status: kindStatus[kind], Kind: kind, Message: message}
	if kind == KindRateLimit || kind == KindQuota {
		if d := resetDuration(message); d > 0 {
			out.ResetsAt = now.Add(d).UTC()
		}
	}
	return out
}
