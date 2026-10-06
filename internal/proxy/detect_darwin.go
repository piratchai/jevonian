//go:build darwin

package proxy

import (
	"context"
	"os/exec"
	"time"
)

// DetectSystemProxy reads macOS's system proxy via `scutil --proxy`.
// Other platforms have no cheap equivalent; see detect_other.go.
func DetectSystemProxy() *SystemProxy {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "scutil", "--proxy")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return ParseScutilProxy(string(out))
}
