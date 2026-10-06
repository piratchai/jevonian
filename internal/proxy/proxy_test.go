package proxy_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/proxy"
)

const enabled = `<dictionary> {
  ExceptionsList : <array> {
    0 : 119.29.29.29.dns
    1 : *.abchina.com.cn
    2 : 192.168.0.0/16
    3 : *.local
    4 : localhost
  }
  ExcludeSimpleHostnames : 1
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 1082
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 1082
  HTTPSProxy : 127.0.0.1
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 0
  SOCKSProxy : 127.0.0.1
}`

const httpOnly = `<dictionary> {
  HTTPEnable : 1
  HTTPPort : 7890
  HTTPProxy : 10.0.0.2
  HTTPSEnable : 0
  ProxyAutoConfigEnable : 1
  ProxyAutoConfigURLString : http://wpad/wpad.dat
}`

const disabled = `<dictionary> {
  ExcludeSimpleHostnames : 0
  HTTPEnable : 0
  HTTPSEnable : 0
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 0
}`

const empty = "<dictionary> {\n}"

func TestParseScutilProxyEnabled(t *testing.T) {
	got := proxy.ParseScutilProxy(enabled)
	if got == nil {
		t.Fatal("expected proxy, got nil")
	}
	if got.URL != "http://127.0.0.1:1082" {
		t.Fatalf("URL = %q, want http://127.0.0.1:1082", got.URL)
	}
	want := []string{
		"localhost",
		"127.0.0.1",
		"::1",
		"119.29.29.29.dns",
		"*.abchina.com.cn",
		"192.168.0.0/16",
		"*.local",
	}
	if len(got.Bypass) != len(want) {
		t.Fatalf("Bypass = %#v, want %#v", got.Bypass, want)
	}
	for i := range want {
		if got.Bypass[i] != want[i] {
			t.Fatalf("Bypass[%d] = %q, want %q", i, got.Bypass[i], want[i])
		}
	}
}

func TestParseScutilProxyHTTPOnly(t *testing.T) {
	got := proxy.ParseScutilProxy(httpOnly)
	if got == nil {
		t.Fatal("expected proxy, got nil")
	}
	if got.URL != "http://10.0.0.2:7890" {
		t.Fatalf("URL = %q, want http://10.0.0.2:7890", got.URL)
	}
	if strings.Contains(got.URL, "wpad") {
		t.Fatalf("must not mistake a PAC URL for a proxy: %q", got.URL)
	}
}

func TestParseScutilProxyDisabled(t *testing.T) {
	if proxy.ParseScutilProxy(disabled) != nil {
		t.Fatal("expected nil for disabled proxy")
	}
	if proxy.ParseScutilProxy(empty) != nil {
		t.Fatal("expected nil for empty dictionary")
	}
}

func TestApplySystemProxyWritesEnv(t *testing.T) {
	detected := &proxy.SystemProxy{URL: "http://127.0.0.1:1082", Bypass: []string{"127.0.0.1", "*.local"}}
	env := map[string]string{}
	got := proxy.ApplySystemProxy(env, func() *proxy.SystemProxy { return detected })
	if got == nil || got.URL != detected.URL {
		t.Fatalf("ApplySystemProxy = %#v, want %#v", got, detected)
	}
	if env["HTTPS_PROXY"] != "http://127.0.0.1:1082" || env["HTTP_PROXY"] != "http://127.0.0.1:1082" {
		t.Fatalf("proxy env = %#v", env)
	}
	if env["NO_PROXY"] != "localhost,127.0.0.1,::1,*.local" {
		t.Fatalf("NO_PROXY = %q", env["NO_PROXY"])
	}
}

func TestApplySystemProxyMergesBypass(t *testing.T) {
	detected := &proxy.SystemProxy{URL: "http://127.0.0.1:1082", Bypass: []string{"127.0.0.1", "*.local"}}
	env := map[string]string{"NO_PROXY": "example.test, ,127.0.0.1"}
	proxy.ApplySystemProxy(env, func() *proxy.SystemProxy { return detected })
	if env["NO_PROXY"] != "example.test,127.0.0.1,localhost,::1,*.local" {
		t.Fatalf("NO_PROXY = %q", env["NO_PROXY"])
	}
}

func TestApplySystemProxyRespectsExistingProxy(t *testing.T) {
	detected := &proxy.SystemProxy{URL: "http://127.0.0.1:1082", Bypass: []string{"*.local"}}
	env := map[string]string{"HTTPS_PROXY": "http://corp:8080"}
	if proxy.ApplySystemProxy(env, func() *proxy.SystemProxy { return detected }) != nil {
		t.Fatal("expected stand-down when HTTPS_PROXY is set")
	}
	if env["HTTPS_PROXY"] != "http://corp:8080" {
		t.Fatalf("HTTPS_PROXY mutated: %q", env["HTTPS_PROXY"])
	}
	if _, ok := env["HTTP_PROXY"]; ok {
		t.Fatal("HTTP_PROXY should remain unset")
	}
}

func TestApplySystemProxyOptOut(t *testing.T) {
	detected := &proxy.SystemProxy{URL: "http://127.0.0.1:1082", Bypass: nil}
	env := map[string]string{"JEVONIAN_SYSTEM_PROXY": "off"}
	if proxy.ApplySystemProxy(env, func() *proxy.SystemProxy { return detected }) != nil {
		t.Fatal("expected nil when opted out")
	}
	if _, ok := env["HTTPS_PROXY"]; ok {
		t.Fatal("HTTPS_PROXY should remain unset")
	}
}

func TestApplySystemProxyNoDetection(t *testing.T) {
	env := map[string]string{}
	if proxy.ApplySystemProxy(env, func() *proxy.SystemProxy { return nil }) != nil {
		t.Fatal("expected nil when machine has no proxy")
	}
	if _, ok := env["HTTPS_PROXY"]; ok {
		t.Fatal("HTTPS_PROXY should remain unset")
	}
}

func TestNewTransportDefaults(t *testing.T) {
	tr := proxy.NewTransport(proxy.TransportOptions{})
	if tr.MaxConnsPerHost != proxy.DefaultMaxConnsPerHost {
		t.Fatalf("MaxConnsPerHost = %d", tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != proxy.DefaultResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout != proxy.DefaultIdleConnTimeout {
		t.Fatalf("IdleConnTimeout = %v", tr.IdleConnTimeout)
	}
	if tr.IdleConnTimeout <= 4*time.Second {
		t.Fatal("idle timeout must exceed undici's 4s default")
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must be disabled")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil empty map to disable HTTP/2")
	}
}

func TestNewTransportWithProxyBypass(t *testing.T) {
	tr := proxy.NewTransport(proxy.TransportOptions{
		Proxy: &proxy.SystemProxy{
			URL:    "http://127.0.0.1:1082",
			Bypass: []string{"*.local"},
		},
	})

	req, err := http.NewRequest(http.MethodGet, "https://localhost/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL != nil {
		t.Fatalf("localhost must bypass proxy, got %v", proxyURL)
	}

	req, err = http.NewRequest(http.MethodGet, "https://api.example.com/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err = tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:1082" {
		t.Fatalf("proxy = %v, want http://127.0.0.1:1082", proxyURL)
	}

	// Ensure the proxy URL itself parses.
	if _, err := url.Parse(proxyURL.String()); err != nil {
		t.Fatal(err)
	}
}

func TestScrubProxyEnv(t *testing.T) {
	scrubbed := proxy.ScrubProxyEnv([]string{
		"HTTPS_PROXY=http://127.0.0.1:1082",
		"http_proxy=http://127.0.0.1:1082",
		"PATH=/usr/bin",
		"NO_PROXY=localhost",
	})
	joined := strings.Join(scrubbed, "\n")
	if strings.Contains(joined, "HTTPS_PROXY=") || strings.Contains(joined, "http_proxy=") {
		t.Fatalf("proxy vars not scrubbed: %#v", scrubbed)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "NO_PROXY=localhost") {
		t.Fatalf("non-proxy vars lost: %#v", scrubbed)
	}
}

func TestIsTransientProxyError(t *testing.T) {
	err := fmt.Errorf("terminated: %w", syscall.ECONNRESET)
	if !proxy.IsTransientProxyError(err) {
		t.Fatal("expected transient")
	}
	if proxy.IsTransientProxyError(errors.New("no Jev brain is configured")) {
		t.Fatal("ordinary errors must not be transient")
	}
	formatted := proxy.FormatFetchError(err)
	if !strings.Contains(formatted, "terminated") {
		t.Fatalf("FormatFetchError = %q", formatted)
	}
}

// src/proxy.ts proxyAgentOptions takes connections / connectTimeout / headersTimeout
// from the provider-concurrency and upstream-timeout environment variables.
func TestNewTransportReadsEnvLimits(t *testing.T) {
	t.Setenv("JEVONIAN_PROVIDER_CONCURRENCY", "3")
	t.Setenv("JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS", "7000")
	t.Setenv("JEVONIAN_UPSTREAM_CONNECT_TIMEOUT_MS", "2500")
	tr := proxy.NewTransport(proxy.TransportOptions{})
	if tr.MaxConnsPerHost != 3 {
		t.Fatalf("MaxConnsPerHost = %d, want 3", tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 7*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}
	if tr.TLSHandshakeTimeout != 2500*time.Millisecond {
		t.Fatalf("TLSHandshakeTimeout = %v", tr.TLSHandshakeTimeout)
	}
	// A typo falls back and an extreme value is clamped, as in TS.
	t.Setenv("JEVONIAN_PROVIDER_CONCURRENCY", "lots")
	t.Setenv("JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS", "5")
	tr = proxy.NewTransport(proxy.TransportOptions{})
	if tr.MaxConnsPerHost != proxy.DefaultMaxConnsPerHost {
		t.Fatalf("typo MaxConnsPerHost = %d", tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != time.Second {
		t.Fatalf("floor ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}
	// Explicit options still win over the environment.
	tr = proxy.NewTransport(proxy.TransportOptions{MaxConnsPerHost: 11})
	if tr.MaxConnsPerHost != 11 {
		t.Fatalf("explicit MaxConnsPerHost = %d", tr.MaxConnsPerHost)
	}
}
