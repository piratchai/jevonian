package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// TimeoutPhase names which layered clock fired.
type TimeoutPhase string

const (
	PhaseConnect   TimeoutPhase = "connect"
	PhaseHeaders   TimeoutPhase = "headers"
	PhaseTotal     TimeoutPhase = "total"
	PhaseFirstByte TimeoutPhase = "first-byte"
	PhaseIdle      TimeoutPhase = "idle"
)

// TimeoutError is raised when a layered upstream clock fires.
type TimeoutError struct {
	Phase TimeoutPhase
	MS    int
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("upstream %s timed out after %dms", e.Phase, e.MS)
}

// Timeouts are the clocks that bound an upstream attempt.
type Timeouts struct {
	ConnectMS   int
	HeadersMS   int
	TotalMS     int
	FirstByteMS int
	IdleMS      int
}

// DefaultTimeouts match src/upstream-timeout.ts.
var DefaultTimeouts = Timeouts{
	ConnectMS:   15_000,
	HeadersMS:   60_000,
	TotalMS:     90_000,
	FirstByteMS: 60_000,
	IdleMS:      120_000,
}

const (
	minTimeoutMS = 1_000
	maxTimeoutMS = 30 * 60_000
)

// LoadTimeouts reads JEVONIAN_UPSTREAM_*_TIMEOUT_MS overrides from the environment.
func LoadTimeouts() Timeouts {
	return Timeouts{
		ConnectMS:   readTimeoutMS("JEVONIAN_UPSTREAM_CONNECT_TIMEOUT_MS", DefaultTimeouts.ConnectMS),
		HeadersMS:   readTimeoutMS("JEVONIAN_UPSTREAM_HEADERS_TIMEOUT_MS", DefaultTimeouts.HeadersMS),
		TotalMS:     readTimeoutMS("JEVONIAN_UPSTREAM_TOTAL_TIMEOUT_MS", DefaultTimeouts.TotalMS),
		FirstByteMS: readTimeoutMS("JEVONIAN_UPSTREAM_FIRST_BYTE_TIMEOUT_MS", DefaultTimeouts.FirstByteMS),
		IdleMS:      readTimeoutMS("JEVONIAN_UPSTREAM_STREAM_IDLE_TIMEOUT_MS", DefaultTimeouts.IdleMS),
	}
}

func readTimeoutMS(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if parsed < minTimeoutMS {
		return minTimeoutMS
	}
	if parsed > maxTimeoutMS {
		return maxTimeoutMS
	}
	return parsed
}

// IsTimeout reports whether err is (or wraps) an upstream / deadline timeout.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var te *TimeoutError
	if errors.As(err, &te) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeout interface{ Timeout() bool }
	var t timeout
	if errors.As(err, &t) && t.Timeout() {
		return true
	}
	return false
}

// withTimeout returns a child context that cancels with TimeoutError after ms.
func withTimeout(parent context.Context, ms int, phase TimeoutPhase) (context.Context, context.CancelFunc) {
	if ms <= 0 {
		return context.WithCancel(parent)
	}
	ctx, cancel := context.WithCancelCause(parent)
	timer := time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
		cancel(&TimeoutError{Phase: phase, MS: ms})
	})
	return ctx, func() {
		timer.Stop()
		cancel(nil)
	}
}
