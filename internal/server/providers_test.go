package server_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/server"
)

func TestWorkbuddyThroughInferenceSurface(t *testing.T) {
	session := filepath.Join(t.TempDir(), "workbuddy.json")
	if err := os.WriteFile(session, []byte(`{"uid":"account-two","accessToken":"session-token","domain":"tenant.example"}`), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer session-token" || r.Header.Get("X-User-Id") != "account-two" {
			t.Errorf("auth headers=%v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["stream"] != true {
			t.Errorf("WorkBuddy must always stream: %v", body)
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
			t.Errorf("system prompt not injected: %v", messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"hello\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := config.Config{Providers: []config.Provider{{Name: "workbuddy", Type: config.ProviderTypeOpenAI, Auth: config.AuthOAuth, OAuthSource: config.OAuthWorkbuddyAI, BaseURL: upstream.URL + "/v2", Login: &config.ProviderLogin{CredentialsPath: session}, Models: []config.ModelEntry{{ID: "shared-model"}}}}}
	srv := server.New("", server.Deps{Config: &cfg})
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		payload := `{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`
		if endpoint == "/v1/responses" {
			payload = `{"model":"shared-model","input":"hello"}`
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", endpoint, strings.NewReader(payload)))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "lookup") || !strings.Contains(rr.Body.String(), "hello") {
			t.Fatalf("%s: %d %s", endpoint, rr.Code, rr.Body.String())
		}
	}
}

func TestDevinLeadingRefusalFailsOverThroughInferenceSurface(t *testing.T) {
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/GetChatMessage") || r.Header.Get("Content-Type") != "application/connect+proto" {
			t.Errorf("not Connect request: %s %v", r.URL.Path, r.Header)
		}
		frame := []byte(`{"error":{"code":"resource_exhausted","message":"Reached free model rate limit; reset in 2 hours"}}`)
		prefix := make([]byte, 5)
		prefix[0] = 2
		binary.BigEndian.PutUint32(prefix[1:], uint32(len(frame)))
		w.Write(prefix)
		w.Write(frame)
	}))
	defer rejected.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}]}`)
	}))
	defer healthy.Close()
	cfg := config.Config{DefaultProvider: "devin", Providers: []config.Provider{
		{Name: "devin", Type: config.ProviderTypeDevin, Auth: config.AuthOAuth, OAuthSource: config.OAuthStatic, APIKey: "devin-token", BaseURL: rejected.URL, Models: []config.ModelEntry{{ID: "shared-model"}}},
		{Name: "healthy", Type: config.ProviderTypeOpenAI, APIKey: "key", BaseURL: healthy.URL, Models: []config.ModelEntry{{ID: "shared-model"}}},
	}}
	rr := httptest.NewRecorder()
	server.New("", server.Deps{Config: &cfg}).Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`)))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "healthy") || rr.Header().Get("X-Jevonian-Provider") != "healthy" {
		t.Fatalf("%d %s %v", rr.Code, rr.Body.String(), rr.Header())
	}
}
