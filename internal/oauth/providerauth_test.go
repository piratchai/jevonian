package oauth

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

func providerWith(raw map[string]any) config.Provider {
	cfg, err := config.ParseConfig(raw)
	if err != nil {
		panic(err)
	}
	return cfg.Providers[0]
}

func basicProvider(baseURL string) config.Provider {
	return providerWith(map[string]any{
		"providers": []any{map[string]any{
			"name": "p", "type": "both", "baseUrl": baseURL,
			"apiKey": "test", "models": []any{"m"},
		}},
	})
}

func TestSessionAffinity(t *testing.T) {
	headers := map[string]string{}
	WithSessionAffinity(headers, basicProvider("https://opencode.ai/zen/go/v1"), "sess-1", nil)
	if headers["x-opencode-session"] != "sess-1" {
		t.Fatalf("affinity %+v", headers)
	}

	forwarded := map[string]string{}
	WithSessionAffinity(forwarded, basicProvider("https://opencode.ai/zen/go/v1"), "sess-2",
		http.Header{"X-Opencode-Session": {"client-session"}})
	if forwarded["x-opencode-session"] != "client-session" {
		t.Fatalf("client session lost: %+v", forwarded)
	}

	plain := map[string]string{}
	WithSessionAffinity(plain, basicProvider("https://api.deepseek.com/v1"), "sess-1", nil)
	if _, ok := plain["x-opencode-session"]; ok {
		t.Fatalf("non-opencode provider got session: %+v", plain)
	}
}

func TestOpenRouterAttribution(t *testing.T) {
	headers := map[string]string{}
	WithOpenRouterAttribution(headers, "https://openrouter.ai/api/v1")
	if headers["HTTP-Referer"] != "https://github.com/xinyao27/jevonian" ||
		headers["X-Title"] != "Jevonian" {
		t.Fatalf("attribution %+v", headers)
	}
	plain := map[string]string{}
	WithOpenRouterAttribution(plain, "https://api.deepseek.com/v1")
	if len(plain) != 0 {
		t.Fatalf("untouched %+v", plain)
	}
	kept := map[string]string{"HTTP-Referer": "https://example.com/app", "X-Title": "Example"}
	WithOpenRouterAttribution(kept, "https://openrouter.ai/api/v1")
	if kept["HTTP-Referer"] != "https://example.com/app" || kept["X-Title"] != "Example" {
		t.Fatalf("overwrote caller headers: %+v", kept)
	}
}

func TestResolveProviderAuthOpenRouter(t *testing.T) {
	r := &Resolver{}
	auth, err := r.ResolveProviderAuth(t.Context(), basicProvider("https://openrouter.ai/api/v1"), WireKindOpenAI, "")
	if err != nil {
		t.Fatal(err)
	}
	if auth.Headers["HTTP-Referer"] != "https://github.com/xinyao27/jevonian" ||
		auth.Headers["X-Title"] != "Jevonian" ||
		auth.Headers["authorization"] != "Bearer test" ||
		auth.Token != "test" {
		t.Fatalf("%+v", auth)
	}
}

func TestResolveProviderAuthDevinRawToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(path, []byte(`windsurf_api_key = "devin-session-token$abc"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	devin := providerWith(map[string]any{"providers": []any{map[string]any{
		"name": "devin-subscription", "type": "devin",
		"baseUrl": "https://server.codeium.com", "auth": "oauth", "oauthSource": "devin",
		"models": []any{},
	}}})
	r := &Resolver{}
	auth, err := r.ResolveProviderAuth(t.Context(), devin, WireKindOpenAI, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(auth.Headers) != 0 || auth.Token != "devin-session-token$abc" {
		t.Fatalf("%+v", auth)
	}
}

func TestResolveProviderAuthMissingDevin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", filepath.Join(dir, "missing.toml"))
	devin := providerWith(map[string]any{"providers": []any{map[string]any{
		"name": "devin-subscription", "type": "devin",
		"baseUrl": "https://server.codeium.com", "auth": "oauth", "oauthSource": "devin",
		"models": []any{},
	}}})
	r := &Resolver{}
	_, err := r.ResolveProviderAuth(t.Context(), devin, WireKindOpenAI, "")
	if err == nil || !strings.Contains(err.Error(), "devin auth login") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveProviderAuthWorkbuddyHeaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-ai.json")
	if err := writeJSONFile(path, map[string]any{
		"uid":              "uid-1",
		"accessToken":      "wb-token",
		"refreshToken":     "wb-refresh",
		"expiresAt":        time.Now().UnixMilli() + 3_600_000,
		"refreshExpiresAt": time.Now().UnixMilli() + 7_200_000,
		"domain":           "www.workbuddy.ai",
	}, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", path)
	wb := providerWith(map[string]any{"providers": []any{map[string]any{
		"name": "workbuddy-ai-subscription", "type": "openai",
		"baseUrl": "https://www.workbuddy.ai/v2", "auth": "oauth",
		"oauthSource": "workbuddy-ai", "billing": "subscription",
		"models": []any{"primary-model"},
	}}})
	r := &Resolver{}
	auth, err := r.ResolveProviderAuth(t.Context(), wb, WireKindOpenAI, "sessabcdef")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"authorization":     "Bearer wb-token",
		"x-user-id":         "uid-1",
		"x-domain":          "www.workbuddy.ai",
		"x-product":         "SaaS",
		"x-requested-with":  "XMLHttpRequest",
		"x-conversation-id": "sessabcdef",
	}
	for k, v := range want {
		if auth.Headers[k] != v {
			t.Fatalf("%s = %q, want %q", k, auth.Headers[k], v)
		}
	}
	if !strings.HasPrefix(auth.Headers["user-agent"], "WorkBuddy/") {
		t.Fatalf("user-agent %q", auth.Headers["user-agent"])
	}
}

func TestResolveProviderAuthClaudeHeaders(t *testing.T) {
	p := providerWith(map[string]any{"providers": []any{map[string]any{
		"name": "claude-sub", "type": "anthropic", "baseUrl": "https://api.anthropic.com",
		"apiKey": "sk-ant-x", "models": []any{"m"},
	}}})
	r := &Resolver{}
	auth, err := r.ResolveProviderAuth(t.Context(), p, WireKindAnthropic, "")
	if err != nil {
		t.Fatal(err)
	}
	if auth.Headers["x-api-key"] != "sk-ant-x" || auth.Headers["anthropic-version"] != "2023-06-01" {
		t.Fatalf("%+v", auth.Headers)
	}
	if _, hasBearer := auth.Headers["authorization"]; hasBearer {
		t.Fatal("anthropic api-key must not set bearer")
	}

	// api-key on a "both" provider also sends Bearer for the openai surface.
	both := providerWith(map[string]any{"providers": []any{map[string]any{
		"name": "multi", "type": "both", "baseUrl": "https://x.example/v1",
		"apiKey": "sk-b", "models": []any{"m"},
	}}})
	auth, err = r.ResolveProviderAuth(t.Context(), both, WireKindAnthropic, "")
	if err != nil || auth.Headers["authorization"] != "Bearer sk-b" {
		t.Fatalf("%+v %v", auth, err)
	}
}

func TestResolveProviderAuthNoKeyLocals(t *testing.T) {
	p := providerWith(map[string]any{"providers": []any{map[string]any{
		"name": "ollama", "type": "openai", "baseUrl": "http://127.0.0.1:11434/v1",
		"noKey": true, "models": []any{"m"},
	}}})
	auth, err := (&Resolver{}).ResolveProviderAuth(t.Context(), p, WireKindOpenAI, "")
	if err != nil || len(auth.Headers) != 1 || auth.Headers["content-type"] != "application/json" {
		t.Fatalf("%+v %v", auth, err)
	}
	p.NoKey = false
	_, err = (&Resolver{}).ResolveProviderAuth(t.Context(), p, WireKindOpenAI, "")
	var missing *ErrMissingAuth
	if err == nil || !errors.As(err, &missing) || !strings.Contains(missing.Error(), "Missing API key") {
		t.Fatalf("err = %v", err)
	}
}
