package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Client POSTs to providers with layered timeouts and same-host retries.
// Provider health gating lives in internal/guard + internal/quota on the
// attempt runner; Client only knows how to POST with retries.
type Client struct {
	HTTP     *http.Client
	Timeouts Timeouts
	// SameHost overrides JEVONIAN_SAME_HOST_RETRIES when non-nil.
	SameHost *int
	Sleep    func(time.Duration)
}

// PostResult is one finished same-host attempt cycle (ok body still streaming when Stream).
type PostResult struct {
	Response *http.Response
	// Text is the non-ok body (already drained). Empty when Response.OK.
	Text    string
	Retries int
}

// PostOptions configure a single upstream POST.
type PostOptions struct {
	URL     string
	Headers http.Header
	Body    []byte
	Stream  bool
	OnRetry func(RetryAttempt)
}

// Post sends body to url with same-host retry. The caller owns Response.Body when status is ok.
func (c *Client) Post(ctx context.Context, opts PostOptions) (PostResult, error) {
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	timeouts := c.Timeouts
	if timeouts == (Timeouts{}) {
		timeouts = LoadTimeouts()
	}
	sameHost := ConfiguredSameHostRetries()
	if c.SameHost != nil {
		sameHost = *c.SameHost
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	budgetMS := timeouts.TotalMS
	phase := PhaseTotal
	if opts.Stream {
		budgetMS = timeouts.HeadersMS
		phase = PhaseHeaders
	}

	var retries int
	outcome, err := WithRetry(func() (PostResult, error) {
		// The request context must live until the response body is consumed.
		// Canceling on return from Do truncates bodies that arrive after headers.
		reqCtx, cancelCause := context.WithCancelCause(ctx)
		cancel := func() { cancelCause(nil) }
		var timer *time.Timer
		timerDone := make(chan struct{})
		if budgetMS > 0 {
			timer = time.AfterFunc(time.Duration(budgetMS)*time.Millisecond, func() {
				cancelCause(&TimeoutError{Phase: phase, MS: budgetMS})
				close(timerDone)
			})
		}
		cleanup := func() {
			if timer != nil {
				timer.Stop()
			}
			cancel()
		}

		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, opts.URL, bytes.NewReader(opts.Body))
		if err != nil {
			cleanup()
			return PostResult{}, err
		}
		for k, vals := range opts.Headers {
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}

		resp, err := httpClient.Do(req)
		err = mapContextError(reqCtx, err)
		if err != nil {
			cleanup()
			return PostResult{}, err
		}
		if opts.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 && timer != nil {
			// Headers budget ends here; first-byte and idle clocks own the body.
			if !timer.Stop() {
				// Stop can lose a race to a callback that has not canceled yet.
				// Wait for it before handing the stream to the caller.
				<-timerDone
				_ = resp.Body.Close()
				err := mapContextError(reqCtx, reqCtx.Err())
				cleanup()
				return PostResult{}, err
			}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body = &contextBody{ReadCloser: resp.Body, ctx: reqCtx, cleanup: cleanup}
			return PostResult{Response: resp}, nil
		}
		defer cleanup()
		// Drain non-ok bodies so the pooled connection is reusable for a retry.
		text, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil && len(text) == 0 {
			return PostResult{}, readErr
		}
		return PostResult{Response: &http.Response{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       http.NoBody,
		}, Text: string(text)}, nil
	}, RetryOptions[PostResult]{
		Attempts: sameHost + 1,
		RetryWhen: func(r PostResult) *RetryFailure {
			if r.Response != nil && IsRetryableStatus(r.Response.StatusCode) {
				return &RetryFailure{Status: r.Response.StatusCode}
			}
			return nil
		},
		Discard: func(r PostResult) {
			if r.Response != nil && r.Response.Body != nil {
				_ = r.Response.Body.Close()
			}
		},
		OnRetry: func(info RetryAttempt) {
			retries++
			if opts.OnRetry != nil {
				opts.OnRetry(info)
			}
		},
		Sleep: sleep,
	})
	outcome.Retries = retries
	return outcome, err
}

// contextBody transfers request cleanup to the consumer of the response body.
// Non-streaming bodies retain the total timeout; streaming bodies retain parent
// cancellation after the headers timer has stopped.
type contextBody struct {
	io.ReadCloser
	ctx     context.Context
	cleanup func()
	once    sync.Once
}

func (b *contextBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		err = mapContextError(b.ctx, err)
		b.once.Do(b.cleanup)
	}
	return n, err
}

func (b *contextBody) Close() error {
	b.once.Do(b.cleanup)
	return b.ReadCloser.Close()
}
