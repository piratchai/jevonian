package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/server"
)

// A ChatGPT Desktop call naming a native model keeps using the real backend
// with the client's own credential instead of being routed by Jevonian.
// src/upstream.ts proxyNativeCodex.
func TestNativeCodexPassthroughForwardsToChatGPT(t *testing.T) {
	var gotPath, gotAuth, gotAccount string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		gotAccount = r.Header.Get("chatgpt-account-id")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer backend.Close()
	t.Setenv("JEVONIAN_CHATGPT_CODEX_UPSTREAM", backend.URL)

	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &config.Config{},
		Client: http.DefaultClient,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5.6-sol","input":"hi"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer account-token")
	req.Header.Set("chatgpt-account-id", "acct-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	// The /v1 prefix is stripped for the ChatGPT backend.
	if gotPath != "/responses" {
		t.Fatalf("forwarded path = %q", gotPath)
	}
	if gotAuth != "Bearer account-token" || gotAccount != "acct-1" {
		t.Fatalf("forwarded headers auth=%q account=%q", gotAuth, gotAccount)
	}
	if !bytes.Contains(gotBody, []byte(`"gpt-5.6-sol"`)) {
		t.Fatalf("forwarded body = %s", gotBody)
	}
	if !bytes.Contains(raw, []byte("response.completed")) {
		t.Fatalf("client body = %s", raw)
	}
}

// A concrete model with a desktop sentinel token cannot reach OpenAI, so the
// authentication error is answered directly instead of being routed locally.
func TestNativeCodexSentinelTokenRefused(t *testing.T) {
	srv := server.New("127.0.0.1:0", server.Deps{
		Config: &config.Config{},
		Client: http.DefaultClient,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5.6-sol","input":"hi"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer jevonian-local")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
