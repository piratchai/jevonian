package server

import (
	"net"
	"net/http"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/keys"
)

// LocalClientKeys are the sentinel credentials used by desktop clients that are
// configured to talk to a loopback Jevonian instance.
//
// Desktop apps cannot be given a real `sk-jev-...` key: the header they send is
// derived from their own upstream credentials (a ChatGPT account token for
// Codex, a placeholder for Claude Desktop), and they re-read those credentials
// on every launch. Writing a real Jevonian key into their config would be
// overwritten and, worse, would leak a working key onto disk.
//
// These values are NOT credentials. They are only accepted from a loopback peer
// and never authorize access to a tunneled or LAN-facing instance.
var LocalClientKeys = []string{"jevonian-local", "ollama-local-codex", "ollama"}

// IsLocalClientKey reports whether token is one of the desktop sentinel values.
func IsLocalClientKey(token string) bool {
	for _, sentinel := range LocalClientKeys {
		if token == sentinel {
			return true
		}
	}
	return false
}

// IsLoopbackOrigin reports whether the request arrived over the loopback
// interface. Accepts a bare IP, a host, or a host:port — whatever a socket peer
// name looks like.
func IsLoopbackOrigin(remoteAddress string) bool {
	host := remoteAddress
	if h, _, err := net.SplitHostPort(remoteAddress); err == nil {
		host = h
	}
	// Node reports IPv4-mapped IPv6 addresses as ::ffff:127.0.0.1
	normalized := strings.TrimPrefix(host, "::ffff:")
	return normalized == "127.0.0.1" ||
		normalized == "::1" ||
		normalized == "localhost" ||
		strings.HasPrefix(normalized, "127.")
}

// RemoteAddressOf returns the peer address of the request (the TCP remote end,
// without trusting any client-supplied forwarding header).
func RemoteAddressOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// IsLoopbackEndpoint reports whether the configured listen host binds loopback
// only. The sentinel is only safe then: a `0.0.0.0` or `::` binding accepts LAN
// traffic, so it must NOT qualify — the sentinel would otherwise let anyone on
// the network in without a key.
func IsLoopbackEndpoint(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return IsLoopbackOrigin(cfg.Listen.Host)
}

// IsLocalClientRequest decides whether a request may bypass Jevonian's own
// API-key check.
//
// Two kinds of loopback client qualify, and both are gated on the peer being
// loopback AND the server being bound to loopback, so neither can ever unlock a
// tunneled or LAN-facing instance:
//
//  1. A sentinel key (`jevonian-local`). Used when Codex had no `auth.json`, so
//     Jevonian created one and the client echoes our placeholder back.
//  2. A ChatGPT account session, identified by the `ChatGPT-Account-ID` header.
//     This is the normal case once a real login exists: Codex sends the user's
//     own ChatGPT token, which is not — and must never be — a Jevonian key.
//     Accepting it is safe because the token is never trusted as authorization:
//     it is discarded, and Jevonian resolves its own upstream credentials.
func IsLocalClientRequest(r *http.Request, cfg *config.Config) bool {
	if r == nil || !IsLoopbackOrigin(RemoteAddressOf(r)) {
		return false
	}
	if !IsLoopbackEndpoint(cfg) {
		return false
	}
	if strings.TrimSpace(r.Header.Get("chatgpt-account-id")) != "" {
		return true
	}
	token := keys.TokenFromHeaders(r.Header.Get("authorization"), r.Header.Get("x-api-key"))
	if token == "" {
		return false
	}
	return IsLocalClientKey(token)
}

// FallbackProvider returns any provider that can serve the OpenAI surface so a
// desktop client pointed at Jevonian always resolves to something usable when
// the default provider is missing or not declared.
func FallbackProvider(cfg *config.Config) *config.Provider {
	if cfg == nil {
		return nil
	}
	if cfg.DefaultProvider != "" {
		for i := range cfg.Providers {
			if cfg.Providers[i].Name == cfg.DefaultProvider {
				return &cfg.Providers[i]
			}
		}
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].Type == config.ProviderTypeOpenAI || cfg.Providers[i].Type == config.ProviderTypeBoth {
			return &cfg.Providers[i]
		}
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].Type == config.ProviderTypeResponses {
			return &cfg.Providers[i]
		}
	}
	if len(cfg.Providers) > 0 {
		return &cfg.Providers[0]
	}
	return nil
}
