package upstream

import (
	"os"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// TestProbeConfiguredWires prints each configured provider's model wires so a
// migration can classify historical rows by their real serving convention.
// It skips when the developer has no config on disk.
func TestProbeConfiguredWires(t *testing.T) {
	cfg, _, err := config.Load()
	if err != nil {
		t.Skip("no config")
	}
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
			exc, ok := ExclusiveInputFor(p, m.ID)
			t.Logf("%-28s %-34s type=%-10s client=openai -> wire=%s bridge=%q exclusive=%v ok=%v", p.Name, m.ID, p.Type, plan.Wire, plan.Bridge, exc, ok)
		}
	}
}
