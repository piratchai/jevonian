//go:build windows

package tunnel

import (
	"golang.org/x/sys/windows"
	"os"
)

// Windows does not support POSIX process-group signals. Termination acts on
// the identified process; status uses a query handle rather than treating every
// positive PID as alive.
func terminateProcess(pid int) error { return killProcess(pid) }
func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	return windows.GetExitCodeProcess(handle, &code) == nil && code == 259
}
