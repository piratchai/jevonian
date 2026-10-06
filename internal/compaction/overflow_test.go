package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/brain"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/routing"
)

// Port of the compactForOverflow behaviour in src/upstream.ts: brain-guided
// deletion, failover across brain channels, the 5% reduction guard, the 10%
// outgoing-size guard and the "no brain" refusal.

func bigBody(results int) map[string]any {
	msgs := []any{map[string]any{"role": "user", "content": "Keep this prose verbatim: fix the bug"}}
	for i := 0; i < results; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "Read", "arguments": fmt.Sprintf(`{"file_path":"src/%d.ts"}`, i)}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat("old output line\n", 400)})
	}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("recent %d", i)})
	}
	return map[string]any{"model": "auto", "messages": msgs}
}

func brainServer(t *testing.T, noul float64, hits *atomic.Int32) *httptest.Server {
	return brainServerBy(t, func(string) float64 { return noul }, hits)
}

func brainServerBy(t *testing.T, noul func(string) float64, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req struct{ Questions map[string]any }
		_ = json.Unmarshal(raw, &req)
		answers := map[string]any{}
		for name := range req.Questions {
			answers[name] = map[string]any{"noul": noul(name)}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev", "answers": answers})
	}))
}

func asker(urls ...string) (BrainAsker, int) {
	bs := []config.BrainConfig{}
	for _, u := range urls {
		bs = append(bs, config.BrainConfig{Channel: "typesafe", BaseURL: u, APIKeyEnv: "COMPACTION_TEST_KEY", TimeoutMs: 2000})
	}
	return BrainAsker{Client: &brain.Client{Sleep: func(time.Duration) {}}, Brains: bs}, len(bs)
}

func TestCompactForOverflowDeletesStaleToolResults(t *testing.T) {
	t.Setenv("COMPACTION_TEST_KEY", "k")
	var hits atomic.Int32
	// Keep the call (it is cheap to re-run), drop the bulky result.
	srv := brainServerBy(t, func(q string) float64 {
		if strings.HasPrefix(q, "call_") {
			return 0.9
		}
		return 0.05
	}, &hits)
	defer srv.Close()
	a, n := asker(srv.URL)
	body := bigBody(12)
	res := CompactForOverflow(context.Background(), n, a, body)
	if !res.OK {
		t.Fatalf("not ok: %s", res.Error)
	}
	if hits.Load() == 0 || res.Stats.ResultsDropped == 0 {
		t.Fatalf("hits=%d stats=%+v", hits.Load(), res.Stats)
	}
	if routing.CompactionEstimate(res.Body) >= routing.CompactionEstimate(body) {
		t.Fatal("body not smaller")
	}
	msgs := res.Body["messages"].([]any)
	if first := msgs[0].(map[string]any); first["content"] != "Keep this prose verbatim: fix the bug" {
		t.Fatalf("prose changed: %v", first)
	}
	if last := msgs[len(msgs)-1].(map[string]any); last["content"] != "recent 3" {
		t.Fatalf("recent prose changed: %v", last)
	}
	if res.Body["model"] != "auto" {
		t.Fatal("non-message fields must be preserved")
	}
	// Dropped results are truncated with a note, never silently emptied.
	found := false
	for _, m := range msgs {
		if c, _ := m.(map[string]any)["content"].(string); strings.Contains(c, "jevonian truncated") {
			found = true
		}
	}
	if !found {
		t.Fatal("no truncation note in rewritten body")
	}
}

func TestCompactForOverflowDropsWholeStaleCalls(t *testing.T) {
	t.Setenv("COMPACTION_TEST_KEY", "k")
	var hits atomic.Int32
	srv := brainServer(t, 0.05, &hits)
	defer srv.Close()
	a, n := asker(srv.URL)
	res := CompactForOverflow(context.Background(), n, a, bigBody(12))
	if !res.OK || res.Stats.CallsDropped != 12 {
		t.Fatalf("res = %+v", res)
	}
	if got := len(res.Body["messages"].([]any)); got != 5 {
		t.Fatalf("messages = %d, want the 5 prose messages", got)
	}
}

func TestCompactForOverflowKeepsWhenBrainWantsEverything(t *testing.T) {
	t.Setenv("COMPACTION_TEST_KEY", "k")
	var hits atomic.Int32
	srv := brainServer(t, 0.99, &hits)
	defer srv.Close()
	a, n := asker(srv.URL)
	res := CompactForOverflow(context.Background(), n, a, bigBody(12))
	if res.OK || !strings.Contains(res.Error, "only reduced the history by 0%") {
		t.Fatalf("res = %+v", res)
	}
}

func TestCompactForOverflowFailsOverAcrossBrains(t *testing.T) {
	t.Setenv("COMPACTION_TEST_KEY", "k")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 403) }))
	defer bad.Close()
	var hits atomic.Int32
	good := brainServer(t, 0.05, &hits)
	defer good.Close()
	a, n := asker(bad.URL, good.URL)
	if res := CompactForOverflow(context.Background(), n, a, bigBody(12)); !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if hits.Load() == 0 {
		t.Fatal("second brain never asked")
	}
}

func TestCompactForOverflowErrorsInsteadOfGuessing(t *testing.T) {
	t.Setenv("COMPACTION_TEST_KEY", "k")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	a, n := asker(bad.URL)
	body := bigBody(12)
	res := CompactForOverflow(context.Background(), n, a, body)
	if res.OK || res.Body != nil || !strings.Contains(res.Error, "Jev request failed") {
		t.Fatalf("a brain failure must not rewrite history: %+v", res)
	}
}

func TestCompactForOverflowGuards(t *testing.T) {
	if r := CompactForOverflow(context.Background(), 0, nil, bigBody(3)); r.OK || r.Error != "no Jev brain is configured" {
		t.Fatalf("no brain: %+v", r)
	}
	if r := CompactForOverflow(context.Background(), 1, nil, map[string]any{"model": "m"}); r.OK || r.Error != "the request has no messages to compact" {
		t.Fatalf("no messages: %+v", r)
	}
}
