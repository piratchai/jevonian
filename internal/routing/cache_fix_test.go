package routing

import (
	"context"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestCacheReuseAcrossProviderSwitch(t *testing.T) {
	oldBody := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "first"}}}
	newBody := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "first"}, map[string]any{"role": "assistant", "content": "reply"}, map[string]any{"role": "user", "content": "next"}}}
	store := NewSessionStore(600000)
	store.Set("s", SessionState{Provider: "b", Model: "m", UpdatedAt: 1000})
	store.ObserveCache("s", CacheObservation{Provider: "a", Model: "m", At: 1000, CacheReadTokens: 4096, Success: true, UsageKnown: true, Scope: "scope", Prefix: BuildCachePrefix(oldBody)})
	cfg := &config.Config{Providers: []config.Provider{{Name: "b", Type: config.ProviderTypeOpenAI, Models: []config.ModelEntry{{ID: "m"}}}, {Name: "a", Type: config.ProviderTypeOpenAI, Models: []config.ModelEntry{{ID: "m"}}}}}
	dep := Deps{CacheEvidence: func(_, _ string) (string, CachePrefix) { return "scope", BuildCachePrefix(newBody) }}
	d, err := Decide(context.Background(), dep, Input{Config: cfg, Body: newBody, Headers: map[string]string{"x-session-id": "s"}, Store: store, Kind: KindOpenAI, Now: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "a" {
		t.Fatalf("provider=%q, want warm historical target a", d.Provider)
	}
}

func TestSessionStoreDoesNotLoseCacheOnMissingOrStaleSnapshot(t *testing.T) {
	store := NewSessionStore(600000)
	old := CacheObservation{Provider: "a", Model: "m", At: 1000, CacheReadTokens: 4096, Success: true, UsageKnown: true}
	store.Set("s", SessionState{Provider: "a", Model: "m", UpdatedAt: 1000})
	store.ObserveCache("s", old)
	stale, _ := store.Get("s", 1001)
	missing := old
	missing.At = 1002
	missing.CacheReadTokens = 0
	missing.UsageKnown = false
	missing.InputTokens = 0
	missing.UncachedInputTokens = 0
	store.ObserveCache("s", missing)
	stale.UpdatedAt = 1003
	store.Set("s", stale)
	got, _ := store.Get("s", 1004)
	if got.Cache == nil || got.Cache.At != 1002 || got.Cache.CacheReadTokens != 4096 {
		t.Fatalf("legacy observation not reconciled: %+v", got.Cache)
	}
	if got.Caches[PlanKey("a", "m")].CacheReadTokens != 4096 {
		t.Fatalf("missing usage overwrote evidence: %+v", got.Caches)
	}
}
