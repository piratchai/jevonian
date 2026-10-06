package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestRetryBackoffRespectsContextDeadline(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{textResponse(503, "busy")}}
	c := &Client{HTTP: &http.Client{Transport: tr}}
	started := time.Now()
	out := c.Ask(context.Background(), Input{
		Brain:  config.BrainConfig{Channel: "custom", BaseURL: "http://brain.invalid", TimeoutMs: 30},
		APIKey: "fixture-key", State: map[string]any{},
	})
	if out.Failure == nil {
		t.Fatal("expected timeout failure")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("retry backoff exceeded the 30ms deadline: %s", elapsed)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("retry sent after deadline: %d calls", len(tr.requests))
	}
}

func TestConcurrentCallsKeepVerdictsAndFailuresIsolated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State struct {
				ID int `json:"id"`
			} `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		id := body.State.ID
		if id%3 == 0 {
			w.WriteHeader(402)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{"model": map[string]any{
				"choice": fmt.Sprintf("turn-%d", id), "confidence": 0.9,
			}},
		})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	const count = 64
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			out := c.Ask(context.Background(), Input{
				Brain:  config.BrainConfig{Channel: "custom", BaseURL: srv.URL, TimeoutMs: 3000},
				APIKey: "fixture-key", State: map[string]any{"id": id},
			})
			if id%3 == 0 {
				if out.Failure == nil || out.Failure.Status != 402 || out.Verdict != nil {
					errs <- fmt.Errorf("turn %d: wrong failure: %+v", id, out)
				}
			} else if out.Verdict == nil || out.Verdict.Model != fmt.Sprintf("turn-%d", id) || out.Failure != nil {
				errs <- fmt.Errorf("turn %d: wrong verdict: %+v", id, out)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
