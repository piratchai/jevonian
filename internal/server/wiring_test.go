package server_test

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/xinyao27/jevonian/internal/brain"
	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/server"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// TestMain keeps every capture (request bodies, brain bodies) out of the real
// data directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "jevonian-server-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("JEVONIAN_DATA_DIR", dir)
	os.Setenv("WIRING_TEST_BRAIN_KEY", "k")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Tests for the routing/compaction wiring: proactive compaction, retry after
// a provider's context rejection, Codex remote compaction, brain body capture
// and catalog capabilities (src/upstream.ts, src/routing.ts).

const oldOutput = "old output line\n"

// bigConversation is a tool-heavy chat whose stale tool results the brain can
// drop. The last four user messages are the recent prose that must survive.
func bigConversation(model string, results int) map[string]any {
	msgs := []any{map[string]any{"role": "user", "content": "Keep this prose verbatim: fix the bug"}}
	for i := 0; i < results; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "Read", "arguments": fmt.Sprintf(`{"file_path":"src/%d.ts"}`, i)}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat(oldOutput, 400)})
	}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("recent %d", i)})
	}
	return map[string]any{"model": model, "messages": msgs}
}

// fakeBrain answers compaction questions: every question gets `noul`.
func fakeBrain(t *testing.T, noul float64, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req struct{ Questions map[string]any }
		_ = json.Unmarshal(raw, &req)
		answers := map[string]any{}
		for name := range req.Questions {
			answers[name] = map[string]any{"noul": noul}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingUpstream is an OpenAI-shaped host that records every request body.
type recordingUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (u *recordingUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func (u *recordingUpstream) body(i int) map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[i]
}

func (u *recordingUpstream) serve(t *testing.T, respond func(n int, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		n := len(u.bodies)
		u.mu.Unlock()
		respond(n, w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okResponder(_ int, w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	_, _ = io.WriteString(w, chatOK)
}

func overflowResponder(w http.ResponseWriter) {
	w.WriteHeader(400)
	_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens."}}`)
}

func bodyText(m map[string]any) string {
	raw, _ := json.Marshal(m)
	return string(raw)
}

func brainsFor(url string) []config.BrainConfig {
	return []config.BrainConfig{{Channel: "typesafe", BaseURL: url, APIKeyEnv: "WIRING_TEST_BRAIN_KEY", TimeoutMs: 2000, MinConfidence: 0.6}}
}

func postJSON(t *testing.T, h http.Handler, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return doReq(t, h, "POST", path, string(raw), "127.0.0.1:1", hdr)
}

func noSleep(time.Duration) {}

// ---- 1. proactive compaction ---------------------------------------------

func TestProactiveCompactionOnContextOverflowThenForward(t *testing.T) {
	var brainHits atomic.Int32
	brainSrv := fakeBrain(t, 0.05, &brainHits)
	up := &recordingUpstream{}
	upSrv := up.serve(t, okResponder)

	cfg := baseCfg(upSrv.URL, "m")
	cfg.Routing.Brains = brainsFor(brainSrv.URL)
	// m's window cannot hold the conversation, so the whole routing overflows.
	window := 3000
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{"m": {ContextWindow: &window}}
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		return routing.AskResult{Choice: &routing.Choice{Model: "execute", Confidence: 0.9}}
	})
	h := server.New("", server.Deps{
		Config: &cfg, Routing: routing.Deps{Scorer: scorer, Prices: priceTable},
		Brain: &brain.Client{Sleep: noSleep}, Sleep: noSleep,
	}).Handler()

	orig := bigConversation("jevonian/auto", 12)
	if est := routing.CompactionEstimate(orig); est < 10000 {
		t.Fatalf("test conversation too small: %d tokens", est)
	}
	rr := postJSON(t, h, "/v1/chat/completions", orig, map[string]string{"x-jevonian-session": "overflow-1"})
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if brainHits.Load() == 0 {
		t.Fatal("compaction never asked the brain")
	}
	if up.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits())
	}
	sent := up.body(0)
	if strings.Contains(bodyText(sent), "old output line") {
		t.Fatal("stale tool results reached the provider")
	}
	msgs, _ := sent["messages"].([]any)
	if len(msgs) == 0 || msgs[0].(map[string]any)["content"] != "Keep this prose verbatim: fix the bug" {
		t.Fatalf("prose changed: %v", msgs)
	}
	// Proactive compaction is not a retry: no context-retry suffix.
	if reason := rr.Header().Get("x-jevonian-reason"); strings.Contains(reason, "context-retry") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestProactiveCompactionFailureStillForwardsOriginal(t *testing.T) {
	// No brain can answer: the original body goes to the provider, which makes
	// the final call (the estimate may be a false positive).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	up := &recordingUpstream{}
	upSrv := up.serve(t, okResponder)
	cfg := baseCfg(upSrv.URL, "m")
	cfg.Routing.Brains = brainsFor(bad.URL)
	window := 3000
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{"m": {ContextWindow: &window}}
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		return routing.AskResult{Choice: &routing.Choice{Model: "execute", Confidence: 0.9}}
	})
	h := server.New("", server.Deps{
		Config: &cfg, Routing: routing.Deps{Scorer: scorer},
		Brain: &brain.Client{Sleep: noSleep}, Sleep: noSleep,
	}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", bigConversation("jevonian/auto", 12), nil)
	if rr.Code != 200 || up.hits() != 1 {
		t.Fatalf("status %d hits %d: %s", rr.Code, up.hits(), rr.Body.String())
	}
	if !strings.Contains(bodyText(up.body(0)), "old output line") {
		t.Fatal("a failed compaction must not rewrite history")
	}
}

// ---- 2. provider context rejection -> compact -> retry ---------------------

func TestProviderContextRejectionCompactsAndRetriesOnce(t *testing.T) {
	var brainHits atomic.Int32
	brainSrv := fakeBrain(t, 0.05, &brainHits)
	up := &recordingUpstream{}
	upSrv := up.serve(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			overflowResponder(w)
			return
		}
		okResponder(n, w)
	})
	cfg := baseCfg(upSrv.URL, "m")
	cfg.Routing.Brains = brainsFor(brainSrv.URL)
	zero := 0
	h := server.New("", server.Deps{
		Config: &cfg, Brain: &brain.Client{Sleep: noSleep}, SameHostRetries: &zero, Sleep: noSleep,
	}).Handler()

	rr := postJSON(t, h, "/v1/chat/completions", bigConversation("m", 12), map[string]string{"x-jevonian-session": "ctx-retry"})
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (reject, then retry)", up.hits())
	}
	if !strings.Contains(bodyText(up.body(0)), "old output line") {
		t.Fatal("first attempt should carry the full history")
	}
	if strings.Contains(bodyText(up.body(1)), "old output line") {
		t.Fatal("retry should carry the compacted history")
	}
	if reason := rr.Header().Get("x-jevonian-reason"); reason != "pinned-model:context-retry" {
		t.Fatalf("reason = %q", reason)
	}
	if brainHits.Load() == 0 {
		t.Fatal("brain never asked")
	}
}

func TestProviderContextRejectionRetriesAtMostOnce(t *testing.T) {
	var brainHits atomic.Int32
	brainSrv := fakeBrain(t, 0.05, &brainHits)
	up := &recordingUpstream{}
	upSrv := up.serve(t, func(_ int, w http.ResponseWriter) { overflowResponder(w) })
	cfg := baseCfg(upSrv.URL, "m")
	cfg.Routing.Brains = brainsFor(brainSrv.URL)
	zero := 0
	h := server.New("", server.Deps{
		Config: &cfg, Brain: &brain.Client{Sleep: noSleep}, SameHostRetries: &zero, Sleep: noSleep,
	}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", bigConversation("m", 12), nil)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "context_length_exceeded") {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want exactly 2", up.hits())
	}
}

func TestProviderContextRejectionWithoutBrainPassesThrough(t *testing.T) {
	up := &recordingUpstream{}
	upSrv := up.serve(t, func(_ int, w http.ResponseWriter) { overflowResponder(w) })
	cfg := baseCfg(upSrv.URL, "m")
	zero := 0
	h := server.New("", server.Deps{Config: &cfg, SameHostRetries: &zero, Sleep: noSleep}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", bigConversation("m", 12), nil)
	if rr.Code != 400 || up.hits() != 1 {
		t.Fatalf("status %d hits %d", rr.Code, up.hits())
	}
}

func TestPlainClientErrorIsNotCompacted(t *testing.T) {
	var brainHits atomic.Int32
	brainSrv := fakeBrain(t, 0.05, &brainHits)
	up := &recordingUpstream{}
	upSrv := up.serve(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"bad parameter"}}`)
	})
	cfg := baseCfg(upSrv.URL, "m")
	cfg.Routing.Brains = brainsFor(brainSrv.URL)
	zero := 0
	h := server.New("", server.Deps{Config: &cfg, Brain: &brain.Client{Sleep: noSleep}, SameHostRetries: &zero, Sleep: noSleep}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", bigConversation("m", 12), nil)
	if rr.Code != 400 || up.hits() != 1 || brainHits.Load() != 0 {
		t.Fatalf("status %d hits %d brain %d", rr.Code, up.hits(), brainHits.Load())
	}
}

// ---- 3. remote compaction --------------------------------------------------

const compactionSSE = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"encrypted_content\":\"x\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

func remoteCompactionConfig(chatURL, otherURL string) config.Config {
	return config.Config{
		Listen:          config.ListenConfig{Host: "127.0.0.1", Port: 8787},
		DefaultProvider: "other",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{
			{Name: "other", Type: config.ProviderTypeOpenAI, BaseURL: otherURL + "/v1", APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "gpt-other"}}},
			{Name: "chatgpt", Type: config.ProviderTypeResponses, BaseURL: chatURL + "/v1", APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "gpt-codex"}}},
		},
	}
}

func TestRemoteCompactionPinsToResponsesProvider(t *testing.T) {
	var chatHits, otherHits atomic.Int32
	var chatBodies []map[string]any
	var mu sync.Mutex
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		chatBodies = append(chatBodies, b)
		mu.Unlock()
		chatHits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, compactionSSE)
	}))
	defer chat.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		okResponder(0, w)
	}))
	defer other.Close()
	cfg := remoteCompactionConfig(chat.URL, other.URL)
	scorerCalls := 0
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		scorerCalls++
		return routing.AskResult{Choice: &routing.Choice{Model: "plan", Confidence: 0.9}}
	})
	cfg.Routing.Brains = brainsFor("http://127.0.0.1:9")
	h := server.New("", server.Deps{Config: &cfg, Routing: routing.Deps{Scorer: scorer}, Sleep: noSleep}).Handler()

	// The client names a model another provider serves; compaction still pins to
	// the Responses provider and never consults the brain.
	body := map[string]any{
		"model": "jevonian/auto",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
			map[string]any{"type": "compaction_trigger"},
		},
	}
	rr := postJSON(t, h, "/v1/responses", body, nil)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("x-jevonian-provider") != "chatgpt" || rr.Header().Get("x-jevonian-model") != "gpt-codex" || rr.Header().Get("x-jevonian-reason") != "remote-compaction" {
		t.Fatalf("headers: %v", rr.Header())
	}
	if chatHits.Load() != 1 || otherHits.Load() != 0 || scorerCalls != 0 {
		t.Fatalf("chat=%d other=%d brain=%d", chatHits.Load(), otherHits.Load(), scorerCalls)
	}
	if !strings.Contains(rr.Body.String(), `"compaction"`) {
		t.Fatalf("compaction item lost: %s", rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if inputs, _ := chatBodies[0]["input"].([]any); len(inputs) != 2 {
		t.Fatalf("upstream input = %v", chatBodies[0]["input"])
	}
}

func TestRemoteCompactionWithoutResponsesProviderIs400(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	body := map[string]any{"model": "m", "input": []any{map[string]any{"type": "compaction_trigger"}}}
	rr := postJSON(t, h, "/v1/responses", body, nil)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "Remote compaction requires a ChatGPT subscription (Responses) provider") || hits != 0 {
		t.Fatalf("status %d hits %d: %s", rr.Code, hits, rr.Body.String())
	}
}

func TestRemoteCompactionDoesNotCompactOrFailOver(t *testing.T) {
	// A hard context rejection on a remote-compaction turn is the client's
	// answer: no compaction (it would corrupt the trigger) and no failover onto
	// a bridge.
	var chatHits, otherHits atomic.Int32
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chatHits.Add(1)
		overflowResponder(w)
	}))
	defer chat.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		okResponder(0, w)
	}))
	defer other.Close()
	cfg := remoteCompactionConfig(chat.URL, other.URL)
	var brainHits atomic.Int32
	brainSrv := fakeBrain(t, 0.05, &brainHits)
	cfg.Routing.Brains = brainsFor(brainSrv.URL)
	zero := 0
	h := server.New("", server.Deps{Config: &cfg, Brain: &brain.Client{Sleep: noSleep}, SameHostRetries: &zero, Sleep: noSleep}).Handler()
	body := map[string]any{"model": "gpt-codex", "input": []any{map[string]any{"type": "compaction_trigger"}}}
	rr := postJSON(t, h, "/v1/responses", body, nil)
	if rr.Code != 400 || chatHits.Load() != 1 || otherHits.Load() != 0 || brainHits.Load() != 0 {
		t.Fatalf("status %d chat=%d other=%d brain=%d", rr.Code, chatHits.Load(), otherHits.Load(), brainHits.Load())
	}
}

// ---- 3b. Responses host serving an OpenAI client --------------------------
//
// src/upstream.ts `translated = provider.type === "responses" && clientKind === "openai"`:
// a Responses host that answers a Chat Completions client must fold back to the
// chat shape, streamed and folded alike.

const responsesChatSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-codex\"}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"hello from responses\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from responses\"}]}],\"usage\":{\"input_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":40},\"output_tokens\":10}}}\n\n"

func responsesChatConfig(responsesURL string) config.Config {
	return config.Config{
		Listen:          config.ListenConfig{Host: "127.0.0.1", Port: 8787},
		DefaultProvider: "chatgpt",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{
			{Name: "chatgpt", Type: config.ProviderTypeResponses, BaseURL: responsesURL + "/v1", APIKey: "k", Auth: config.AuthAPIKey, Models: []config.ModelEntry{{ID: "gpt-codex"}}},
		},
	}
}

func TestResponsesHostServesOpenAIStreamClient(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, responsesChatSSE)
	}))
	defer up.Close()
	cfg := responsesChatConfig(up.URL)
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", map[string]any{
		"model": "gpt-codex", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.Contains(body, "hello from responses") {
		t.Fatalf("Responses stream not folded to chat chunks: %s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("raw Responses frames leaked to the chat client: %s", body)
	}
}

func TestResponsesHostServesOpenAINonStreamClient(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, responsesChatSSE)
	}))
	defer up.Close()
	cfg := responsesChatConfig(up.URL)
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", map[string]any{
		"model": "gpt-codex", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, "hello from responses") {
		t.Fatalf("Responses body not folded to a chat completion: %s", body)
	}
}

// ---- 4. brain call body capture -------------------------------------------

func TestBrainCallsAreCapturedAndLedgered(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "")
	up := &recordingUpstream{}
	upSrv := up.serve(t, okResponder)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := baseCfg(upSrv.URL, "m", "n")
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "X", TimeoutMs: 1000, MinConfidence: 0.6}}
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		return routing.AskResult{Choice: &routing.Choice{
			Model: "plan", Confidence: 0.91, Effort: "low", ModelName: "jev-1",
			Probabilities: map[string]float64{"plan": 0.91, "execute": 0.09},
			Usage:         &routing.Usage{Input: 120, Output: 7},
		}}
	})
	// No RecordBrainCall in Routing: the server supplies the default recorder.
	h := server.New("", server.Deps{Config: &cfg, Ledger: db, Routing: routing.Deps{Scorer: scorer, Prices: priceTable}}).Handler()
	rr := postJSON(t, h, "/v1/chat/completions", map[string]any{
		"model": "jevonian/auto", "messages": []any{map[string]any{"role": "user", "content": "plan the feature"}},
	}, nil)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	reqID := rr.Header().Get("x-jevonian-request-id")
	server.FlushBodies()

	logs, err := admin.OpenSQLiteLogs(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	_, calls, err := logs.LogDetail(context.Background(), reqID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("brain rows for %s = %d (%v)", reqID, len(calls), err)
	}
	row := calls[0]
	id, _ := row["id"].(string)
	if id == "" || id == reqID || row["kind"] != "brain" || row["provider"] != "brain:typesafe" || row["model"] != "jev-1" {
		t.Fatalf("brain row = %v", row)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bodies", id+".json"))
	if err != nil {
		t.Fatalf("brain body not captured under the row id: %v", err)
	}
	var got struct {
		Kind, Channel, Model string
		State                map[string]any
		Verdict              struct {
			Model         string
			Confidence    float64
			Effort        string
			Probabilities map[string]float64
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "brain" || got.Channel != "typesafe" || got.Model != "jev-1" || len(got.State) == 0 ||
		got.Verdict.Model != "plan" || got.Verdict.Confidence != 0.91 || got.Verdict.Effort != "low" || got.Verdict.Probabilities["plan"] != 0.91 {
		t.Fatalf("brain body = %s", raw)
	}
	if info, _ := os.Stat(filepath.Join(dir, "bodies", id+".json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	// The log detail endpoint joins the row with its body.
	detail := doReq(t, server.New("", server.Deps{Config: &cfg, Ledger: db, Admin: &admin.Deps{Ledger: logs}}).Handler(),
		"GET", "/api/logs/"+reqID, "", "127.0.0.1:1", nil)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `"brainCalls"`) || !strings.Contains(detail.Body.String(), `"channel":"typesafe"`) {
		t.Fatalf("detail %d: %s", detail.Code, detail.Body.String())
	}
}

// ---- 5. capabilities filter a too-small context window ---------------------

func TestCatalogCapabilitiesSkipTooSmallContextWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	snapshot := map[string]any{"capabilities": map[string]any{
		"small-model": map[string]any{"contextWindow": 1000},
		"big-model":   map[string]any{"contextWindow": 1_000_000},
	}}
	data, _ := json.Marshal(snapshot)
	if err := os.WriteFile(filepath.Join(dir, "pricing.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	up := &recordingUpstream{}
	upSrv := up.serve(t, okResponder)
	cfg := baseCfg(upSrv.URL, "small-model", "big-model")
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "X", TimeoutMs: 1000, MinConfidence: 0.6}}
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		return routing.AskResult{Choice: &routing.Choice{Model: "execute", Confidence: 0.9}}
	})
	h := server.New("", server.Deps{
		Config: &cfg,
		Routing: routing.Deps{
			Scorer: scorer, Prices: priceTable,
			Capabilities: catalogsync.Capabilities(), Identity: catalogsync.NewIdentity(),
		},
	}).Handler()
	body := map[string]any{
		"model":    "jevonian/auto",
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("some long context sentence. ", 400)}},
	}
	rr := postJSON(t, h, "/v1/chat/completions", body, nil)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("x-jevonian-model") != "big-model" {
		t.Fatalf("model = %q (headers %v)", rr.Header().Get("x-jevonian-model"), rr.Header())
	}
	skipped := rr.Header().Get("x-jevonian-skipped")
	if !strings.Contains(skipped, "p1/small-model=context(") {
		t.Fatalf("skipped = %q", skipped)
	}
	if got := up.body(0)["model"]; got != "big-model" {
		t.Fatalf("upstream model = %v", got)
	}

	// Without a capability source the same turn is never filtered.
	h2 := server.New("", server.Deps{Config: &cfg, Routing: routing.Deps{Scorer: scorer, Prices: priceTable}}).Handler()
	rr = postJSON(t, h2, "/v1/chat/completions", body, nil)
	if rr.Code != 200 || rr.Header().Get("x-jevonian-skipped") != "" {
		t.Fatalf("nil capabilities: %d skipped=%q", rr.Code, rr.Header().Get("x-jevonian-skipped"))
	}
}
