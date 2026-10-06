package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// GuardFirstByte waits for the first body byte before the caller commits to the client.
// On success it returns a Response whose body replays that byte then the rest of the stream.
// On timeout / read error the original body is closed and an error is returned (failover-safe).
func GuardFirstByte(ctx context.Context, resp *http.Response, firstByteMS int) (*http.Response, error) {
	if resp == nil || resp.Body == nil {
		return resp, nil
	}
	if firstByteMS <= 0 {
		firstByteMS = DefaultTimeouts.FirstByteMS
	}

	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 1)
		n, err := resp.Body.Read(buf)
		if n > 0 {
			ch <- result{b: buf[:n], err: nil}
			return
		}
		ch <- result{err: err}
	}()

	timer := time.NewTimer(time.Duration(firstByteMS) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		_ = resp.Body.Close()
		return nil, mapContextError(ctx, ctx.Err())
	case <-timer.C:
		_ = resp.Body.Close()
		return nil, &TimeoutError{Phase: PhaseFirstByte, MS: firstByteMS}
	case r := <-ch:
		if r.err != nil {
			_ = resp.Body.Close()
			if r.err == io.EOF {
				// Empty body is still a completed response.
				cloned := *resp
				cloned.Body = io.NopCloser(bytes.NewReader(nil))
				return &cloned, nil
			}
			return nil, r.err
		}
		cloned := *resp
		cloned.Body = &prefixReader{prefix: r.b, rest: resp.Body}
		return &cloned, nil
	}
}

type prefixReader struct {
	prefix []byte
	rest   io.ReadCloser
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *prefixReader) Close() error {
	return p.rest.Close()
}
