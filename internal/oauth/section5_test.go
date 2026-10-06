package oauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

// Fixture-only tests for the env/path vocabulary of section 5. Every path lives
// in a temp HOME; no real credential, config or keychain is touched.

func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return home
}

func TestCredentialsFilePathAndMode(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("JEVONIAN_CREDENTIALS", "")
	store := multiacct.DefaultStore()
	if err := store.Set("deepseek", "sk-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("brain:typesafe", "bk-1"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "jevonian", "credentials.json")
	info, err := os.Stat(want)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("default path/mode: %v %v", info, err)
	}
	if store.Get("brain:typesafe") != "bk-1" || store.Get("deepseek") != "sk-a" {
		t.Fatal("provider + brain:<channel> keys")
	}
	// XDG_CONFIG_HOME moves the default.
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := multiacct.DefaultStore().Set("p", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(xdg, "jevonian", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	// JEVONIAN_CREDENTIALS beats both.
	override := filepath.Join(home, "elsewhere", "c.json")
	t.Setenv("JEVONIAN_CREDENTIALS", override)
	if err := multiacct.DefaultStore().Set("q", "k2"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed["q"]["apiKey"] != "k2" || len(parsed) != 1 {
		t.Fatalf("override content %s %v", raw, err)
	}
	// Entries without a string apiKey are ignored on read, like loadCredentials.
	_ = os.WriteFile(override, []byte(`{"a":{"apiKey":"x"},"b":{"apiKey":""},"c":{"apiKey":5},"d":7}`), 0o600)
	if got := multiacct.DefaultStore().Load(); len(got) != 1 || got["a"].APIKey != "x" {
		t.Fatalf("load %v", got)
	}
}

func TestDefaultPathsUnderTempHome(t *testing.T) {
	home := isolatedHome(t)
	for _, k := range []string{"JEVONIAN_CLAUDE_CREDENTIALS", "JEVONIAN_CODEX_AUTH", "JEVONIAN_DEVIN_CREDENTIALS", "JEVONIAN_CURSOR_AUTH", "JEVONIAN_ANTIGRAVITY_TOKEN"} {
		t.Setenv(k, "")
	}
	if got := ClaudeCredentialsPath(nil); got != filepath.Join(home, ".claude", ".credentials.json") {
		t.Fatalf("claude %s", got)
	}
	if got := CodexAuthPath(nil); got != filepath.Join(home, ".codex", "auth.json") {
		t.Fatalf("codex %s", got)
	}
	if got := DevinCredentialsPath(nil); got != filepath.Join(home, ".local", "share", "devin", "credentials.toml") {
		t.Fatalf("devin %s", got)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	if got := DevinCredentialsPath(nil); got != filepath.Join(home, "data", "devin", "credentials.toml") {
		t.Fatalf("devin xdg %s", got)
	}
	// env overrides
	t.Setenv("JEVONIAN_CLAUDE_CREDENTIALS", "/x/claude.json")
	t.Setenv("JEVONIAN_CODEX_AUTH", "/x/codex.json")
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", "/x/devin.toml")
	t.Setenv("JEVONIAN_CURSOR_AUTH", "/x/cursor.json")
	if ClaudeCredentialsPath(nil) != "/x/claude.json" || CodexAuthPath(nil) != "/x/codex.json" ||
		DevinCredentialsPath(nil) != "/x/devin.toml" || CursorAuthPath(nil) != "/x/cursor.json" {
		t.Fatal("env overrides")
	}
}

func TestClaudeEnvPathDisablesKeychainFallback(t *testing.T) {
	isolatedHome(t)
	t.Setenv("JEVONIAN_CLAUDE_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	called := false
	r := &Resolver{Keychain: keychainFunc(func() { called = true })}
	_, err := r.Resolve(t.Context(), ResolveOptions{Source: "claude-code"})
	if err == nil || !strings.Contains(err.Error(), "Claude Code credentials not found") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("keychain consulted although JEVONIAN_CLAUDE_CREDENTIALS is set")
	}
	if r.HasCredential("claude-code", nil) {
		t.Fatal("HasCredential must follow the env path, not assume the keychain")
	}
}

type keychainFunc func()

func (f keychainFunc) Find(_ context.Context, _, _ string) (string, error) {
	f()
	return "", nil
}

func (f keychainFunc) Add(_ context.Context, _, _, _ string) error { f(); return nil }

func TestAntigravityProjectResolution(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("JEVONIAN_ANTIGRAVITY_PROJECT", "")
	if got := ResolveAntigravityProject(); got != "default-cli-project" {
		t.Fatalf("default %q", got)
	}
	cache := filepath.Join(home, ".gemini", "antigravity-cli", "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "default_project_id.txt"), []byte("  cached-proj \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAntigravityProject(); got != "cached-proj" {
		t.Fatalf("cache %q", got)
	}
	t.Setenv("JEVONIAN_ANTIGRAVITY_PROJECT", " env-proj ")
	if got := ResolveAntigravityProject(); got != "env-proj" {
		t.Fatalf("env %q", got)
	}
}

func TestDevinClientVersionEnvAndServerURL(t *testing.T) {
	isolatedHome(t)
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("windsurf_api_key = \"k\"\napi_server_url = \"https://srv.example/\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	if DevinServerURL(nil) != "https://srv.example" {
		t.Fatalf("server %s", DevinServerURL(nil))
	}
}
