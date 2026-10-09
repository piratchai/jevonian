package upstream

import (
	"os"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// TestProbeConfiguredWires prints each configured provider's recorded cache
// convention so a migration can verify historical rows.
// It skips when the developer has no config on disk.
func TestProbeConfiguredWires(t *testing.T) {
	cfg, _, err := config.Load()
	if err != nil {
		t.Skip("no config")
	}
	classify := ConventionClassifierFor(&cfg)
	for _, p := range cfg.Providers {
		if os.Getenv("PROBE_PROVIDER") != "" && os.Getenv("PROBE_PROVIDER") != p.Name {
			continue
		}
		for i, m := range p.Models {
			if i > 3 {
				break
			}
			plan, err := PlanUpstreamWire(p, KindOpenAI, m.ID)
			if err != nil {
				continue
			}
			now, _ := classify(p.Name, m.ID, "/chat/completions", true, time.Now())
			legacy, _ := classify(p.Name, m.ID, "/chat/completions", true, GoEngineCutover.Add(-time.Hour))
			t.Logf("%-28s %-34s type=%-10s client=openai -> wire=%s bridge=%q go-era-exclusive=%v legacy-exclusive=%v",
				p.Name, m.ID, p.Type, plan.Wire, plan.Bridge, now, legacy)
		}
	}
}
