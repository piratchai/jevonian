package softstream

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// IdleTimeoutError is returned by IdleGuard when the upstream stays silent
// longer than the idle budget after the stream started. It mirrors
// UpstreamTimeoutError("idle", ms) in src/upstream-timeout.ts: a verdict about
// the host, reported through the same soft-close path as a dropped stream.
type IdleTimeoutError struct{ MS int }

func (e *IdleTimeoutError) Error() string {
	return fmt.Sprintf("upstream idle timed out after %dms", e.MS)
}

// Timeout lets net.Error-style checks (upstream.IsTimeout) recognise it.
func (e *IdleTimeoutError) Timeout() bool { return true }

// IdleGuard bounds the silence between reads of a stream that has already
// produced its first byte (guardUpstreamStream in src/upstream-timeout.ts: the
// first-byte clock is enforced by upstream.GuardFirstByte, this is the idle
// clock that follows it). When a read stays pending past idleMS the body is
// closed and the read returns *IdleTimeoutError, so a stream that starts and
// then wedges ends instead of staying open for the full total timeout.
// idleMS <= 0 disables the guard.
func IdleGuard(body io.ReadCloser, idleMS int) io.ReadCloser {
	if body == nil || idleMS <= 0 {
		return body
	}
	return &idleReader{body: body, idle: time.Duration(idleMS) * time.Millisecond, ms: idleMS}
}

type idleReader struct {
	body io.ReadCloser
	idle time.Duration
	ms   int

	mu       sync.Mutex
	timedOut bool
	pending  *readResult // a read still running after a prior timeout is never reused
}

type readResult struct {
	n   int
	err error
}

func (r *idleReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.timedOut {
		r.mu.Unlock()
		return 0, &IdleTimeoutError{MS: r.ms}
	}
	r.mu.Unlock()

	done := make(chan readResult, 1)
	buf := make([]byte, len(p))
	go func() {
		n, err := r.body.Read(buf)
		done <- readResult{n, err}
	}()
	timer := time.NewTimer(r.idle)
	defer timer.Stop()
	select {
	case res := <-done:
		copy(p, buf[:res.n])
		return res.n, res.err
	case <-timer.C:
		r.mu.Lock()
		r.timedOut = true
		r.mu.Unlock()
		_ = r.body.Close() // unblocks the pending read
		return 0, &IdleTimeoutError{MS: r.ms}
	}
}

func (r *idleReader) Close() error { return r.body.Close() }
