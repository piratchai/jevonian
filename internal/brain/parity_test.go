package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// rewriteTransport sends every request to a local httptest server while
// recording the original URL, so channel default URLs are verified without
// touching the network.
type rewriteTransport struct {
	target *url.URL
	seen   []string
	hdrs   []http.Header
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req.URL.String())
	r.hdrs = append(r.hdrs, req.Header.Clone())
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func verdictServer(t *testing.T, status *atomic.Int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if code := int(status.Load()); code != 200 {
			http.Error(w, "boom", code)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"answers": map[string]any{"model": map[string]any{"choice": "execute", "confidence": 0.8}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestChannelsDefaultURLsAndAttribution(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv, _ := verdictServer(t, &status)
	target, _ := url.Parse(srv.URL)
	rt := &rewriteTransport{target: target}
	c := &Client{HTTP: &http.Client{Transport: rt}, Sleep: func(time.Duration) {}}
	for _, ch := range []string{"typesafe", "openrouter", "opencode-zen"} {
		out := c.Ask(context.Background(), Input{
			Brain:  config.BrainConfig{Channel: ch, TimeoutMs: 2000, MinConfidence: 0.6},
			APIKey: "k", State: map[string]any{},
		})
		if out.Verdict == nil || out.Verdict.Model != "execute" {
			t.Fatalf("%s: %+v", ch, out)
		}
	}
	want := []string{
		"https://api.typesafe.ai/v1/systemone",
		"https://openrouter.ai/api/alpha/decisions",
		"https://opencode.ai/zen/v1/systemone",
	}
	for i, w := range want {
		if rt.seen[i] != w {
			t.Errorf("url[%d]=%s want %s", i, rt.seen[i], w)
		}
	}
	if rt.hdrs[1].Get("HTTP-Referer") == "" || rt.hdrs[1].Get("X-Title") != "Jevonian" {
		t.Errorf("openrouter attribution missing: %v", rt.hdrs[1])
	}
	if rt.hdrs[0].Get("X-Title") != "" || rt.hdrs[2].Get("X-Title") != "" {
		t.Error("attribution leaked to non-openrouter hosts")
	}
}

func TestCustomChannelUsesFreeFormBaseURLAndKey(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv, hits := verdictServer(t, &status)
	c := &Client{HTTP: srv.Client(), Sleep: func(time.Duration) {}}
	out := c.Ask(context.Background(), Input{
		Brain:  config.BrainConfig{Channel: "custom", BaseURL: srv.URL + "/my/systemone", TimeoutMs: 2000},
		APIKey: "secret", State: map[string]any{},
	})
	if out.Verdict == nil || hits.Load() != 1 {
		t.Fatalf("%+v hits=%d", out, hits.Load())
	}
	// Custom without any key: no credential (no keyOptional).
	t.Setenv("CUSTOM_BRAIN_KEY", "")
	none := c.Ask(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "custom", BaseURL: srv.URL, TimeoutMs: 2000}, State: map[string]any{}})
	if none.Failure == nil || none.Failure.Error != "no credential" {
		t.Fatalf("%+v", none.Failure)
	}
}

func TestCredentialStoreKeyResolution(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"model": map[string]any{"choice": "plan"}}})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Credentials: func(name string) string {
		if name == "brain:custom" {
			return "stored-key"
		}
		return ""
	}}
	out := c.Ask(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "custom", BaseURL: srv.URL, TimeoutMs: 2000}, State: map[string]any{}})
	if out.Verdict == nil || gotAuth != "Bearer stored-key" {
		t.Fatalf("%+v auth=%q", out, gotAuth)
	}
}

func TestOneRetryOn5xxThenFailureAndTimeout(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	srv, hits := verdictServer(t, &status)
	c := &Client{HTTP: srv.Client(), Sleep: func(time.Duration) {}}
	in := Input{Brain: config.BrainConfig{Channel: "custom", BaseURL: srv.URL, TimeoutMs: 2000}, APIKey: "k", State: map[string]any{}}
	out := c.Ask(context.Background(), in)
	if out.Failure == nil || out.Failure.Status != 503 || hits.Load() != 2 {
		t.Fatalf("want exactly one retry (2 hits): %+v hits=%d", out.Failure, hits.Load())
	}
	// 4xx other than 429 is not retried.
	status.Store(402)
	hits.Store(0)
	out = c.Ask(context.Background(), in)
	if out.Failure == nil || out.Failure.Status != 402 || hits.Load() != 1 {
		t.Fatalf("402 must not retry: %+v hits=%d", out.Failure, hits.Load())
	}
	// A hung brain fails at TimeoutMs, not later.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	started := time.Now()
	out = c.Ask(context.Background(), Input{
		Brain:  config.BrainConfig{Channel: "custom", BaseURL: slow.URL, TimeoutMs: 150},
		APIKey: "k", State: map[string]any{}})
	if out.Failure == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout not honored: %v %+v", time.Since(started), out.Failure)
	}
}

func TestBreakerCooldownExpiresAndFailureIsolation(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := &Client{Now: func() time.Time { return now }}
	for i := 0; i < BreakerThreshold; i++ {
		c.RecordOutcome(false)
	}
	if !c.BreakerOpen() {
		t.Fatal("open after threshold")
	}
	now = now.Add(BreakerCooldown - time.Second)
	if !c.BreakerOpen() {
		t.Fatal("still open inside cooldown")
	}
	now = now.Add(2 * time.Second)
	if c.BreakerOpen() {
		t.Fatal("closed after cooldown")
	}
	// Two failures then a success never open it.
	c.RecordOutcome(false)
	c.RecordOutcome(false)
	c.RecordOutcome(true)
	c.RecordOutcome(false)
	c.RecordOutcome(false)
	if c.BreakerOpen() {
		t.Fatal("success must reset the consecutive count")
	}
}

func TestKevDefaultMinConfidenceAndPlaceholderHeader(t *testing.T) {
	if FindChannel("kev").DefaultMinConfidence != 0.4 {
		t.Fatal("kev default minConfidence")
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"model": map[string]any{
			"choice": "plan", "confidence": 0.01, "probabilities": map[string]any{"plan": 0.7, "execute": 0.3}}}})
	}))
	defer srv.Close()
	t.Setenv("KEV_API_KEY", "")
	c := &Client{HTTP: srv.Client()}
	out := c.Ask(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "kev", BaseURL: srv.URL, TimeoutMs: 2000}, State: map[string]any{}})
	if out.Verdict == nil || out.Verdict.Confidence != 0.7 || gotAuth != "Bearer local" {
		t.Fatalf("%+v auth=%q", out, gotAuth)
	}
	_ = strings.TrimSpace
}
