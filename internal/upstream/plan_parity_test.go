package upstream

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// testdata/plan_ts.json is the output of src/wire.ts planUpstreamWire,
// upstreamUrlFor and wiresOf over every provider type x host x model x client.
func TestPlanUpstreamWireMatchesTS(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan_ts.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Type, BaseURL, Model, Client, Wire, Bridge, URL string
		Err                                             bool
		Wires                                           []string
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	models := []config.ModelEntry{
		{ID: "gpt-5"}, {ID: "claude-sonnet-4-5"}, {ID: "deepseek-chat"},
		{ID: "pinned-ant", Wire: []config.UpstreamWire{"anthropic"}},
		{ID: "pinned-resp", Wire: []config.UpstreamWire{"responses"}},
		{ID: "pinned-both", Wire: []config.UpstreamWire{"openai", "responses", "anthropic"}},
	}
	bad := 0
	for _, r := range rows {
		p := config.Provider{Name: "p", Type: config.ProviderType(r.Type), BaseURL: r.BaseURL, Auth: config.AuthAPIKey, Billing: config.BillingAPI, Models: models}
		plan, err := PlanUpstreamWire(p, ClientKind(r.Client), r.Model)
		if (err != nil) != r.Err {
			bad++
			t.Errorf("%s %s %s %s: err=%v want err=%v", r.Type, r.BaseURL, r.Model, r.Client, err, r.Err)
			continue
		}
		if r.Err {
			continue
		}
		wantBridge := r.Bridge
		if wantBridge == "none" {
			wantBridge = ""
		}
		if string(plan.Wire) != r.Wire || plan.Bridge != wantBridge {
			bad++
			t.Errorf("%s %s %s %s: plan=%+v want wire=%s bridge=%s", r.Type, r.BaseURL, r.Model, r.Client, plan, r.Wire, wantBridge)
		}
		if p.Type != config.ProviderTypeChatGPTWeb {
			if p.Type != config.ProviderTypeChatGPTWeb {
				if u := UpstreamURLFor(p, plan.Wire); u != r.URL {
					bad++
					t.Errorf("%s %s %s %s: url=%s want %s", r.Type, r.BaseURL, r.Model, r.Client, u, r.URL)
				}
			}
		}
		var got []string
		for _, w := range WiresOf(p, r.Model) {
			got = append(got, string(w))
		}
		if !reflect.DeepEqual(got, r.Wires) {
			bad++
			t.Errorf("%s %s %s: wires=%v want %v", r.Type, r.BaseURL, r.Model, got, r.Wires)
		}
		if bad > 15 {
			t.Fatal("too many diffs")
		}
	}
}
