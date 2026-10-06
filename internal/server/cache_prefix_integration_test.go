package server_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/server"
)

func TestServerRecordsCachePrefixEvidence(t *testing.T) {
	up := &recordingUpstream{}
	upstreamServer := up.serve(t, okResponder)
	cfg := baseCfg(upstreamServer.URL, "m")
	cfg.TokenSaver = config.TokenSaverConfig{}
	cfg.Routing.Mode = "auto"
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", APIKeyEnv: "WIRING_TEST_BRAIN_KEY"}}
	cfg.Routing.Routings[0].Models = []string{"m"}
	store := routing.NewSessionStore(60_000)
	store.Set("prefix-test", routing.SessionState{Provider: "p1", Model: "m", UpdatedAt: time.Now().UnixMilli()})
	handler := server.New("", server.Deps{Config: &cfg, Store: store}).Handler()
	body := map[string]any{
		"model":    "m",
		"system":   "stable system",
		"messages": []any{map[string]any{"role": "user", "content": "private request text"}},
	}
	response := postJSON(t, handler, "/v1/chat/completions", body, map[string]string{"x-jevonian-session": "prefix-test"})
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	state, ok := store.Get("prefix-test", time.Now().UnixMilli())
	if !ok || state.Cache == nil {
		t.Fatal("successful request did not record a cache observation")
	}
	if !state.Cache.Prefix.Known || len(state.Cache.Prefix.MessageHash) != 1 {
		t.Fatalf("prefix evidence = %#v", state.Cache.Prefix)
	}
	if strings.Contains(state.Cache.Prefix.MessageHash[0], "private request text") {
		t.Fatal("cache evidence contains raw prompt text")
	}
	provider := cfg.Providers[0]
	// No keys are configured, so authorize() labels the turn "unauthenticated".
	wantScope := routing.CacheScope(provider, string(server.KindOpenAI)+"\x00"+"unauthenticated"+"\x00"+promptAndSaverJSON(cfg.PromptPolicy, cfg.TokenSaver), config.ResolveAPIKey(provider))
	if state.Cache.Scope == "" || state.Cache.Scope != wantScope {
		t.Fatal("cache observation has wrong provider/protocol/policy scope")
	}
}

func promptAndSaverJSON(policy config.PromptPolicyConfig, tokenSaver config.TokenSaverConfig) string {
	data, _ := json.Marshal(struct {
		Prompt     config.PromptPolicyConfig
		TokenSaver config.TokenSaverConfig
	}{policy, tokenSaver})
	return string(data)
}
