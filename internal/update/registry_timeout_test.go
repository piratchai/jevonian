package update

import (
	"testing"
	"time"
)

func TestRegistryTimeoutEnv(t *testing.T) {
	cases := map[string]time.Duration{
		"":        30 * time.Second,
		"abc":     30 * time.Second,
		"5000":    5 * time.Second,
		"10":      time.Second,     // clamped up to 1s
		"9999999": 5 * time.Minute, // clamped down to 5m
		" 2000 ":  2 * time.Second,
	}
	for raw, want := range cases {
		t.Setenv("JEVONIAN_REGISTRY_TIMEOUT_MS", raw)
		if got := RegistryTimeout(); got != want {
			t.Errorf("%q => %v want %v", raw, got, want)
		}
	}
}
