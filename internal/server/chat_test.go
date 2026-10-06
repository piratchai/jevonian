package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/guard"
	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/server"
)

// A transport failure on the pinned provider fails the turn over to the next
// candidate rather than returning a 502 — the original complaint this whole
// router exists to fix.
func TestChatCompletionsTransportFailover(t *testing.T) {
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijack")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer flaky.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"from-healthy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer healthy.Close()

	db := openLedger(t)
	defer db.Close()

	cfg := config.Config{
		DefaultProvider: "flaky",
		Providers: []config.Provider{
			{
				Name: "flaky", Type: config.ProviderTypeOpenAI, BaseURL: flaky.URL + "/v1",
				APIKey: "flaky-key", Auth: config.AuthAPIKey,
				Models: []config.ModelEntry{{ID: "shared-model"}},
			},
			{
				Name: "healthy", Type: config.ProviderTypeOpenAI, BaseURL: healthy.URL + "/v1",
				APIKey: "healthy-key", Auth: config.AuthAPIKey,
				Models: []config.ModelEntry{{ID: "shared-model"}},
			},
		},
	}

	zero := 0
	srv := server.New("127.0.0.1:0", server.Deps{
		Config:          &cfg,
		Ledger:          db,
		Client:          http.DefaultClient,
		SameHostRetries: &zero,
		Sleep:           func(time.Duration) {},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		bytes.NewReader(mustJSON(t, map[string]any{
			"model":    "shared-model",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if resp.Header.Get("x-jevonian-provider") != "healthy" {
		t.Fatalf("provider header = %q", resp.Header.Get("x-jevonian-provider"))
	}
	if got := resp.Header.Get("x-jevonian-reason"); !bytes.Contains([]byte(got), []byte("quota-failover")) {
		t.Fatalf("reason = %q", got)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Choices[0].Message.Content != "from-healthy" {
		t.Fatalf("content = %q", body.Choices[0].Message.Content)
	}
	n, err := db.Count()
	if err != nil || n != 1 {
		t.Fatalf("ledger count = %d err=%v", n, err)
	}
}

func TestChatCompletionsInvalidJSON(t *testing.T) {
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &config.Config{},
		Client: http.DefaultClient,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(`not-json`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// Open mode (empty keys.json): unauthenticated traffic is allowed and the
// ledger row carries the "unauthenticated" key identity, matching src/server.ts.
func TestChatCompletionsOpenMode(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	db := openLedger(t)
	defer db.Close()
	store := keys.Open(t.TempDir(), db) // empty dir → open mode

	cfg := config.Config{
		DefaultProvider: "p1",
		Providers: []config.Provider{{
			Name: "p1", Type: config.ProviderTypeOpenAI, BaseURL: up.URL + "/v1",
			APIKey: "k", Auth: config.AuthAPIKey,
			Models: []config.ModelEntry{{ID: "m"}},
		}},
	}
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &cfg, Ledger: db, Client: http.DefaultClient, Keys: store,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		bytes.NewReader(mustJSON(t, map[string]any{
			"model":    "m",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
}

// With keys present, a request without a token is a 401 naming the dashboard.
func TestChatCompletionsMissingKey(t *testing.T) {
	db := openLedger(t)
	defer db.Close()
	dir := t.TempDir()
	store := keys.Open(dir, db)
	if _, err := store.Create(keys.CreateOptions{Name: "test"}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Listen: config.ListenConfig{Port: 8787}}
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &cfg, Ledger: db, Client: http.DefaultClient, Keys: store,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		bytes.NewReader(mustJSON(t, map[string]any{
			"model":    "m",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 401 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q", body.Error.Type)
	}
	if !bytes.Contains(raw, []byte("http://127.0.0.1:8787/keys")) {
		t.Fatalf("message = %q", body.Error.Message)
	}
}

// A valid key reaches the upstream and its identity lands on the ledger row.
func TestChatCompletionsValidKeyLedgerIdentity(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	db := openLedger(t)
	defer db.Close()
	store := keys.Open(t.TempDir(), db)
	created, err := store.Create(keys.CreateOptions{Name: "ci-key"})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		DefaultProvider: "p1",
		Providers: []config.Provider{{
			Name: "p1", Type: config.ProviderTypeOpenAI, BaseURL: up.URL + "/v1",
			APIKey: "k", Auth: config.AuthAPIKey,
			Models: []config.ModelEntry{{ID: "m"}},
		}},
	}
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &cfg, Ledger: db, Client: http.DefaultClient, Keys: store,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		bytes.NewReader(mustJSON(t, map[string]any{
			"model":    "m",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+created.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	total, err := db.KeyAllTime(created.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if total.Requests != 1 {
		t.Fatalf("key ledger rows = %d", total.Requests)
	}
}

// A provider benched by the quota tracker is skipped in favour of the next
// candidate — the wire between MarkSpent and candidate selection.
func TestChatCompletionsQuotaSkipsProvider(t *testing.T) {
	benchedHit := make(chan struct{}, 1)
	benched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case benchedHit <- struct{}{}:
		default:
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"benched"},"finish_reason":"stop"}]}`)
	}))
	defer benched.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer healthy.Close()

	db := openLedger(t)
	defer db.Close()
	tracker := quota.New(db)
	tracker.MarkSpent("benched", quota.MarkSpentOptions{})

	cfg := config.Config{
		DefaultProvider: "benched",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{
			{
				Name: "benched", Type: config.ProviderTypeOpenAI, BaseURL: benched.URL + "/v1",
				APIKey: "k", Auth: config.AuthAPIKey,
				Models: []config.ModelEntry{{ID: "m"}},
			},
			{
				Name: "healthy", Type: config.ProviderTypeOpenAI, BaseURL: healthy.URL + "/v1",
				APIKey: "k", Auth: config.AuthAPIKey,
				Models: []config.ModelEntry{{ID: "m"}},
			},
		},
	}
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &cfg, Ledger: db, Client: http.DefaultClient, Quota: tracker,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		bytes.NewReader(mustJSON(t, map[string]any{
			"model":    "m",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if resp.Header.Get("x-jevonian-provider") != "healthy" {
		t.Fatalf("provider = %q", resp.Header.Get("x-jevonian-provider"))
	}
	select {
	case <-benchedHit:
		t.Fatal("benched provider was contacted despite quota mark")
	default:
	}
}

// A hung provider at its concurrency cap must not queue the next turn: the
// second request fails over immediately instead of waiting on the first.
// This is the 'one failing provider drags down concurrent requests' fix.
func TestChatCompletionsConcurrencyCapFailsOver(t *testing.T) {
	hungHit := make(chan struct{}, 1)
	releaseHung := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hungHit <- struct{}{}:
		default:
		}
		select {
		case <-releaseHung:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hung"},"finish_reason":"stop"}]}`)
	}))
	defer hung.Close()

	healthyHit := make(chan struct{}, 4)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case healthyHit <- struct{}{}:
		default:
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer healthy.Close()

	db := openLedger(t)
	defer db.Close()

	// Cap of 1: the hung request holds the only slot; a second turn must
	// fail over to the healthy provider rather than queue.
	g := guard.New(guard.Options{Concurrency: 1})

	cfg := config.Config{
		DefaultProvider: "hung",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{
			{
				Name: "hung", Type: config.ProviderTypeOpenAI, BaseURL: hung.URL + "/v1",
				APIKey: "k", Auth: config.AuthAPIKey,
				Models: []config.ModelEntry{{ID: "m"}},
			},
			{
				Name: "healthy", Type: config.ProviderTypeOpenAI, BaseURL: healthy.URL + "/v1",
				APIKey: "k", Auth: config.AuthAPIKey,
				Models: []config.ModelEntry{{ID: "m"}},
			},
		},
	}
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &cfg, Ledger: db, Client: http.DefaultClient, Guard: g,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func() *http.Response {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			bytes.NewReader(mustJSON(t, map[string]any{
				"model":    "m",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Occupy the hung provider's only slot; the request blocks until the test
	// releases the upstream handler.
	go func() {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			bytes.NewReader(mustJSON(t, map[string]any{
				"model":    "m",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})))
		if err != nil {
			return // released by teardown
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
	}()
	select {
	case <-hungHit:
	case <-time.After(3 * time.Second):
		t.Fatal("hung provider was not contacted for the first request")
	}

	// The next request must fail over without waiting on the held slot.
	done := make(chan *http.Response, 1)
	go func() { done <- post() }()

	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request queued behind a saturated provider instead of failing over")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if resp.Header.Get("x-jevonian-provider") != "healthy" {
		t.Fatalf("provider = %q", resp.Header.Get("x-jevonian-provider"))
	}
	select {
	case <-healthyHit:
	default:
		t.Fatal("healthy provider was not contacted")
	}
	// Unblock the held upstream call so the test servers can shut down.
	close(releaseHung)
}

func openLedger(t *testing.T) *ledger.DB {
	t.Helper()
	db, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
