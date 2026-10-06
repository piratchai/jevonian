// Package httpx is the small retrying HTTP helper the credential and WorkBuddy
// code uses for bookkeeping calls (token refresh, sign-in polls, quota/config
// reads). It mirrors retryingFetch in src/retry.ts: transport failures and
// gateway statuses (408/500/502/503/504) are repeated with jittered backoff,
// up to JEVONIAN_UPSTREAM_RETRIES extra attempts.
//
// It deliberately does not import internal/upstream: that package is the
// server's egress layer and is expected to import the oauth seam, so a
// dependency the other way would cycle.
package httpx

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultRetries = 2
	maxRetries     = 5
	baseDelayMS    = 250
	maxDelayMS     = 2000
)

// Retries is JEVONIAN_UPSTREAM_RETRIES clamped to [0, 5], default 2.
func Retries() int {
	raw := strings.TrimSpace(os.Getenv("JEVONIAN_UPSTREAM_RETRIES"))
	if raw == "" {
		return defaultRetries
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultRetries
	}
	return min(n, maxRetries)
}

// IsRetryableStatus reports a gateway status the same host may recover from.
func IsRetryableStatus(status int) bool {
	switch status {
	case 408, 500, 502, 503, 504:
		return true
	}
	return false
}

// IsRetryableError reports a transport failure worth repeating. A caller's own
// cancellation is never retried.
func IsRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) {
		return true
	}
	for _, code := range []error{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.EPIPE,
		syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN,
	} {
		if errors.Is(err, code) {
			return true
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"connection reset", "terminated", "other side closed", "socket hang up"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func delay(attempt int) time.Duration {
	backoff := baseDelayMS * (1 << (attempt - 1))
	backoff = min(backoff, maxDelayMS)
	return time.Duration(float64(backoff)*(0.5+rand.Float64()*0.5)) * time.Millisecond
}

// Request describes one call; Body is re-sent on every attempt.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// Do sends req through client with transient-failure retries. A nil client
// means http.DefaultClient. The caller owns the returned body.
func Do(ctx context.Context, client *http.Client, req Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	attempts := Retries() + 1
	for attempt := 1; ; attempt++ {
		var body io.Reader
		if req.Body != nil {
			body = strings.NewReader(string(req.Body))
		}
		method := req.Method
		if method == "" {
			method = http.MethodGet
		}
		httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
		if err != nil {
			return nil, err
		}
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			if attempt >= attempts || !IsRetryableError(err) {
				return nil, err
			}
		} else if attempt >= attempts || !IsRetryableStatus(resp.StatusCode) {
			return resp, nil
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay(attempt)):
		}
	}
}

// ReadAll sends req and returns the status and full body text.
func ReadAll(ctx context.Context, client *http.Client, req Request) (int, []byte, error) {
	resp, err := Do(ctx, client, req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}
