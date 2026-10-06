//go:build windows

package cli

import "syscall"

func detachProcAttr() *syscall.SysProcAttr { return nil }
