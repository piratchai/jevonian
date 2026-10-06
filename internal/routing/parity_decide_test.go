package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// TestGoldenDecide replays testdata/ts_decide_golden.json — decideRoute output
// from the TS reference on the same config/body/headers/brain answer — through
// Decide and compares every routing-visible field.
func TestGoldenDecide(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_decide_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name string
		Scen struct {
			Config     map[string]any
			Body       map[string]any
			Headers    map[string]string
			Kind       string
			Choice     string
			Confidence *float64
			Effort     string
			BrainFail  bool
		}
		Want struct {
			Error           string
			Status          int
			Model           string
			Provider        string
			Phase           string
			Reason          string
			Effort          string
			EffortNote      string
			Virtual         bool
			Routed          bool
			Canonical       string
			ContextOverflow bool
			Skipped         []string
			CacheKeep       string
			Brain           string
			Order           []string
		}
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg, err := config.ParseConfig(c.Scen.Config)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			kind := KindOpenAI
			if c.Scen.Kind != "" {
				kind = RequestKind(c.Scen.Kind)
			}
			scen := c.Scen
			scorer := ScorerFunc(func(_ context.Context, _ config.BrainConfig, state map[string]any, _ bool) AskResult {
				if scen.BrainFail {
					return AskResult{Failure: &Failure{Status: 500, Error: "no"}}
				}
				var ids []string
				for _, r := range asArray(state["routings"]) {
					ids = append(ids, asRecord(r)["id"].(string))
				}
				choice := scen.Choice
				if choice == "plan" || choice == "execute" {
					if !contains(ids, choice) {
						choice = ids[0]
					}
				} else if choice == "" {
					choice = ids[0]
				}
				conf := 0.9
				if scen.Confidence != nil {
					conf = *scen.Confidence
				}
				return AskResult{Choice: &Choice{Model: choice, Confidence: conf, Effort: scen.Effort}}
			})
			deps := Deps{Scorer: scorer, Sleep: func(_ time.Duration) {},
				Prices: func(m, _ string) *Price { return bundledPrices[m] }}
			d, err := Decide(context.Background(), deps, Input{
				Config: &cfg, Body: c.Scen.Body, Headers: nonNilHeaders(c.Scen.Headers),
				Store: NewSessionStore(60_000), Kind: kind, Now: 1_000_000, RequestID: "rid",
			})
			if c.Want.Error != "" {
				if err == nil {
					t.Fatalf("want error %q, got decision %+v", c.Want.Error, d)
				}
				if err.Error() != c.Want.Error {
					t.Fatalf("error = %q want %q", err.Error(), c.Want.Error)
				}
				if re, ok := err.(*RouteError); ok && re.Status != c.Want.Status {
					t.Fatalf("status = %d want %d", re.Status, c.Want.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			skipped := []string{}
			for _, s := range d.Skipped {
				skipped = append(skipped, fmt.Sprintf("%s/%s/%s", s.Provider, s.Model, s.Reason))
			}
			order := []string{}
			for _, o := range d.Order {
				order = append(order, o.Provider+"/"+o.Model)
			}
			got := map[string]any{
				"model": d.Model, "provider": d.Provider, "phase": d.Phase, "reason": d.Reason,
				"effort": d.Effort, "effortNote": d.EffortNote, "virtual": d.Virtual, "routed": d.Routed,
				"canonical": d.Canonical, "overflow": d.ContextOverflow, "skipped": skipped,
				"keep": string(d.CacheKeep), "brain": string(d.Brain), "order": order,
			}
			w := c.Want
			if w.Skipped == nil {
				w.Skipped = []string{}
			}
			if w.Order == nil {
				w.Order = []string{}
			}
			want := map[string]any{
				"model": w.Model, "provider": w.Provider, "phase": w.Phase, "reason": w.Reason,
				"effort": w.Effort, "effortNote": w.EffortNote, "virtual": w.Virtual, "routed": w.Routed,
				"canonical": w.Canonical, "overflow": w.ContextOverflow, "skipped": w.Skipped,
				"keep": w.CacheKeep, "brain": w.Brain, "order": w.Order,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("\n got  %v\n want %v", got, want)
			}
		})
	}
}

func nonNilHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}
