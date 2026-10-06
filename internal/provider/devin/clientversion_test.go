package devin

import "testing"

func TestClientVersionEnvOverride(t *testing.T) {
	t.Setenv("JEVONIAN_DEVIN_CLIENT_VERSION", "")
	if clientVersion() != defaultClientVersion {
		t.Fatalf("default %q", clientVersion())
	}
	t.Setenv("JEVONIAN_DEVIN_CLIENT_VERSION", "  9.9.9  ")
	if clientVersion() != "9.9.9" {
		t.Fatalf("override %q", clientVersion())
	}
}
