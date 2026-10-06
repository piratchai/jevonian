package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/server"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// Parity tests for docs/go-feature-parity.md section 2 (HTTP surface).

const chatOK = `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

func fakeOpenAI(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func baseCfg(upURL string, models ...string) config.Config {
	entries := make([]config.ModelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, config.ModelEntry{ID: m})
	}
	return config.Config{
		Listen:          config.ListenConfig{Host: "127.0.0.1", Port: 8787},
		DefaultProvider: "p1",
		Routing:         config.DefaultRouting(),
		Providers: []config.Provider{{
			Name: "p1", Type: config.ProviderTypeOpenAI, BaseURL: upURL + "/v1",
			APIKey: "k", Auth: config.AuthAPIKey, Models: entries,
		}},
	}
}

func doReq(t *testing.T, h http.Handler, method, path, body, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if remote != "" {
		req.RemoteAddr = remote
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

func TestParityHealthzMainAndPublic(t *testing.T) {
	cfg := baseCfg("http://127.0.0.1:9", "m")
	main := server.New("", server.Deps{Config: &cfg})
	rr := doReq(t, main.Handler(), "GET", "/healthz", "", "127.0.0.1:1", nil)
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || rr.Code != 200 {
		t.Fatalf("main: %d %s", rr.Code, rr.Body.String())
	}
	if got["ok"] != true || got["routing"] != "auto" {
		t.Fatalf("main healthz = %v", got)
	}
	if _, ok := got["sessions"]; !ok {
		t.Fatalf("main healthz missing sessions: %v", got)
	}

	// Public surface: with a key present, healthz demands the key (TS app.use("*")).
	store := keys.Open(t.TempDir(), nil)
	created, _ := store.Create(keys.CreateOptions{Name: "k"})
	pub := server.NewPublicServer("", server.Deps{Config: &cfg, Keys: store})
	rr = doReq(t, pub.Handler(), "GET", "/healthz", "", "10.0.0.5:1", map[string]string{"authorization": "Bearer " + created.Key})
	if strings.TrimSpace(rr.Body.String()) != `{"ok":true,"public":true}` {
		t.Fatalf("public healthz = %q", rr.Body.String())
	}
	rr = doReq(t, pub.Handler(), "GET", "/healthz", "", "10.0.0.5:1", nil)
	if rr.Code != 401 {
		t.Fatalf("public healthz without key = %d", rr.Code)
	}
}

func TestParityModelsListing(t *testing.T) {
	cfg := baseCfg("http://127.0.0.1:9", "m")
	cfg.Routing.Routings = append(cfg.Routing.Routings, config.RoutingEntry{ID: "frontend"})
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := doReq(t, h, "GET", "/v1/models", "", "127.0.0.1:1", nil)
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Object != "list" {
		t.Fatalf("models: %s", rr.Body.String())
	}
	ids := map[string]bool{}
	for _, d := range out.Data {
		ids[d.ID] = true
		if d.Object != "model" || d.OwnedBy != "jevonian" {
			t.Fatalf("entry %+v", d)
		}
	}
	for _, want := range []string{"jevonian/auto", "jevonian/plan", "jevonian/execute", "jevonian/utility", "jevonian/chat", "jevonian/frontend"} {
		if !ids[want] {
			t.Fatalf("missing %s in %v", want, ids)
		}
	}
	if ids["m"] {
		t.Fatalf("provider model leaked with routing auto: %v", ids)
	}

	// Routing off: provider ids advertised (deduped); baselineModel overrides.
	cfg.Routing.Mode = "off"
	cfg.Providers = append(cfg.Providers, config.Provider{Name: "p2", Type: config.ProviderTypeOpenAI, BaseURL: "http://x", Models: []config.ModelEntry{{ID: "m"}, {ID: "n"}}})
	h = server.New("", server.Deps{Config: &cfg}).Handler()
	rr = doReq(t, h, "GET", "/v1/models", "", "127.0.0.1:1", nil)
	if strings.Count(rr.Body.String(), `"id":"m"`) != 1 || !strings.Contains(rr.Body.String(), `"id":"n"`) {
		t.Fatalf("off listing = %s", rr.Body.String())
	}
	cfg.Routing.BaselineModel = "pinned"
	h = server.New("", server.Deps{Config: &cfg}).Handler()
	rr = doReq(t, h, "GET", "/v1/models", "", "127.0.0.1:1", nil)
	if strings.Count(rr.Body.String(), `"id"`) != 1 || !strings.Contains(rr.Body.String(), `"id":"pinned"`) {
		t.Fatalf("baseline listing = %s", rr.Body.String())
	}

	// Anthropic dialect when anthropic-version present (desktop slot = jevonian/auto).
	cfg.Routing.Mode = "auto"
	h = server.New("", server.Deps{Config: &cfg}).Handler()
	rr = doReq(t, h, "GET", "/v1/models", "", "127.0.0.1:1", map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(rr.Body.String(), `"anthropic_family_tier"`) || !strings.Contains(rr.Body.String(), "Jevonian Auto") || !strings.Contains(rr.Body.String(), `"has_more":false`) {
		t.Fatalf("anthropic list = %s", rr.Body.String())
	}
}

func TestParityEndpointsStreamAndNonStream(t *testing.T) {
	var openaiHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHits++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"streamed\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()
	cfg := baseCfg(up.URL, "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()

	// chat completions non-stream
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"content":"ok"`) {
		t.Fatalf("chat: %d %s", rr.Code, rr.Body.String())
	}
	// chat completions stream
	rr = doReq(t, h, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("content-type"), "text/event-stream") || !strings.Contains(rr.Body.String(), "streamed") || !strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("chat stream: %d %s %s", rr.Code, rr.Header().Get("content-type"), rr.Body.String())
	}
	// anthropic messages (client wire anthropic over an openai host: bridged)
	rr = doReq(t, h, "POST", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"type":"message"`) || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("messages: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, h, "POST", "/v1/messages", `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "message_start") || !strings.Contains(rr.Body.String(), "streamed") || !strings.Contains(rr.Body.String(), "message_stop") {
		t.Fatalf("messages stream: %d %s", rr.Code, rr.Body.String())
	}
	// responses (client wire responses over an openai host: bridged)
	rr = doReq(t, h, "POST", "/v1/responses", `{"model":"m","input":"hi"}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"object":"response"`) || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("responses: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, h, "POST", "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "response.created") || !strings.Contains(rr.Body.String(), "response.completed") || !strings.Contains(rr.Body.String(), "streamed") {
		t.Fatalf("responses stream: %d %s", rr.Code, rr.Body.String())
	}
	if openaiHits != 6 {
		t.Fatalf("upstream hits = %d", openaiHits)
	}
}

func TestParityCountTokensMainOnly(t *testing.T) {
	cfg := baseCfg("http://127.0.0.1:9", "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := doReq(t, h, "POST", "/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"hello world"}]}`, "127.0.0.1:1", nil)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out["input_tokens"] == nil || out["input_tokens"].(float64) <= 0 {
		t.Fatalf("count_tokens: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, h, "POST", "/v1/messages/count_tokens", `nope`, "127.0.0.1:1", nil)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "Invalid JSON body") {
		t.Fatalf("count_tokens bad json: %d %s", rr.Code, rr.Body.String())
	}
	// Public app has no count_tokens route (TS createPublicApp).
	store := keys.Open(t.TempDir(), nil)
	created, _ := store.Create(keys.CreateOptions{Name: "k"})
	pub := server.NewPublicServer("", server.Deps{Config: &cfg, Keys: store}).Handler()
	rr = doReq(t, pub, "POST", "/v1/messages/count_tokens", `{"messages":[]}`, "10.0.0.5:1", map[string]string{"authorization": "Bearer " + created.Key})
	if rr.Code != 404 {
		t.Fatalf("public count_tokens = %d", rr.Code)
	}
}

func TestParityWebSocketUpgrade426(t *testing.T) {
	cfg := baseCfg("http://127.0.0.1:9", "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := doReq(t, h, "GET", "/v1/responses", "", "127.0.0.1:1", map[string]string{"upgrade": "websocket", "connection": "Upgrade"})
	if rr.Code != 426 || !strings.Contains(rr.Body.String(), "HTTP Responses transport") {
		t.Fatalf("ws upgrade: %d %s", rr.Code, rr.Body.String())
	}
	// Upgrade without Connection: upgrade is an ordinary request (404 for GET /v1/responses).
	rr = doReq(t, h, "GET", "/v1/responses", "", "127.0.0.1:1", map[string]string{"upgrade": "websocket"})
	if rr.Code == 426 {
		t.Fatalf("bare Upgrade header must not 426")
	}
	// /v1/models registered before the guard: never 426.
	rr = doReq(t, h, "GET", "/v1/models", "", "127.0.0.1:1", map[string]string{"upgrade": "websocket", "connection": "upgrade"})
	if rr.Code != 200 {
		t.Fatalf("models with upgrade = %d", rr.Code)
	}
}

func TestParityAccountProbeProxyThroughServer(t *testing.T) {
	var gotAcct, gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcct = r.Header.Get("chatgpt-account-id")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"plan":"pro"}`)
	}))
	defer backend.Close()
	t.Setenv("JEVONIAN_CHATGPT_UPSTREAM", backend.URL)
	cfg := baseCfg("http://127.0.0.1:9", "m")
	store := keys.Open(t.TempDir(), nil)
	_, _ = store.Create(keys.CreateOptions{Name: "k"})
	h := server.New("", server.Deps{Config: &cfg, Keys: store}).Handler()
	// Account session from loopback: passes auth via chatgpt-account-id, then proxied.
	rr := doReq(t, h, "GET", "/v1/me", "", "127.0.0.1:1", map[string]string{"chatgpt-account-id": "acct", "authorization": "Bearer chatgpt-token"})
	if rr.Code != 200 || rr.Body.String() != `{"plan":"pro"}` || gotAcct != "acct" || gotPath != "/v1/me" {
		t.Fatalf("probe: %d %q acct=%q path=%q", rr.Code, rr.Body.String(), gotAcct, gotPath)
	}
}

func TestParityFirstRunOpenAuth(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	db := openLedger(t)
	defer db.Close()
	cfg := baseCfg(up.URL, "m")
	store := keys.Open(t.TempDir(), db)
	h := server.New("", server.Deps{Config: &cfg, Ledger: db, Keys: store}).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", nil)
	if rr.Code != 200 {
		t.Fatalf("open mode: %d %s", rr.Code, rr.Body.String())
	}
	requestID := rr.Header().Get("x-jevonian-request-id")
	n, _ := db.Count()
	if n != 1 {
		t.Fatalf("ledger rows = %d", n)
	}
	total, err := db.Totals()
	if err != nil || total.Requests != 1 {
		t.Fatalf("totals: %+v %v", total, err)
	}
	_ = requestID
}

func TestParityKeyAuthBearerAndXAPIKey(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m")
	store := keys.Open(t.TempDir(), nil)
	created, _ := store.Create(keys.CreateOptions{Name: "k"})
	h := server.New("", server.Deps{Config: &cfg, Keys: store}).Handler()

	for name, hdr := range map[string]map[string]string{
		"bearer":        {"authorization": "Bearer " + created.Key},
		"bearer-case":   {"authorization": "bEaReR " + created.Key},
		"x-api-key":     {"x-api-key": created.Key},
		"bearer-wins":   {"authorization": "Bearer " + created.Key, "x-api-key": "junk"},
		"x-api-key-pad": {"x-api-key": "  " + created.Key + "  "},
	} {
		rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "203.0.113.9:1", hdr)
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	for name, hdr := range map[string]map[string]string{
		"none":     nil,
		"wrong":    {"authorization": "Bearer sk-jev-wrong"},
		"basic":    {"authorization": "Basic abc"},
		"empty":    {"authorization": "Bearer "},
		"bad-bear": {"authorization": "Bearer " + created.Key + "x"},
	} {
		rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "203.0.113.9:1", hdr)
		if rr.Code != 401 || !strings.Contains(rr.Body.String(), `"invalid_request_error"`) || !strings.Contains(rr.Body.String(), "Invalid API key. Create one at http://127.0.0.1:8787/keys") {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	// Request counter bumped.
	_ = store.Flush()
	list, _ := store.List()
	if list[0].Requests != 5 {
		t.Fatalf("requests = %d", list[0].Requests)
	}
}

func TestParityLoopbackSentinel(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m")
	store := keys.Open(t.TempDir(), nil)
	_, _ = store.Create(keys.CreateOptions{Name: "k"})
	h := server.New("", server.Deps{Config: &cfg, Keys: store}).Handler()

	// `auto` is desktop-routed, so the turn stays on Jevonian and the sentinel
	// only has to pass authorization (a missing brain is a later 400, not 401).
	// A concrete model with a sentinel is forwarded to the real backend instead
	// (src/upstream.ts proxyNativeCodex) and cannot reach OpenAI; that case is
	// asserted in TestNativeCodexSentinelTokenRefused.
	desktopBody := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	for _, s := range []string{"jevonian-local", "ollama-local-codex", "ollama"} {
		rr := doReq(t, h, "POST", "/v1/chat/completions", desktopBody, "127.0.0.1:5", map[string]string{"authorization": "Bearer " + s})
		if rr.Code == 401 {
			t.Fatalf("sentinel %s loopback rejected: %d %s", s, rr.Code, rr.Body.String())
		}
	}
	rr := doReq(t, h, "POST", "/v1/chat/completions", desktopBody, "[::1]:5", map[string]string{"x-api-key": "jevonian-local"})
	if rr.Code == 401 {
		t.Fatalf("sentinel ipv6 loopback rejected: %d", rr.Code)
	}
	// Non-loopback peer: rejected even with the sentinel.
	rr = doReq(t, h, "POST", "/v1/chat/completions", desktopBody, "192.168.1.9:5", map[string]string{"authorization": "Bearer jevonian-local"})
	if rr.Code != 401 {
		t.Fatalf("sentinel from LAN peer: %d", rr.Code)
	}
	// Server bound to 0.0.0.0: sentinel never qualifies, even from loopback peer.
	wide := cfg
	wide.Listen.Host = "0.0.0.0"
	hw := server.New("", server.Deps{Config: &wide, Keys: store}).Handler()
	rr = doReq(t, hw, "POST", "/v1/chat/completions", desktopBody, "127.0.0.1:5", map[string]string{"authorization": "Bearer jevonian-local"})
	if rr.Code != 401 {
		t.Fatalf("sentinel on wide bind: %d", rr.Code)
	}
	// Account header from loopback is accepted without a key (desktop-routed
	// model, so it is not forwarded to the ChatGPT backend).
	rr = doReq(t, h, "POST", "/v1/chat/completions", desktopBody, "127.0.0.1:5", map[string]string{"chatgpt-account-id": "a", "authorization": "Bearer real-chatgpt"})
	if rr.Code == 401 {
		t.Fatalf("account session rejected: %d %s", rr.Code, rr.Body.String())
	}
	// Public surface never honours the sentinel or account header, from loopback peers too.
	pub := server.NewPublicServer("", server.Deps{Config: &cfg, Keys: store}).Handler()
	for _, hdr := range []map[string]string{
		{"authorization": "Bearer jevonian-local"},
		{"chatgpt-account-id": "a"},
	} {
		rr = doReq(t, pub, "POST", "/v1/chat/completions", desktopBody, "127.0.0.1:5", hdr)
		if rr.Code != 401 {
			t.Fatalf("public sentinel %v: %d", hdr, rr.Code)
		}
	}
	// Public with no keys at all: 401 (TS: "No Jevonian API key exists yet").
	empty := server.NewPublicServer("", server.Deps{Config: &cfg, Keys: keys.Open(t.TempDir(), nil)}).Handler()
	rr = doReq(t, empty, "POST", "/v1/chat/completions", chatBody, "10.0.0.1:5", nil)
	if rr.Code != 401 || !strings.Contains(rr.Body.String(), "No Jevonian API key exists yet") {
		t.Fatalf("public no keys: %d %s", rr.Code, rr.Body.String())
	}
}

func TestParityCreditLimit429(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	db := openLedger(t)
	defer db.Close()
	cfg := baseCfg(up.URL, "m")
	store := keys.Open(t.TempDir(), db)
	limit := 0.10
	created, _ := store.Create(keys.CreateOptions{Name: "capped", LimitUSD: &limit})
	h := server.New("", server.Deps{Config: &cfg, Ledger: db, Keys: store}).Handler()
	cost := func(v float64) *float64 { return &v }
	// Subscription spend is excluded.
	_ = db.Append(ledger.Record{TS: time.Now(), Provider: "p", Model: "m", Status: 200, CostUSD: cost(5), Billing: "subscription", KeyID: created.Record.ID})
	hdr := map[string]string{"authorization": "Bearer " + created.Key}
	if rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", hdr); rr.Code != 200 {
		t.Fatalf("subscription spend counted: %d %s", rr.Code, rr.Body.String())
	}
	_ = db.Append(ledger.Record{TS: time.Now(), Provider: "p", Model: "m", Status: 200, CostUSD: cost(0.10), Billing: "api", KeyID: created.Record.ID})
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", hdr)
	if rr.Code != 429 {
		t.Fatalf("limit: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Error.Type != "credit_limit_exceeded" || out.Error.Code != "credit_limit_exceeded" ||
		out.Error.Message != `Credit limit reached ($0.10) for API key "capped". Increase or remove the limit in the Jevonian dashboard.` {
		t.Fatalf("limit body = %s", rr.Body.String())
	}
	// Public surface also enforces it.
	pub := server.NewPublicServer("", server.Deps{Config: &cfg, Ledger: db, Keys: store}).Handler()
	if rr := doReq(t, pub, "POST", "/v1/chat/completions", chatBody, "10.0.0.1:1", hdr); rr.Code != 429 {
		t.Fatalf("public limit: %d", rr.Code)
	}
}

func TestParityPublicListenerOnlyV1AndHealthz(t *testing.T) {
	cfg := baseCfg("http://127.0.0.1:9", "m")
	store := keys.Open(t.TempDir(), nil)
	created, _ := store.Create(keys.CreateOptions{Name: "k"})
	pub := server.NewPublicServer("", server.Deps{Config: &cfg, Keys: store}).Handler()
	hdr := map[string]string{"authorization": "Bearer " + created.Key}
	for _, p := range []string{"/", "/api/state", "/stats", "/providers", "/assets/x.js", "/logs/abc"} {
		rr := doReq(t, pub, "GET", p, "", "10.0.0.1:1", hdr)
		if rr.Code != 404 {
			t.Fatalf("public %s = %d %s", p, rr.Code, rr.Body.String())
		}
	}
	if rr := doReq(t, pub, "GET", "/v1/models", "", "10.0.0.1:1", hdr); rr.Code != 200 {
		t.Fatalf("public /v1/models = %d", rr.Code)
	}
}

func TestParityStatsSummary(t *testing.T) {
	db := openLedger(t)
	defer db.Close()
	cost := 0.25
	_ = db.Append(ledger.Record{TS: time.Now(), Provider: "p", Model: "m", Status: 200, PromptTokens: 100, CacheReadTokens: 40, CostUSD: &cost})
	_ = db.Append(ledger.Record{TS: time.Now(), Provider: "p", Model: "m", Status: 200, PromptTokens: 50, CacheReadTokens: 10})
	cfg := baseCfg("http://127.0.0.1:9", "m")
	h := server.New("", server.Deps{Config: &cfg, Ledger: db}).Handler()
	rr := doReq(t, h, "GET", "/stats", "", "127.0.0.1:1", nil)
	var out map[string]float64
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["requests"] != 2 || out["costUsd"] != 0.25 || out["cacheReadTokens"] != 50 || out["promptTokens"] != 150 {
		t.Fatalf("stats = %s", rr.Body.String())
	}
	if _, ok := out["sessions"]; !ok {
		t.Fatalf("stats missing sessions: %s", rr.Body.String())
	}
}

// ---- 2.2 decision headers -------------------------------------------------

func scorerFor(choice, effort string) routing.ScorerFunc {
	return func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		return routing.AskResult{Choice: &routing.Choice{Model: choice, Confidence: 0.9, Effort: effort}}
	}
}

func priceTable(model, _ string) *routing.Price {
	return &routing.Price{Input: 0.5, Output: 1.5}
}

func TestParityDecisionHeadersPinned(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", map[string]string{"x-jevonian-session": "sess-pinned"})
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	want := map[string]string{
		"x-jevonian-model":    "m",
		"x-jevonian-provider": "p1",
		"x-jevonian-reason":   "pinned-model",
		"x-jevonian-session":  "sess-pinned",
	}
	for k, v := range want {
		if got := rr.Header().Get(k); got != v {
			t.Fatalf("%s = %q want %q (headers %v)", k, got, v, rr.Header())
		}
	}
	if rr.Header().Get("x-jevonian-phase") == "" {
		t.Fatalf("phase header missing: %v", rr.Header())
	}
	if len(rr.Header().Get("x-jevonian-request-id")) < 32 {
		t.Fatalf("request id = %q", rr.Header().Get("x-jevonian-request-id"))
	}
	for _, absent := range []string{"x-jevonian-retries", "x-jevonian-brain", "x-jevonian-brain-channel", "x-jevonian-canonical", "x-jevonian-effort", "x-jevonian-effort-note", "x-jevonian-skipped", "x-jevonian-soft-error", "x-jevonian-quota-failovers"} {
		if v := rr.Header().Get(absent); v != "" {
			t.Fatalf("%s should be absent, got %q", absent, v)
		}
	}
}

func TestParityDecisionHeadersCanonical(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "deepseek-v4.1-flash")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"deepseek-v4-1-flash","messages":[{"role":"user","content":"x"}]}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || rr.Header().Get("x-jevonian-canonical") != "deepseek-v4-1-flash" || rr.Header().Get("x-jevonian-model") != "deepseek-v4.1-flash" || rr.Header().Get("x-jevonian-reason") != "canonical-model" {
		t.Fatalf("canonical: %d %v", rr.Code, rr.Header())
	}
}

func TestParityDecisionHeadersBrainAndCacheAndEffort(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m", "n")
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "X", TimeoutMs: 1000, MinConfidence: 0.6}}
	two := 2
	_ = two
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{"m": {Efforts: []string{"low", "medium"}}}
	deps := server.Deps{
		Config:  &cfg,
		Routing: routing.Deps{Scorer: scorerFor("plan", "max"), Prices: priceTable},
	}
	h := server.New("", deps).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"jevonian/auto","messages":[{"role":"user","content":"build a feature"}]}`, "127.0.0.1:1", map[string]string{"x-jevonian-session": "s-brain"})
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	h2 := rr.Header()
	if h2.Get("x-jevonian-brain") != "jev" || h2.Get("x-jevonian-brain-channel") != "typesafe" {
		t.Fatalf("brain headers: %v", h2)
	}
	if h2.Get("x-jevonian-phase") != "plan" || !strings.HasPrefix(h2.Get("x-jevonian-reason"), "brain:plan") {
		t.Fatalf("phase/reason: %v", h2)
	}
	if h2.Get("x-jevonian-cache-state") == "" || h2.Get("x-jevonian-cache-keep") != "first" {
		t.Fatalf("cache headers: %v", h2)
	}
	if h2.Get("x-jevonian-effort") == "" || h2.Get("x-jevonian-effort-note") == "" || !strings.Contains(h2.Get("x-jevonian-reason"), "effort-clamped") {
		t.Fatalf("effort headers: %v", h2)
	}
}

func TestParityRequestHeadersPhaseEffortAffinitySession(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m", "n")
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "X", TimeoutMs: 1000, MinConfidence: 0.6}}
	brainCalls := 0
	scorer := routing.ScorerFunc(func(_ context.Context, _ config.BrainConfig, _ map[string]any, _ bool) routing.AskResult {
		brainCalls++
		return routing.AskResult{Choice: &routing.Choice{Model: "plan", Confidence: 0.9}}
	})
	cfg.Routing.Capacities = map[string]config.ModelCapacityConfig{
		"m": {Efforts: []string{"low"}},
		"n": {Efforts: []string{"low", "high"}},
	}
	deps := server.Deps{Config: &cfg, Routing: routing.Deps{Scorer: scorer, Prices: priceTable}}
	h := server.New("", deps).Handler()
	body := `{"model":"jevonian/auto","messages":[{"role":"user","content":"hello"}]}`

	// x-jevonian-phase forces the route without consulting the brain.
	rr := doReq(t, h, "POST", "/v1/chat/completions", body, "127.0.0.1:1", map[string]string{"x-jevonian-phase": "execute"})
	if rr.Code != 200 || rr.Header().Get("x-jevonian-phase") != "execute" || !strings.HasPrefix(rr.Header().Get("x-jevonian-reason"), "explicit:execute") || rr.Header().Get("x-jevonian-brain") != "" || brainCalls != 0 {
		t.Fatalf("phase header: %d %v brainCalls=%d", rr.Code, rr.Header(), brainCalls)
	}
	// x-jevonian-effort is a floor on the brain path: shallower model is skipped.
	rr = doReq(t, h, "POST", "/v1/chat/completions", body, "127.0.0.1:1", map[string]string{"x-jevonian-effort": "high"})
	// Same shape as the TS golden "effort-floor-skip": the shallow model is listed as
	// skipped, and the level actually sent is reported with a clamp note.
	if rr.Code != 200 || rr.Header().Get("x-jevonian-effort") == "" || rr.Header().Get("x-jevonian-effort-note") == "" {
		t.Fatalf("effort floor: %d %v", rr.Code, rr.Header())
	}
	skipped := rr.Header().Get("x-jevonian-skipped")
	if !strings.Contains(skipped, "p1/m=effort(") || !strings.Contains(rr.Header().Get("x-jevonian-reason"), "effort-skip") {
		t.Fatalf("effort skip: skipped=%q reason=%q", skipped, rr.Header().Get("x-jevonian-reason"))
	}
	// x-jevonian-session pins the session id.
	rr = doReq(t, h, "POST", "/v1/chat/completions", body, "127.0.0.1:1", map[string]string{"x-jevonian-phase": "execute", "x-jevonian-session": "pin-me"})
	if rr.Header().Get("x-jevonian-session") != "pin-me" {
		t.Fatalf("session pin: %v", rr.Header())
	}
	// Affinity modes: off disables keep; second turn of same session shows the difference.
	for mode, wantKeep := range map[string]string{"off": "off"} {
		rr = doReq(t, h, "POST", "/v1/chat/completions", body, "127.0.0.1:1", map[string]string{"x-jevonian-phase": "execute", "x-jevonian-session": "aff-" + mode, "x-jevonian-affinity": mode})
		if got := rr.Header().Get("x-jevonian-cache-keep"); got != wantKeep {
			t.Fatalf("affinity %s: cache-keep=%q", mode, got)
		}
	}
	for _, mode := range []string{"auto", "session", "turn"} {
		rr = doReq(t, h, "POST", "/v1/chat/completions", body, "127.0.0.1:1", map[string]string{"x-jevonian-phase": "execute", "x-jevonian-session": "aff2-" + mode, "x-jevonian-affinity": mode})
		if got := rr.Header().Get("x-jevonian-cache-keep"); got == "" || got == "off" {
			t.Fatalf("affinity %s: cache-keep=%q", mode, got)
		}
	}
}

func TestParityRetriesAndSoftErrorHeaders(t *testing.T) {
	// Same-host retry: first call 502, second succeeds -> x-jevonian-retries: 1.
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(502)
			_, _ = io.WriteString(w, "bad gateway")
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()
	cfg := baseCfg(up.URL, "m")
	one := 1
	h := server.New("", server.Deps{Config: &cfg, SameHostRetries: &one, Sleep: func(time.Duration) {}}).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", nil)
	if rr.Code != 200 || rr.Header().Get("x-jevonian-retries") != "1" {
		t.Fatalf("retries: %d %v", rr.Code, rr.Header())
	}

	// Soft error: streaming request, upstream refuses (400) -> 200 SSE + soft-error header
	// and the decision headers ride along.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
	}))
	defer bad.Close()
	cfg2 := baseCfg(bad.URL, "m")
	h2 := server.New("", server.Deps{Config: &cfg2, SameHostRetries: new(int)}).Handler()
	rr = doReq(t, h2, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "127.0.0.1:1", nil)
	if rr.Code != 200 || rr.Header().Get("x-jevonian-soft-error") != "1" || rr.Header().Get("x-jevonian-provider") != "p1" || !strings.HasPrefix(rr.Header().Get("content-type"), "text/event-stream") {
		t.Fatalf("soft error: %d %v %s", rr.Code, rr.Header(), rr.Body.String())
	}
	// Mid-stream drop is covered by streaming_test.go (TestWriteStreamClosesDroppedStreamSoftly).
}

func TestParityRequestIDJoinsLedgerRow(t *testing.T) {
	var hits int
	up := fakeOpenAI(t, &hits)
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := baseCfg(up.URL, "m")
	h := server.New("", server.Deps{Config: &cfg, Ledger: db}).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", nil)
	id := rr.Header().Get("x-jevonian-request-id")
	logs, err := admin.OpenSQLiteLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := logs.LogDetail(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("no ledger row for request id %q: %v", id, err)
	}
	if rec["requestId"] != id && rec["request_id"] != id {
		t.Fatalf("row = %v", rec)
	}
}

func TestParityBodyCaptureJoinsRequestID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "")
	var hits int
	up := fakeOpenAI(t, &hits)
	cfg := baseCfg(up.URL, "m")
	h := server.New("", server.Deps{Config: &cfg}).Handler()
	rr := doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", nil)
	id := rr.Header().Get("x-jevonian-request-id")
	server.FlushBodies()
	path := filepath.Join(dir, "bodies", id+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no capture for %s: %v", id, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("capture mode = %v", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Kind     string
		Path     string
		Decision struct{ Provider, Model, Reason string }
		Body     map[string]any
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "request" || got.Path != "/chat/completions" || got.Decision.Provider != "p1" || got.Decision.Model != "m" || got.Body["model"] != "m" {
		t.Fatalf("capture = %s", raw)
	}

	// JEVONIAN_CAPTURE_BODIES=0 disables it.
	t.Setenv("JEVONIAN_CAPTURE_BODIES", "0")
	rr = doReq(t, h, "POST", "/v1/chat/completions", chatBody, "127.0.0.1:1", nil)
	server.FlushBodies()
	if _, err := os.Stat(filepath.Join(dir, "bodies", rr.Header().Get("x-jevonian-request-id")+".json")); err == nil {
		t.Fatal("capture written despite JEVONIAN_CAPTURE_BODIES=0")
	}
}
