package sentinel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/oauth/jsoncedit"
)

func TestModelLabel(t *testing.T) {
	if ModelLabel("jevonian/auto") != "Jevonian Auto" || ModelLabel("jevonian/utility") != "Jevonian Utility" {
		t.Fatalf("%q %q", ModelLabel("jevonian/auto"), ModelLabel("jevonian/utility"))
	}
	if ModelLabel("deepseek-v4-1-flash") != "Jevonian Deepseek V4 1 Flash" {
		t.Fatalf("%q", ModelLabel("deepseek-v4-1-flash"))
	}
}

func TestResolveModels(t *testing.T) {
	cases := []struct {
		models   []string
		override string
		want     ModelRouting
	}{
		{[]string{"jevonian/auto", "jevonian/utility"}, "", ModelRouting{"jevonian/auto", "jevonian/utility"}},
		{[]string{"jevonian/auto"}, "", ModelRouting{"jevonian/auto", "jevonian/auto"}},
		{[]string{"jevonian/auto"}, "jevonian/plan", ModelRouting{"jevonian/plan", "jevonian/plan"}},
		{nil, "", ModelRouting{"jevonian/auto", "jevonian/auto"}},
	}
	for _, c := range cases {
		if got := ResolveModels(c.models, c.override); got != c.want {
			t.Errorf("%v/%q → %+v want %+v", c.models, c.override, got, c.want)
		}
	}
}

func TestEnv(t *testing.T) {
	env := Env(8787, []string{"jevonian/auto", "jevonian/utility"}, "")
	checks := map[string]string{
		"ANTHROPIC_BASE_URL":                  "http://127.0.0.1:8787",
		"ANTHROPIC_AUTH_TOKEN":                "jevonian-local",
		"ANTHROPIC_API_KEY":                   "",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":        "jevonian/auto",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":      "jevonian/auto",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":       "jevonian/utility",
		"CLAUDE_CODE_SUBAGENT_MODEL":          "jevonian/auto",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "Jevonian Auto",
		"ANTHROPIC_CUSTOM_MODEL_OPTION":       "jevonian/auto",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":  "Jevonian Auto",
	}
	for k, v := range checks {
		got, ok := env[k]
		if !ok || got != v {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, v)
		}
	}
	for k := range env {
		if !slices.Contains(ManagedEnvKeys, k) {
			t.Errorf("env key %s not in ManagedEnvKeys", k)
		}
	}
	if len(env) != len(ManagedEnvKeys) {
		t.Errorf("env has %d keys, managed list %d", len(env), len(ManagedEnvKeys))
	}
}

func setupHome(t *testing.T) (settings string, state RestoreState) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	return SettingsPath(), RestoreState{DataDir: filepath.Join(home, "data"), Client: "claude-code"}
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, data)
	}
	return out
}

func TestApplyAndRestorePriorContent(t *testing.T) {
	path, state := setupHome(t)
	original, _ := json.MarshalIndent(map[string]any{
		"env":   map[string]any{"API_TIMEOUT_MS": "1200000", "KEEP_ME": "yes"},
		"theme": "dark",
	}, "", "  ")
	if err := os.WriteFile(path, append(original, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyClaudeCode(8787, []string{"jevonian/auto", "jevonian/utility"}, state)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status.Status != "connected" || applied.Status.BaseURL != "http://127.0.0.1:8787" {
		t.Fatalf("status %+v", applied.Status)
	}
	connected := readJSONMap(t, path)
	env := connected["env"].(map[string]any)
	if connected["theme"] != "dark" || env["KEEP_ME"] != "yes" || env["API_TIMEOUT_MS"] != "1200000" ||
		env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787" ||
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "jevonian/utility" ||
		env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"] != "Jevonian Auto" {
		t.Fatalf("connected %+v", connected)
	}

	RestoreClaudeCode(state)
	restored := readJSONMap(t, path)
	want := map[string]any{
		"env":   map[string]any{"API_TIMEOUT_MS": "1200000", "KEEP_ME": "yes"},
		"theme": "dark",
	}
	if !reflect.DeepEqual(restored, want) {
		t.Fatalf("restored %+v", restored)
	}
}

func TestApplyCreatesAndRestoreRemoves(t *testing.T) {
	path, state := setupHome(t)
	if _, err := ApplyClaudeCode(9090, []string{"jevonian/auto"}, state); err != nil {
		t.Fatal(err)
	}
	written := readJSONMap(t, path)
	if written["env"].(map[string]any)["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9090" {
		t.Fatalf("written %+v", written)
	}
	RestoreClaudeCode(state)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("settings.json should be gone: %v", err)
	}
}

func TestApplyIsSurgical(t *testing.T) {
	path, state := setupHome(t)
	original := `{
  // my hand-written layout
  "theme": "dark",
  "env": {
    "API_TIMEOUT_MS": "1200000",   /* keep */
    "KEEP_ME": "yes"
  },
  "attribution": { "commit": "" }
}
`
	_ = os.WriteFile(path, []byte(original), 0o644)
	if _, err := ApplyClaudeCode(8787, []string{"jevonian/auto"}, state); err != nil {
		t.Fatal(err)
	}
	connected, _ := os.ReadFile(path)
	text := string(connected)
	for _, needle := range []string{
		"// my hand-written layout",
		`"API_TIMEOUT_MS": "1200000",   /* keep */`,
		`"KEEP_ME": "yes"`,
		`"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"`,
		`"attribution": { "commit": "" }`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("missing %q in:\n%s", needle, text)
		}
	}
	keys := jsoncedit.ObjectKeys(text, []string{"env"})
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "KEEP_ME", "API_TIMEOUT_MS"} {
		if !slices.Contains(keys, k) {
			t.Fatalf("env keys %v missing %s", keys, k)
		}
	}
}

func TestRestoreStripsOnlyOwnKeys(t *testing.T) {
	path, state := setupHome(t)
	_ = os.WriteFile(path, []byte(`{
  // settings I keep by hand
  "theme": "dark",
  "env": {
    "KEEP_ME": "yes"
  }
}
`), 0o644)
	if _, err := ApplyClaudeCode(8787, []string{"jevonian/auto"}, state); err != nil {
		t.Fatal(err)
	}
	// Drop captured state so restore takes the surgical strip path.
	state.Clear()
	RestoreClaudeCode(state)
	restored, _ := os.ReadFile(path)
	text := string(restored)
	if !strings.Contains(text, "// settings I keep by hand") || !strings.Contains(text, `"KEEP_ME": "yes"`) ||
		strings.Contains(text, "ANTHROPIC_BASE_URL") || strings.Contains(text, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("restored:\n%s", text)
	}
}

func TestRestoreRemovesEmptyEnv(t *testing.T) {
	path, state := setupHome(t)
	_ = os.WriteFile(path, []byte("{\n  \"theme\": \"dark\"\n}\n"), 0o644)
	if _, err := ApplyClaudeCode(8787, []string{"jevonian/auto"}, state); err != nil {
		t.Fatal(err)
	}
	state.Clear()
	RestoreClaudeCode(state)
	got := readJSONMap(t, path)
	if !reflect.DeepEqual(got, map[string]any{"theme": "dark"}) {
		t.Fatalf("restored %+v", got)
	}
}

func TestApplyRequiresModels(t *testing.T) {
	_, state := setupHome(t)
	if _, err := ApplyClaudeCode(8787, nil, state); err == nil {
		t.Fatal("expected error")
	}
}

func TestCodexSentinelNeverOverwritesLogin(t *testing.T) {
	dir := t.TempDir()
	s := CodexSentinel{Dir: dir}
	created, path, err := s.Ensure()
	if err != nil || !created || path != filepath.Join(dir, "auth.json") {
		t.Fatalf("%v %q %v", created, path, err)
	}
	got := readJSONMap(t, path)
	if got["OPENAI_API_KEY"] != Marker || got["auth_mode"] != "apikey" {
		t.Fatalf("sentinel %+v", got)
	}
	if !s.IsOurs() {
		t.Fatal("IsOurs")
	}
	s.Clear()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("sentinel not cleared")
	}

	// A real login is never touched.
	real := `{"tokens":{"access_token":"real","refresh_token":"r"}}`
	_ = os.WriteFile(path, []byte(real), 0o600)
	created, _, err = s.Ensure()
	if err != nil || created {
		t.Fatalf("overwrote login: created=%v err=%v", created, err)
	}
	s.Clear()
	data, _ := os.ReadFile(path)
	if string(data) != real {
		t.Fatalf("login modified: %s", data)
	}
}

func TestPrepareLaunch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	look := func(string) (string, error) { return bin, nil }
	spec, err := PrepareLaunch(8787, []string{"jevonian/auto"}, "", []string{"--resume"},
		[]string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-leaked"}, look)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Path != bin || !reflect.DeepEqual(spec.Args, []string{"--model", "jevonian/auto", "--resume"}) {
		t.Fatalf("%+v", spec)
	}
	if slices.Contains(spec.Env, "ANTHROPIC_API_KEY=sk-leaked") || !slices.Contains(spec.Env, "ANTHROPIC_API_KEY=") ||
		!slices.Contains(spec.Env, "PATH=/usr/bin") || !slices.Contains(spec.Env, "ANTHROPIC_AUTH_TOKEN=jevonian-local") {
		t.Fatalf("env %v", spec.Env)
	}
}

func TestGatewayProfileFieldsCarrySentinel(t *testing.T) {
	fields := GatewayProfileFields("http://127.0.0.1:8787", nil)
	if fields["inferenceGatewayApiKey"] != Marker || fields["inferenceProvider"] != "gateway" {
		t.Fatalf("%+v", fields)
	}
}
