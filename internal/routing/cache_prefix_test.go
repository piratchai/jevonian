package routing

import (
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestCachePrefixComparison(t *testing.T) {
	first := BuildCachePrefix(map[string]any{
		"model": "a", "stream": true, "max_tokens": 100,
		"system": "stable system", "messages": []any{map[string]any{"role": "user", "content": "one"}},
	})
	continued := BuildCachePrefix(map[string]any{
		"model": "b", "stream": false, "max_tokens": 200,
		"system": "stable system", "messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "assistant", "content": "two"},
		},
	})
	if got := CompareCachePrefix(first, continued); got != "extends" {
		t.Fatalf("CompareCachePrefix() = %q, want extends", got)
	}
	changed := BuildCachePrefix(map[string]any{
		"system": "changed", "messages": []any{map[string]any{"role": "user", "content": "one"}},
	})
	if got := CompareCachePrefix(first, changed); got != "changed" {
		t.Fatalf("CompareCachePrefix() = %q, want changed", got)
	}
	if got := CompareCachePrefix(CachePrefix{}, continued); got != "unknown" {
		t.Fatalf("missing evidence comparison = %q, want unknown", got)
	}
}

func TestCachePrefixIncludesPromptAffectingFieldsAndStoresNoPrompt(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"content": "private prompt"}}}
	prefix := BuildCachePrefix(body)
	if !prefix.Known {
		t.Fatal("expected known message evidence")
	}
	if prefix.MessageHash[0] == "private prompt" {
		t.Fatal("stored raw prompt")
	}
	withToolChange := BuildCachePrefix(map[string]any{
		"messages": body["messages"], "tools": []any{map[string]any{"name": "search"}},
	})
	if got := CompareCachePrefix(prefix, withToolChange); got != "changed" {
		t.Fatalf("tool change comparison = %q, want changed", got)
	}
}

func TestCacheScopeChangesWithProviderAndClientKind(t *testing.T) {
	provider := config.Provider{Name: "p", APIKey: "secret", BaseURL: "https://example.test"}
	base := CacheScope(provider, "openai", "secret")
	changed := provider
	changed.APIKey = "rotated"
	if CacheScope(changed, "openai", "rotated") == base {
		t.Fatal("credential change did not change scope")
	}
	if CacheScope(provider, "anthropic", "secret") == base {
		t.Fatal("client kind change did not change scope")
	}
	if base == provider.APIKey {
		t.Fatal("scope exposed credential")
	}
}
