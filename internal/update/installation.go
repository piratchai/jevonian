// Package update checks and installs releases without owning server or CLI lifecycle.
package update

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const PackageName = "jevonian"

type Channel string

const (
	NPM     Channel = "npm"
	PNPM    Channel = "pnpm"
	Binary  Channel = "binary"
	Source  Channel = "source"
	Unknown Channel = "unknown"
)

type Installation struct {
	Channel     Channel
	Bin         string // Absolute package-manager binary when available.
	Executable  string // Resolved binary to replace, not the PATH symlink.
	PackagePath string // package.json belonging to the npm shim.
	Entry       string // The shim's bin/jevonian.js, for npm-channel installs.
	Command     string // Display only; never passed to a shell.
}

type DetectionOptions struct {
	Executable     string
	Entry          string // Optional JS shim entry; do not use the Go executable as Node's entry.
	NodeExecutable string
	Platform       string
	Env            map[string]string // A non-nil map isolates detection from the host environment.
}

func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}
func resolved(path string) string {
	if path == "" {
		return ""
	}
	if p, err := filepath.Abs(path); err == nil {
		path = p
	}
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	return path
}
func sourceCheckout(path string) bool {
	for dir := filepath.Dir(path); path != ""; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return true
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return strings.Contains(filepath.ToSlash(path), "/src/cli.")
}

// DetectInstallation is deliberately conservative: an arbitrary checkout or local
// node_modules dependency must not trigger a global npm install. Direct release
// binaries opt in with JEVONIAN_INSTALL_CHANNEL=binary (or explicit Installation).
func DetectInstallation(o DetectionOptions) Installation {
	exe := o.Executable
	if exe == "" {
		exe, _ = os.Executable()
	}
	exe = resolved(exe)
	entry := o.Entry
	if entry == "" {
		entry = envValue(o.Env, "JEVONIAN_NPM_ENTRY")
	}
	entry = resolved(entry)
	install := Installation{Channel: Unknown, Executable: exe, Entry: entry}
	if sourceCheckout(exe) || sourceCheckout(entry) {
		install.Channel = Source
		return install
	}
	override := Channel(envValue(o.Env, "JEVONIAN_INSTALL_CHANNEL"))
	if override == Source || override == Unknown {
		install.Channel = override
		return install
	}
	p := filepath.ToSlash(entry)
	global := strings.Contains(p, "/lib/node_modules/jevonian/") ||
		strings.Contains(p, "/pnpm/global/") || strings.Contains(p, "/AppData/Roaming/npm/node_modules/jevonian/")
	// A shim can attest to a nonstandard global prefix, but a source checkout above wins.
	global = global || envValue(o.Env, "JEVONIAN_INSTALL_GLOBAL") == "1"
	if global && strings.Contains(p, "/node_modules/jevonian/") {
		install.Channel = NPM
		if strings.Contains(p, "/pnpm/") || strings.Contains(p, "/.pnpm/") {
			install.Channel = PNPM
		}
		if override == NPM || override == PNPM {
			install.Channel = override
		}
		i := strings.LastIndex(p, "/node_modules/jevonian/")
		install.PackagePath = filepath.FromSlash(p[:i] + "/node_modules/jevonian/package.json")
		install.Entry = filepath.FromSlash(p)
		node := o.NodeExecutable
		if node == "" {
			node = envValue(o.Env, "JEVONIAN_NODE_EXECUTABLE")
		}
		platform := o.Platform
		if platform == "" {
			platform = runtime.GOOS
		}
		if node != "" {
			install.Bin = ResolvePackageManagerBin(install.Channel, node, platform)
		}
		install.Command = InstallCommand(install.Channel, install.Bin, "latest")
	} else if override == Binary {
		install.Channel = Binary
		install.Command = "jevonian update"
	}
	return install
}

func ResolvePackageManagerBin(channel Channel, node, platform string) string {
	name := string(channel)
	if platform == "windows" {
		name += ".cmd"
	}
	path := filepath.Join(filepath.Dir(node), name)
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path
	}
	return ""
}
func InstallArgs(channel Channel, version string) []string {
	if channel == PNPM {
		return []string{"add", "--global", PackageName + "@" + version}
	}
	return []string{"install", "--global", PackageName + "@" + version}
}
func shellQuote(value string) string {
	if value != "" && regexp.MustCompile(`^[\w@%+=:,./-]+$`).MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func InstallCommand(channel Channel, bin, version string) string {
	if bin == "" {
		bin = string(channel)
	}
	if version == "" {
		version = "latest"
	}
	return shellQuote(bin) + " " + strings.Join(InstallArgs(channel, version), " ")
}

// IsNewerVersion mirrors TS's three-component comparison (prerelease suffixes
// do not affect ordering). Registry and asset paths still use strict validation.
func IsNewerVersion(candidate, current string) bool {
	a, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	b, ok := parseVersion(current)
	if !ok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
func validVersion(v string) bool { _, ok := parseVersion(v); return ok && strings.TrimSpace(v) == v }
func parseVersion(v string) ([3]uint64, bool) {
	var out [3]uint64
	m := regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]+)?$`).FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return out, false
	}
	for i := range out {
		if _, err := fmt.Sscan(m[i+1], &out[i]); err != nil {
			return out, false
		}
	}
	return out, true
}
