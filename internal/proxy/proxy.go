// Package proxy detects the machine's HTTP(S) proxy and builds egress transports.
//
// On macOS the system proxy usually lives in network settings (scutil --proxy)
// rather than the environment. An existing HTTP(S)_PROXY / ALL_PROXY setting
// always wins; JEVONIAN_SYSTEM_PROXY=off opts out of detection entirely.
package proxy

import (
	"os"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/platform/darwin"
)

// SystemProxy is the proxy Jevonian's egress should use.
type SystemProxy = darwin.SystemProxy

// ParseScutilProxy parses macOS `scutil --proxy` output.
func ParseScutilProxy(output string) *SystemProxy {
	return darwin.ParseScutilProxy(output)
}

// Proxy environment spellings Node and Go both read.
var proxyEnvNames = []string{
	"HTTPS_PROXY",
	"https_proxy",
	"HTTP_PROXY",
	"http_proxy",
	"ALL_PROXY",
	"all_proxy",
}

// Defaults match the TypeScript undici hardening (provider concurrency + upstream timeouts).
const (
	DefaultMaxConnsPerHost       = 8
	DefaultResponseHeaderTimeout = 60 * time.Second
	DefaultIdleConnTimeout       = 120 * time.Second
	DefaultDialTimeout           = 15 * time.Second
)

// TransportOptions tunes the egress http.Transport.
type TransportOptions struct {
	// MaxConnsPerHost caps per-origin sockets (default 8).
	MaxConnsPerHost int
	// ResponseHeaderTimeout is time to the response status line (default 60s).
	ResponseHeaderTimeout time.Duration
	// IdleConnTimeout holds idle keep-alive sockets (default 120s).
	IdleConnTimeout time.Duration
	// DialTimeout is the TCP/TLS (or proxy CONNECT) handshake ceiling (default 15s).
	DialTimeout time.Duration
	// Proxy, when set, routes through that URL with its bypass list.
	// When nil, ProxyFromEnvironment is used.
	Proxy *SystemProxy
	// ExtraNoProxy is merged into the bypass list (e.g. an existing NO_PROXY value).
	ExtraNoProxy string
}

// DetectFunc reads the machine's system proxy. Tests inject a stub.
type DetectFunc func() *SystemProxy

func osGetenv(key string) string { return os.Getenv(key) }

// IsOptedOut reports whether JEVONIAN_SYSTEM_PROXY disables detection.
func IsOptedOut(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "0", "false", "no":
		return true
	default:
		return false
	}
}

// HasProxyEnv is true when any standard proxy variable is already set.
func HasProxyEnv(getenv func(string) string) bool {
	if getenv == nil {
		getenv = osGetenv
	}
	for _, name := range proxyEnvNames {
		if strings.TrimSpace(getenv(name)) != "" {
			return true
		}
	}
	return false
}

// MergeBypass unions a host list into a NO_PROXY value, keeping existing entries.
func MergeBypass(existing string, bypass []string) string {
	entries := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(raw string) {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return
		}
		if _, ok := seen[trimmed]; ok {
			return
		}
		seen[trimmed] = struct{}{}
		entries = append(entries, trimmed)
	}
	for _, entry := range strings.Split(existing, ",") {
		add(entry)
	}
	for _, entry := range bypass {
		add(entry)
	}
	return strings.Join(entries, ",")
}

// ScrubProxyEnv copies env without proxy variables so children dial the network directly.
func ScrubProxyEnv(env []string) []string {
	drop := make(map[string]struct{}, len(proxyEnvNames))
	for _, name := range proxyEnvNames {
		drop[name] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := drop[key]; skip {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ApplySystemProxy writes the detected proxy into env (map form for tests / explicit control).
//
// An environment that already names a proxy wins. Opt-out via JEVONIAN_SYSTEM_PROXY
// stands down entirely. Loopback is always added to NO_PROXY.
func ApplySystemProxy(env map[string]string, detect DetectFunc) *SystemProxy {
	if env == nil {
		env = map[string]string{}
	}
	if detect == nil {
		detect = DetectSystemProxy
	}
	if IsOptedOut(env["JEVONIAN_SYSTEM_PROXY"]) {
		return nil
	}
	if hasProxyEnvMap(env) {
		return nil
	}
	proxy := detect()
	if proxy == nil {
		return nil
	}
	env["HTTPS_PROXY"] = proxy.URL
	env["HTTP_PROXY"] = proxy.URL
	env["NO_PROXY"] = MergeBypass(env["NO_PROXY"], append(append([]string{}, darwin.LoopbackBypass...), proxy.Bypass...))
	return proxy
}

// ResolveSystemProxy decides which system proxy (if any) should back egress,
// without mutating the environment. Existing proxy env vars and opt-out win.
func ResolveSystemProxy(getenv func(string) string, detect DetectFunc) *SystemProxy {
	if getenv == nil {
		getenv = osGetenv
	}
	if detect == nil {
		detect = DetectSystemProxy
	}
	if IsOptedOut(getenv("JEVONIAN_SYSTEM_PROXY")) {
		return nil
	}
	if HasProxyEnv(getenv) {
		return nil
	}
	return detect()
}

func hasProxyEnvMap(env map[string]string) bool {
	for _, name := range proxyEnvNames {
		if strings.TrimSpace(env[name]) != "" {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// hostBypassed implements a practical subset of NO_PROXY matching:
// exact host, leading-dot / *.suffix. CIDR text from scutil is matched literally.
func hostBypassed(host string, rules []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, rule := range rules {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if rule == "" {
			continue
		}
		if rule == "*" {
			return true
		}
		if strings.HasPrefix(rule, "*.") {
			suffix := rule[1:] // ".example.com"
			bare := rule[2:]   // "example.com"
			if host == bare || strings.HasSuffix(host, suffix) {
				return true
			}
			continue
		}
		if strings.HasPrefix(rule, ".") {
			if strings.HasSuffix(host, rule) || host == strings.TrimPrefix(rule, ".") {
				return true
			}
			continue
		}
		if host == rule {
			return true
		}
	}
	return false
}
