package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/server"
)

func localCfg(host string) *config.Config {
	return &config.Config{Listen: config.ListenConfig{Host: host, Port: 8787}}
}

func TestIsLoopbackOrigin(t *testing.T) {
	t.Run("accepts loopback in every form the socket reports", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "127.0.0.5", "localhost", "127.0.0.1:4321"} {
			if !server.IsLoopbackOrigin(addr) {
				t.Fatalf("IsLoopbackOrigin(%q) = false", addr)
			}
		}
	})

	t.Run("rejects remote and unknown peers", func(t *testing.T) {
		for _, addr := range []string{"192.168.1.10", "::ffff:8.8.8.8", "", "203.0.113.7:80"} {
			if server.IsLoopbackOrigin(addr) {
				t.Fatalf("IsLoopbackOrigin(%q) = true", addr)
			}
		}
	})
}

// A sentinel request built like a desktop client's: loopback peer + a token.
func sentinelRequest(token, remoteAddr string, accountID bool) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8787/v1/chat/completions", nil)
	if strings.Contains(remoteAddr, ":") && !strings.HasPrefix(remoteAddr, "[") {
		req.RemoteAddr = "[" + remoteAddr + "]:5555"
	} else {
		req.RemoteAddr = remoteAddr + ":5555"
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	if accountID {
		req.Header.Set("chatgpt-account-id", "acct-1")
	}
	return req
}

func TestIsLocalClientRequest(t *testing.T) {
	t.Run("accepts a sentinel from loopback on a loopback-bound server", func(t *testing.T) {
		if !server.IsLocalClientRequest(sentinelRequest("jevonian-local", "127.0.0.1", false), localCfg("127.0.0.1")) {
			t.Fatal("jevonian-local from loopback refused")
		}
		if !server.IsLocalClientRequest(sentinelRequest("ollama-local-codex", "::1", false), localCfg("127.0.0.1")) {
			t.Fatal("ollama-local-codex from ::1 refused")
		}
		if !server.IsLocalClientRequest(sentinelRequest("ollama", "::ffff:127.0.0.1", false), localCfg("localhost")) {
			t.Fatal("ollama sentinel refused")
		}
	})

	t.Run("accepts a ChatGPT account session from loopback", func(t *testing.T) {
		// The normal path once auth.json holds a real login: the token is the
		// user's own ChatGPT credential, which is never a Jevonian key.
		if !server.IsLocalClientRequest(sentinelRequest("a-real-chatgpt-token", "127.0.0.1", true), localCfg("127.0.0.1")) {
			t.Fatal("account session refused")
		}
	})

	t.Run("rejects a sentinel arriving from a remote peer", func(t *testing.T) {
		// The critical case: a tunneled request must never use the sentinel.
		if server.IsLocalClientRequest(sentinelRequest("jevonian-local", "203.0.113.7", false), localCfg("127.0.0.1")) {
			t.Fatal("sentinel accepted from remote peer")
		}
	})

	t.Run("rejects an account session arriving from a remote peer", func(t *testing.T) {
		// Otherwise anyone could reach the tunnel by sending a bogus account header.
		if server.IsLocalClientRequest(sentinelRequest("a-real-chatgpt-token", "203.0.113.7", true), localCfg("127.0.0.1")) {
			t.Fatal("account session accepted from remote peer")
		}
		if server.IsLocalClientRequest(sentinelRequest("a-real-chatgpt-token", "::ffff:8.8.8.8", true), localCfg("127.0.0.1")) {
			t.Fatal("account session accepted from mapped remote peer")
		}
	})

	t.Run("rejects the sentinel when the server is bound to all interfaces", func(t *testing.T) {
		// 0.0.0.0 accepts LAN traffic, so the sentinel would be a public bypass.
		for _, host := range []string{"0.0.0.0", "::"} {
			if server.IsLocalClientRequest(sentinelRequest("jevonian-local", "127.0.0.1", false), localCfg(host)) {
				t.Fatalf("sentinel accepted on %s binding", host)
			}
			if server.IsLocalClientRequest(sentinelRequest("tok", "127.0.0.1", true), localCfg(host)) {
				t.Fatalf("account session accepted on %s binding", host)
			}
		}
	})

	t.Run("never accepts a real-looking key or empty token as a sentinel", func(t *testing.T) {
		cfg := localCfg("127.0.0.1")
		for _, token := range []string{"sk-jev-deadbeef", "", "a-real-chatgpt-token"} {
			if server.IsLocalClientRequest(sentinelRequest(token, "127.0.0.1", false), cfg) {
				t.Fatalf("token %q accepted as local client", token)
			}
		}
	})

	t.Run("recognizes every declared sentinel", func(t *testing.T) {
		for _, token := range server.LocalClientKeys {
			if !server.IsLocalClientRequest(sentinelRequest(token, "127.0.0.1", false), localCfg("127.0.0.1")) {
				t.Fatalf("sentinel %q refused", token)
			}
		}
	})
}

func TestFallbackProvider(t *testing.T) {
	providers := func(entries ...config.Provider) *config.Config {
		return &config.Config{Providers: entries}
	}
	openai := config.Provider{Name: "o", Type: config.ProviderTypeOpenAI}
	responses := config.Provider{Name: "r", Type: config.ProviderTypeResponses}
	anthropic := config.Provider{Name: "a", Type: config.ProviderTypeAnthropic}

	t.Run("prefers the configured default", func(t *testing.T) {
		cfg := providers(anthropic, openai)
		cfg.DefaultProvider = "a"
		if got := server.FallbackProvider(cfg); got == nil || got.Name != "a" {
			t.Fatalf("fallback = %+v", got)
		}
	})
	t.Run("falls back to an openai-capable provider", func(t *testing.T) {
		cfg := providers(anthropic, responses, openai)
		if got := server.FallbackProvider(cfg); got == nil || got.Name != "o" {
			t.Fatalf("fallback = %+v", got)
		}
	})
	t.Run("then responses, then first", func(t *testing.T) {
		if got := server.FallbackProvider(providers(anthropic, responses)); got == nil || got.Name != "r" {
			t.Fatalf("fallback = %+v", got)
		}
		if got := server.FallbackProvider(providers(anthropic)); got == nil || got.Name != "a" {
			t.Fatalf("fallback = %+v", got)
		}
		if got := server.FallbackProvider(providers()); got != nil {
			t.Fatalf("fallback = %+v", got)
		}
		if got := server.FallbackProvider(nil); got != nil {
			t.Fatalf("fallback = %+v", got)
		}
	})
}
