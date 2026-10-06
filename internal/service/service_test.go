package service

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlistAndEntry(t *testing.T) {
	env := map[string]string{"PATH": "/bin", "HTTPS_PROXY": "http://a&b", "JEVONIAN_NO_OPEN": "1"}
	body := BuildPlist("/opt/Jevonian & Tools/jevonian", "/tmp/serve.log", env)
	if !strings.Contains(body, "&amp;") || strings.Contains(body, "<string>node</string>") {
		t.Fatal(body)
	}
	decoder := xml.NewDecoder(strings.NewReader(body))
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatal(err)
			}
			break
		}
	}
	path := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := InstalledEntry(path)
	if err != nil || entry != "/opt/Jevonian & Tools/jevonian" {
		t.Fatalf("%s %v", entry, err)
	}
	old := `<plist><dict><key>ProgramArguments</key><array><string>/usr/bin/node</string><string>/old/cli.js</string><string>serve</string></array></dict></plist>`
	os.WriteFile(path, []byte(old), 0o644)
	entry, err = InstalledEntry(path)
	if err != nil || entry != "/old/cli.js" {
		t.Fatalf("%s %v", entry, err)
	}
}
func TestRedirectGuardBeforeAnyCommand(t *testing.T) {
	calls := 0
	m := Manager{Platform: "darwin", Home: t.TempDir(), Env: map[string]string{"JEVONIAN_SERVICE_PLIST": "/tmp/test.plist"}, Command: func(...string) CommandResult { calls++; return CommandResult{} }}
	if _, err := m.Ensure(); err == nil {
		t.Fatal("ensure accepted override")
	}
	if _, err := m.Start(); err == nil {
		t.Fatal("start accepted override")
	}
	if _, err := m.Stop(true); err == nil {
		t.Fatal("stop accepted override")
	}
	if _, err := m.Restart(); err == nil {
		t.Fatal("restart accepted override")
	}
	if calls != 0 {
		t.Fatal("live launchctl reached")
	}
}
func TestTakeoverRefusal(t *testing.T) {
	calls := 0
	home := t.TempDir()
	oldBin := filepath.Join(home, "old", "jevonian")
	os.MkdirAll(filepath.Dir(oldBin), 0o755)
	os.WriteFile(oldBin, []byte("#!/bin/sh\n"), 0o755)
	m := Manager{Platform: "darwin", Home: home, Executable: "/new/jevonian", Env: map[string]string{}, Command: func(...string) CommandResult { calls++; return CommandResult{} }, Sleep: func(time.Duration) {}}
	path := m.DefaultPlistPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(BuildPlist(oldBin, "/tmp/log", nil)), 0o644)
	if _, err := m.Ensure(); err == nil || !strings.Contains(err.Error(), "Refusing takeover") {
		t.Fatalf("%v", err)
	}
	if calls != 0 {
		t.Fatal("refusal touched live job")
	}
}
func TestTakeoverMissingEntry(t *testing.T) {
	loaded := false
	home := t.TempDir()
	t.Setenv("JEVONIAN_SERVE_LOG", filepath.Join(home, "data", "serve.log"))
	m := Manager{Platform: "darwin", Home: home, UID: "501", Executable: "/new/jevonian", Env: map[string]string{}, Sleep: func(time.Duration) {}, Command: func(args ...string) CommandResult {
		switch args[0] {
		case "print":
			if loaded {
				return CommandResult{Output: "pid = 99"}
			}
			return CommandResult{Err: errors.New("not loaded")}
		case "bootout":
			loaded = false
		case "bootstrap":
			loaded = true
		}
		return CommandResult{}
	}}
	path := m.DefaultPlistPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	// Stale Node LaunchAgent after cutover: dist/cli.mjs is gone.
	os.WriteFile(path, []byte(`<plist><dict><key>ProgramArguments</key><array><string>/usr/bin/node</string><string>/missing/dist/cli.mjs</string><string>serve</string></array></dict></plist>`), 0o644)
	result, err := m.Ensure()
	if err != nil || result.Action != "updated" || result.Status.PID != 99 {
		t.Fatalf("%+v %v", result, err)
	}
	entry, err := InstalledEntry(path)
	if err != nil || entry != "/new/jevonian" {
		t.Fatalf("%s %v", entry, err)
	}
}
func TestEnsureFakeLifecycleAndEnvHygiene(t *testing.T) {
	loaded := false
	var calls []string
	home := t.TempDir()
	t.Setenv("JEVONIAN_SERVE_LOG", filepath.Join(home, "data", "serve.log"))
	m := Manager{Platform: "darwin", Home: home, UID: "501", Executable: "/new/jevonian", Env: map[string]string{"PATH": "/usr/bin:/bin", "JEVONIAN_CONFIG": "/scratch/config", "JEVONIAN_DATA_DIR": "/scratch/data", "JEVONIAN_LEDGER": "/scratch/ledger", "HTTPS_PROXY": "http://proxy"}, Sleep: func(time.Duration) {}, Command: func(args ...string) CommandResult {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "print":
			if loaded {
				return CommandResult{Output: "pid = 123"}
			}
			return CommandResult{Err: errors.New("not loaded")}
		case "bootout":
			loaded = false
		case "bootstrap":
			loaded = true
		}
		return CommandResult{}
	}}
	result, err := m.Ensure()
	if err != nil || result.Action != "installed" || result.Status.PID != 123 {
		t.Fatalf("%+v %v", result, err)
	}
	b, _ := os.ReadFile(m.DefaultPlistPath())
	if strings.Contains(string(b), "/scratch/") || !strings.Contains(string(b), "HTTPS_PROXY") {
		t.Fatal(string(b))
	}
	before := len(calls)
	result, err = m.Ensure()
	if err != nil || result.Action != "running" {
		t.Fatalf("%+v %v", result, err)
	}
	for _, call := range calls[before:] {
		if !strings.HasPrefix(call, "print ") {
			t.Fatalf("healthy job mutated: %s", call)
		}
	}
	if _, err := m.Stop(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.DefaultPlistPath()); !os.IsNotExist(err) {
		t.Fatal("uninstall did not remove plist")
	}
}
func TestNonDarwin(t *testing.T) {
	m := Manager{Platform: "linux", Home: t.TempDir(), Env: map[string]string{}, Command: func(...string) CommandResult { t.Fatal("launchctl on Linux"); return CommandResult{} }}
	if _, err := m.Ensure(); err == nil {
		t.Fatal("Linux ensure accepted")
	}
	s := m.Status()
	if s.Loaded || !strings.Contains(s.Detail, "macOS") {
		t.Fatalf("%+v", s)
	}
}
func TestRestartOntoCurrent(t *testing.T) {
	loaded := true
	home := t.TempDir()
	t.Setenv("JEVONIAN_SERVE_LOG", filepath.Join(home, "data", "serve.log"))
	bin := filepath.Join(home, "native", "jevonian")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	var calls []string
	m := Manager{Platform: "darwin", Home: home, UID: "501", Executable: bin, Env: map[string]string{}, Sleep: func(time.Duration) {}, Command: func(args ...string) CommandResult {
		calls = append(calls, args[0])
		switch args[0] {
		case "print":
			if loaded {
				return CommandResult{Output: "pid = 42"}
			}
			return CommandResult{Err: errors.New("not loaded")}
		case "bootout":
			loaded = false
		case "bootstrap":
			loaded = true
		}
		return CommandResult{}
	}}
	path := m.DefaultPlistPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(BuildPlist(bin, "/tmp/log", nil)), 0o644)
	if _, err := m.RestartOntoCurrent(); err != nil {
		t.Fatal(err)
	}
	foundKick := false
	for _, c := range calls {
		if c == "kickstart" {
			foundKick = true
		}
	}
	if !foundKick {
		t.Fatalf("same-path restart skipped kickstart: %v", calls)
	}
	os.WriteFile(path, []byte(`<plist><dict><key>ProgramArguments</key><array><string>/usr/bin/node</string><string>/gone/dist/cli.mjs</string><string>serve</string></array></dict></plist>`), 0o644)
	if _, err := m.RestartOntoCurrent(); err != nil {
		t.Fatal(err)
	}
	entry, err := InstalledEntry(path)
	if err != nil || !sameInstall(entry, bin) {
		t.Fatalf("stale entry not rewritten: %s %v (want %s)", entry, err, bin)
	}
}
func TestAllowTakeoverSamePackageCutover(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "lib", "node_modules", "jevonian")
	old := filepath.Join(pkg, "dist", "cli.mjs")
	neu := filepath.Join(pkg, "native", "jevonian-darwin-arm64")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.MkdirAll(filepath.Dir(neu), 0o755)
	os.WriteFile(old, []byte("// stale"), 0o644)
	os.WriteFile(neu, []byte("#!/bin/sh\n"), 0o755)
	if !allowTakeover(old, neu) {
		t.Fatal("same-package Node→Go should allow takeover")
	}
	other := filepath.Join(root, "other", "jevonian")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)
	if allowTakeover(other, neu) {
		t.Fatal("different install must still refuse")
	}
	if allowTakeover("/missing/dist/cli.mjs", neu) != true {
		t.Fatal("missing entry should allow")
	}
}
func TestHealIfNeeded(t *testing.T) {
	loaded := false
	home := t.TempDir()
	t.Setenv("JEVONIAN_SERVE_LOG", filepath.Join(home, "data", "serve.log"))
	bin := filepath.Join(home, "lib", "node_modules", "jevonian", "native", "jevonian")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	m := Manager{Platform: "darwin", Home: home, UID: "501", Executable: bin, Env: map[string]string{}, Sleep: func(time.Duration) {}, Command: func(args ...string) CommandResult {
		switch args[0] {
		case "print":
			if loaded {
				return CommandResult{Output: "pid = 7"}
			}
			return CommandResult{Err: errors.New("not loaded")}
		case "bootout":
			loaded = false
		case "bootstrap":
			loaded = true
		}
		return CommandResult{}
	}}
	path := m.DefaultPlistPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`<plist><dict><key>ProgramArguments</key><array><string>/usr/bin/node</string><string>/gone/dist/cli.mjs</string><string>serve</string></array></dict></plist>`), 0o644)
	healed, err := m.HealIfNeeded()
	if err != nil || !healed {
		t.Fatalf("healed=%v err=%v", healed, err)
	}
	entry, _ := InstalledEntry(path)
	if !sameInstall(entry, bin) {
		t.Fatalf("entry %s", entry)
	}
	healed, err = m.HealIfNeeded()
	if err != nil || healed {
		t.Fatalf("second heal should be no-op: %v %v", healed, err)
	}
}
