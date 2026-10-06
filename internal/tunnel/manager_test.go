package tunnel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// ---------------------------------------------------------------------------
// extractTunnelUrl / buildTunnelCommand (command.go)
// ---------------------------------------------------------------------------

func TestExtractURL(t *testing.T) {
	cases := []struct{ provider, text, want string }{
		{"cloudflare", "2026-09-19 INF |  https://plain-iris-tunnel.trycloudflare.com  |", "https://plain-iris-tunnel.trycloudflare.com"},
		{"ngrok", `t=2026-09-19 level=info msg="url=https://ab12-1-2-3.ngrok-free.app"`, "https://ab12-1-2-3.ngrok-free.app"},
		{"ngrok", `t=2026-09-21 level=info msg="url=https://casqued-dominique-memorably.ngrok-free.dev"`, "https://casqued-dominique-memorably.ngrok-free.dev"},
		{"custom", "listening at https://bore.example.com:8080 now", "https://bore.example.com:8080"},
		{"cloudflare", "no url here", ""},
	}
	for _, c := range cases {
		if got := ExtractURL(c.provider, c.text); got != c.want {
			t.Errorf("%s %q → %q want %q", c.provider, c.text, got, c.want)
		}
	}
}

func TestBuildCommand(t *testing.T) {
	got, err := BuildCommand(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8788)
	want := Command{Name: "cloudflared", Args: []string{"tunnel", "--url", "http://127.0.0.1:8788", "--no-autoupdate"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}

	got, err = BuildCommand(config.TunnelConfig{Enabled: true, Provider: "ngrok"}, 8788)
	want = Command{Name: "ngrok", Args: []string{"http", "127.0.0.1:8788", "--log", "stdout"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}

	got, err = BuildCommand(config.TunnelConfig{
		Enabled: true, Provider: "ngrok",
		URL: "https://casqued-dominique-memorably.ngrok-free.dev",
	}, 8788)
	want = Command{
		Name: "ngrok",
		Args: []string{"http", "127.0.0.1:8788", "--url", "https://casqued-dominique-memorably.ngrok-free.dev", "--log", "stdout"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}

	got, err = BuildCommand(config.TunnelConfig{
		Enabled: true, Provider: "custom", Command: "bore local {port} --to bore.pub",
	}, 8788)
	want = Command{Name: "sh", Args: []string{"-c", "bore local 8788 --to bore.pub"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}

	if _, err := BuildCommand(config.TunnelConfig{Enabled: true, Provider: "custom"}, 8788); err == nil ||
		!strings.Contains(err.Error(), "needs a command") {
		t.Fatalf("missing command err %v", err)
	}
}

func TestCommandMatches(t *testing.T) {
	// `sh -c cloudflared …` execs into cloudflared; `ps` never shows the wrapper.
	recorded := "sh -c cloudflared tunnel --config /home/u/t.yml run"
	if !CommandMatches("cloudflared tunnel --config /home/u/t.yml run", recorded) {
		t.Fatal("unwrapped sh -c")
	}
	if !CommandMatches(recorded, recorded) {
		t.Fatal("identity")
	}
	if !CommandMatches("bore local 8788 --to bore.pub", `sh -c "bore local 8788 --to bore.pub"`) {
		t.Fatal("quoted wrapper")
	}
	if !CommandMatches("cloudflared tunnel run", "sh -c exec cloudflared tunnel run") {
		t.Fatal("exec prefix")
	}
	// A shorter configured command must not match a longer unrelated invocation.
	if CommandMatches("cloudflared tunnel --config other.yml run", "cloudflared tunnel") {
		t.Fatal("prefix overmatch")
	}
	if CommandMatches("ngrok http 9999 --log stdout", "sh -c ngrok http 8788") {
		t.Fatal("different args")
	}
}

// ---------------------------------------------------------------------------
// Manager with a fake spawn + host (tunnel.test.ts TunnelManager describes)
// ---------------------------------------------------------------------------

// fakeChild is an injectable Child.
type fakeChild struct {
	mu     sync.Mutex
	out    chan []byte
	done   chan int
	pid    int
	closed bool
}

func newFakeChild(pid int) *fakeChild {
	return &fakeChild{out: make(chan []byte, 64), done: make(chan int, 1), pid: pid}
}
func (f *fakeChild) Output() <-chan []byte { return f.out }
func (f *fakeChild) Wait() <-chan int      { return f.done }
func (f *fakeChild) PID() int              { return f.pid }
func (f *fakeChild) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}
func (f *fakeChild) emit(data string) { f.out <- []byte(data) }
func (f *fakeChild) exit(code int) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if !closed {
		f.Close()
	}
	f.done <- code
}

// fakeSpawn captures the last spawn.
type fakeSpawner struct {
	mu       sync.Mutex
	pid      int
	last     *fakeChild
	lastCmd  Command
	lastOpts SpawnOptions
	spawns   int
}

func (s *fakeSpawner) spawn(_ context.Context, cmd Command, opts SpawnOptions) (Child, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := newFakeChild(s.pid)
	s.last, s.lastCmd, s.lastOpts = child, cmd, opts
	s.spawns++
	return child, nil
}

// fakeHost is an injectable Host.
type fakeHost struct {
	mu         sync.Mutex
	terminated []int
	alive      bool
	identify   bool
	find       func(command string) []int
}

func (h *fakeHost) Alive(int) bool            { return h.alive }
func (h *fakeHost) Identify(int, string) bool { return h.identify }
func (h *fakeHost) Terminate(pid int, _ bool) {
	h.mu.Lock()
	h.terminated = append(h.terminated, pid)
	h.mu.Unlock()
}
func (h *fakeHost) Find(command string) []int {
	if h.find == nil {
		return nil
	}
	return h.find(command)
}
func (h *fakeHost) kills() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.terminated...)
}

func newManager(cfg config.TunnelConfig, port int, s *fakeSpawner, h *fakeHost, statePath string) *Manager {
	return NewManager(cfg, port, Options{
		Spawn:     s.spawn,
		Timeout:   time.Second,
		StatePath: statePath,
		Host:      h,
		Env:       []string{"PATH=/usr/bin:/bin", "HOME=/tmp"},
	})
}

func TestManagerLifecycle(t *testing.T) {
	s := &fakeSpawner{}
	h := &fakeHost{alive: true, identify: true}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, "")
	if m.Status().Status != StatusOff {
		t.Fatal("initial")
	}
	m.Start(context.Background())
	if m.Status().Status != StatusStarting {
		t.Fatalf("starting %+v", m.Status())
	}
	s.last.emit("https://cool-name.trycloudflare.com\n")
	deadline := time.Now().Add(2 * time.Second)
	for m.Status().Status != StatusOn && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	st := m.Status()
	if st.Status != StatusOn || st.URL != "https://cool-name.trycloudflare.com" || st.PublicPort != 8788 {
		t.Fatalf("%+v", st)
	}
	m.Stop()
	if m.Status().Status != StatusOff {
		t.Fatalf("stop %+v", m.Status())
	}
}

func TestManagerExitBeforeURL(t *testing.T) {
	s := &fakeSpawner{}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, &fakeHost{}, "")
	m.Start(context.Background())
	s.last.exit(1)
	deadline := time.Now().Add(2 * time.Second)
	for m.Status().Status != StatusError && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	st := m.Status()
	if st.Status != StatusError || !strings.Contains(st.Error, "exited") {
		t.Fatalf("%+v", st)
	}
}

func TestManagerScrubsProxyEnv(t *testing.T) {
	s := &fakeSpawner{}
	m := NewManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, Options{
		Spawn:   s.spawn,
		Timeout: time.Second,
		Host:    &fakeHost{},
		Env:     []string{"PATH=/usr/bin", "HTTPS_PROXY=http://127.0.0.1:1082", "https_proxy=http://x"},
	})
	m.Start(context.Background())
	defer m.Stop()
	for _, kv := range s.lastOpts.Env {
		if strings.HasPrefix(kv, "HTTPS_PROXY=") || strings.HasPrefix(kv, "https_proxy=") {
			t.Fatalf("proxy leaked: %v", s.lastOpts.Env)
		}
	}
	var path string
	for _, kv := range s.lastOpts.Env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if path != AugmentPath("/usr/bin", AugmentPathOptions{}) {
		t.Fatalf("PATH %q", path)
	}
}

func TestManagerConfiguredURL(t *testing.T) {
	s := &fakeSpawner{}
	m := newManager(config.TunnelConfig{
		Enabled: true, Provider: "custom",
		Command: "cloudflared tunnel run my-named-tunnel", URL: "https://ai.example.com",
	}, 8787, s, &fakeHost{}, "")
	m.Start(context.Background())
	defer m.Stop()
	st := m.Status()
	if st.Status != StatusOn || st.URL != "https://ai.example.com" {
		t.Fatalf("%+v", st)
	}
}

func TestManagerNgrokReservedDomain(t *testing.T) {
	s := &fakeSpawner{}
	domain := "https://casqued-dominique-memorably.ngrok-free.dev"
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "ngrok", URL: domain}, 8787, s, &fakeHost{}, "")
	m.Start(context.Background())
	defer m.Stop()
	st := m.Status()
	if st.Status != StatusOn || st.URL != domain {
		t.Fatalf("%+v", st)
	}
	want := Command{Name: "ngrok", Args: []string{"http", "127.0.0.1:8788", "--url", domain, "--log", "stdout"}}
	if !reflect.DeepEqual(s.lastCmd, want) {
		t.Fatalf("%+v", s.lastCmd)
	}
}

func TestManagerStaleURLIgnored(t *testing.T) {
	s := &fakeSpawner{}
	m := newManager(config.TunnelConfig{
		Enabled: true, Provider: "cloudflare", URL: "https://stale.ngrok-free.dev",
	}, 8787, s, &fakeHost{}, "")
	m.Start(context.Background())
	defer m.Stop()
	st := m.Status()
	if st.Status != StatusStarting || st.URL != "" {
		t.Fatalf("%+v", st)
	}
}

func TestManagerConfigChangeResetsStateAndAllowsRestart(t *testing.T) {
	for _, provider := range []string{"cloudflare", "custom"} {
		t.Run(provider, func(t *testing.T) {
			s := &fakeSpawner{pid: 7777}
			h := &fakeHost{}
			cfg := config.TunnelConfig{Enabled: true, Provider: provider}
			if provider == "custom" {
				cfg.Command = "bore local {port} --to bore.pub"
				cfg.URL = "https://old.example.com"
			}
			m := newManager(cfg, 8787, s, h, statePath(t))
			m.Start(context.Background())
			defer m.Stop()
			cfg.URL = "https://new.example.com"
			m.Update(cfg, 9000)
			if st := m.Status(); st.Status != StatusOff || st.URL != "" || st.StartedAt != "" || st.PublicPort != 9001 {
				t.Fatalf("config change kept stale state: %+v", st)
			}
			if !reflect.DeepEqual(h.kills(), []int{7777}) {
				t.Fatalf("old child not stopped: %v", h.kills())
			}
			m.Start(context.Background())
			if s.spawns != 2 {
				t.Fatalf("changed config cannot restart: %d spawns", s.spawns)
			}
			if provider == "custom" && m.Status().URL != cfg.URL {
				t.Fatalf("new configured URL not applied: %+v", m.Status())
			}
		})
	}
}

func TestManagerCustomNeedsCommand(t *testing.T) {
	s := &fakeSpawner{}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "custom"}, 8787, s, &fakeHost{}, "")
	st := m.Start(context.Background())
	if st.Status != StatusError || !strings.Contains(st.Error, "needs a command") || s.spawns != 0 {
		t.Fatalf("%+v spawns=%d", st, s.spawns)
	}
}

// ---------------------------------------------------------------------------
// Restart adoption + orphan sweep
// ---------------------------------------------------------------------------

func writeRecord(t *testing.T, path string, rec Record) {
	t.Helper()
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func statePath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "tunnel-state.json")
}

func TestAdoptsLiveTunnel(t *testing.T) {
	path := statePath(t)
	writeRecord(t, path, Record{
		PID: 4242, Provider: "cloudflare", PublicPort: 8788,
		Command:   "cloudflared tunnel --url http://127.0.0.1:8788 --no-autoupdate",
		URL:       "https://steady-name.trycloudflare.com",
		StartedAt: "2026-09-20T00:00:00.000Z",
	})
	s := &fakeSpawner{}
	h := &fakeHost{alive: true, identify: true}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	st := m.Start(context.Background())
	if st.Status != StatusOn || st.URL != "https://steady-name.trycloudflare.com" || st.StartedAt != "2026-09-20T00:00:00.000Z" {
		t.Fatalf("%+v", st)
	}
	if s.spawns != 0 {
		t.Fatal("spawned despite adoption")
	}
	m.Stop()
	if !reflect.DeepEqual(h.kills(), []int{4242}) {
		t.Fatalf("terminated %v", h.kills())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("record not cleared")
	}
}

func TestStaleRecordReplaced(t *testing.T) {
	path := statePath(t)
	writeRecord(t, path, Record{
		PID: 4242, Provider: "cloudflare", PublicPort: 8788,
		Command: "cloudflared tunnel --url http://127.0.0.1:8788 --no-autoupdate",
		URL:     "https://dead-name.trycloudflare.com",
	})
	s := &fakeSpawner{}
	h := &fakeHost{alive: false}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	st := m.Start(context.Background())
	defer m.Stop()
	if s.spawns == 0 || st.Status != StatusStarting {
		t.Fatalf("%+v spawns=%d", st, s.spawns)
	}
	if len(h.kills()) != 0 {
		t.Fatalf("terminated %v", h.kills())
	}
}

func TestProviderMismatchRestarts(t *testing.T) {
	path := statePath(t)
	writeRecord(t, path, Record{
		PID: 4242, Provider: "ngrok", PublicPort: 9999,
		Command: "ngrok http 9999 --log stdout",
		URL:     "https://old.ngrok-free.app",
	})
	s := &fakeSpawner{}
	h := &fakeHost{alive: true, identify: true}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	m.Start(context.Background())
	defer m.Stop()
	if s.spawns == 0 {
		t.Fatal("did not spawn")
	}
	if len(h.kills()) != 0 {
		t.Fatalf("terminated %v", h.kills())
	}
}

func TestRecordLifecycle(t *testing.T) {
	path := statePath(t)
	s := &fakeSpawner{pid: 12345}
	h := &fakeHost{alive: true, identify: true}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	m.Start(context.Background())
	s.last.emit("https://fresh-name.trycloudflare.com\n")
	deadline := time.Now().Add(2 * time.Second)
	for m.Status().Status != StatusOn && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var rec Record
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rec) != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Provider != "cloudflare" || rec.PublicPort != 8788 || rec.URL != "https://fresh-name.trycloudflare.com" {
		t.Fatalf("record %+v", rec)
	}
	m.Stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("record not cleared on stop")
	}
}

func TestOrphanSweepOnStart(t *testing.T) {
	path := statePath(t)
	s := &fakeSpawner{pid: 7777}
	h := &fakeHost{
		alive:    false,
		identify: false,
		find: func(command string) []int {
			if strings.Contains(command, "cloudflared") {
				return []int{5150}
			}
			return nil
		},
	}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	m.Start(context.Background())
	defer m.Stop()
	if !reflect.DeepEqual(h.kills(), []int{5150}) {
		t.Fatalf("terminated %v", h.kills())
	}
	if s.spawns == 0 {
		t.Fatal("no spawn")
	}
}

func TestOrphanSweepSkipsUnrelated(t *testing.T) {
	path := statePath(t)
	s := &fakeSpawner{pid: 7777}
	h := &fakeHost{find: func(string) []int { return nil }}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	m.Start(context.Background())
	defer m.Stop()
	if len(h.kills()) != 0 {
		t.Fatalf("terminated %v", h.kills())
	}
}

func TestSweepKeepsAdopted(t *testing.T) {
	path := statePath(t)
	writeRecord(t, path, Record{
		PID: 4242, Provider: "cloudflare", PublicPort: 8788,
		Command: "cloudflared tunnel --url http://127.0.0.1:8788 --no-autoupdate",
		URL:     "https://steady-name.trycloudflare.com",
	})
	s := &fakeSpawner{}
	h := &fakeHost{
		alive:    true,
		identify: true,
		find:     func(string) []int { return []int{4242, 5150, 5151} },
	}
	m := newManager(config.TunnelConfig{Enabled: true, Provider: "cloudflare"}, 8787, s, h, path)
	st := m.Start(context.Background())
	if st.Status != StatusOn {
		t.Fatalf("%+v", st)
	}
	if s.spawns != 0 {
		t.Fatal("spawned")
	}
	kills := h.kills()
	slices.Sort(kills)
	if !reflect.DeepEqual(kills, []int{5150, 5151}) {
		t.Fatalf("terminated %v", kills)
	}
	m.Stop()
}
