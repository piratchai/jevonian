package tunnel

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CandidateUserBinDirs are directories where users commonly keep CLI tools
// (Homebrew, cargo, …). launchd and some GUI-spawned processes ship a stripped
// PATH (/usr/bin:/bin:/usr/sbin:/sbin) that omits these, so ngrok / cloudflared
// look "not installed" even when they are on disk. Port of src/user-path.ts.
func CandidateUserBinDirs(goos, home string) []string {
	if goos == "windows" {
		return []string{
			filepath.Join(home, "AppData", "Local", "Microsoft", "WindowsApps"),
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		}
	}
	return []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
		"/home/linuxbrew/.linuxbrew/bin",
		"/opt/local/bin",
		"/snap/bin",
	}
}

// AugmentPathOptions tune AugmentPath; zero values mean the real system.
type AugmentPathOptions struct {
	// Dirs to consider; nil → CandidateUserBinDirs that exist.
	Dirs      []string
	Exists    func(dir string) bool
	Separator string
}

// AugmentPath prepends missing user/package-manager bin dirs onto a PATH.
// Existing entries win on conflict; only directories present on disk and not
// already listed are added.
func AugmentPath(pathValue string, opts AugmentPathOptions) string {
	sep := opts.Separator
	if sep == "" {
		sep = string(os.PathListSeparator)
	}
	dirs := opts.Dirs
	if dirs == nil {
		exists := opts.Exists
		if exists == nil {
			exists = func(dir string) bool {
				info, err := os.Stat(dir)
				return err == nil && info.IsDir()
			}
		}
		home, _ := os.UserHomeDir()
		for _, dir := range CandidateUserBinDirs(runtime.GOOS, home) {
			if exists(dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	var current []string
	for _, entry := range strings.Split(pathValue, sep) {
		if entry != "" {
			current = append(current, entry)
		}
	}
	seen := make(map[string]bool, len(current))
	for _, entry := range current {
		seen[entry] = true
	}
	var prepend []string
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		prepend = append(prepend, dir)
		seen[dir] = true
	}
	return strings.Join(append(prepend, current...), sep)
}

// WithAugmentedPath copies env (KEY=value form) with PATH expanded for
// user-installed CLIs. A missing PATH is added.
func WithAugmentedPath(env []string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "PATH="); ok {
			out = append(out, "PATH="+AugmentPath(value, AugmentPathOptions{}))
			found = true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+AugmentPath("", AugmentPathOptions{}))
	}
	return out
}

// ApplyUserBinPath sets this process's PATH so later spawns can find Homebrew
// / local binaries under launchd-style minimal environments.
func ApplyUserBinPath() string {
	next := AugmentPath(os.Getenv("PATH"), AugmentPathOptions{})
	_ = os.Setenv("PATH", next)
	return next
}
