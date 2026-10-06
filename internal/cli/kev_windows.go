//go:build windows

package cli

import (
	"os"
	"os/exec"
)

func detachKevCmd(cmd *exec.Cmd) {}

func killKevProcessGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
