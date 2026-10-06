package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const UpdateInterval = 24 * time.Hour

type Status struct {
	Current         string  `json:"current"`
	Installed       string  `json:"installed"`
	Latest          string  `json:"latest,omitempty"`
	UpdateAvailable bool    `json:"updateAvailable"`
	RestartRequired bool    `json:"restartRequired"`
	Channel         Channel `json:"channel"`
	InstallCommand  string  `json:"installCommand,omitempty"`
	CheckedAt       string  `json:"checkedAt,omitempty"`
	Error           string  `json:"error,omitempty"`
}

type Process func(context.Context, string, ...string) (string, error)
type Options struct {
	Current              string
	CachePath            string
	Installation         *Installation
	Detection            DetectionOptions
	HTTP                 HTTPClient
	RegistryURL          string
	Now                  func() time.Time
	FetchLatest          func(context.Context) (string, error)
	Run                  Process
	ReadInstalledVersion func() string
	Native               *NativeInstaller
}

type Manager struct {
	opMu                                  sync.Mutex // Serializes checks and installs without blocking status polling.
	mu                                    sync.Mutex // Protects status and cache state.
	current, latest, checkedAt, lastError string
	installation                          Installation
	options                               Options
}

func New(o Options) *Manager {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Run == nil {
		o.Run = RunProcess
	}
	installation := DetectInstallation(o.Detection)
	if o.Installation != nil {
		installation = *o.Installation
	}
	if installation.Channel == "" {
		installation.Channel = Unknown
	}
	if installation.Command == "" {
		if installation.Channel == NPM || installation.Channel == PNPM {
			installation.Command = InstallCommand(installation.Channel, installation.Bin, "latest")
		}
		if installation.Channel == Binary {
			installation.Command = "jevonian update"
		}
	}
	if o.FetchLatest == nil {
		address := o.RegistryURL
		if address == "" {
			address = envValue(o.Detection.Env, "JEVONIAN_NPM_REGISTRY")
		}
		o.FetchLatest = (Registry{HTTP: o.HTTP, URL: address}).FetchLatest
	}
	if o.Current == "" {
		o.Current = "0.0.0"
	}
	if o.ReadInstalledVersion == nil {
		o.ReadInstalledVersion = func() string {
			if installation.PackagePath != "" {
				if b, err := os.ReadFile(installation.PackagePath); err == nil {
					var p struct {
						Version string `json:"version"`
					}
					if json.Unmarshal(b, &p) == nil && validVersion(p.Version) {
						return p.Version
					}
				}
				return "0.0.0"
			}
			// Probe only explicit direct installs; unknown/source builds never spawn themselves.
			if installation.Channel == Binary && installation.Executable != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if s, err := o.Run(ctx, installation.Executable, "version"); err == nil {
					if v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "jevonian ")); validVersion(v) {
						return v
					}
				}
			}
			return o.Current
		}
	}
	m := &Manager{current: o.Current, installation: installation, options: o}
	m.loadCache()
	return m
}
func (m *Manager) Status() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.status() }
func (m *Manager) status() Status {
	disk := m.options.ReadInstalledVersion()
	return Status{Current: m.current, Installed: disk, Latest: m.latest,
		UpdateAvailable: IsNewerVersion(m.latest, m.current), RestartRequired: IsNewerVersion(disk, m.current),
		Channel: m.installation.Channel, InstallCommand: m.installation.Command, CheckedAt: m.checkedAt, Error: m.lastError}
}
func (m *Manager) Check(ctx context.Context, force bool) Status {
	if !force {
		if !m.opMu.TryLock() {
			return m.Status()
		}
	} else {
		m.opMu.Lock()
	}
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.check(ctx, force)
}
func (m *Manager) check(ctx context.Context, force bool) Status {
	if m.installation.Channel == Source || m.installation.Channel == Unknown {
		return m.status()
	}
	if checked, err := time.Parse(time.RFC3339Nano, m.checkedAt); !force && err == nil {
		age := m.options.Now().Sub(checked)
		if age >= 0 && age < UpdateInterval {
			return m.status()
		}
	}
	// opMu owns the operation; let status polling read the prior snapshot during I/O.
	m.mu.Unlock()
	latest, err := m.options.FetchLatest(ctx)
	m.mu.Lock()
	if err == nil && !validVersion(latest) {
		err = fmt.Errorf("registry response did not contain a valid version")
	}
	if err != nil {
		m.lastError = err.Error()
	} else {
		m.latest, m.checkedAt, m.lastError = latest, m.options.Now().UTC().Format("2006-01-02T15:04:05.000Z"), ""
		m.saveCache()
	}
	return m.status()
}

// Install pins the just-probed version and verifies disk afterward. Unlike the
// old TS manager, Current remains the actual running build until process restart;
// installing a file does not change machine code already loaded in this process.
func (m *Manager) Install(ctx context.Context) (Status, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	status := m.check(ctx, true)
	m.mu.Unlock()
	if status.Error != "" {
		return status, fmt.Errorf("update check failed: %s", status.Error)
	}
	if status.Latest == "" || !IsNewerVersion(status.Latest, status.Installed) {
		return status, nil
	}
	var err error
	switch m.installation.Channel {
	case NPM, PNPM:
		bin := m.installation.Bin
		if bin == "" {
			bin = string(m.installation.Channel)
		}
		_, err = m.options.Run(ctx, bin, InstallArgs(m.installation.Channel, status.Latest)...)
		if err == nil {
			// npm reinstall deletes the package dir, so the cached native binary
			// under native/ and the LaunchAgent's ProgramArguments are gone. Re-
			// download it before verify so `jevonian start` and launchd find a
			// binary to exec.
			err = m.refetchNative(ctx, status.Latest)
		}
	case Binary:
		installer := m.options.Native
		if installer == nil {
			installer = &NativeInstaller{HTTP: m.options.HTTP, Run: m.options.Run}
		}
		err = installer.Install(ctx, m.installation.Executable, status.Latest)
	default:
		err = fmt.Errorf("cannot update a %s installation; no changes were made", m.installation.Channel)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		installed := m.options.ReadInstalledVersion()
		if installed != status.Latest {
			err = fmt.Errorf("installer finished but jevonian is still %s (expected %s). Tried: %s", installed, status.Latest, InstallCommand(m.installation.Channel, m.installation.Bin, status.Latest))
		}
	}
	if err != nil {
		m.lastError = err.Error()
		return m.status(), err
	}
	m.lastError = ""
	m.saveCache()
	return m.status(), nil
}

// refetchNative re-downloads the native binary the npm shim caches. The package
// reinstall wiped native/, and the running service points at that binary, so we
// seed it for the next start before launchd (or the dashboard relaunch) needs it.
// It runs the freshly installed shim with --download-only through the new Node.
func (m *Manager) refetchNative(ctx context.Context, version string) error {
	if m.installation.Entry == "" {
		return fmt.Errorf("npm shim entry is unknown; cannot re-download the native binary")
	}
	node := m.options.Detection.NodeExecutable
	if node == "" {
		node = envValue(m.options.Detection.Env, "JEVONIAN_NODE_EXECUTABLE")
	}
	if node == "" {
		node = "node"
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := m.options.Run(runCtx, node, m.installation.Entry, "--download-only")
	if err != nil {
		return fmt.Errorf("re-download native binary failed: %w%s", err, formatRunTail(out))
	}
	target, err := npmNativeBinaryPath(m.installation, version)
	if err != nil {
		return err
	}
	if st, err := os.Stat(target); err != nil || st.IsDir() {
		return fmt.Errorf("native binary missing after re-download: %s", target)
	}
	return nil
}

func formatRunTail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if len(out) > 240 {
		out = out[len(out)-240:]
	}
	return ": " + out
}

// npmNativeBinaryPath is where the shim caches the Go binary for this install.
func npmNativeBinaryPath(install Installation, version string) (string, error) {
	root := ""
	if install.PackagePath != "" {
		root = filepath.Dir(install.PackagePath)
	} else if install.Entry != "" {
		// …/jevonian/bin/jevonian.js → package root
		root = filepath.Dir(filepath.Dir(install.Entry))
	}
	if root == "" {
		return "", fmt.Errorf("cannot resolve npm package root for native binary")
	}
	platform, arch := runtime.GOOS, runtime.GOARCH
	asset, err := AssetName(platform, arch)
	if err != nil {
		return "", err
	}
	_ = version // version is pinned in package.json; the shim marker records it
	return filepath.Join(root, "native", asset), nil
}

type cache struct {
	Channel   Channel `json:"channel"`
	CheckedAt string  `json:"checkedAt"`
	Latest    string  `json:"latest"`
}

func (m *Manager) loadCache() {
	b, err := os.ReadFile(m.options.CachePath)
	if err != nil {
		return
	}
	var c cache
	if json.Unmarshal(b, &c) != nil || c.Channel != m.installation.Channel || !validVersion(c.Latest) {
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, c.CheckedAt); err != nil {
		return
	}
	m.latest, m.checkedAt = c.Latest, c.CheckedAt
}
func (m *Manager) saveCache() {
	if m.options.CachePath == "" || m.latest == "" || m.installation.Channel == Source {
		return
	}
	b, _ := json.MarshalIndent(cache{m.installation.Channel, m.checkedAt, m.latest}, "", "  ")
	_ = atomicWrite(m.options.CachePath, append(b, '\n'), 0o600)
}
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".jevonian-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type outputTail struct{ b []byte }

func (w *outputTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= 2000 {
		w.b = append(w.b[:0], p[len(p)-2000:]...)
	} else {
		if len(w.b)+len(p) > 2000 {
			w.b = w.b[len(w.b)+len(p)-2000:]
		}
		w.b = append(w.b, p...)
	}
	return n, nil
}

// RunProcess uses argv, never a shell. Bounded combined output preserves useful
// npm failure diagnostics without allowing unbounded installer logs in memory.
func RunProcess(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var output outputTail
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	s := strings.TrimSpace(string(output.b))
	if err != nil {
		return s, fmt.Errorf("installer %v: %s", err, strings.Join(strings.Fields(s), " "))
	}
	return s, nil
}
