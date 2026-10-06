//go:build windows

package tunnel

import "syscall"

func detachProcAttr() *syscall.SysProcAttr { return nil }
