package upstream

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultRetries         = 2
	defaultSameHostRetries = 1
	maxRetries             = 5
	baseDelayMS            = 250
	maxDelayMS             = 2_000
)

var retryableStatus = map[int]bool{
	408: true, 500: true, 502: true, 503: true, 504: true,
}

// IsRetryableStatus reports whether an upstream status may succeed on a second attempt.
func IsRetryableStatus(status int) bool {
	return retryableStatus[status]
}

// IsRetryableError reports whether a thrown error looks like a transport failure worth repeating.
// A deliberate caller cancel is not retried; timeouts are.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if IsTimeout(err) {
		return true
	}
	// Caller hang-up (not a host failure). Timeouts are delivered as TimeoutError, not bare Canceled.
	if errors.Is(err, context.Canceled) {
		return false
	}

	current := err
	for depth := 0; current != nil && depth < 6; depth++ {
		msg := strings.ToLower(current.Error())
		if strings.Contains(msg, "fetch failed") ||
			strings.Contains(msg, "terminated") ||
			strings.Contains(msg, "other side closed") ||
			strings.Contains(msg, "socket hang up") ||
			strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "broken pipe") ||
			strings.Contains(msg, "connection refused") {
			return true
		}
		if errors.Is(current, io.EOF) ||
			errors.Is(current, io.ErrUnexpectedEOF) ||
			errors.Is(current, syscall.ECONNRESET) ||
			errors.Is(current, syscall.ECONNREFUSED) ||
			errors.Is(current, syscall.ECONNABORTED) ||
			errors.Is(current, syscall.EPIPE) ||
			errors.Is(current, syscall.ETIMEDOUT) ||
			errors.Is(current, syscall.ENETUNREACH) ||
			errors.Is(current, syscall.EHOSTUNREACH) ||
			errors.Is(current, syscall.ENETDOWN) ||
			errors.Is(current, os.ErrDeadlineExceeded) {
			return true
		}
		var netErr net.Error
		if errors.As(current, &netErr) && netErr.Timeout() {
			return true
		}
		var dnsErr *net.DNSError
		if errors.As(current, &dnsErr) {
			return true
		}
		unwrapped := errors.Unwrap(current)
		if unwrapped == nil {
			break
		}
		current = unwrapped
	}
	return false
}

// ConfiguredRetries is JEVONIAN_UPSTREAM_RETRIES (general egress budget).
func ConfiguredRetries() int {
	return readRetryBudget("JEVONIAN_UPSTREAM_RETRIES", defaultRetries)
}

// ConfiguredSameHostRetries is how many times one host is repeated before failover.
func ConfiguredSameHostRetries() int {
	return readRetryBudget("JEVONIAN_SAME_HOST_RETRIES", defaultSameHostRetries)
}

func readRetryBudget(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return fallback
	}
	if parsed > maxRetries {
		return maxRetries
	}
	return parsed
}

// RetryDelayMS is exponential backoff with jitter.
func RetryDelayMS(attempt int) int {
	backoff := baseDelayMS * (1 << (attempt - 1))
	if backoff > maxDelayMS {
		backoff = maxDelayMS
	}
	return int(float64(backoff) * (0.5 + rand.Float64()*0.5))
}

// RetryFailure describes why an attempt is being repeated.
type RetryFailure struct {
	Err    error
	Status int
}

func (f RetryFailure) String() string {
	if f.Status != 0 {
		return "HTTP " + strconv.Itoa(f.Status)
	}
	return DescribeError(f.Err)
}

// RetryAttempt is one failed attempt about to be repeated.
type RetryAttempt struct {
	Attempt int
	DelayMS int
	Failure RetryFailure
}

// RetryOptions configure WithRetry.
type RetryOptions[T any] struct {
	Attempts  int
	RetryWhen func(T) *RetryFailure
	Discard   func(T)
	OnRetry   func(RetryAttempt)
	Sleep     func(time.Duration)
}

// WithRetry runs fn, repeating while the failure looks transient and attempts remain.
func WithRetry[T any](fn func() (T, error), opts RetryOptions[T]) (T, error) {
	attempts := opts.Attempts
	if attempts < 1 {
		attempts = 1 + defaultRetries
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var zero T
	for attempt := 1; ; attempt++ {
		outcome, err := fn()
		if err != nil {
			if attempt >= attempts || !IsRetryableError(err) {
				return zero, err
			}
			delay := RetryDelayMS(attempt)
			if opts.OnRetry != nil {
				opts.OnRetry(RetryAttempt{Attempt: attempt, DelayMS: delay, Failure: RetryFailure{Err: err}})
			}
			sleep(time.Duration(delay) * time.Millisecond)
			continue
		}
		var failure *RetryFailure
		if opts.RetryWhen != nil {
			failure = opts.RetryWhen(outcome)
		}
		if attempt >= attempts || failure == nil {
			return outcome, nil
		}
		if opts.Discard != nil {
			opts.Discard(outcome)
		}
		delay := RetryDelayMS(attempt)
		if opts.OnRetry != nil {
			opts.OnRetry(RetryAttempt{Attempt: attempt, DelayMS: delay, Failure: *failure})
		}
		sleep(time.Duration(delay) * time.Millisecond)
	}
}

// DescribeError returns a compact unwrap chain for logs / client errors.
func DescribeError(err error) string {
	if err == nil {
		return ""
	}
	parts := make([]string, 0, 5)
	for current := err; current != nil && len(parts) < 5; current = errors.Unwrap(current) {
		parts = append(parts, current.Error())
		if errors.Unwrap(current) == current {
			break
		}
	}
	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, " caused by ")
}

// mapContextError replaces a bare context cancel with its TimeoutError cause when present.
func mapContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	cause := context.Cause(ctx)
	if cause != nil && cause != context.Canceled && cause != context.DeadlineExceeded {
		return cause
	}
	return err
}
