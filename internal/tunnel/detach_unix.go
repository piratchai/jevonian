//go:build !windows

package tunnel

import "syscall"

// detachProcAttr starts the tunnel in its own process group so a group
// SIGTERM reaps the `sh -c` wrapper and the binary together, and a Ctrl-C to
// serve does not take an adopted tunnel down with it.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
