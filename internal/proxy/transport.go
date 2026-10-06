package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/xinyao27/jevonian/internal/platform/darwin"
)

// NewTransport builds an http.Transport with proxy + hardening knobs applied.
//
// HTTP/2 is disabled so keep-alive / idle policy stays on the HTTP/1.1 pool path,
// matching the TypeScript allowH2: false intent.
func NewTransport(opts TransportOptions) *http.Transport {
	if opts.MaxConnsPerHost <= 0 {
		opts.MaxConnsPerHost = envMaxConns()
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = envDuration("JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS", DefaultResponseHeaderTimeout)
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = DefaultIdleConnTimeout
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = envDuration("JEVONIAN_UPSTREAM_CONNECT_TIMEOUT_MS", DefaultDialTimeout)
	}

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxConnsPerHost = opts.MaxConnsPerHost
	base.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
	base.IdleConnTimeout = opts.IdleConnTimeout
	base.TLSHandshakeTimeout = opts.DialTimeout
	base.ForceAttemptHTTP2 = false
	// Empty non-nil map disables HTTP/2 upgrade paths on this transport.
	base.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	base.Proxy = proxyFunc(opts.Proxy, opts.ExtraNoProxy)

	dialer := &net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}
	base.DialContext = dialer.DialContext

	return base
}

// UseSystemProxy detects the machine proxy and returns a hardened transport.
//
// System-detected proxies are installed via Transport.Proxy rather than written
// into the process environment, so tunnel children do not inherit HTTPS_PROXY.
// An environment that already names a proxy still wins (ProxyFromEnvironment).
func UseSystemProxy() (*http.Transport, *SystemProxy) {
	return useSystemProxy(osGetenv, DetectSystemProxy)
}

func useSystemProxy(getenv func(string) string, detect DetectFunc) (*http.Transport, *SystemProxy) {
	if getenv == nil {
		getenv = osGetenv
	}
	if detect == nil {
		detect = DetectSystemProxy
	}

	opts := TransportOptions{
		ExtraNoProxy: getenv("NO_PROXY"),
	}

	if IsOptedOut(getenv("JEVONIAN_SYSTEM_PROXY")) || HasProxyEnv(getenv) {
		return NewTransport(opts), nil
	}

	proxy := detect()
	if proxy == nil {
		return NewTransport(opts), nil
	}
	opts.Proxy = proxy
	return NewTransport(opts), proxy
}

func proxyFunc(proxy *SystemProxy, extraNoProxy string) func(*http.Request) (*url.URL, error) {
	if proxy == nil {
		return http.ProxyFromEnvironment
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil || proxyURL.Host == "" {
		return http.ProxyFromEnvironment
	}
	bypass := MergeBypass(extraNoProxy, append(append([]string{}, darwin.LoopbackBypass...), proxy.Bypass...))
	rules := splitCSV(bypass)
	return func(req *http.Request) (*url.URL, error) {
		if req.URL == nil {
			return nil, nil
		}
		if hostBypassed(req.URL.Hostname(), rules) {
			return nil, nil
		}
		return proxyURL, nil
	}
}
