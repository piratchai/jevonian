//go:build darwin

package proxy_test

import (
	"os/exec"
	"testing"

	"github.com/xinyao27/jevonian/internal/proxy"
)

// Integration: exercises the real scutil binary. Skips when scutil is unavailable
// (e.g. restricted CI images). Parsing itself is covered by fixture unit tests.
func TestDetectSystemProxyIntegration(t *testing.T) {
	if _, err := exec.LookPath("scutil"); err != nil {
		t.Skip("scutil not on PATH")
	}
	// Must not panic; nil is fine when the machine has no proxy enabled.
	_ = proxy.DetectSystemProxy()
}
