package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/guard"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/wire"
)

// fbUpstream is a Freebuff server: sessions, runs, and a chat endpoint whose
// behaviour each test scripts.
type fbUpstream struct {
	mu       sync.Mutex
	sessions atomic.Int32
	chats    atomic.Int32
	chatFn   func(n int, w http.ResponseWriter, r *http.Request, body map[string]any)
	bodies   []map[string]any
	headers  []http.Header
}

const sseOK = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

func (f *fbUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/freebuff/session":
		f.sessions.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "active", "instanceId": r.Header.Get("x-freebuff-instance-id"),
			"model": r.Header.Get("x-freebuff-model"), "remainingMs": 3_600_000,
		})
	case "/api/v1/agent-runs":
		json.NewEncoder(w).Encode(map[string]any{"runId": "run-" + time.Now().Format("150405.000000")})
	case "/api/v1/chat/completions":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		n := int(f.chats.Add(1))
		if f.chatFn != nil {
			f.chatFn(n, w, r, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	default:
		http.NotFound(w, r)
	}
}

func fbRunner(t *testing.T, up *fbUpstream, tracker *quota.Tracker) (*Runner, config.Provider, *guard.Guard) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cred := filepath.Join(t.TempDir(), "freebuff.json")
	if err := os.WriteFile(cred, []byte(`{"authToken":"fb-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_FREEBUFF_AUTH", cred)
	cfg, err := config.ParseConfig(map[string]any{"providers": []any{map[string]any{
		"name": "freebuff-subscription", "type": "openai", "baseUrl": srv.URL,
		"auth": "oauth", "oauthSource": "freebuff", "billing": "subscription",
		"models": []any{"deepseek-v4-flash"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	g := guard.New(guard.Options{Concurrency: 8})
	r := NewRunner(ForwardDeps{
		HTTP: srv.Client(), Guard: g, Quota: tracker, Auth: &oauth.Resolver{HTTP: srv.Client()},
		Sleep: func(time.Duration) {},
	})
	return r, cfg.Providers[0], g
}

func fbTry(r *Runner, p config.Provider, stream bool) Attempt {
	return r.Try(context.Background(), AttemptRequest{
		ClientKind: KindOpenAI,
		Stream:     stream,
		ClientBody: wire.Body{
			"model":    "jevonian/auto",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
	}, PlanEntry{Provider: p, Model: "deepseek-v4-flash"})
}

func TestFreebuffTurnSendsTheCLIEnvelope(t *testing.T) {
	up := &fbUpstream{}
	r, p, _ := fbRunner(t, up, nil)
	at := fbTry(r, p, true)
	if at.Outcome.Kind != OutcomeSuccess || at.Response == nil {
		t.Fatalf("outcome %+v err=%v text=%q", at.Outcome, at.Err, at.Text)
	}
	got, _ := io.ReadAll(at.Response.Body)
	at.Response.Body.Close()
	if !strings.Contains(string(got), `"content":"hel"`) {
		t.Fatalf("stream not forwarded: %q", got)
	}

	body := up.bodies[0]
	if body["model"] != "deepseek/deepseek-v4-flash" || body["stream"] != true {
		t.Fatalf("model/stream: %v / %v", body["model"], body["stream"])
	}
	msgs := body["messages"].([]any)
	if first := msgs[0].(map[string]any); first["role"] != "system" ||
		!strings.HasPrefix(first["content"].(string), "You are Buffy, the strategic coding assistant.") {
		t.Fatalf("first message: %v", msgs[0])
	}
	meta := body["codebuff_metadata"].(map[string]any)
	if meta["cost_mode"] != "free" || meta["run_id"] == "" || meta["freebuff_instance_id"] == "" || len(meta["client_id"].(string)) != 13 {
		t.Fatalf("metadata: %v", meta)
	}
	if body["provider"].(map[string]any)["data_collection"] != "deny" {
		t.Fatalf("provider: %v", body["provider"])
	}
	h := up.headers[0]
	if h.Get("Authorization") != "Bearer fb-secret" || h.Get("x-freebuff-model") != "deepseek/deepseek-v4-flash" ||
		h.Get("x-freebuff-instance-id") != meta["freebuff_instance_id"] {
		t.Fatalf("headers: %v", h)
	}
}

func TestFreebuffNonStreamClientStillStreamsUpstream(t *testing.T) {
	up := &fbUpstream{}
	r, p, _ := fbRunner(t, up, nil)
	at := fbTry(r, p, false) // client wants JSON; upstream is stream-only
	if at.Outcome.Kind != OutcomeSuccess {
		t.Fatalf("outcome %+v err=%v", at.Outcome, at.Err)
	}
	if !at.Stream {
		t.Fatal("attempt must stream upstream so the server can fold it")
	}
	at.Response.Body.Close()
	if up.bodies[0]["stream"] != true {
		t.Fatalf("upstream stream = %v", up.bodies[0]["stream"])
	}
}

func TestFreebuffReusesSessionAcrossTurns(t *testing.T) {
	up := &fbUpstream{}
	r, p, _ := fbRunner(t, up, nil)
	for i := 0; i < 3; i++ {
		at := fbTry(r, p, true)
		if at.Outcome.Kind != OutcomeSuccess {
			t.Fatalf("turn %d: %+v", i, at.Outcome)
		}
		io.Copy(io.Discard, at.Response.Body)
		at.Response.Body.Close()
	}
	if got := up.sessions.Load(); got != 1 {
		t.Fatalf("created %d sessions for 3 turns", got)
	}
}

func TestFreebuffStaleSessionIsRetriedOnce(t *testing.T) {
	up := &fbUpstream{chatFn: func(n int, w http.ResponseWriter, r *http.Request, body map[string]any) {
		if n == 1 {
			w.WriteHeader(http.StatusPreconditionRequired) // 428
			io.WriteString(w, `{"error":"waiting_room_required"}`)
			return
		}
		io.WriteString(w, sseOK)
	}}
	r, p, _ := fbRunner(t, up, nil)
	at := fbTry(r, p, true)
	if at.Outcome.Kind != OutcomeSuccess {
		t.Fatalf("outcome %+v status=%d text=%q", at.Outcome, at.Status, at.Text)
	}
	at.Response.Body.Close()
	if up.sessions.Load() != 2 || up.chats.Load() != 2 {
		t.Fatalf("sessions=%d chats=%d: want a fresh session and one retry", up.sessions.Load(), up.chats.Load())
	}
	if at.Retries < 1 {
		t.Fatalf("retry not counted: %d", at.Retries)
	}
}

func TestFreebuffPersistentStaleSessionFailsOver(t *testing.T) {
	up := &fbUpstream{chatFn: func(n int, w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"session_superseded"}`)
	}}
	r, p, _ := fbRunner(t, up, nil)
	at := fbTry(r, p, true)
	if at.Outcome.Kind != OutcomeProviderRefusal || !at.Outcome.Failoverable() {
		t.Fatalf("outcome %+v", at.Outcome)
	}
	if up.chats.Load() != 2 {
		t.Fatalf("chats = %d, want exactly one retry", up.chats.Load())
	}
}

func TestFreebuffRateLimitBenchesUntilReset(t *testing.T) {
	reset := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	up := &fbUpstream{chatFn: func(n int, w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"status":"rate_limited","resetAt":"`+reset.Format(time.RFC3339)+`"}`)
	}}
	tracker := quota.New(nil)
	r, p, _ := fbRunner(t, up, tracker)
	at := fbTry(r, p, true)
	if at.Outcome.Kind != OutcomeQuotaRefusal || !at.Outcome.Failoverable() {
		t.Fatalf("outcome %+v", at.Outcome)
	}
	if at.Outcome.Resets != reset.UnixMilli() {
		t.Fatalf("reset = %d want %d", at.Outcome.Resets, reset.UnixMilli())
	}
	health := tracker.ProviderHealth(p, quota.HealthOptions{})
	if health.Status != quota.StatusExhausted {
		t.Fatalf("provider not benched: %+v", health)
	}
	if got, err := time.Parse(time.RFC3339Nano, health.ResetsAt); err != nil || !got.Equal(reset) {
		t.Fatalf("benched until %q, want the stated reset %v", health.ResetsAt, reset)
	}
	if up.chats.Load() != 1 {
		t.Fatalf("a 429 must not be retried: %d chats", up.chats.Load())
	}
}

func TestFreebuffAuthRejectionIsAProviderRefusal(t *testing.T) {
	up := &fbUpstream{chatFn: func(n int, w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid token"}`)
	}}
	r, p, _ := fbRunner(t, up, nil)
	at := fbTry(r, p, true)
	if at.Outcome.Kind != OutcomeProviderRefusal || at.Status != 401 {
		t.Fatalf("outcome %+v status=%d", at.Outcome, at.Status)
	}
}

func TestFreebuffMissingCredentialFailsOver(t *testing.T) {
	up := &fbUpstream{}
	r, p, _ := fbRunner(t, up, nil)
	t.Setenv("JEVONIAN_FREEBUFF_AUTH", filepath.Join(t.TempDir(), "missing.json"))
	at := fbTry(r, p, true)
	if at.Outcome.Kind != OutcomeProviderRefusal || at.Err == nil {
		t.Fatalf("outcome %+v err=%v", at.Outcome, at.Err)
	}
	if up.sessions.Load() != 0 {
		t.Fatal("no session may be taken without a credential")
	}
}

func TestFreebuffConcurrencyIsCapped(t *testing.T) {
	release := make(chan struct{})
	up := &fbUpstream{chatFn: func(n int, w http.ResponseWriter, r *http.Request, body map[string]any) {
		<-release
		io.WriteString(w, sseOK)
	}}
	r, p, g := fbRunner(t, up, nil)
	// If the cap regresses, all four turns block on release; free them at
	// cleanup so the test fails with a message instead of hanging.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var wg sync.WaitGroup
	results := make(chan Attempt, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- fbTry(r, p, true)
		}()
	}
	// Wait until the cap is reached and the overflow turns have been refused.
	deadline := time.Now().Add(5 * time.Second)
	for up.chats.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := g.Snapshot(p.Name).InFlight; got != 2 {
		unblock()
		wg.Wait()
		t.Fatalf("in flight = %d, want the cap of 2", got)
	}
	unblock()
	wg.Wait()
	close(results)
	blocked, ok := 0, 0
	for at := range results {
		switch {
		case at.Blocked == guard.BlockSaturated:
			blocked++
		case at.Outcome.Kind == OutcomeSuccess:
			ok++
			at.Response.Body.Close()
		}
	}
	if blocked != 2 || ok != 2 {
		t.Fatalf("blocked=%d ok=%d, want 2 and 2", blocked, ok)
	}
}
