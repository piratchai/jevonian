//go:build !windows

package tunnel

import "syscall"

func terminateProcess(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
func killProcess(pid int) error      { return syscall.Kill(pid, syscall.SIGKILL) }
func processAlive(pid int) bool      { return syscall.Kill(pid, 0) == nil }
