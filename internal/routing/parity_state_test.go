package routing

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// TestGoldenBrainState compares the exact `state` object sent to the brain
// (everything except routings/candidates payloads, whose per-candidate cache
// arithmetic is covered elsewhere) with what the TS reference sent for the
// same request bodies, then checks the key set of each routing model entry.
func TestGoldenBrainState(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_state_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Kind          string
		Body          map[string]any
		State         map[string]any
		RoutingModels []struct {
			ID     string
			Models [][]string
		}
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfig(map[string]any{
		"defaultProvider": "mock",
		"providers": []any{map[string]any{"name": "mock", "type": "both", "baseUrl": "http://127.0.0.1:9/v1", "apiKey": "k",
			"models": []any{"deepseek-v4-pro", "deepseek-v4.1-flash"}}},
		"routing": map[string]any{"brains": []any{map[string]any{"channel": "typesafe", "apiKeyEnv": "TYPESAFE_API_KEY", "timeoutMs": 1000}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		var sent map[string]any
		scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
			b, _ := json.Marshal(state)
			_ = json.Unmarshal(b, &sent)
			return AskResult{Choice: &Choice{Model: "plan", Confidence: 0.9}}
		})
		_, err := Decide(context.Background(), Deps{Scorer: scorer, Prices: func(m, _ string) *Price { return bundledPrices[m] }},
			Input{Config: &cfg, Body: c.Body, Headers: map[string]string{}, Store: NewSessionStore(60_000), Kind: RequestKind(c.Kind), Now: 1_000_000})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		routings := sent["routings"]
		delete(sent, "routings")
		delete(sent, "candidates")
		if !reflect.DeepEqual(normalize(sent), normalize(c.State)) {
			gb, _ := json.MarshalIndent(normalize(sent), "", " ")
			wb, _ := json.MarshalIndent(normalize(c.State), "", " ")
			t.Errorf("case %d (%s) state mismatch\n got  %s\n want %s", i, c.Kind, gb, wb)
		}
		if i == 0 {
			for j, r := range asArray(routings) {
				if j >= len(c.RoutingModels) {
					break
				}
				for _, m := range asArray(asRecord(r)["models"]) {
					keys := []string{}
					for k := range asRecord(m) {
						keys = append(keys, k)
					}
					t.Logf("routing %s model keys (go): %v; ts: %v", asRecord(r)["id"], keys, c.RoutingModels[j].Models)
					break
				}
			}
		}
	}
}

// normalize round-trips through JSON so int/float and nil/empty slices compare.
func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
