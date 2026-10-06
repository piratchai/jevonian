package tunnel

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Host is the OS process surface the manager drives. Injectable so tests
// never touch real processes.
type Host interface {
	// Alive reports whether pid exists.
	Alive(pid int) bool
	// Identify reports whether pid's command line runs command.
	Identify(pid int, command string) bool
	// Terminate sends SIGTERM (to the process group when group) and escalates
	// to SIGKILL shortly after.
	Terminate(pid int, group bool)
	// Find lists pids whose command line runs command; own pid excluded.
	// Return nil when the platform cannot enumerate.
	Find(command string) []int
}

// SystemHost is the real process host (ps + signals).
type SystemHost struct {
	// KillGrace is how long SIGTERM gets before SIGKILL (default 2s).
	KillGrace time.Duration
}

var psLineRe = regexp.MustCompile(`^\s*(\d+)\s+(.+)$`)

// Alive implements Host.
func (SystemHost) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return processAlive(pid)
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Identify implements Host.
func (SystemHost) Identify(pid int, command string) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return CommandMatches(string(out), command)
}

// Find implements Host.
func (SystemHost) Find(command string) []int {
	if runtime.GOOS == "windows" {
		return nil
	}
	// pid + full command line for every process of this user. -ww keeps ps from
	// truncating the line, which is what hides the args we match on.
	out, err := exec.Command("ps", "-ww", "-x", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		m := psLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		if CommandMatches(m[2], command) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Terminate implements Host.
func (h SystemHost) Terminate(pid int, group bool) {
	if pid <= 0 {
		return
	}
	targets := []int{pid}
	if group && runtime.GOOS != "windows" {
		targets = []int{-pid, pid}
	}
	sent := false
	for _, target := range targets {
		if terminateProcess(target) == nil {
			sent = true
			break
		}
	}
	if !sent {
		return
	}
	// cloudflared (and other tunnel daemons) can stall or ignore SIGTERM during
	// a graceful shutdown while holding dozens of upstream connections. Escalate
	// to SIGKILL shortly after so a restart does not leave an orphan behind.
	grace := h.KillGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	time.AfterFunc(grace, func() {
		for _, target := range targets {
			if processAlive(target) {
				_ = killProcess(target)
			}
		}
	})
}
