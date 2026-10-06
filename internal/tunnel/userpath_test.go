package tunnel

import (
	"slices"
	"strings"
	"testing"
)

func TestCandidateUserBinDirs(t *testing.T) {
	dirs := CandidateUserBinDirs("darwin", "/Users/me")
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/Users/me/.local/bin"} {
		if !slices.Contains(dirs, want) {
			t.Fatalf("%v missing %s", dirs, want)
		}
	}
}

func TestAugmentPath(t *testing.T) {
	got := AugmentPath("/usr/bin:/bin:/usr/sbin:/sbin", AugmentPathOptions{
		Dirs:      []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"},
		Separator: ":",
	})
	if got != "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("%q", got)
	}

	got = AugmentPath("/opt/homebrew/bin:/usr/bin", AugmentPathOptions{
		Dirs:      []string{"/opt/homebrew/bin", "/usr/local/bin"},
		Separator: ":",
	})
	if got != "/usr/local/bin:/opt/homebrew/bin:/usr/bin" {
		t.Fatalf("%q", got)
	}
}

func TestWithAugmentedPath(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin", "HOME=/Users/me"}
	next := WithAugmentedPath(env)
	if !slices.Contains(next, "HOME=/Users/me") {
		t.Fatalf("%v", next)
	}
	var path string
	for _, kv := range next {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if !strings.Contains(path, "/usr/bin") || path == "/usr/bin:/bin" {
		t.Fatalf("PATH %q", path)
	}
}

func TestApplyUserBinPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	got := ApplyUserBinPath()
	if strings.HasPrefix(got, "/usr/bin") {
		t.Fatalf("did not prepend: %q", got)
	}
}
