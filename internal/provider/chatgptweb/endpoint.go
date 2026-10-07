package chatgptweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxQueueDepth = 8
	preflightTimeout     = 3 * time.Second
)

// endpointGate synchronizes operations accessing the same Chrome CDP port.
type endpointGate struct {
	mu         sync.Mutex
	active     int
	waiting    int
	maxWaiting int
	lock       chan struct{}
}

func newEndpointGate(maxWaiting int) *endpointGate {
	return &endpointGate{
		maxWaiting: maxWaiting,
		lock:       make(chan struct{}, 1),
	}
}

func (g *endpointGate) acquire(ctx context.Context) (func(), *Error) {
	g.mu.Lock()
	if g.waiting >= g.maxWaiting {
		g.mu.Unlock()
		return nil, &Error{
			Status:  http.StatusTooManyRequests,
			Kind:    KindRateLimit,
			Message: "CDP endpoint busy: queue capacity exceeded",
		}
	}
	g.waiting++
	g.mu.Unlock()

	select {
	case g.lock <- struct{}{}:
		g.mu.Lock()
		g.waiting--
		g.active++
		g.mu.Unlock()

		var once sync.Once
		release := func() {
			once.Do(func() {
				g.mu.Lock()
				g.active--
				g.mu.Unlock()
				<-g.lock
			})
		}
		return release, nil
	case <-ctx.Done():
		g.mu.Lock()
		g.waiting--
		g.mu.Unlock()
		return nil, &Error{
			Status:  499,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("context cancelled while waiting for CDP endpoint: %v", ctx.Err()),
		}
	}
}

// Global registry of endpoint gates keyed by normalized endpoint.
var (
	endpointRegistryMu sync.Mutex
	endpointRegistry   = make(map[string]*endpointGate)
)

func getEndpointGate(normalized string) *endpointGate {
	endpointRegistryMu.Lock()
	defer endpointRegistryMu.Unlock()

	gate, exists := endpointRegistry[normalized]
	if !exists {
		gate = newEndpointGate(defaultMaxQueueDepth)
		endpointRegistry[normalized] = gate
	}
	return gate
}

// ValidateAndNormalizeEndpoint ensures the CDP endpoint is a valid loopback address
// and returns the canonical host:port format.
func ValidateAndNormalizeEndpoint(rawURL string) (string, *Error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}

	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") && !strings.HasPrefix(trimmed, "ws://") {
		trimmed = "http://" + trimmed
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindInvalid,
			Message: fmt.Sprintf("invalid CDP endpoint URL: %v", err),
		}
	}

	hostname := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "9222"
	}

	if !isLoopback(hostname) {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindInvalid,
			Message: fmt.Sprintf("non-loopback CDP endpoint %q rejected; only loopback endpoints are allowed", rawURL),
		}
	}

	canonicalHost := "127.0.0.1"
	return net.JoinHostPort(canonicalHost, port), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// VersionInfo holds the Chrome /json/version response.
type VersionInfo struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	V8Version            string `json:"V8-Version"`
	WebKitVersion        string `json:"WebKit-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// PreflightChrome verifies that the endpoint is a running Chrome instance.
func PreflightChrome(ctx context.Context, client *http.Client, hostPort string) (*VersionInfo, *Error) {
	if client == nil {
		client = http.DefaultClient
	}

	endpointURL := fmt.Sprintf("http://%s/json/version", hostPort)
	reqCtx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpointURL, nil)
	if err != nil {
		return nil, &Error{
			Status:  http.StatusInternalServerError,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to construct preflight request: %v", err),
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("CDP endpoint not running or connection refused at %s: %v", hostPort, err),
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("CDP preflight returned HTTP %d from %s (not a standard Chrome CDP port)", resp.StatusCode, endpointURL),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to read /json/version response: %v", err),
		}
	}

	var version VersionInfo
	if err := json.Unmarshal(body, &version); err != nil {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("invalid JSON from /json/version at %s (possible non-Chrome endpoint)", hostPort),
		}
	}

	if version.WebSocketDebuggerURL == "" && version.Browser == "" {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("endpoint %s does not appear to be Chrome CDP (/json/version missing Browser and webSocketDebuggerUrl)", hostPort),
		}
	}

	return &version, nil
}
