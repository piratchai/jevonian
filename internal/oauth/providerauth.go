package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/provider/freebuff"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
)

// This file ports src/auth.ts: turning a provider's auth declaration into the
// concrete headers an upstream call carries. The resolver (credentials, refresh,
// write-back) lives in oauth.go; this file owns the header vocabulary.

// AuthResolution is the outbound headers (and the raw token for wires that do
// not authenticate with a header). Devin's and Cursor's Connect-RPC wire embeds
// the session token in the request body, so for type "devin"/"cursor" Headers
// is empty and their wire modules build their own from Token.
type AuthResolution struct {
	Headers map[string]string
	Project string
	Token   string
}

// ErrMissingAuth is the class of error reported for absent credentials.
type ErrMissingAuth struct{ Message string }

func (e *ErrMissingAuth) Error() string { return e.Message }

// WireKind is which upstream wire shape a request uses.
type WireKind string

const (
	WireKindOpenAI    WireKind = "openai"
	WireKindAnthropic WireKind = "anthropic"
	WireKindResponses WireKind = "responses"
)

func antigravityPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin/arm64"
	case "windows":
		return "windows/amd64"
	default:
		return "linux/amd64"
	}
}

func mergeBeta(existing, beta string) string {
	if existing == "" {
		return beta
	}
	var parts []string
	for _, part := range strings.Split(existing, ",") {
		if p := strings.TrimSpace(part); p != "" {
			if p == beta {
				return strings.Join(parts, ",")
			}
			parts = append(parts, p)
		}
	}
	return strings.Join(append(parts, beta), ",")
}

const (
	openRouterAppURL   = "https://github.com/xinyao27/jevonian"
	openRouterAppTitle = "Jevonian"
)

func hostMatches(baseURL, suffix string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// WithOpenRouterAttribution adds OpenRouter's HTTP-Referer and X-Title app
// attribution. Added whenever a request goes to OpenRouter, including model
// discovery and balance checks, so OpenRouter's activity view attributes every
// call to Jevonian. Caller headers win.
func WithOpenRouterAttribution(headers map[string]string, baseURL string) map[string]string {
	if !hostMatches(baseURL, "openrouter.ai") {
		return headers
	}
	if headers["HTTP-Referer"] == "" && headers["http-referer"] == "" {
		headers["HTTP-Referer"] = openRouterAppURL
	}
	if headers["X-Title"] == "" && headers["x-title"] == "" {
		headers["X-Title"] = openRouterAppTitle
	}
	return headers
}

// WithSessionAffinity pins opencode.ai requests to the session a turn belongs
// to. A client-sent header wins; then the caller's session.
func WithSessionAffinity(headers map[string]string, provider config.Provider, session string, incoming http.Header) {
	if !strings.Contains(provider.BaseURL, "opencode.ai") {
		return
	}
	if headers["x-opencode-session"] != "" {
		return
	}
	if v := incoming.Get("x-opencode-session"); v != "" {
		headers["x-opencode-session"] = v
		return
	}
	headers["x-opencode-session"] = session
}

// ResolveProviderAuth turns provider auth into outbound headers (port of
// resolveProviderAuth in src/auth.ts).
//
//	kind selects which wire vocabulary the call speaks; "responses" OAuth is the
//	Codex headers. session is the client session for WorkBuddy conversation ids.
func (r *Resolver) ResolveProviderAuth(ctx context.Context, provider config.Provider, kind WireKind, session string) (AuthResolution, error) {
	headers := map[string]string{"content-type": "application/json"}
	for k, v := range provider.Headers {
		headers[k] = v
	}

	var token, accountID, domain string

	if provider.Auth == config.AuthOAuth {
		if provider.OAuthSource != "" && provider.OAuthSource != config.OAuthStatic {
			resolved, err := r.Resolve(ctx, ResolveOptions{
				Source:   Source(provider.OAuthSource),
				Login:    provider.Login,
				Endpoint: workbuddy.EndpointFromBaseURL(provider.BaseURL),
			})
			if err != nil {
				return AuthResolution{}, err
			}
			token = resolved.Token
			accountID = resolved.AccountID
			domain = resolved.Domain
		} else {
			token = r.apiKey(provider)
			if token == "" {
				return AuthResolution{}, &ErrMissingAuth{
					Message: fmt.Sprintf("Missing OAuth token for provider %q", provider.Name),
				}
			}
		}
	} else {
		token = r.apiKey(provider)
		if token == "" {
			if provider.NoKey {
				// Ollama / LM Studio and other keyless local servers talk without a Bearer.
				return AuthResolution{Headers: headers}, nil
			}
			return AuthResolution{}, &ErrMissingAuth{
				Message: fmt.Sprintf("Missing API key for provider %q", provider.Name),
			}
		}
	}

	// Devin's and Cursor's wire modules build their own Connect-RPC headers
	// from the raw token.
	if provider.Type == config.ProviderTypeDevin || provider.Type == config.ProviderTypeCursor {
		return AuthResolution{Token: token}, nil
	}

	anthropicWire := provider.Type == config.ProviderTypeAnthropic ||
		(provider.Type == config.ProviderTypeBoth && kind == WireKindAnthropic)
	switch {
	case anthropicWire:
		claudeHeaders(headers, provider, token)
	case provider.Type == config.ProviderTypeResponses && provider.Auth == config.AuthOAuth:
		codexHeaders(headers, token, accountID)
	case provider.OAuthSource == config.OAuthFreebuff:
		freebuff.ApplyHeaders(headers, token)
	case provider.OAuthSource == config.OAuthWorkbuddyAI:
		workbuddy.ApplyHeaders(headers, workbuddy.Creds{
			UID:         accountID,
			AccessToken: token,
			Domain:      domain,
		}, session)
	default:
		headers["authorization"] = "Bearer " + token
	}

	WithOpenRouterAttribution(headers, provider.BaseURL)

	if provider.Type == config.ProviderTypeGemini {
		if headers["user-agent"] == "" {
			headers["user-agent"] = "antigravity/1.15.8 " + antigravityPlatform()
		}
		if headers["x-goog-api-client"] == "" {
			headers["x-goog-api-client"] = "google-cloud-sdk vscode_cloudshelleditor/0.1"
		}
		if headers["client-metadata"] == "" {
			meta, _ := json.Marshal(map[string]string{
				"ideType":    "ANTIGRAVITY",
				"platform":   "PLATFORM_UNSPECIFIED",
				"pluginType": "GEMINI",
			})
			headers["client-metadata"] = string(meta)
		}
		return AuthResolution{Headers: headers, Project: ResolveAntigravityProject(), Token: token}, nil
	}
	return AuthResolution{Headers: headers, Token: token}, nil
}

// apiKey resolves the provider's API key: inline config, then the credentials
// store, then apiKeyEnv — matching resolveApiKey in src/config.ts.
func (r *Resolver) apiKey(provider config.Provider) string {
	if provider.APIKey != "" {
		return provider.APIKey
	}
	store := r.Credentials
	if store == nil {
		store = multiacct.DefaultStore()
	}
	if key := store.Get(provider.Name); key != "" {
		return key
	}
	if provider.APIKeyEnv != "" {
		return getenv(provider.APIKeyEnv)
	}
	return ""
}

func claudeHeaders(headers map[string]string, provider config.Provider, token string) {
	if provider.Auth == config.AuthOAuth {
		headers["authorization"] = "Bearer " + token
		headers["anthropic-beta"] = mergeBeta(provider.Headers["anthropic-beta"], "oauth-2025-04-20")
		// Anthropic gates subscription models on the Claude Code version in
		// User-Agent, so a stale version here hides newer models. Bump it when a
		// new model is refused on OAuth; operators can override per provider via
		// headers["user-agent"].
		if headers["user-agent"] == "" {
			if v := provider.Headers["user-agent"]; v != "" {
				headers["user-agent"] = v
			} else {
				headers["user-agent"] = "claude-cli/2.1.280 (external, cli)"
			}
		}
	} else {
		headers["x-api-key"] = token
		if provider.Type == config.ProviderTypeBoth {
			headers["authorization"] = "Bearer " + token
		}
	}
	if headers["anthropic-version"] == "" {
		headers["anthropic-version"] = "2023-06-01"
	}
}

func codexHeaders(headers map[string]string, token, accountID string) {
	headers["authorization"] = "Bearer " + token
	if accountID != "" {
		headers["chatgpt-account-id"] = accountID
	}
	if headers["originator"] == "" {
		headers["originator"] = "codex_cli_rs"
	}
	if headers["openai-beta"] == "" {
		headers["openai-beta"] = "responses=experimental"
	}
	if headers["user-agent"] == "" {
		headers["user-agent"] = "codex_cli_rs/0.114.0"
	}
}
