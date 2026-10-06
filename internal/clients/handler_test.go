package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func apiConfig(t *testing.T, mode string) *config.Config {
	t.Helper()
	cfg, err := config.ParseConfig(map[string]any{"listen": map[string]any{"host": "127.0.0.1", "port": 8787}, "providers": []any{map[string]any{"name": "sub", "type": "openai", "baseUrl": "http://127.0.0.1:1/v1", "apiKey": "test", "models": []any{"m"}}}, "routing": map[string]any{"mode": mode, "tiers": map[string]any{"plan": []any{"m"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return &cfg
}
func requestHandler(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("not JSON")
	}
	return w.Code, out
}
func TestHandlerWebAPIContract(t *testing.T) {
	m := testManager(t, "linux")
	mkdir(t, m.opts.ClaudeConfigDir)
	cfg := apiConfig(t, "auto")
	h := NewHandler(HandlerOptions{Config: func() *config.Config { return cfg }, Manager: m})
	status, body := requestHandler(t, h, "GET", "/api/clients", "")
	if status != 200 || body["hostname"] != "fixture-host" || body["platform"] != "linux" {
		t.Fatal(status, body)
	}
	targets := body["clients"].([]any)
	if len(targets) != 2 {
		t.Fatal(targets)
	}
	for _, raw := range targets {
		v := raw.(map[string]any)
		for _, key := range []string{"id", "label", "installed", "status", "logo"} {
			if _, ok := v[key]; !ok {
				t.Fatal("missing", key)
			}
		}
	}
	// Relative routes supplied through admin.Deps.Extensions work unchanged.
	ext := h.Extensions()
	if len(ext) != 3 {
		t.Fatal(ext)
	}
	status, body = requestHandler(t, ext["POST /clients/{id}"], "POST", "/clients/claude", `{"restart":false}`)
	if status != 200 {
		t.Fatal(status, body)
	}
	result := body["result"].(map[string]any)
	if result["restarted"] != false || len(result["written"].([]any)) != 1 || result["target"].(map[string]any)["status"] != "connected" {
		t.Fatal(result)
	}
	// Desktop inject is only Auto, while Claude Code receives phase aliases.
	env := readObject(m.ClaudeCodeSettingsPath())["env"].(map[string]any)
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "jevonian/auto" || env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "jevonian/utility" {
		t.Fatal(env)
	}
	status, body = requestHandler(t, h, "POST", "/api/clients/chatgpt", `["not-an-object"]`)
	if status != 200 || body["result"].(map[string]any)["target"].(map[string]any)["status"] != "connected" {
		t.Fatal(status, body)
	}
	status, body = requestHandler(t, h, "DELETE", "/api/clients/claude", `{malformed`)
	if status != 200 || len(body["clients"].([]any)) != 2 {
		t.Fatal(status, body)
	}
	status, body = requestHandler(t, h, "POST", "/api/clients/unknown", `{}`)
	if status != 400 || body["error"] != "Unknown client." {
		t.Fatal(status, body)
	}
}
func TestHandlerRestartRequiredDoesNotWriteBeforeConfirmation(t *testing.T) {
	m := testManager(t, "linux")
	cfg := apiConfig(t, "auto")
	restarts := 0
	m.opts.Running = func(ClientID) bool { return true }
	m.opts.Restart = func(context.Context, ClientID) error { restarts++; return nil }
	h := NewHandler(HandlerOptions{Config: func() *config.Config { return cfg }, Manager: m})
	for _, method := range []string{"POST", "DELETE"} {
		status, body := requestHandler(t, h, method, "/api/clients/chatgpt", `{"restart":"true"}`)
		if status != 409 || body["error"] != "restart-required" || body["running"] != true || body["client"] != "chatgpt" || body["message"] == nil {
			t.Fatal(status, body)
		}
	}
	if exists(m.CodexConfigPath()) || restarts != 0 {
		t.Fatal("unconfirmed apply touched disk or app")
	}
	status, body := requestHandler(t, h, "POST", "/api/clients/chatgpt", `{"restart":true}`)
	if status != 200 || body["result"].(map[string]any)["restarted"] != true || restarts != 1 {
		t.Fatal(status, body, restarts)
	}
	status, body = requestHandler(t, h, "DELETE", "/api/clients/chatgpt", `{"restart":true}`)
	if status != 200 || restarts != 2 || body["clients"] == nil {
		t.Fatal(status, body, restarts)
	}
}
func TestHandlerEmptyModelsConfigUnavailableAndLoopback(t *testing.T) {
	m := testManager(t, "linux")
	cfg := &config.Config{Listen: config.ListenConfig{Host: "127.0.0.1", Port: 8787}}
	cfg.Routing.Mode = "off"
	h := NewHandler(HandlerOptions{Config: func() *config.Config { return cfg }, Manager: m})
	status, body := requestHandler(t, h, "POST", "/api/clients/chatgpt", `{}`)
	if status != 400 || body["error"] != "Configure at least one routed model before connecting." {
		t.Fatal(status, body)
	}
	if exists(m.statePath("chatgpt")) {
		t.Fatal("invalid connect captured state")
	}
	cfg = nil
	status, body = requestHandler(t, h, "GET", "/api/clients", "")
	if status != 503 || body["error"] != "Configuration is unavailable." {
		t.Fatal(status, body)
	}
	r := httptest.NewRequest("POST", "/api/clients/chatgpt", strings.NewReader(`{}`))
	r.RemoteAddr = "203.0.113.7:1000"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestExactLoopbackPortStatus(t *testing.T) {
	m := testManager(t, "linux")
	put(t, m.ClaudeCodeSettingsPath(), `{"env":{"ANTHROPIC_BASE_URL":"http://evil.example:8787","ANTHROPIC_AUTH_TOKEN":"jevonian-local"}}`)
	if m.ClaudeCodeStatus(8787).Status == Connected {
		t.Fatal("external gateway reported connected")
	}
	put(t, m.CodexConfigPath(), setTomlRootString(setTomlRootString("", "openai_base_url", "http://127.0.0.1:87870/v1"), "model_catalog_json", m.CodexCatalogPath()))
	if m.ChatGPTStatus(8787).Status == Connected {
		t.Fatal("wrong port reported connected")
	}
}
