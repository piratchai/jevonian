package routing

import (
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// Port of src/affinity.test.ts describe("applyCacheKeep") plus a guard-aware
// ordering check from src/routing.ts standingOf (breaker open / saturated host
// is ordered like a spent one).

func affinityPicks(providers ...string) []TierPick {
	out := make([]TierPick, len(providers))
	for i, p := range providers {
		out[i] = TierPick{Provider: p, Model: "glm-5.2"}
	}
	return out
}

func warmSession(read int) *SessionState {
	return &SessionState{Phase: "plan", Provider: "warm", Model: "glm-5.2", Turns: 2, UpdatedAt: 1_000_000,
		Cache: &CacheObservation{Provider: "warm", Model: "glm-5.2", At: 1_000_000, UncachedInputTokens: 100,
			CacheReadTokens: read, Success: true}}
}

func keepFor(prev *SessionState) CacheKeep {
	return cacheAffinityKeep(prev, AffinityAuto, false, affinityPicks("warm", "other"), 1_000_000, 0)
}

func TestApplyCacheKeepMovesAnswererToFront(t *testing.T) {
	prev := warmSession(CacheWorthTokens)
	out := applyCacheKeep(affinityPicks("cold", "warm", "other"), prev, keepFor(prev))
	if out[0].Provider != "warm" || out[1].Provider != "cold" || out[2].Provider != "other" {
		t.Fatalf("order = %+v", out)
	}
}

func TestApplyCacheKeepNoVerdictLeavesOrder(t *testing.T) {
	prev := warmSession(0)
	in := affinityPicks("cold", "warm", "other")
	out := applyCacheKeep(in, prev, keepFor(prev))
	if &out[0] != &in[0] {
		t.Fatalf("order changed: %+v", out)
	}
}

func TestApplyCacheKeepAlreadyFirstUntouched(t *testing.T) {
	prev := warmSession(CacheWorthTokens)
	in := affinityPicks("warm", "cold")
	out := applyCacheKeep(in, prev, keepFor(prev))
	if &out[0] != &in[0] {
		t.Fatalf("order changed: %+v", out)
	}
}

func TestOrderByQuotaResetGuardSpentGoesLast(t *testing.T) {
	cfg := defaultCfg()
	cfg.Providers = []config.Provider{provider("wedged", "glm-5.2"), provider("fine", "glm-5.2")}
	picks := []TierPick{{Provider: "wedged", Model: "glm-5.2"}, {Provider: "fine", Model: "glm-5.2"}}
	deps := Deps{Guard: GuardViewFunc(func(p string, _ int64) bool { return p == "wedged" })}
	out := OrderByQuotaReset(picks, cfg, deps, orderOptions{now: 1_000})
	if out[0].Provider != "fine" || out[1].Provider != "wedged" {
		t.Fatalf("order = %+v", out)
	}
}
