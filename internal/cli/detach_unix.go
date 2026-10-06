//go:build !windows

package cli

import "syscall"

// detachProcAttr starts the relaunched server in its own process group so the
// exiting parent cannot take it down with a group signal.
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
