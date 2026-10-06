package routing

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// TestGoldenConfigRouting replays testdata/ts_config_routing_golden.json — the
// routing block src/config.ts parseConfig produced for the same raw config —
// through config.ParseConfig. Covers builtin routings, custom ids, the slug
// rule, the legacy `tiers` mirror, `providerOrder`, capacities and aliases
// (checklist 3.1 / 3.2 config items). Regenerate with /tmp/parity/gen5.mts.
func TestGoldenConfigRouting(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_config_routing_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name string
		Raw  map[string]any
		Want struct {
			Error        string
			Routing      map[string]any
			ModelAliases map[string]any
		}
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg, err := config.ParseConfig(c.Raw)
			if c.Want.Error != "" {
				if err == nil {
					t.Fatalf("want error %q, parsed OK", c.Want.Error)
				}
				if err.Error() != c.Want.Error {
					t.Fatalf("error = %q want %q", err.Error(), c.Want.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			b, _ := json.Marshal(cfg.Routing)
			var got map[string]any
			_ = json.Unmarshal(b, &got)
			for _, k := range []string{"mode", "routings", "tiers", "sessionTtlMinutes", "baselineModel", "capacities", "defaultEffort", "brainPicksEffort"} {
				g, w := got[k], c.Want.Routing[k]
				if !reflect.DeepEqual(g, w) {
					t.Errorf("routing.%s:\n got  %v\n want %v", k, g, w)
				}
			}
			ab, _ := json.Marshal(cfg.ModelAliases)
			var aliases map[string]any
			_ = json.Unmarshal(ab, &aliases)
			if len(aliases) == 0 {
				aliases = nil
			}
			if !reflect.DeepEqual(aliases, c.Want.ModelAliases) {
				t.Errorf("modelAliases: got %v want %v", aliases, c.Want.ModelAliases)
			}
		})
	}
}
