package config

import "testing"

func TestProvidersForModelDefaultFirst(t *testing.T) {
	cfg := Config{
		DefaultProvider: "b",
		Providers: []Provider{
			{Name: "a", Models: []ModelEntry{{ID: "m"}}},
			{Name: "b", Models: []ModelEntry{{ID: "m"}}},
			{Name: "c", Models: []ModelEntry{{ID: "other"}}},
		},
	}
	got := ProvidersForModel(&cfg, "m")
	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveAPIKey(t *testing.T) {
	t.Setenv("TEST_JEV_KEY", "from-env")
	if ResolveAPIKey(Provider{APIKey: "inline"}) != "inline" {
		t.Fatal("inline")
	}
	if ResolveAPIKey(Provider{APIKeyEnv: "TEST_JEV_KEY"}) != "from-env" {
		t.Fatal("env")
	}
}
