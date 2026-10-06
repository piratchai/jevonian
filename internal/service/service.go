// Package service controls the macOS LaunchAgent. Runtime operations are
// injectable so tests never touch the live ai.jevonian.serve job.
package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/paths"
)

const Label = "ai.jevonian.serve"

type Status struct {
	Platform       string `json:"platform"`
	Label          string `json:"label"`
	PlistPath      string `json:"plistPath"`
	PlistInstalled bool   `json:"plistInstalled"`
	Loaded         bool   `json:"loaded"`
	PID            int    `json:"pid,omitempty"`
	LogPath        string `json:"logPath"`
	Detail         string `json:"detail,omitempty"`
}

type Result struct {
	Status Status
	Action string
}
type CommandResult struct {
	Output string
	Err    error
}

// Manager's zero value uses the real host. Tests must inject Command and Home.
type Manager struct {
	Platform   string
	Home       string
	UID        string
	Executable string
	Env        map[string]string
	Command    func(args ...string) CommandResult
	Sleep      func(time.Duration)
}

func ManagedByLaunchd() bool { return os.Getenv("XPC_SERVICE_NAME") == Label }
func (m Manager) platform() string {
	if m.Platform != "" {
		return m.Platform
	}
	return runtime.GOOS
}
func (m Manager) home() string {
	if m.Home != "" {
		return m.Home
	}
	h, _ := os.UserHomeDir()
	return h
}
func (m Manager) getenv(k string) string {
	if m.Env != nil {
		return m.Env[k]
	}
	return os.Getenv(k)
}
func (m Manager) DefaultPlistPath() string {
	return filepath.Join(m.home(), "Library", "LaunchAgents", Label+".plist")
}
func (m Manager) PlistPath() string {
	if p := m.getenv("JEVONIAN_SERVICE_PLIST"); p != "" {
		return p
	}
	return m.DefaultPlistPath()
}
func (m Manager) target() string {
	uid := m.UID
	if uid == "" {
		uid = currentUID()
	}
	return "gui/" + uid + "/" + Label
}
func (m Manager) run(args ...string) CommandResult {
	if m.Command != nil {
		return m.Command(args...)
	}
	cmd := exec.Command("/bin/launchctl", args...)
	b, err := cmd.CombinedOutput()
	return CommandResult{string(b), err}
}
func (m Manager) sleep(d time.Duration) {
	if m.Sleep != nil {
		m.Sleep(d)
	} else {
		time.Sleep(d)
	}
}

// AssertLive refuses redirected plists BEFORE any mutating launchctl invocation.
func (m Manager) AssertLive() error {
	if m.PlistPath() != m.DefaultPlistPath() {
		return fmt.Errorf("refusing to control the live LaunchAgent while JEVONIAN_SERVICE_PLIST is set (override %s; live %s). Unset it, or use --foreground for a local instance", m.PlistPath(), m.DefaultPlistPath())
	}
	if m.platform() != "darwin" {
		return fmt.Errorf("background service control is only available on macOS. Use `jevonian serve`")
	}
	return nil
}
func (m Manager) Status() Status {
	s := Status{Platform: m.platform(), Label: Label, PlistPath: m.PlistPath(), LogPath: paths.ServeLogPath()}
	_, err := os.Stat(s.PlistPath)
	s.PlistInstalled = err == nil
	if m.platform() != "darwin" {
		s.Detail = "launchd is only available on macOS"
		return s
	}
	r := m.run("print", m.target())
	if r.Err != nil {
		s.Detail = strings.TrimSpace(r.Output)
		if s.Detail == "" {
			s.Detail = "not loaded"
		}
		if len(s.Detail) > 200 {
			s.Detail = s.Detail[:200]
		}
		return s
	}
	s.Loaded = true
	s.Detail = "loaded"
	if p := regexp.MustCompile(`\bpid\s*=\s*(\d+)`).FindStringSubmatch(r.Output); len(p) > 1 {
		s.PID, _ = strconv.Atoi(p[1])
	}
	return s
}
func EscapeXML(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

// BuildPlist runs the native Go binary directly; no Node runtime is required.
func BuildPlist(executable, logPath string, env map[string]string) string {
	e := map[string]string{"JEVONIAN_NO_OPEN": "1"}
	for k, v := range env {
		e[k] = v
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string></array>
<key>WorkingDirectory</key><string>%s</string>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>5</integer><key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
<key>EnvironmentVariables</key><dict>
`, Label, EscapeXML(executable), EscapeXML(filepath.Dir(executable)), EscapeXML(logPath), EscapeXML(logPath))
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "<key>%s</key><string>%s</string>\n", EscapeXML(k), EscapeXML(e[k]))
	}
	b.WriteString("</dict></dict></plist>\n")
	return b.String()
}

// InstalledEntry returns Go [binary,serve] or TS [node,entry,serve] ownership.
func InstalledEntry(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	key := ""
	inArgs := false
	depth := 0
	var args []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "key" {
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return "", err
				}
				key = v
			}
			if t.Name.Local == "array" && key == "ProgramArguments" {
				inArgs = true
				depth++
			}
			if t.Name.Local == "string" && inArgs {
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return "", err
				}
				args = append(args, v)
			}
		case xml.EndElement:
			if t.Name.Local == "array" && inArgs {
				depth--
				if depth == 0 {
					inArgs = false
				}
			}
		}
	}
	for i, v := range args {
		if v == "serve" && i > 0 {
			return args[i-1], nil
		}
	}
	return "", fmt.Errorf("installed plist does not contain Jevonian ProgramArguments")
}
func (m Manager) env() map[string]string {
	e := map[string]string{"PATH": augmentPath(m.getenv("PATH"), m.home())}
	for _, k := range []string{"JEVONIAN_WEB_DIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		if v := m.getenv(k); v != "" {
			e[k] = v
		}
	}
	// Never bake scratch config/data/ledger overrides into production.
	return e
}
func (m Manager) bootstrap() error {
	if m.Status().Loaded {
		return nil
	}
	var last CommandResult
	for i := 0; i < 6; i++ {
		if i > 0 {
			m.sleep(time.Duration(100*(1<<(i-1))) * time.Millisecond)
		}
		last = m.run("bootstrap", strings.TrimSuffix(m.target(), "/"+Label), m.DefaultPlistPath())
		if last.Err == nil || m.Status().Loaded {
			return nil
		}
		lower := strings.ToLower(last.Output)
		if strings.Contains(lower, "already loaded") || strings.Contains(lower, "already bootstrapped") {
			return nil
		}
		if !strings.Contains(lower, "input/output error") && !strings.Contains(lower, "bootstrap failed: 5") {
			break
		}
	}
	return fmt.Errorf("launchctl bootstrap failed: %s (%v)", strings.TrimSpace(last.Output), last.Err)
}
func (m Manager) bootout() error {
	r := m.run("bootout", m.target())
	if r.Err != nil && m.Status().Loaded {
		return fmt.Errorf("launchctl bootout failed: %s (%v)", strings.TrimSpace(r.Output), r.Err)
	}
	m.sleep(200 * time.Millisecond)
	return nil
}
func (m Manager) kick() error {
	r := m.run("kickstart", "-k", m.target())
	if r.Err != nil {
		return fmt.Errorf("launchctl kickstart failed: %s (%v)", strings.TrimSpace(r.Output), r.Err)
	}
	return nil
}
func (m Manager) Ensure() (Result, error) {
	if err := m.AssertLive(); err != nil {
		return Result{}, err
	}
	exe := m.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return Result{}, err
		}
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return Result{}, err
	}
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	old, readErr := InstalledEntry(m.DefaultPlistPath())
	installed := !os.IsNotExist(readErr)
	if installed && readErr != nil {
		return Result{}, fmt.Errorf("refusing to replace unrecognized LaunchAgent: %w; stop --uninstall first", readErr)
	}
	force := strings.ToLower(strings.TrimSpace(m.getenv("JEVONIAN_SERVICE_TAKEOVER")))
	if old != "" && old != exe && force != "1" && force != "true" && force != "yes" && force != "on" {
		return Result{}, fmt.Errorf("a Jevonian service points at a different install: %s (this run: %s). Refusing takeover. Run `jevonian stop --uninstall` first, or set JEVONIAN_SERVICE_TAKEOVER=1", old, exe)
	}
	if old == exe {
		s := m.Status()
		if s.Loaded && s.PID > 0 {
			return Result{s, "running"}, nil
		}
		s, err := m.Start()
		return Result{s, "started"}, err
	}
	for _, dir := range []string{filepath.Dir(paths.ServeLogPath()), filepath.Dir(m.DefaultPlistPath())} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Result{}, err
		}
	}
	if err := m.bootout(); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(m.DefaultPlistPath(), []byte(BuildPlist(exe, paths.ServeLogPath(), m.env())), 0o644); err != nil {
		return Result{}, err
	}
	if err := m.bootstrap(); err != nil {
		return Result{}, err
	}
	if err := m.kick(); err != nil {
		return Result{}, err
	}
	action := "installed"
	if installed {
		action = "updated"
	}
	return Result{m.Status(), action}, nil
}
func (m Manager) Start() (Status, error) {
	if err := m.AssertLive(); err != nil {
		return Status{}, err
	}
	if _, err := os.Stat(m.DefaultPlistPath()); err != nil {
		return Status{}, fmt.Errorf("service is not installed. Run `jevonian start`: %w", err)
	}
	if err := m.bootstrap(); err != nil {
		return Status{}, err
	}
	if err := m.kick(); err != nil {
		return Status{}, err
	}
	return m.Status(), nil
}
func (m Manager) Stop(uninstall bool) (Status, error) {
	if err := m.AssertLive(); err != nil {
		return Status{}, err
	}
	if err := m.bootout(); err != nil {
		return Status{}, err
	}
	if uninstall {
		if err := os.Remove(m.DefaultPlistPath()); err != nil && !os.IsNotExist(err) {
			return Status{}, err
		}
	}
	return m.Status(), nil
}
func (m Manager) Restart() (Status, error) {
	if err := m.AssertLive(); err != nil {
		return Status{}, err
	}
	if !m.Status().Loaded {
		return m.Start()
	}
	if err := m.kick(); err != nil {
		if err := m.bootout(); err != nil {
			return Status{}, err
		}
		return m.Start()
	}
	return m.Status(), nil
}
func ReadLogTail(maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	b, err := os.ReadFile(paths.ServeLogPath())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}
