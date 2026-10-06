package routing

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// TestGoldenCacheEvidence replays testdata/ts_cache_golden.json: for each
// (body size, clock, stored session cache observation, ttl) the TS router's
// chosen target, reason, cache evidence, switch penalty and the exact
// per-candidate cache view the brain was sent. Checklist 3.2 "Cache affinity
// (measured)" and "Cache evidence in brain state". Regenerate: gen7.mts.
func TestGoldenCacheEvidence(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_cache_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Body map[string]any
		Now  int64
		TTL  int64
		Prev *struct {
			Provider, Model string
			Cache           *struct {
				Provider, Model string
				AgoMs           int64
				Unc, Read       int
				Write           int
				Success         bool
			}
		}
		Want map[string]any
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	prov := func(name string, models ...string) map[string]any {
		ms := make([]any, len(models))
		for i, m := range models {
			ms[i] = m
		}
		return map[string]any{"name": name, "type": "openai", "baseUrl": "http://127.0.0.1:9/" + name + "/v1", "apiKey": "k", "models": ms}
	}
	cfg, err := config.ParseConfig(map[string]any{
		"defaultProvider": "a",
		"providers": []any{
			prov("a", "deepseek-v4-pro", "deepseek-v4.1-flash", "zz-unpriced"),
			prov("b", "deepseek-v4-pro", "deepseek-v4.1-flash"),
		},
		"routing": map[string]any{
			"brains": []any{map[string]any{"channel": "typesafe", "apiKeyEnv": "TYPESAFE_API_KEY", "timeoutMs": 1000}},
			"routings": []any{
				map[string]any{"id": "plan", "label": "Plan", "models": []any{"deepseek-v4-pro", "zz-unpriced"}},
				map[string]any{"id": "execute", "label": "Execute", "models": []any{"deepseek-v4.1-flash"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		var sent map[string]any
		scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
			sent = normalize(state).(map[string]any)
			return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.9}}
		})
		store := NewSessionStore(10 * 60 * 60_000)
		if c.Prev != nil {
			store.Set("s1", SessionState{Phase: "plan", Model: c.Prev.Model, Provider: c.Prev.Provider, Turns: 1, UpdatedAt: c.Now})
			if k := c.Prev.Cache; k != nil {
				store.ObserveCache("s1", CacheObservation{Provider: k.Provider, Model: k.Model, At: c.Now - k.AgoMs,
					UncachedInputTokens: k.Unc, CacheReadTokens: k.Read, CacheWriteTokens: k.Write, Success: k.Success, UsageKnown: true})
			}
		}
		in := Input{Config: &cfg, Body: c.Body, Headers: map[string]string{"x-session-id": "s1"}, Store: store,
			Kind: KindOpenAI, Now: c.Now, CacheTTL: time.Duration(c.TTL) * time.Millisecond}
		d, err := Decide(context.Background(), Deps{Scorer: scorer, Prices: func(m, _ string) *Price { return bundledPrices[m] }}, in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got := map[string]any{"model": d.Model, "provider": d.Provider, "reason": d.Reason, "cacheKeep": string(d.CacheKeep)}
		got["cache"], got["switchPenaltyUsd"] = nil, nil
		if d.Cache != nil {
			got["cache"] = normalize(d.Cache)
		}
		if d.SwitchPenaltyUSD != nil {
			got["switchPenaltyUsd"] = *d.SwitchPenaltyUSD
		}
		var routings []any
		for _, r := range asArray(sent["routings"]) {
			var models []any
			for _, m := range asArray(asRecord(r)["models"]) {
				mm := asRecord(m)
				models = append(models, map[string]any{"model": mm["model"], "provider": mm["provider"], "rank": mm["preference_rank"], "cache": mm["cache"], "switchPenaltyUsd": mm["switchPenaltyUsd"]})
			}
			routings = append(routings, map[string]any{"id": asRecord(r)["id"], "models": models})
		}
		got["routings"] = routings
		var cands []any
		for _, m := range asArray(sent["candidates"]) {
			mm := asRecord(m)
			cands = append(cands, map[string]any{"model": mm["model"], "provider": mm["provider"], "cache": mm["cache"], "switchPenaltyUsd": mm["switchPenaltyUsd"]})
		}
		got["candidates"] = cands
		want := map[string]any{}
		for _, k := range []string{"model", "provider", "reason", "cacheKeep", "cache", "switchPenaltyUsd", "routings", "candidates"} {
			want[k] = c.Want[k]
		}
		if !jsonClose(normalize(got), normalize(want)) {
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			t.Errorf("case %d (now=%d ttl=%d prev=%+v):\n got  %s\n want %s", i, c.Now, c.TTL, c.Prev, gb, wb)
		}
	}
}

// jsonClose is DeepEqual with a 1e-9 tolerance on floats (JS and Go sum the
// same terms in the same order, but keep the check robust to last-bit noise).
func jsonClose(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || math.Abs(x-y) <= 1e-9*math.Max(1, math.Abs(y)))
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if !jsonClose(v, y[k]) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonClose(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// TestEstimateTokensSurrogateRounding pins a case where adding 1.8 per non-BMP
// rune (instead of the two 0.9 UTF-16 units TS charges) rounded the ceil()
// up by one. Expected value comes from src/compaction.ts estimateTokens.
func TestEstimateTokensSurrogateRounding(t *testing.T) {
	text := `{"i":16,"role":"user","text":"{} {} line\nbreak beta line\nbreak {} é 😀 beta alpha é alpha é é alpha 42 😀 x line\nbreak line\nbreak alpha é foo.ts alpha {} x gamma foo.ts 😀 é line\nbreak é 42 😀 😀 gamma x é"}`
	if got := EstimateTokens(text); got != 79 {
		t.Fatalf("EstimateTokens = %d, want 79", got)
	}
}
