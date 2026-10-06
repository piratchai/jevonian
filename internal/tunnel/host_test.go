//go:build !windows

package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A harmless local `sh` stands in for the tunnel binary: no real tunnel is ever
// started. The child ignores SIGTERM, so only the SIGKILL escalation reaps it.
func TestSystemHostEscalatesHungProcessToSIGKILL(t *testing.T) {
	child, err := ExecSpawn(context.Background(),
		Command{Name: "sh", Args: []string{"-c", "trap '' TERM; while :; do sleep 0.05; done"}},
		SpawnOptions{Env: os.Environ(), Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := child.PID()
	host := SystemHost{KillGrace: 300 * time.Millisecond}
	time.Sleep(150 * time.Millisecond) // let sh install the trap
	if !host.Alive(pid) {
		t.Fatal("child should be alive")
	}
	host.Terminate(pid, true)
	time.Sleep(100 * time.Millisecond)
	if !host.Alive(pid) {
		t.Fatal("SIGTERM was ignored; child must survive the grace period")
	}
	select {
	case <-child.Wait():
	case <-time.After(5 * time.Second):
		t.Fatal("hung child was not SIGKILLed after the grace period")
	}
}

func TestSystemHostFindAndIdentifyMatchWrappedCommand(t *testing.T) {
	marker := "sleep 31337.25"
	cmd := Command{Name: "sh", Args: []string{"-c", marker}}
	child, err := ExecSpawn(context.Background(), cmd, SpawnOptions{Env: os.Environ(), Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := child.PID()
	host := SystemHost{KillGrace: 200 * time.Millisecond}
	defer host.Terminate(pid, true)
	// `sh -c "sleep N"` execs into sleep, so ps shows the unwrapped command.
	waitFor(t, "identify", func() bool { return host.Identify(pid, cmd.Label()) })
	found := false
	for _, p := range host.Find(cmd.Label()) {
		if p == pid {
			found = true
		}
		if p == os.Getpid() {
			t.Fatal("Find must exclude own pid")
		}
	}
	if !found {
		t.Fatal("Find did not locate the wrapped process")
	}
	if host.Identify(pid, "sh -c sleep 999") {
		t.Fatal("Identify matched an unrelated command")
	}
}

// Detached start => process-group leader, so a group SIGTERM reaps the wrapper
// and the binary together; and the child survives the manager (restart adoption).
func TestExecSpawnDetachedOwnsProcessGroup(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tunnel.log")
	cfg := config.TunnelConfig{Enabled: true, Provider: "custom",
		Command: "echo https://fake.example.com; sleep 31338.5"}
	m := NewManager(cfg, 8787, Options{LogPath: logPath, Host: SystemHost{KillGrace: 200 * time.Millisecond},
		StatePath: filepath.Join(t.TempDir(), "state.json"), Timeout: 5 * time.Second})
	m.Start(context.Background())
	waitFor(t, "url from log file", func() bool { return m.Status().Status == StatusOn })
	st := m.Status()
	if st.URL != "https://fake.example.com" || !strings.Contains(st.Command, "sleep 31338.5") {
		t.Fatalf("%+v", st)
	}
	pid := m.child.PID()
	host := SystemHost{}
	m.Stop()
	waitFor(t, "group terminated", func() bool { return !host.Alive(pid) || !processAliveNonZombie(pid) })
}

func processAliveNonZombie(pid int) bool {
	out, err := execPS(pid)
	return err == nil && out != "" && !strings.HasPrefix(out, "Z")
}

func execPS(pid int) (string, error) {
	b, err := execCommandOutput("ps", "-p", itoa(pid), "-o", "stat=")
	return strings.TrimSpace(b), err
}
