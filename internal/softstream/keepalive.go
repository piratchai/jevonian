package softstream

import (
	"context"
	"io"
	"sync"
	"time"
)

// Keepalive is the SSE comment written while the upstream is silent. Parsers
// ignore it; idle proxies and agent clients reset their timers on it.
var keepaliveBytes = []byte(": keepalive\n\n")

// DefaultKeepaliveInterval is the silence before a keepalive comment is written
// (matches src/stream-keepalive.ts STREAM_KEEPALIVE_MS).
const DefaultKeepaliveInterval = 15 * time.Second

// KeepaliveOptions configures Keepalive.
type KeepaliveOptions struct {
	// Interval is the silence before a keepalive is emitted. Defaults to 15s.
	Interval time.Duration
	// OnClientCancel is called when the client abandons the stream before it
	// finishes — the wrapped reader's Close before upstream EOF, or ctx
	// cancellation. Upstream read failures do not invoke it: the turn was already
	// a failure and owns its own ledger row.
	OnClientCancel func()
	// OnFirstChunk is called once, when the first non-keepalive byte reaches the
	// client. This is the first byte the client can render, which is what makes a
	// first-token measurement meaningful; a keepalive comment is not content.
	OnFirstChunk func()
}

// Keepalive wraps an SSE body so idle periods still emit traffic, and so a client
// disconnect can be observed.
//
// Claude's thinking phase can produce no client-facing bytes for well over a
// minute when thinking deltas are dropped by a wire bridge. Idle proxies and
// agent clients then cancel the connection. An SSE comment (`: …`) is ignored by
// parsers but resets those idle timers.
//
// A nil source returns nil (mirrors the TS helper).
func Keepalive(ctx context.Context, source io.ReadCloser, opts KeepaliveOptions) io.ReadCloser {
	if source == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultKeepaliveInterval
	}
	k := &keepaliveReader{
		ctx:      ctx,
		source:   source,
		opts:     opts,
		interval: interval,
		queue:    make(chan []byte, 64),
		aborted:  make(chan struct{}),
	}
	go k.run()
	return k
}

type keepaliveReader struct {
	ctx      context.Context
	source   io.ReadCloser
	opts     KeepaliveOptions
	interval time.Duration

	queue   chan []byte
	aborted chan struct{}

	abortOnce sync.Once
	mu        sync.Mutex
	pending   []byte
	eof       bool
	closed    bool
	finished  bool // upstream ended on its own (EOF) — a later Close is not a client cancel
}

func (k *keepaliveReader) Read(p []byte) (int, error) {
	for len(k.pending) == 0 {
		if k.eof {
			return 0, io.EOF
		}
		select {
		case chunk, ok := <-k.queue:
			if !ok {
				k.eof = true
				k.markFinished()
				return 0, io.EOF
			}
			k.pending = chunk
		case <-k.ctx.Done():
			k.eof = true
			k.abort()
			return 0, io.EOF
		case <-k.aborted:
			k.eof = true
			return 0, io.EOF
		}
	}
	n := copy(p, k.pending)
	k.pending = k.pending[n:]
	return n, nil
}

// Close abandons the stream. When the upstream had not finished, this is a client
// hang-up and OnClientCancel fires — that is how canceled turns get a ledger row
// instead of vanishing.
func (k *keepaliveReader) Close() error {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil
	}
	k.closed = true
	finished := k.finished
	k.mu.Unlock()
	if !finished && k.opts.OnClientCancel != nil {
		k.opts.OnClientCancel()
	}
	k.abort()
	return k.source.Close()
}

func (k *keepaliveReader) markFinished() {
	k.mu.Lock()
	k.finished = true
	k.mu.Unlock()
}

func (k *keepaliveReader) abort() {
	k.abortOnce.Do(func() { close(k.aborted) })
}

// run copies upstream bytes downstream, interleaving keepalive comments after
// `interval` of silence.
func (k *keepaliveReader) run() {
	defer close(k.queue)
	defer k.source.Close()

	readCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	buf := make([]byte, 32<<10)

	var readActive bool
	sawFirst := false
	// Arm the first silence window immediately, mirroring `start` arming the
	// timer before the first chunk.
	timer := time.NewTimer(k.interval)
	defer timer.Stop()

	for {
		if !readActive {
			readActive = true
			go func() {
				n, err := k.source.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					readCh <- chunk
					return
				}
				errCh <- err
			}()
		}

		select {
		case chunk := <-readCh:
			readActive = false
			if !sawFirst {
				sawFirst = true
				if k.opts.OnFirstChunk != nil {
					// Observing a stream must never break it.
					func() {
						defer func() { _ = recover() }()
						k.opts.OnFirstChunk()
					}()
				}
			}
			if !k.emit(chunk) {
				return
			}
			resetTimer(timer, k.interval)
		case err := <-errCh:
			readActive = false
			// io.EOF and read errors both end the keepalive duty; the failure is
			// the caller's to record (the soft wrapper above turns it into a soft
			// completion first).
			_ = err
			return
		case <-timer.C:
			// The upstream is silent; keep the socket warm.
			if !k.emit(keepaliveBytes) {
				return
			}
			timer.Reset(k.interval)
		case <-k.ctx.Done():
			return
		case <-k.aborted:
			return
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (k *keepaliveReader) emit(chunk []byte) bool {
	select {
	case k.queue <- chunk:
		return true
	case <-k.ctx.Done():
		return false
	case <-k.aborted:
		return false
	}
}
