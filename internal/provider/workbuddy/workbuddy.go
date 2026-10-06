// Package workbuddy is the WorkBuddy AI (international) subscription —
// Magpie-parity.
//
// Speaks OpenAI Chat Completions at https://www.workbuddy.ai/v2 under the
// account's access token, with the same headers WorkBuddy's desktop client
// sends. Credentials come from a Jevonian browser sign-in (stored under
// ~/.config/jevonian), or from a plaintext desktop session file when
// credential protection is off.
//
// Port of src/workbuddy.ts.
package workbuddy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

const (
	// ID is the oauthSource value for WorkBuddy AI.
	ID = "workbuddy-ai"
	// Endpoint is the WorkBuddy AI host root.
	Endpoint = "https://www.workbuddy.ai"
	// BaseURL is the OpenAI-compatible chat base.
	BaseURL = Endpoint + "/v2"
	// UAVersion is the WorkBuddy desktop version advertised in headers.
	UAVersion = "5.5.6"
	// AppVersion is the WorkBuddy CLI version advertised in headers.
	AppVersion = "2.0.0"
	// SystemPrompt is injected when a chat omits a system message.
	SystemPrompt = "You are a helpful assistant."

	// DefaultDomain is the X-Domain sent when a session names none.
	DefaultDomain = "www.workbuddy.ai"

	refreshSkew       = 60 * time.Second
	signinPollDefault = time.Second
	signinTimeout     = 5 * time.Minute
	retryCodeToken    = 11217
	retryCodeAccount  = 12151
)

// Creds is one WorkBuddy AI session. Times are Unix milliseconds (0 = unknown),
// matching the JSON the TypeScript build wrote so existing session files read.
type Creds struct {
	UID              string `json:"uid"`
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	Domain           string `json:"domain"`
	TokenType        string `json:"tokenType,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
}

// IsSource reports whether an oauthSource names WorkBuddy AI.
func IsSource(source string) bool {
	return source == ID
}

// DesktopAuthPath is where the WorkBuddy desktop app keeps its session.
func DesktopAuthPath() string {
	home, _ := os.UserHomeDir()
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension")
	case "windows":
		base = filepath.Join(home, "AppData", "Local", "CodeBuddyExtension")
	default:
		base = filepath.Join(home, ".local", "share", "CodeBuddyExtension")
	}
	return filepath.Join(base, "Data", "Public", "auth", "workbuddy-desktop-ai.info")
}

// SessionPath is Jevonian's own session file. JEVONIAN_WORKBUDDY_AI_AUTH wins,
// then a login's credentialsPath, then $XDG_CONFIG_HOME/jevonian/workbuddy-ai.json.
func SessionPath(login *multiacct.Login) string {
	if v := strings.TrimSpace(os.Getenv("JEVONIAN_WORKBUDDY_AI_AUTH")); v != "" {
		return v
	}
	if login != nil && login.CredentialsPath != "" {
		return login.CredentialsPath
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "jevonian", "workbuddy-ai.json")
}

// CacheDir is the WorkBuddy cache directory under dataDir.
func CacheDir(dataDir string) string {
	return filepath.Join(dataDir, "workbuddy-ai")
}

func asRecord(value any) map[string]any {
	if m, ok := value.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// PlainString is a plain-string JSON field; encrypted envelopes ($wbEncrypted)
// read as empty.
func PlainString(raw any) string {
	s, _ := raw.(string)
	return s
}

func number(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func numberOrZero(raw any) int64 {
	v, _ := number(raw)
	return int64(v)
}

func domainOf(c Creds) string {
	if c.Domain != "" {
		return c.Domain
	}
	return DefaultDomain
}

func nowMS(now time.Time) int64 { return now.UnixMilli() }

func nearExpiry(expiresAt int64, now time.Time) bool {
	if expiresAt == 0 {
		return false
	}
	return nowMS(now) >= expiresAt-refreshSkew.Milliseconds()
}

// ParseSession reads either the flat Jevonian-stored shape or the desktop
// `workbuddy-desktop-ai.info` shape. Returns nil when no plaintext uid and
// access token are present (encrypted desktop files are not supported).
func ParseSession(data map[string]any, now time.Time) *Creds {
	// Jevonian-stored shape (flat).
	flatAccess := PlainString(data["accessToken"])
	flatUID := PlainString(data["uid"])
	if flatAccess != "" && flatUID != "" {
		c := &Creds{
			UID:              flatUID,
			AccessToken:      flatAccess,
			RefreshToken:     PlainString(data["refreshToken"]),
			ExpiresAt:        numberOrZero(data["expiresAt"]),
			RefreshExpiresAt: numberOrZero(data["refreshExpiresAt"]),
			Domain:           PlainString(data["domain"]),
			TokenType:        PlainString(data["tokenType"]),
			Nickname:         PlainString(data["nickname"]),
		}
		if c.Domain == "" {
			c.Domain = DefaultDomain
		}
		return c
	}

	// Desktop shape.
	account := asRecord(data["account"])
	auth := asRecord(data["auth"])
	uid := PlainString(account["uid"])
	access := PlainString(auth["accessToken"])
	if uid == "" || access == "" {
		return nil
	}
	expiresAt := numberOrZero(auth["expiresAt"])
	refreshExpiresAt := numberOrZero(auth["refreshExpiresAt"])
	if expiresAt == 0 {
		if v, ok := number(auth["expiresIn"]); ok && v > 0 {
			expiresAt = nowMS(now) + int64(v*1000)
		}
	}
	if refreshExpiresAt == 0 {
		if v, ok := number(auth["refreshExpiresIn"]); ok && v > 0 {
			refreshExpiresAt = nowMS(now) + int64(v*1000)
		}
	}
	c := &Creds{
		UID:              uid,
		AccessToken:      access,
		RefreshToken:     PlainString(auth["refreshToken"]),
		ExpiresAt:        expiresAt,
		RefreshExpiresAt: refreshExpiresAt,
		Domain:           PlainString(auth["domain"]),
		TokenType:        PlainString(auth["tokenType"]),
		Nickname:         PlainString(account["nickname"]),
	}
	if c.Domain == "" {
		c.Domain = DefaultDomain
	}
	return c
}

func readJSONFile(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// ReadSession reads the session for login, or nil when none is usable.
//
// An explicit path (env or second-account login) is the only place to look —
// it does not fall through to the desktop app's file and silently pick up a
// different account.
func ReadSession(login *multiacct.Login) *Creds {
	now := time.Now()
	if session := readJSONFile(SessionPath(login)); session != nil {
		if parsed := ParseSession(session, now); parsed != nil {
			return parsed
		}
	}
	if strings.TrimSpace(os.Getenv("JEVONIAN_WORKBUDDY_AI_AUTH")) != "" ||
		(login != nil && login.CredentialsPath != "") {
		return nil
	}
	desktop := readJSONFile(DesktopAuthPath())
	if desktop == nil {
		return nil
	}
	return ParseSession(desktop, now)
}

// HasCredential reports whether a usable session exists for login.
func HasCredential(login *multiacct.Login) bool {
	return ReadSession(login) != nil
}

// SaveSession writes creds to Jevonian's session file with owner-only mode.
func SaveSession(creds Creds, login *multiacct.Login) error {
	path := SessionPath(login)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AuthHeaders are the auth + product headers every WorkBuddy AI call carries.
// Keys are lowercase.
func AuthHeaders(creds Creds) map[string]string {
	return map[string]string{
		"authorization": "Bearer " + creds.AccessToken,
		"x-user-id":     creds.UID,
		"x-domain":      domainOf(creds),
		"x-product":     "SaaS",
		"x-ide-type":    "WorkBuddy",
		"user-agent":    "WorkBuddy/" + UAVersion,
	}
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ClientHeaders are the extra client headers Magpie sends only for WorkBuddy
// AI chats. A session id (alphanumerics, first 32) becomes the conversation id.
func ClientHeaders(session string) map[string]string {
	fromSession := ""
	if session != "" {
		fromSession = nonAlnum.ReplaceAllString(session, "")
		if len(fromSession) > 32 {
			fromSession = fromSession[:32]
		}
	}
	conversation := fromSession
	if conversation == "" {
		conversation = requestID()
	}
	message := requestID()
	return map[string]string{
		"x-requested-with":          "XMLHttpRequest",
		"x-agent-intent":            "craft",
		"x-agent-type":              "main",
		"x-ide-name":                "WorkBuddy",
		"x-ide-version":             UAVersion,
		"x-conversation-id":         conversation,
		"x-conversation-request-id": conversation,
		"x-conversation-message-id": message,
		"x-request-id":              message,
	}
}

// ApplyHeaders fills headers (lowercase keys) for a WorkBuddy AI chat. Preset
// headers win except the live token, account, and domain, which always
// overwrite a stale preset override.
func ApplyHeaders(headers map[string]string, creds Creds, session string) {
	auth := AuthHeaders(creds)
	for k, v := range auth {
		if _, ok := headers[k]; !ok {
			headers[k] = v
		}
	}
	headers["authorization"] = auth["authorization"]
	headers["x-user-id"] = auth["x-user-id"]
	headers["x-domain"] = auth["x-domain"]
	for k, v := range ClientHeaders(session) {
		if _, ok := headers[k]; !ok {
			headers[k] = v
		}
	}
}

// EnsureSystem injects a generic system prompt when the first message is not a
// system message — WorkBuddy refuses such chats (scripts and plain OpenAI
// clients often omit it). Returns a shallow copy when it changes anything.
func EnsureSystem(body map[string]any) map[string]any {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body
	}
	if asRecord(messages[0])["role"] == "system" {
		return body
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	next := make([]any, 0, len(messages)+1)
	next = append(next, map[string]any{"role": "system", "content": SystemPrompt})
	next = append(next, messages...)
	out["messages"] = next
	return out
}

// EndpointFromBaseURL strips a trailing /v2 from a provider baseUrl, falling
// back to Endpoint when nothing is left.
func EndpointFromBaseURL(baseURL string) string {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v2")
	if trimmed == "" {
		return Endpoint
	}
	return trimmed
}

// APIError is a WorkBuddy `{code, msg}` refusal.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// ErrNoCredentials is wrapped by Resolve when no session exists.
var ErrNoCredentials = errors.New(
	"WorkBuddy AI credentials not found. Use Discover or Save on the dashboard to sign in (browser), run `jevonian add workbuddy-ai-subscription`, or point JEVONIAN_WORKBUDDY_AI_AUTH at a plaintext session file.",
)

// ErrSignedOut is returned when the session cannot be refreshed and holds no token.
var ErrSignedOut = errors.New("WorkBuddy AI account is signed out; sign in again.")
