package routing

import (
	"context"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestDecideReusesHistoricalTargetWithPrefixEvidence(t *testing.T) {
	body := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	prefix := BuildCachePrefix(body)
	for _, test := range []struct {
		name                string
		changed, stale, off bool
		want                string
	}{
		{name: "pinned model does not switch for cache", want: "b"},
		{name: "changed prefix", changed: true, want: "b"},
		{name: "expired cache", stale: true, want: "b"},
		{name: "affinity disabled", off: true, want: "b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewSessionStore(600000)
			store.Set("s", SessionState{Provider: "a", Model: "m", UpdatedAt: 1000})
			store.ObserveCache("s", CacheObservation{Provider: "a", Model: "m", At: 1000, CacheReadTokens: 4096, Success: true, UsageKnown: true, Scope: "scope", Prefix: prefix})
			store.Retarget("s", "b", "m", 1001)
			cfg := &config.Config{Providers: []config.Provider{{Name: "b", Type: config.ProviderTypeOpenAI, Models: []config.ModelEntry{{ID: "m"}}}, {Name: "a", Type: config.ProviderTypeOpenAI, Models: []config.ModelEntry{{ID: "m"}}}}}
			current := body
			if test.changed {
				current = map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "different"}}}
			}
			now := int64(2000)
			if test.stale {
				now = 301000
			}
			headers := map[string]string{"x-session-id": "s"}
			if test.off {
				headers[RequestHeaderAffinity] = "off"
			}
			d, err := Decide(context.Background(), Deps{CacheEvidence: func(_, _ string) (string, CachePrefix) { return "scope", BuildCachePrefix(current) }}, Input{Config: cfg, Body: current, Headers: headers, Store: store, Kind: KindOpenAI, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if d.Provider != test.want {
				t.Fatalf("provider=%s want=%s", d.Provider, test.want)
			}
		})
	}
}

func TestSessionStoreRetainsCacheAcrossTargets(t *testing.T) {
	store := NewSessionStore(600000)
	store.Set("s", SessionState{Provider: "a", Model: "m", UpdatedAt: 100})
	store.ObserveCache("s", CacheObservation{Provider: "a", Model: "m", At: 100, CacheReadTokens: 2048, Success: true, UsageKnown: true})
	store.Retarget("s", "b", "m", 101)
	store.ObserveCache("s", CacheObservation{Provider: "b", Model: "m", At: 101, CacheReadTokens: 4096, Success: true, UsageKnown: true})
	state, _ := store.Get("s", 102)
	if len(state.Caches) != 2 {
		t.Fatalf("cache targets = %d, want 2", len(state.Caches))
	}
	a := state.Caches[PlanKey("a", "m")]
	if a.CacheReadTokens != 2048 {
		t.Fatalf("lost A observation: %+v", a)
	}
	// Returned state must not expose mutable store data.
	delete(state.Caches, PlanKey("a", "m"))
	next, _ := store.Get("s", 103)
	if len(next.Caches) != 2 {
		t.Fatal("Get exposed mutable history")
	}
	// A delayed completion must stay attached to A, not replace B's latest usage.
	store.ObserveCache("s", CacheObservation{Provider: "a", Model: "m", At: 104, CacheReadTokens: 3000, Success: true, UsageKnown: true})
	next, _ = store.Get("s", 105)
	if next.Cache.Provider != "b" || next.Caches[PlanKey("a", "m")].CacheReadTokens != 3000 {
		t.Fatalf("delayed completion corrupted targets: %+v", next)
	}
}
