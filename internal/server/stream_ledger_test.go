package server_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/server"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// compressBody encodes raw with the named HTTP content-coding so the request
// path exercises wire.DecodeBody through the live handler.
func compressBody(t *testing.T, enc string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch enc {
	case "gzip":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(raw)
		_ = w.Close()
	case "deflate":
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(raw)
		_ = w.Close()
	case "zstd":
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(raw)
		_ = w.Close()
	case "br":
		w := brotli.NewWriter(&buf)
		_, _ = w.Write(raw)
		_ = w.Close()
	default:
		t.Fatalf("unknown encoding %q", enc)
	}
	return buf.Bytes()
}

// TestChatDecodesCompressedRequestBodies covers Codex's zstd bodies plus the
// gzip/deflate/br codings: the handler must decode before JSON parsing, and an
// unknown coding must be a 400, never garbage forwarded upstream.
func TestChatDecodesCompressedRequestBodies(t *testing.T) {
	for _, enc := range []string{"gzip", "deflate", "zstd", "br"} {
		t.Run(enc, func(t *testing.T) {
			up := &recordingUpstream{}
			upSrv := up.serve(t, okResponder)
			cfg := baseCfg(upSrv.URL, "m")
			h := server.New("", server.Deps{Config: &cfg}).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				bytes.NewReader(compressBody(t, enc, []byte(chatBody))))
			req.Header.Set("Content-Encoding", enc)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Fatalf("%s: status %d body %s", enc, rr.Code, rr.Body.String())
			}
			if up.hits() != 1 {
				t.Fatalf("%s: upstream hits = %d", enc, up.hits())
			}
			if got := up.body(0)["model"]; got != "m" {
				t.Fatalf("%s: upstream model = %v", enc, got)
			}
		})
	}

	// Unknown coding: reject before routing.
	cfg := baseCfg("http://127.0.0.1:9", "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Content-Encoding", "snappy")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "Failed to read body") {
		t.Fatalf("unknown encoding: %d %s", rr.Code, rr.Body.String())
	}
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// TestLedgerRowStreamFailoverBillingAndSavedTokens drives a streaming turn that
// fails over, compresses a prior tool result, and asserts the row carries the
// TS record(...) fields: billing, savedTokens, ttftMs, tries and failovers.
func TestLedgerRowStreamFailoverBillingAndSavedTokens(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "")

	script := filepath.Join(t.TempDir(), "rtk")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null; printf 'brief\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "boom")
	}))
	defer flaky.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer healthy.Close()

	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ExtendSchema(); err != nil {
		t.Fatal(err)
	}

	zero := 0
	cfg := config.Config{
		DefaultProvider: "flaky",
		Routing:         config.DefaultRouting(),
		TokenSaver:      config.TokenSaverConfig{Enabled: true, Command: script, TimeoutMs: 2000},
		Providers: []config.Provider{
			{Name: "flaky", Type: config.ProviderTypeOpenAI, BaseURL: flaky.URL + "/v1",
				APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "m"}}},
			{Name: "healthy", Type: config.ProviderTypeOpenAI, BaseURL: healthy.URL + "/v1",
				APIKey: "k", Auth: config.AuthAPIKey, Billing: config.BillingSubscription,
				Models: []config.ModelEntry{{ID: "m"}}},
		},
	}
	h := server.New("", server.Deps{
		Config: &cfg, Ledger: db, SameHostRetries: &zero, Sleep: func(time.Duration) {},
	}).Handler()

	body := map[string]any{
		"model": "m", "stream": true,
		"messages": []any{
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": `{"command":"go test ./..."}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": strings.Repeat("old output line\n", 40)},
			map[string]any{"role": "user", "content": "continue"},
		},
	}
	rr := postJSON(t, h, "/v1/chat/completions", body, nil)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("x-jevonian-provider"); got != "healthy" {
		t.Fatalf("provider = %q (headers %v)", got, rr.Header())
	}
	reqID := rr.Header().Get("x-jevonian-request-id")

	logs, err := admin.OpenSQLiteLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	rec, _, err := logs.LogDetail(context.Background(), reqID)
	if err != nil || rec == nil {
		t.Fatalf("no ledger row for %q: %v", reqID, err)
	}
	if rec["billing"] != "subscription" {
		t.Fatalf("billing = %v", rec["billing"])
	}
	if n := intOf(rec["savedTokens"]); n <= 0 {
		t.Fatalf("savedTokens = %v", rec["savedTokens"])
	}
	if rec["ttftMs"] == nil {
		t.Fatalf("ttftMs missing: %v", rec)
	}
	tries, _ := rec["tries"].([]any)
	if len(tries) < 2 {
		t.Fatalf("tries = %v", rec["tries"])
	}
	if n := intOf(rec["failovers"]); n < 1 {
		t.Fatalf("failovers = %v", rec["failovers"])
	}
}

// TestLedgerRowBrainChannelCacheAndSkipped drives a brain-routed turn whose
// effort floor withholds the shallow model, and asserts the row carries
// brainChannel, cache, cacheKeep and skipped.
func TestLedgerRowBrainChannelCacheAndSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "")

	up := &recordingUpstream{}
	upSrv := up.serve(t, okResponder)

	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ExtendSchema(); err != nil {
		t.Fatal(err)
	}

	cfg := baseCfg(upSrv.URL, "m", "n")
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "X", TimeoutMs: 1000, MinConfidence: 0.6}}
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{
		"m": {Efforts: []string{"low"}},
		"n": {Efforts: []string{"low", "high"}},
	}
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		return routing.AskResult{Choice: &routing.Choice{Model: "plan", Confidence: 0.9}}
	})
	h := server.New("", server.Deps{
		Config: &cfg, Ledger: db,
		Routing: routing.Deps{Scorer: scorer, Prices: priceTable},
	}).Handler()

	body := map[string]any{
		"model":    "jevonian/auto",
		"messages": []any{map[string]any{"role": "user", "content": "build a feature"}},
	}
	rr := postJSON(t, h, "/v1/chat/completions", body, map[string]string{
		"x-jevonian-session": "s-ledger", "x-jevonian-effort": "high",
	})
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	reqID := rr.Header().Get("x-jevonian-request-id")

	logs, err := admin.OpenSQLiteLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	rec, _, err := logs.LogDetail(context.Background(), reqID)
	if err != nil || rec == nil {
		t.Fatalf("no ledger row for %q: %v", reqID, err)
	}
	if rec["brainChannel"] != "typesafe" {
		t.Fatalf("brainChannel = %v", rec["brainChannel"])
	}
	if rec["cacheKeep"] == nil || rec["cacheKeep"] == "" {
		t.Fatalf("cacheKeep = %v", rec["cacheKeep"])
	}
	if rec["cache"] == nil {
		t.Fatalf("cache missing: %v", rec)
	}
	skipped, _ := rec["skipped"].([]any)
	if len(skipped) == 0 {
		t.Fatalf("skipped = %v", rec["skipped"])
	}
}

// TestRestartAfterDrainRunsRestartWhenIdle pins the default 60s ceiling and the
// idle path: no in-flight work means restart runs immediately.
func TestRestartAfterDrainRunsRestartWhenIdle(t *testing.T) {
	if server.DefaultDrainTimeout != 60*time.Second {
		t.Fatalf("DefaultDrainTimeout = %v, want 60s", server.DefaultDrainTimeout)
	}
	srv := server.New("", server.Deps{Config: &config.Config{Routing: config.DefaultRouting()}})
	restarted := false
	if err := srv.RestartAfterDrain(context.Background(), func(context.Context) error {
		restarted = true
		return nil
	}); err != nil {
		t.Fatalf("RestartAfterDrain: %v", err)
	}
	if !restarted || !srv.Draining() {
		t.Fatalf("restarted=%v draining=%v", restarted, srv.Draining())
	}
}

// TestDrainCancelsHungStreams holds an upstream stream open, then drains with a
// short deadline: the in-flight request must be cancelled and released before
// restart, matching ServerLifecycle.restartAfterDrain in src/lifecycle.ts.
func TestDrainCancelsHungStreams(t *testing.T) {
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hung.Close()
	// Teardown must not block even if the egress client keeps the socket open.
	defer close(release)

	zero := 0
	cfg := config.Config{
		DefaultProvider: "p1",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{{
			Name: "p1", Type: config.ProviderTypeOpenAI, BaseURL: hung.URL + "/v1",
			APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "m"}},
		}},
	}
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &cfg, Client: http.DefaultClient, SameHostRetries: &zero, Sleep: func(time.Duration) {},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			bytes.NewReader(mustJSON(t, map[string]any{
				"model":    "m",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-hit:
	case <-time.After(3 * time.Second):
		t.Fatal("hung upstream was never contacted")
	}
	if srv.ActiveRequests() != 1 {
		t.Fatalf("active = %d", srv.ActiveRequests())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	restarted := false
	err := srv.RestartAfterDrain(ctx, func(context.Context) error {
		restarted = true
		return nil
	})
	if err == nil {
		t.Fatal("RestartAfterDrain returned nil despite a hung stream")
	}
	if restarted {
		t.Fatal("restart ran before the hung stream was cancelled")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled request did not return")
	}
	deadline := time.Now().Add(2 * time.Second)
	for srv.ActiveRequests() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ActiveRequests() != 0 {
		t.Fatalf("active = %d after drain", srv.ActiveRequests())
	}
	if !srv.Draining() {
		t.Fatal("server is not draining")
	}
}
