package service

import (
	"os"
	"path/filepath"
	"strings"
)

// Keep the service package buildable on every platform without importing
// tunnel's process-control implementation. This mirrors its PATH policy.
func augmentPath(value, home string) string {
	sep := string(os.PathListSeparator)
	current := strings.Split(value, sep)
	seen := map[string]bool{}
	for _, dir := range current {
		seen[dir] = true
	}
	candidates := []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "/usr/local/sbin", "/home/linuxbrew/.linuxbrew/bin", "/opt/local/bin", "/snap/bin"}
	var added []string
	for _, dir := range candidates {
		if seen[dir] {
			continue
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			added = append(added, dir)
			seen[dir] = true
		}
	}
	for _, dir := range current {
		if dir != "" {
			added = append(added, dir)
		}
	}
	return strings.Join(added, sep)
}
