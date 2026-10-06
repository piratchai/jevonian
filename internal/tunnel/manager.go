package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/proxy"
)

// State is the tunnel's reported status.
type State struct {
	Status     Status   `json:"status"`
	Provider   Provider `json:"provider"`
	PublicPort int      `json:"publicPort"`
	URL        string   `json:"url,omitempty"`
	Error      string   `json:"error,omitempty"`
	StartedAt  string   `json:"startedAt,omitempty"`
	Command    string   `json:"command,omitempty"`
}

// Record is what survives a restart (JEVONIAN_TUNNEL_STATE or
// paths.TunnelStatePath) so the next serve adopts a still-running tunnel.
type Record struct {
	PID        int      `json:"pid"`
	Provider   Provider `json:"provider"`
	PublicPort int      `json:"publicPort"`
	Command    string   `json:"command"`
	URL        string   `json:"url,omitempty"`
	StartedAt  string   `json:"startedAt"`
}

// SpawnFunc starts the tunnel child process. Injectable for tests; the real
// default pipes output and is detached on unix so the tunnel survives us.
type SpawnFunc func(ctx context.Context, cmd Command, opts SpawnOptions) (Child, error)

// SpawnOptions are what the spawn gets.
type SpawnOptions struct {
	// LogFile is the opened log destination, or nil → pipe output to us.
	LogFile *os.File
	// Env is the child environment (proxy vars scrubbed, PATH augmented).
	Env []string
	// Detached asks for a new process group on unix.
	Detached bool
}

// Child is a spawned tunnel process.
type Child interface {
	// Output delivers stdout+stderr bytes as they arrive (only when LogFile was nil).
	Output() <-chan []byte
	// Wait resolves once the process exits.
	Wait() <-chan int
	// PID is the process id (0 when spawn raced an immediate failure).
	PID() int
	// Close releases the output readers (the pipe ends) without killing.
	Close()
}

// execChild wraps exec.Cmd.
type execChild struct {
	cmd    *exec.Cmd
	out    chan []byte
	done   chan int
	closed chan struct{}
	once   sync.Once
}

// ExecSpawn is the default SpawnFunc. The child is not bound to ctx: a
// detached tunnel is meant to outlive this process (restart adoption); it is
// stopped through Host.Terminate.
func ExecSpawn(_ context.Context, command Command, opts SpawnOptions) (Child, error) {
	cmd := exec.Command(command.Name, command.Args...)
	cmd.Env = opts.Env
	if opts.Detached {
		cmd.SysProcAttr = detachProcAttr()
	}
	var readers []io.Reader
	if opts.LogFile != nil {
		cmd.Stdout = opts.LogFile
		cmd.Stderr = opts.LogFile
	} else {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}
		readers = []io.Reader{stdout, stderr}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &execChild{
		cmd:    cmd,
		done:   make(chan int, 1),
		closed: make(chan struct{}),
	}
	var pumps sync.WaitGroup
	if len(readers) > 0 {
		c.out = make(chan []byte, 64)
		for _, r := range readers {
			pumps.Add(1)
			go func(r io.Reader) {
				defer pumps.Done()
				c.pump(r)
			}(r)
		}
	}
	go func() {
		// Pipes must be drained before Wait (os/exec contract).
		pumps.Wait()
		err := cmd.Wait()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		if c.out != nil {
			close(c.out)
		}
		c.done <- code
	}()
	return c, nil
}

// pump forwards one pipe. cloudflared prints its URL on stderr while stdout
// stays open, so each pipe needs its own reader.
func (c *execChild) pump(r io.Reader) {
	reader := bufio.NewReaderSize(r, 4096)
	for {
		chunk := make([]byte, 4096)
		n, err := reader.Read(chunk)
		if n > 0 {
			select {
			case c.out <- chunk[:n]:
			case <-c.closed:
				// Keep draining so the child never blocks on a full pipe.
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *execChild) Output() <-chan []byte { return c.out }
func (c *execChild) Wait() <-chan int      { return c.done }
func (c *execChild) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Close stops forwarding output (the pipes keep draining) and, on platforms
// without process groups, kills the child.
func (c *execChild) Close() {
	c.once.Do(func() {
		close(c.closed)
		if runtime.GOOS == "windows" && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
}

// Options configure a Manager.
type Options struct {
	Spawn   SpawnFunc
	Timeout time.Duration
	// StatePath is where the record is written (paths.TunnelStatePath).
	StatePath string
	// LogPath is the optional log file a piped child tails instead (paths.TunnelLogPath).
	LogPath string
	Host    Host
	// Env overrides os.Environ() for the child (tests).
	Env []string
	// Now is the clock.
	Now func() time.Time
	// SweepInterval bounds the log poll when LogPath is set.
	SweepInterval time.Duration
}

// Manager runs and owns one tunnel lifecycle.
type Manager struct {
	mu        sync.Mutex
	cfg       config.TunnelConfig
	spawn     SpawnFunc
	timeout   time.Duration
	statePath string
	logPath   string
	host      Host
	env       []string
	now       func() time.Time
	sweep     time.Duration

	child    Child
	childCtx context.CancelFunc
	stopFlag bool
	adopted  *Record
	state    State
	timer    *time.Timer
	poll     *time.Timer
}

// NewManager builds a tunnel manager for cfg bound to listen port port.
func NewManager(cfg config.TunnelConfig, port int, opts Options) *Manager {
	m := &Manager{
		cfg:       cfg,
		spawn:     opts.Spawn,
		timeout:   opts.Timeout,
		statePath: opts.StatePath,
		logPath:   opts.LogPath,
		host:      opts.Host,
		env:       opts.Env,
		now:       opts.Now,
		sweep:     opts.SweepInterval,
	}
	if m.spawn == nil {
		m.spawn = ExecSpawn
	}
	if m.timeout <= 0 {
		m.timeout = 30 * time.Second
	}
	if m.host == nil {
		m.host = SystemHost{}
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.sweep <= 0 {
		m.sweep = 250 * time.Millisecond
	}
	publicPort := port + 1
	if cfg.PublicPort != nil {
		publicPort = *cfg.PublicPort
	}
	m.state = State{Status: StatusOff, Provider: Provider(cfg.Provider), PublicPort: publicPort}
	return m
}

// Update swaps the config; a running tunnel pointed at the old provider or
// port is restarted.
func (m *Manager) Update(cfg config.TunnelConfig, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	publicPort := port + 1
	if cfg.PublicPort != nil {
		publicPort = *cfg.PublicPort
	}
	changed := cfg.Provider != m.cfg.Provider ||
		cfg.Command != m.cfg.Command ||
		cfg.URL != m.cfg.URL ||
		publicPort != m.state.PublicPort
	m.cfg = cfg
	m.state.Provider = Provider(cfg.Provider)
	m.state.PublicPort = publicPort
	if changed && (m.state.Status == StatusOn || m.state.Status == StatusStarting) {
		m.stopLocked()
		m.state = State{
			Status:     StatusOff,
			Provider:   m.state.Provider,
			PublicPort: publicPort,
		}
	}
}

// Status reports the current state.
func (m *Manager) Status() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Status == StatusOn && m.adopted != nil && !m.host.Alive(m.adopted.PID) {
		m.adopted = nil
		m.clearRecordLocked()
		m.state.Status = StatusError
		m.state.Error = "tunnel process exited"
		m.state.URL = ""
		m.state.StartedAt = ""
	}
	out := m.state
	return out
}

// Start launches (or adopts) the tunnel.
func (m *Manager) Start(ctx context.Context) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Status == StatusStarting || m.state.Status == StatusOn {
		return m.state
	}
	m.stopFlag = false
	if adopted := m.adoptLocked(); adopted != nil {
		// Adopting the recorded tunnel does not clear duplicates left by earlier
		// leaky restarts; sweep them too.
		if built, err := BuildCommand(m.cfg, m.state.PublicPort); err == nil {
			m.sweepOrphansLocked(built.Label())
		}
		return m.state
	}
	built, err := BuildCommand(m.cfg, m.state.PublicPort)
	if err != nil {
		m.state.Status = StatusError
		m.state.Error = err.Error()
		m.state.URL = ""
		return m.state
	}
	label := built.Label()
	// A stale tunnel from a previous serve can outlive the record that named
	// it (adoption only reaps the recorded pid). Sweep any other process running
	// this exact command before spawning, or each restart leaks one orphan.
	m.sweepOrphansLocked(label)
	m.state.Status = StatusStarting
	m.state.Error = ""
	m.state.URL = ""
	m.state.Command = label

	logFile := m.openLogLocked()
	env := m.env
	if env == nil {
		env = os.Environ()
	}
	// Tunnel binaries must dial Cloudflare/ngrok directly: inheriting a local
	// Clash-style HTTPS_PROXY is how a healthy tunnel turns into socket resets.
	// Augment PATH so launchd / GUI-started serves still find Homebrew ngrok.
	env = WithAugmentedPath(proxy.ScrubProxyEnv(env))
	childCtx, cancel := context.WithCancel(ctx)
	child, err := m.spawn(childCtx, built, SpawnOptions{
		LogFile:  logFile,
		Env:      env,
		Detached: runtime.GOOS != "windows",
	})
	if logFile != nil {
		_ = logFile.Close()
	}
	if err != nil {
		cancel()
		m.state.Status = StatusError
		m.state.Error = friendlySpawnError(built.Name, err)
		return m.state
	}
	m.child = child
	m.childCtx = cancel
	startedAt := m.now().UTC().Format(time.RFC3339Nano)
	if pid := child.PID(); pid > 0 {
		m.writeRecordLocked(Record{
			PID:        pid,
			Provider:   m.state.Provider,
			PublicPort: m.state.PublicPort,
			Command:    label,
			StartedAt:  startedAt,
		})
	}
	if stream := child.Output(); stream != nil {
		go m.watchOutput(child, stream, startedAt)
	}
	go m.watchExit(child, startedAt)
	// Named / reserved endpoints already know their public URL; quick tunnels
	// must wait for logs.
	if UsesConfiguredURL(m.cfg) && m.cfg.URL != "" {
		m.settleLocked(child, m.cfg.URL, startedAt)
	}
	if m.logPath != "" {
		m.tickLogLocked(child, startedAt)
	}
	m.timer = time.AfterFunc(m.timeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.child != child || m.state.Status != StatusStarting {
			return
		}
		m.clearTimersLocked()
		m.state.Status = StatusError
		m.state.Error = "tunnel did not report a URL"
	})
	return m.state
}

// Stop terminates the tunnel and clears the record.
func (m *Manager) Stop() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopFlag = true
	m.stopLocked()
	m.state = State{
		Status:     StatusOff,
		Provider:   m.state.Provider,
		PublicPort: m.state.PublicPort,
	}
	return m.state
}

func (m *Manager) stopLocked() {
	m.clearTimersLocked()
	child := m.child
	m.child = nil
	if m.childCtx != nil {
		m.childCtx()
		m.childCtx = nil
	}
	if child != nil {
		m.terminateChildLocked(child)
	}
	if m.adopted != nil && m.host.Identify(m.adopted.PID, m.adopted.Command) {
		m.host.Terminate(m.adopted.PID, runtime.GOOS != "windows")
	}
	m.adopted = nil
	m.clearRecordLocked()
}

// Cleanup reaps whatever the record names (and configured-command orphans)
// even when the tunnel was never started this process — tunnel disabled.
func (m *Manager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record := m.readRecordLocked(); record != nil && record.PID > 0 &&
		m.host.Alive(record.PID) && m.host.Identify(record.PID, record.Command) {
		m.host.Terminate(record.PID, runtime.GOOS != "windows")
	}
	if built, err := BuildCommand(m.cfg, m.state.PublicPort); err == nil {
		m.sweepOrphansLocked(built.Label())
	}
	m.clearRecordLocked()
}

// sweepOrphansLocked terminates every process running this exact tunnel
// command that we did not just adopt. Only ever fires on the *configured*
// command string (which already embeds the user's own args), matched for the
// current user via Host.Find; an unrelated cloudflared on another config is
// not matched.
func (m *Manager) sweepOrphansLocked(command string) {
	find := m.host.Find
	if find == nil {
		return
	}
	keep := map[int]bool{}
	if m.child != nil && m.child.PID() > 0 {
		keep[m.child.PID()] = true
	}
	if m.adopted != nil && m.adopted.PID > 0 {
		keep[m.adopted.PID] = true
	}
	for _, pid := range find(command) {
		if keep[pid] {
			continue
		}
		m.host.Terminate(pid, runtime.GOOS != "windows")
	}
}

// adoptLocked reuses a recorded live tunnel when it matches the config.
func (m *Manager) adoptLocked() *Record {
	record := m.readRecordLocked()
	if record == nil {
		return nil
	}
	matches := record.Provider == m.state.Provider &&
		record.PublicPort == m.state.PublicPort
	alive := matches && record.PID > 0 && m.host.Alive(record.PID)
	ours := alive && m.host.Identify(record.PID, record.Command)
	url := m.cfg.URL
	if url == "" {
		url = record.URL
	}
	if url == "" {
		url = m.scrapeLogLocked()
	}
	if !ours || url == "" {
		if ours {
			m.host.Terminate(record.PID, runtime.GOOS != "windows")
		}
		m.clearRecordLocked()
		return nil
	}
	m.adopted = record
	m.state.Status = StatusOn
	m.state.URL = url
	m.state.Error = ""
	m.state.Command = record.Command
	m.state.StartedAt = record.StartedAt
	return record
}

func (m *Manager) scrapeLogLocked() string {
	if m.logPath == "" {
		return ""
	}
	data, err := os.ReadFile(m.logPath)
	if err != nil {
		return ""
	}
	return ExtractURL(m.state.Provider, string(data))
}

// watchOutput streams child output and settles the URL on first match.
func (m *Manager) watchOutput(child Child, stream <-chan []byte, startedAt string) {
	settled := false
	for chunk := range stream {
		if settled {
			continue // keep draining
		}
		m.mu.Lock()
		url := ExtractURL(m.state.Provider, string(chunk))
		if url != "" {
			m.settleLocked(child, url, startedAt)
			settled = true
		}
		m.mu.Unlock()
	}
}

// watchExit resolves a process exit: error unless it was our own stop.
func (m *Manager) watchExit(child Child, startedAt string) {
	code := <-child.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.child != child {
		return
	}
	m.child = nil
	m.clearTimersLocked()
	m.clearRecordLocked()
	if m.stopFlag {
		return
	}
	m.state.Status = StatusError
	m.state.Error = fmt.Sprintf("tunnel exited (code %d)", code)
	m.state.URL = ""
}

func (m *Manager) settleLocked(child Child, url, startedAt string) {
	if m.child != child || m.state.Status != StatusStarting {
		return
	}
	m.clearTimersLocked()
	m.state.Status = StatusOn
	m.state.URL = url
	m.state.StartedAt = startedAt
	m.state.Error = ""
	m.saveURLLocked(url)
}

// tickLogLocked polls the log file for the URL (when output went to a file).
func (m *Manager) tickLogLocked(child Child, startedAt string) {
	go func() {
		ticker := time.NewTicker(m.sweep)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.mu.Lock()
				if m.child != child || m.state.Status != StatusStarting {
					m.mu.Unlock()
					return
				}
				if url := m.scrapeLogLocked(); url != "" {
					m.settleLocked(child, url, startedAt)
					m.mu.Unlock()
					return
				}
				m.mu.Unlock()
			}
		}
	}()
}

func (m *Manager) terminateChildLocked(child Child) {
	if runtime.GOOS != "windows" && child.PID() > 0 {
		m.host.Terminate(child.PID(), true)
		return
	}
	child.Close()
}

func (m *Manager) clearTimersLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.poll != nil {
		m.poll.Stop()
		m.poll = nil
	}
}

func (m *Manager) openLogLocked() *os.File {
	if m.logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.logPath), 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	return f
}

func (m *Manager) readRecordLocked() *Record {
	if m.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return nil
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}
	if rec.PID <= 0 || rec.PublicPort == 0 || rec.Provider == "" || rec.Command == "" {
		return nil
	}
	return &rec
}

func (m *Manager) writeRecordLocked(rec Record) {
	if m.statePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.statePath), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.statePath, data, 0o644)
}

func (m *Manager) saveURLLocked(url string) {
	if m.statePath == "" {
		return
	}
	rec := m.readRecordLocked()
	if rec == nil {
		return
	}
	rec.URL = url
	m.writeRecordLocked(*rec)
}

func (m *Manager) clearRecordLocked() {
	if m.statePath == "" {
		return
	}
	_ = os.Remove(m.statePath)
}

func friendlySpawnError(name string, err error) string {
	if isNotFound(err) {
		return fmt.Sprintf("%s is not installed", name)
	}
	return err.Error()
}

func isNotFound(err error) bool {
	return strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "no such file or directory")
}
