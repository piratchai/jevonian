//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func detachKevCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killKevProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}
