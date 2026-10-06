package proxy

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// envInt reads an integer environment variable, clamped to [lo, hi]. A missing
// or unparsable value yields fallback.
func envInt(name string, fallback, lo, hi int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return min(hi, max(lo, v))
}

// Env-driven defaults. src/proxy.ts proxyAgentOptions reads the same variables:
// connections = JEVONIAN_PROVIDER_CONCURRENCY, connectTimeout and headersTimeout =
// JEVONIAN_UPSTREAM_CONNECT_TIMEOUT_MS / JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS
// (same 1s floor and 30 min ceiling as src/upstream-timeout.ts).
func envMaxConns() int {
	return envInt("JEVONIAN_PROVIDER_CONCURRENCY", DefaultMaxConnsPerHost, 1, 256)
}

func envDuration(name string, fallback time.Duration) time.Duration {
	ms := envInt(name, int(fallback/time.Millisecond), 1_000, 30*60_000)
	return time.Duration(ms) * time.Millisecond
}
