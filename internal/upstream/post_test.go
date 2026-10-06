package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostSameHostRetryThenSuccess(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	zero := 1
	c := &Client{
		HTTP:     ts.Client(),
		SameHost: &zero,
		Sleep:    func(time.Duration) {},
		Timeouts: Timeouts{TotalMS: 5_000, HeadersMS: 5_000, FirstByteMS: 5_000},
	}
	result, err := c.Post(context.Background(), PostOptions{
		URL:     ts.URL,
		Headers: http.Header{"content-type": []string{"application/json"}},
		Body:    []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	if result.Response.StatusCode != 200 {
		t.Fatalf("status = %d", result.Response.StatusCode)
	}
	if result.Retries != 1 {
		t.Fatalf("retries = %d", result.Retries)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestPostRetryable5xxExhaustsBudget(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer ts.Close()

	zero := 1
	c := &Client{
		HTTP:     ts.Client(),
		SameHost: &zero,
		Sleep:    func(time.Duration) {},
		Timeouts: Timeouts{TotalMS: 5_000, HeadersMS: 5_000},
	}
	result, err := c.Post(context.Background(), PostOptions{
		URL:  ts.URL,
		Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response.StatusCode != 502 {
		t.Fatalf("status = %d", result.Response.StatusCode)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
}

func TestPostTotalTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer ts.Close()

	zero := 0
	c := &Client{
		HTTP:     ts.Client(),
		SameHost: &zero,
		Timeouts: Timeouts{TotalMS: 1000}, // floor is 1000ms from LoadTimeouts helpers; set directly
	}
	// Bypass floor by using withTimeout directly via Post — TotalMS 50 would still work in withTimeout.
	c.Timeouts.TotalMS = 50
	_, err := c.Post(context.Background(), PostOptions{URL: ts.URL, Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !IsTimeout(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestPostResponseBodySurvivesReturn(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "complete response")
				case <-r.Context().Done():
				}
			}))
			defer ts.Close()
			zero := 0
			c := &Client{HTTP: ts.Client(), SameHost: &zero,
				Timeouts: Timeouts{TotalMS: 2000, HeadersMS: 2000}}
			result, err := c.Post(context.Background(), PostOptions{URL: ts.URL, Stream: streaming})
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			defer result.Response.Body.Close()
			body, err := io.ReadAll(result.Response.Body)
			if err != nil || string(body) != "complete response" {
				t.Fatalf("body=%q err=%v", body, err)
			}
		})
	}
}

func TestPostTotalTimeoutCoversResponseBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	zero := 0
	c := &Client{HTTP: ts.Client(), SameHost: &zero, Timeouts: Timeouts{TotalMS: 40}}
	result, err := c.Post(context.Background(), PostOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	_, err = io.ReadAll(result.Response.Body)
	var timeout *TimeoutError
	if !errors.As(err, &timeout) || timeout.Phase != PhaseTotal {
		t.Fatalf("expected total body timeout, got %v", err)
	}
}

func TestPostStreamOutlivesHeadersBudget(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, "delayed event")
	}))
	defer ts.Close()
	zero := 0
	c := &Client{HTTP: ts.Client(), SameHost: &zero, Timeouts: Timeouts{HeadersMS: 40}}
	result, err := c.Post(context.Background(), PostOptions{URL: ts.URL, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	body, err := io.ReadAll(result.Response.Body)
	if err != nil || string(body) != "delayed event" {
		t.Fatalf("headers clock truncated stream: %q %v", body, err)
	}
}

func TestPostBodyCloseCancelsRequest(t *testing.T) {
	canceled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer ts.Close()
	zero := 0
	c := &Client{HTTP: ts.Client(), SameHost: &zero, Timeouts: Timeouts{HeadersMS: 2000}}
	result, err := c.Post(context.Background(), PostOptions{URL: ts.URL, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Response.Body.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("closing the response did not cancel the upstream request")
	}
}

// Transport failover across providers is exercised at the server level
// (internal/server.TestChatCompletionsTransportFailover); Client itself only
// retries the same host and has no provider list.
