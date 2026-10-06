package softstream_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/softstream"
)

type slowBody struct {
	chunks  []string
	gap     time.Duration
	closed  chan struct{}
	release chan struct{}
}

func (b *slowBody) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		select {
		case <-b.release:
			return 0, io.EOF
		case <-b.closed:
			return 0, io.ErrClosedPipe
		}
	}
	select {
	case <-time.After(b.gap):
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
	n := copy(p, b.chunks[0])
	b.chunks = b.chunks[1:]
	return n, nil
}

func (b *slowBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// A stream that starts and then wedges must end with an idle timeout rather than
// hang for the total timeout (src/upstream-timeout.ts guardUpstreamStream).
func TestIdleGuardEndsAStreamThatWedges(t *testing.T) {
	guarded := softstream.IdleGuard(&slowBody{chunks: []string{"data: a\n\n"}, gap: time.Millisecond, closed: make(chan struct{}), release: make(chan struct{})}, 50)
	buf := make([]byte, 64)
	n, err := guarded.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "data: a") {
		t.Fatalf("first read = %q, %v", buf[:n], err)
	}
	start := time.Now()
	_, err = guarded.Read(buf)
	var idle *softstream.IdleTimeoutError
	if !errors.As(err, &idle) || idle.MS != 50 {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("idle guard took too long")
	}
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("idle error must report Timeout()")
	}
	if _, err := guarded.Read(buf); !errors.As(err, &idle) {
		t.Fatalf("reads after a timeout must keep failing: %v", err)
	}
}

func TestIdleGuardPassesActiveStreamAndDisabledBudget(t *testing.T) {
	src := io.NopCloser(strings.NewReader("hello"))
	if softstream.IdleGuard(src, 0) != src {
		t.Fatal("idle <= 0 must return the body untouched")
	}
	guarded := softstream.IdleGuard(io.NopCloser(strings.NewReader("hello world")), 200)
	got, err := io.ReadAll(guarded)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("got %q %v", got, err)
	}
}
