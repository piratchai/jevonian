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
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/wire"
)

func antigravityRunner(t *testing.T, handler http.HandlerFunc) (*Runner, config.Provider) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	token := filepath.Join(dir, "antigravity.json")
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(token, []byte(`{"token":{"access_token":"ag-secret","refresh_token":"r","expiry":"`+expiry+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_ANTIGRAVITY_TOKEN", token)
	t.Setenv("JEVONIAN_ANTIGRAVITY_PROJECT", "proj-42")
	cfg, err := config.ParseConfig(map[string]any{"providers": []any{map[string]any{
		"name": "antigravity", "type": "gemini", "baseUrl": srv.URL, "auth": "oauth",
		"oauthSource": "antigravity", "billing": "subscription", "models": []any{"gemini-3-pro"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(ForwardDeps{HTTP: srv.Client(), Auth: &oauth.Resolver{HTTP: srv.Client()}, Sleep: func(time.Duration) {}})
	return r, cfg.Providers[0]
}

func TestAntigravitySendsCloudCodeEnvelopeAndFoldsReply(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotMeta string
	var gotBody map[string]any
	r, p := antigravityRunner(t, func(w http.ResponseWriter, req *http.Request) {
		gotPath, gotQuery = req.URL.Path, req.URL.RawQuery
		gotAuth, gotMeta = req.Header.Get("Authorization"), req.Header.Get("client-metadata")
		_ = json.NewDecoder(req.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"pong"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}}`)
	})
	at := r.Try(context.Background(), AttemptRequest{
		ClientKind: KindOpenAI,
		ClientBody: wire.Body{"messages": []any{map[string]any{"role": "system", "content": "sys"}, map[string]any{"role": "user", "content": "ping"}}},
	}, PlanEntry{Provider: p, Model: "gemini-3-pro"})
	if at.Outcome.Kind != OutcomeSuccess || at.Response == nil {
		t.Fatalf("outcome %+v err=%v text=%q", at.Outcome, at.Err, at.Text)
	}
	if gotPath != "/v1internal:generateContent" || gotQuery != "" {
		t.Fatalf("url %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer ag-secret" || !strings.Contains(gotMeta, "ANTIGRAVITY") {
		t.Fatalf("headers auth=%q meta=%q", gotAuth, gotMeta)
	}
	if gotBody["project"] != "proj-42" || gotBody["model"] != "gemini-3-pro" || gotBody["userAgent"] != "antigravity" || gotBody["requestId"] == "" {
		t.Fatalf("envelope %v", gotBody)
	}
	request := wire.AsRecord(gotBody["request"])
	if request["contents"] == nil || request["systemInstruction"] == nil || request["messages"] != nil {
		t.Fatalf("not a Gemini request: %v", request)
	}
	raw, _ := io.ReadAll(at.Response.Body)
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	msg := wire.AsRecord(wire.AsRecord(wire.AsSlice(chat["choices"])[0])["message"])
	if msg["content"] != "pong" || chat["object"] != "chat.completion" {
		t.Fatalf("chat %v", chat)
	}
}

func TestAntigravityStreamsGeminiSSEAsChatChunks(t *testing.T) {
	var gotPath, gotQuery string
	r, p := antigravityRunner(t, func(w http.ResponseWriter, req *http.Request) {
		gotPath, gotQuery = req.URL.Path, req.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"he\"}]}}]}}\n\n")
		io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"llo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}}\n\n")
	})
	at := r.Try(context.Background(), AttemptRequest{
		ClientKind: KindResponses, Stream: true,
		ClientBody: wire.Body{"input": "hi", "stream": true},
	}, PlanEntry{Provider: p, Model: "gemini-3-pro"})
	if at.Outcome.Kind != OutcomeSuccess {
		t.Fatalf("outcome %+v err=%v text=%q", at.Outcome, at.Err, at.Text)
	}
	if gotPath != "/v1internal:streamGenerateContent" || gotQuery != "alt=sse" {
		t.Fatalf("url %s?%s", gotPath, gotQuery)
	}
	raw, _ := io.ReadAll(at.Response.Body)
	out := string(raw)
	for _, want := range []string{`"content":"he"`, `"content":"llo"`, `"finish_reason":"stop"`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestAntigravityUpstreamErrorIsNotFolded(t *testing.T) {
	r, p := antigravityRunner(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"bad"}}`)
	})
	at := r.Try(context.Background(), AttemptRequest{ClientKind: KindOpenAI, ClientBody: wire.Body{"messages": []any{map[string]any{"role": "user", "content": "x"}}}},
		PlanEntry{Provider: p, Model: "gemini-3-pro"})
	if at.Status != 400 || at.Outcome.Kind == OutcomeSuccess {
		t.Fatalf("status %d outcome %+v", at.Status, at.Outcome)
	}
}
