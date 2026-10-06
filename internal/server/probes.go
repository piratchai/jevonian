package server

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/xinyao27/jevonian/internal/upstream"
)

// accountPaths are endpoints a Codex/ChatGPT desktop client uses for account and
// session bookkeeping. These are not model inference calls, so Jevonian must
// never answer them itself: doing so is what leaves the app stuck on
// "Loading sign-in requirements…".
var accountPaths = map[string]bool{
	"/v1/me":            true,
	"/v1/account":       true,
	"/v1/account/usage": true,
	"/v1/usage":         true,
	"/v1/subscription":  true,
	"/v1/entitlements":  true,
	"/v1/limits":        true,
	"/v1/user":          true,
	"/v1/organizations": true,
}

// IsInferencePath reports whether path is an endpoint Jevonian genuinely serves
// by routing to a configured provider.
func IsInferencePath(path string) bool {
	switch strings.TrimRight(path, "/") {
	case "/v1/responses",
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/models":
		return true
	default:
		return false
	}
}

// IsAccountProbe detects an account/session probe from a desktop client. These
// requests carry the ChatGPT account header rather than an API key, which is how
// the official client separates "my subscription" traffic from "my API key"
// traffic.
func IsAccountProbe(r *http.Request) bool {
	if r == nil {
		return false
	}
	account := r.Header.Get("chatgpt-account-id")
	if strings.TrimSpace(account) == "" {
		return false
	}

	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	if accountPaths[path] {
		return true
	}

	// Any non-inference path under /v1 coming from an account session is a
	// bookkeeping call; let the real backend answer it.
	return strings.HasPrefix(path, "/v1/") && !IsInferencePath(path)
}

// IsWebSocketUpgrade detects a real WebSocket upgrade request.
//
// A single `Upgrade: websocket` header is not enough: proxies and clients also
// send it on ordinary requests. Ollama's Codex proxy requires the `Connection`
// header to list `upgrade` as a token, and matching that behaviour keeps the
// fallback signal identical to the one Codex already handles.
func IsWebSocketUpgrade(h http.Header) bool {
	if !strings.EqualFold(strings.TrimSpace(h.Get("upgrade")), "websocket") {
		return false
	}
	for _, raw := range h.Values("connection") {
		for _, token := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// ChatGPTUpstream is the default account-probe backend.
const ChatGPTUpstream = "https://chatgpt.com"

// AccountProbeTarget builds the upstream URL for an account probe, honoring the
// JEVONIAN_CHATGPT_UPSTREAM override used in dev and tests.
func AccountProbeTarget(r *http.Request) (*url.URL, bool) {
	target := os.Getenv("JEVONIAN_CHATGPT_UPSTREAM")
	if target == "" {
		target = ChatGPTUpstream
	}
	base, err := url.Parse(strings.TrimRight(target, "/") + "/")
	if err != nil {
		return nil, false
	}
	rel := &url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	return base.ResolveReference(rel), true
}

// ProxyAccountProbe forwards an account probe to the real ChatGPT backend so
// sign-in and entitlement checks keep working while model traffic goes to
// Jevonian.
//
// The probe is repeated through the standard transient-retry policy (the same
// retryingFetch the TypeScript uses for bookkeeping calls), then written to w —
// status, headers and a streamed body — unmodified. The bool result is false
// when the upstream could not be reached, letting the caller fall through to
// its normal handling instead of masking the failure.
func ProxyAccountProbe(w http.ResponseWriter, r *http.Request, client *http.Client) bool {
	target, ok := AccountProbeTarget(r)
	if !ok {
		return false
	}
	if client == nil {
		client = http.DefaultClient
	}

	var body io.Reader
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		payload, err := readRequestBody(r)
		if err != nil {
			return false
		}
		body = strings.NewReader(string(payload))
	}

	do := func() (*http.Response, error) {
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
		if err != nil {
			return nil, err
		}
		copyProbeHeaders(outReq.Header, r.Header)
		return client.Do(outReq)
	}

	// Retry transient transport failures and retryable statuses, matching
	// retryingFetch in src/retry.ts. The body is a *strings.Reader, so each
	// attempt rewinds it before dialing.
	resp, _ := upstream.WithRetry(func() (*http.Response, error) {
		if seeker, ok := body.(io.Seeker); ok {
			_, _ = seeker.Seek(0, io.SeekStart)
		}
		return do()
	}, upstream.RetryOptions[*http.Response]{
		Attempts: upstream.ConfiguredRetries() + 1,
		RetryWhen: func(res *http.Response) *upstream.RetryFailure {
			if res != nil && upstream.IsRetryableStatus(res.StatusCode) {
				return &upstream.RetryFailure{Status: res.StatusCode}
			}
			return nil
		},
		Discard: func(res *http.Response) {
			if res != nil && res.Body != nil {
				_ = res.Body.Close()
			}
		},
	})
	if resp == nil {
		return false
	}
	defer resp.Body.Close()

	// Hop-by-hop / framing headers must not be forwarded back to the client.
	out := w.Header()
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-encoding" || lk == "content-length" || lk == "connection" ||
			lk == "transfer-encoding" || lk == "keep-alive" {
			continue
		}
		for _, v := range vals {
			out.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// copyProbeHeaders forwards the incoming headers while dropping hop-by-hop and
// framing fields that must reflect the upstream instead.
func copyProbeHeaders(dst, src http.Header) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "host", "connection", "content-length", "content-encoding",
			"transfer-encoding", "keep-alive", "upgrade", "te", "trailer",
			"proxy-authorization", "proxy-authenticate":
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}
