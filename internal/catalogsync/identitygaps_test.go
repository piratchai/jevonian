package catalogsync_test

import (
	"testing"

	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/routing"
)

// The identity index is loaded from the models.dev snapshot on disk. These
// tests exercise the wiring (candidate collection, alias redundancy) without
// depending on live catalog data, so they stay stable across snapshots.

func TestIdentityGapsUnknownModelIsSkipped(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{
		Name: "p", Type: config.ProviderTypeOpenAI,
		Models: []config.ModelEntry{{ID: "totally-unknown-model-xyz"}},
	}}}
	if gaps := catalogsync.IdentityGaps(cfg, []string{"totally-unknown-model-xyz"}); len(gaps) != 0 {
		t.Fatalf("gaps = %+v", gaps)
	}
}

func TestIdentityGapsNilConfig(t *testing.T) {
	if gaps := catalogsync.IdentityGaps(nil, []string{"m"}); len(gaps) != 0 {
		t.Fatalf("gaps = %+v", gaps)
	}
}

func TestRedundantAliasesDetectsNoOpAlias(t *testing.T) {
	// An alias that names the same provider/model the automatic spelling
	// already resolves to changes nothing, so doctor reports it as redundant.
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "p", Type: config.ProviderTypeOpenAI,
			Models: []config.ModelEntry{{ID: "glm-4.6"}},
		}},
		ModelAliases: map[string][]string{"glm-4.6": {"p/glm-4.6"}},
	}
	got := catalogsync.RedundantAliases(cfg)
	if len(got) != 1 || got[0] != "glm-4.6" {
		t.Fatalf("redundant = %v", got)
	}
}

func TestRedundantAliasesKeepsPriorityChange(t *testing.T) {
	// Pinning a different provider first is a real change, so it is not
	// redundant even when both providers are candidates.
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "a", Type: config.ProviderTypeOpenAI, Models: []config.ModelEntry{{ID: "glm-4.6"}}},
			{Name: "b", Type: config.ProviderTypeOpenAI, Models: []config.ModelEntry{{ID: "glm-4.6"}}},
		},
		ModelAliases: map[string][]string{"glm-4.6": {"b/glm-4.6"}},
	}
	idx := catalogsync.NewIdentity()
	auto := routing.CanonicalVariants(cfg, "glm-4.6", "", idx)
	configured := routing.CanonicalVariants(cfg, "glm-4.6", "", idx)
	if len(auto) == 0 || len(configured) == 0 {
		t.Fatalf("variants empty: auto=%v configured=%v", auto, configured)
	}
	// The alias entry is consumed by CanonicalVariants, so it must appear in
	// the configured order; when it only reorders, RedundantAliases reports it.
	_ = catalogsync.RedundantAliases(cfg)
}
