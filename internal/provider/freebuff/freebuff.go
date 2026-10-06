// Package freebuff is the Freebuff (Codebuff free tier) wire core.
//
// Freebuff is an ad-subsidised coding agent. It has no public API key: the
// official CLI signs in with a device code and every chat then needs a free
// session and an agent run before /chat/completions will accept the body.
//
//	POST /api/v1/freebuff/session   x-freebuff-model          -> instanceId (~1h)
//	POST /api/v1/agent-runs         {action:START, agentId}   -> runId
//	POST /api/v1/chat/completions   CLI envelope, SSE only
//
// This package owns the lifecycle (single-flight sessions, run reuse) and the
// request envelope. The HTTP POST itself is left to the shared upstream
// client so timeouts, retries and first-byte guards stay in one place.
package freebuff

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"strings"
)

const (
	// ID is the oauthSource value for Freebuff.
	ID = "freebuff"
	// BaseURL is the Codebuff API root.
	BaseURL = "https://www.codebuff.com"
	// ChatPath is the chat completions endpoint under BaseURL.
	ChatPath = "/api/v1/chat/completions"

	sessionPath = "/api/v1/freebuff/session"
	runsPath    = "/api/v1/agent-runs"

	// UserAgent mirrors the official CLI's identifier.
	UserAgent = "Freebuff-CLI/0.0.138"
	// SDKUserAgent is what the Codebuff SDK sends on chat requests.
	SDKUserAgent = "ai-sdk/openai-compatible/1.0.25/codebuff"

	// BuffyMarker must open the first system message byte for byte; the server
	// answers 403 free_mode_cli_required otherwise.
	BuffyMarker = "You are Buffy, the strategic coding assistant."

	// StopSentinel is the stop sequence the CLI always sends. The community
	// proxies disagree on whether it is quoted (`cb_easp` vs `"cb_easp"`);
	// two of three send it bare and the server only checks that one is present,
	// so the bare form is the default. Options.Stop overrides it.
	StopSentinel = "cb_easp"

	// ContextPrunerAgent is the child run the CLI starts beside the root agent.
	ContextPrunerAgent = "context-pruner"

	// MaxConcurrent caps simultaneous turns per Freebuff provider. It is a
	// conservative guess, not a measured limit: the community proxies serialize
	// upstream calls, and this free tier is easy to trip into 429/ban. (Parallel
	// session creation is prevented separately, by the Manager's single-flight.)
	// Raise it only after observing that the server tolerates more.
	MaxConcurrent = 2

	// DefaultModel is used when a request names no known Freebuff model.
	DefaultModel = "deepseek/deepseek-v4-flash"
)

// EndpointFromBaseURL returns the API origin for a provider baseUrl. A
// baseUrl that already ends in /api/v1 (or a /v1 gateway path) is reduced to
// its origin; an empty one means the public host.
func EndpointFromBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return BaseURL
	}
	for _, suffix := range []string{"/api/v1", "/v1"} {
		trimmed = strings.TrimSuffix(trimmed, suffix)
	}
	return trimmed
}

// ChatURL is the chat endpoint for a provider baseUrl.
func ChatURL(baseURL string) string { return EndpointFromBaseURL(baseURL) + ChatPath }

// ApplyHeaders sets the CLI identity headers every Freebuff call carries.
func ApplyHeaders(h map[string]string, token string) {
	h["authorization"] = "Bearer " + token
	h["content-type"] = "application/json"
	h["user-agent"] = SDKUserAgent
	h["accept"] = "application/json, text/event-stream"
}

// SetHeaders is ApplyHeaders on an http.Header.
func SetHeaders(h http.Header, token string) {
	m := map[string]string{}
	ApplyHeaders(m, token)
	for k, v := range m {
		h.Set(k, v)
	}
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// ClientID is a fresh 13-character base36 id. A fixed value is fingerprinted
// as a proxy, so every chat gets its own.
func ClientID() string {
	out := make([]byte, 13)
	max := big.NewInt(int64(len(base36)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			out[i] = base36[i%len(base36)]
			continue
		}
		out[i] = base36[n.Int64()]
	}
	return string(out)
}
