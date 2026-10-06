package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/server"
)

func probeRequest(path string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestIsAccountProbe(t *testing.T) {
	account := map[string]string{"chatgpt-account-id": "acct-1"}

	t.Run("detects account endpoints carrying the ChatGPT account header", func(t *testing.T) {
		for _, path := range []string{"/v1/me", "/v1/subscription", "/v1/me/", "/v1/account/usage", "/v1/some/other/bookkeeping"} {
			if !server.IsAccountProbe(probeRequest(path, account)) {
				t.Fatalf("IsAccountProbe(%q) = false", path)
			}
		}
	})

	t.Run("never claims inference paths even with an account header", func(t *testing.T) {
		// A Codex model call carries the account header too; proxying it to
		// chatgpt.com would bypass Jevonian's routing entirely.
		for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages", "/v1/messages/count_tokens", "/v1/models"} {
			if server.IsAccountProbe(probeRequest(path, account)) {
				t.Fatalf("IsAccountProbe(%q) = true", path)
			}
		}
	})

	t.Run("ignores requests without an account header", func(t *testing.T) {
		if server.IsAccountProbe(probeRequest("/v1/me", nil)) {
			t.Fatal("probe without header detected")
		}
		if server.IsAccountProbe(probeRequest("/v1/me", map[string]string{"chatgpt-account-id": "  "})) {
			t.Fatal("probe with blank header detected")
		}
	})

	t.Run("ignores paths outside /v1", func(t *testing.T) {
		if server.IsAccountProbe(probeRequest("/api/state", account)) {
			t.Fatal("non-/v1 path detected as probe")
		}
	})
}

func TestIsWebSocketUpgrade(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Add(kv[i], kv[i+1])
		}
		return out
	}
	t.Run("detects a real websocket upgrade", func(t *testing.T) {
		if !server.IsWebSocketUpgrade(h("upgrade", "websocket", "connection", "Upgrade")) {
			t.Fatal("plain upgrade missed")
		}
		if !server.IsWebSocketUpgrade(h("Upgrade", "WebSocket", "Connection", "keep-alive, Upgrade")) {
			t.Fatal("mixed-case token list missed")
		}
	})
	t.Run("requires Connection to list upgrade", func(t *testing.T) {
		if server.IsWebSocketUpgrade(h("upgrade", "websocket")) {
			t.Fatal("upgrade without connection token accepted")
		}
		if server.IsWebSocketUpgrade(h("upgrade", "websocket", "connection", "keep-alive")) {
			t.Fatal("keep-alive accepted as upgrade")
		}
	})
	t.Run("ignores ordinary requests", func(t *testing.T) {
		if server.IsWebSocketUpgrade(h("connection", "keep-alive")) || server.IsWebSocketUpgrade(h()) {
			t.Fatal("ordinary request flagged")
		}
	})
}

func TestProxyAccountProbePassesThroughUnmodified(t *testing.T) {
	var gotPath, gotQuery, gotAccount, gotAuth, gotHost, gotBody, gotMethod string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAccount = r.Header.Get("chatgpt-account-id")
		gotAuth = r.Header.Get("authorization")
		gotHost = r.Host
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-upstream", "chatgpt")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"plan":"pro"}`)
	}))
	defer backend.Close()
	t.Setenv("JEVONIAN_CHATGPT_UPSTREAM", backend.URL)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/v1/me?x=1", strings.NewReader(`{"ping":true}`))
	req.Header.Set("chatgpt-account-id", "acct-1")
	req.Header.Set("authorization", "Bearer real-chatgpt-token")
	rec := httptest.NewRecorder()

	if !server.ProxyAccountProbe(rec, req, http.DefaultClient) {
		t.Fatal("proxy reported unreachable")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != `{"plan":"pro"}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("x-upstream") != "chatgpt" {
		t.Fatalf("upstream headers not forwarded: %v", rec.Header())
	}
	if gotPath != "/v1/me" || gotQuery != "x=1" || gotMethod != http.MethodPost {
		t.Fatalf("forwarded %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if gotAccount != "acct-1" || gotAuth != "Bearer real-chatgpt-token" {
		t.Fatalf("credentials not passed through: account=%q auth=%q", gotAccount, gotAuth)
	}
	if strings.Contains(gotHost, "8787") {
		t.Fatalf("Host header leaked from loopback request: %q", gotHost)
	}
	if gotBody != `{"ping":true}` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestProxyAccountProbeRetriesTransientStatus(t *testing.T) {
	t.Setenv("JEVONIAN_UPSTREAM_RETRIES", "2")
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		// The rewound body must arrive intact on the retry.
		_, _ = w.Write(raw)
	}))
	defer backend.Close()
	t.Setenv("JEVONIAN_CHATGPT_UPSTREAM", backend.URL)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/v1/account", strings.NewReader("payload"))
	req.Header.Set("chatgpt-account-id", "acct-1")
	rec := httptest.NewRecorder()
	if !server.ProxyAccountProbe(rec, req, http.DefaultClient) {
		t.Fatal("proxy reported unreachable")
	}
	if calls != 2 || rec.Code != 200 || rec.Body.String() != "payload" {
		t.Fatalf("calls=%d status=%d body=%q", calls, rec.Code, rec.Body.String())
	}
}

func TestProxyAccountProbeUnreachableFallsThrough(t *testing.T) {
	t.Setenv("JEVONIAN_UPSTREAM_RETRIES", "0")
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := backend.URL
	backend.Close() // nothing is listening any more
	t.Setenv("JEVONIAN_CHATGPT_UPSTREAM", url)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/v1/me", nil)
	req.Header.Set("chatgpt-account-id", "acct-1")
	rec := httptest.NewRecorder()
	if server.ProxyAccountProbe(rec, req, http.DefaultClient) {
		t.Fatal("unreachable upstream reported as proxied")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("response written despite fall-through: %q", rec.Body.String())
	}
}
