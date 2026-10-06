package softstream_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/softstream"
)

// timedSource yields one chunk immediately then blocks until Close.
type timedSource struct {
	closed    chan struct{}
	closeOnce sync.Once
	once      bool // only touched by the (sequential) reader goroutines
}

func newTimedSource() *timedSource { return &timedSource{closed: make(chan struct{})} }

func (s *timedSource) Read(p []byte) (int, error) {
	if !s.once {
		s.once = true
		return copy(p, "data: hi\n\n"), nil
	}
	<-s.closed
	return 0, io.EOF
}

func (s *timedSource) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// errorSource yields one chunk, then fails like a wedged upstream.
type errorSource struct {
	once bool
}

func (s *errorSource) Read(p []byte) (int, error) {
	if !s.once {
		s.once = true
		return copy(p, "data: hi\n\n"), nil
	}
	return 0, errors.New("upstream boom")
}

func (s *errorSource) Close() error { return nil }

// eofSource yields one chunk then a clean EOF.
type eofSource struct {
	once bool
}

func (s *eofSource) Read(p []byte) (int, error) {
	if !s.once {
		s.once = true
		return copy(p, "data: hi\n\n"), nil
	}
	return 0, io.EOF
}

func (s *eofSource) Close() error { return nil }

func TestKeepaliveNilSource(t *testing.T) {
	if got := softstream.Keepalive(context.Background(), nil, softstream.KeepaliveOptions{}); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestKeepaliveForwardsAndEmitsAfterSilence(t *testing.T) {
	src := newTimedSource()
	stream := softstream.Keepalive(context.Background(), src, softstream.KeepaliveOptions{
		Interval: 20 * time.Millisecond,
	})
	defer stream.Close()

	buf := make([]byte, 64)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := string(buf[:n]); got != "data: hi\n\n" {
		t.Fatalf("first chunk = %q", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	var second []byte
	for len(second) == 0 && time.Now().Before(deadline) {
		n, err = stream.Read(buf)
		if err != nil {
			t.Fatalf("keepalive read: %v", err)
		}
		second = append(second, buf[:n]...)
	}
	if got := string(second); got != ": keepalive\n\n" {
		t.Fatalf("keepalive chunk = %q", got)
	}
}

func TestKeepaliveCallsOnClientCancelMidStream(t *testing.T) {
	canceled := 0
	src := newTimedSource()
	stream := softstream.Keepalive(context.Background(), src, softstream.KeepaliveOptions{
		Interval:       time.Minute,
		OnClientCancel: func() { canceled++ },
	})
	buf := make([]byte, 64)
	if _, err := stream.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("onClientCancel = %d", canceled)
	}
}

func TestKeepaliveNoCancelCallbackAfterCleanEnd(t *testing.T) {
	canceled := 0
	stream := softstream.Keepalive(context.Background(), &eofSource{}, softstream.KeepaliveOptions{
		Interval:       time.Minute,
		OnClientCancel: func() { canceled++ },
	})
	defer stream.Close()
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Give the producer goroutine a moment to report EOF.
	time.Sleep(20 * time.Millisecond)
	_ = stream.Close()
	if canceled != 0 {
		t.Fatalf("onClientCancel fired after clean end: %d", canceled)
	}
}

func TestKeepaliveCtxCancelCountsAsClientCancel(t *testing.T) {
	canceled := 0
	ctx, cancel := context.WithCancel(context.Background())
	stream := softstream.Keepalive(ctx, newTimedSource(), softstream.KeepaliveOptions{
		Interval:       time.Minute,
		OnClientCancel: func() { canceled++ },
	})
	defer stream.Close()
	cancel()
	// The Read side observes ctx, but OnClientCancel fires on Close when the
	// stream had not finished — matching the harness hang-up ledger row.
	_ = stream.Close()
	if canceled != 1 {
		t.Fatalf("onClientCancel = %d", canceled)
	}
}

func TestKeepaliveOnFirstChunk(t *testing.T) {
	first := 0
	stream := softstream.Keepalive(context.Background(), &eofSource{}, softstream.KeepaliveOptions{
		Interval:     time.Minute,
		OnFirstChunk: func() { first++ },
	})
	defer stream.Close()
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if first != 1 {
		t.Fatalf("onFirstChunk = %d", first)
	}
}

func TestKeepaliveUpstreamErrorEndsStream(t *testing.T) {
	canceled := 0
	stream := softstream.Keepalive(context.Background(), &errorSource{}, softstream.KeepaliveOptions{
		Interval:       time.Minute,
		OnClientCancel: func() { canceled++ },
	})
	defer stream.Close()
	// The upstream error ends the keepalive duty — the failure itself belongs to
	// the caller (soft wrapper records it), not the client-cancel path.
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if canceled != 0 {
		t.Fatalf("upstream error must not be a client cancel: %d", canceled)
	}
}
