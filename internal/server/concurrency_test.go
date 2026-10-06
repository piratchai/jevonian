package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/guard"
	"github.com/xinyao27/jevonian/internal/server"
)

func TestHungProviderDoesNotDelayConcurrentHealthyTurns(t *testing.T) {
	hit := make(chan struct{})
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(hit)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hung.Close()
	defer close(release)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}]}`)
	}))
	defer healthy.Close()
	cfg := config.Config{DefaultProvider: "hung", Providers: []config.Provider{
		{Name: "hung", Type: config.ProviderTypeOpenAI, BaseURL: hung.URL, APIKey: "k", Models: []config.ModelEntry{{ID: "m"}}},
		{Name: "healthy", Type: config.ProviderTypeOpenAI, BaseURL: healthy.URL, APIKey: "k", Models: []config.ModelEntry{{ID: "m"}}},
	}}
	g := guard.New(guard.Options{Concurrency: 128})
	// Hold all hung slots except one, which is held by an actual canceled turn.
	held := make([]*guard.Attempt, 127)
	for i := range held {
		held[i], _ = g.Begin("hung")
		defer held[i].Release()
	}
	srv := httptest.NewServer(server.New("", server.Deps{Config: &cfg, Guard: g}).Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-hit:
	case <-time.After(3 * time.Second):
		t.Fatal("hung turn did not begin")
	}
	const concurrent = 64
	var wg sync.WaitGroup
	failures := make(chan string, concurrent)
	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				failures <- err.Error()
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || resp.Header.Get("X-Jevonian-Provider") != "healthy" || !strings.Contains(string(body), "healthy") {
				failures <- fmt.Sprintf("status=%d body=%s", resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	t.Logf("%d healthy turns while provider hung: %s", concurrent, time.Since(start))
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("client cancellation blocked")
	}
	deadline := time.Now().Add(time.Second)
	for g.Snapshot("hung").InFlight != 127 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := g.Snapshot("hung").InFlight; got != 127 {
		t.Fatalf("cancel leaked provider slot: %d", got)
	}
	if got := g.Snapshot("healthy").InFlight; got != 0 {
		t.Fatalf("healthy turns leaked slots: %d", got)
	}
}
