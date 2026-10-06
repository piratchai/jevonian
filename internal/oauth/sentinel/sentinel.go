// Package sentinel owns the loopback-only placeholder credential desktop
// clients are given so they can talk to Jevonian without a real key.
//
// Port of the auth.json / settings.json parts of src/clients.ts and the
// Claude Code env block of src/claude-code.ts. The desktop apps re-read their
// own credentials on every launch, so a real `sk-jev-…` key written into their
// config would be overwritten — and worse, would leak a working key onto disk.
// The sentinel is NOT a credential: the server accepts it only from a loopback
// peer (internal/server/localclient.go).
//
// Surgical JSONC editing (comments, key order, formatting survive) is supplied
// by internal/oauth/jsoncedit.
package sentinel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/oauth/jsoncedit"
)

// Marker is the sentinel value desktop clients present. Same token the
// TypeScript used; internal/server/localclient.go carries its own list.
const Marker = "jevonian-local"

// ---------------------------------------------------------------------------
// Codex auth.json
// ---------------------------------------------------------------------------

// Codex sends whatever credential is in auth.json to `openai_base_url`. A real
// ChatGPT login would be rejected by Jevonian, so the client is given a local
// sentinel instead.
//
// Critically, an existing auth.json is NEVER overwritten: a real login holds a
// refresh token that cannot be regenerated without the user signing in again,
// so Jevonian only creates the file when it is absent — exactly what Ollama's
// Codex launcher does with O_CREATE|O_EXCL.
type CodexSentinel struct {
	// Dir is ~/.codex or $CODEX_HOME; override in tests.
	Dir string
}

func (c CodexSentinel) dir() string {
	if c.Dir != "" {
		return c.Dir
	}
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// AuthPath is dir()/auth.json.
func (c CodexSentinel) AuthPath() string {
	return filepath.Join(c.dir(), "auth.json")
}

// Ensure writes the sentinel auth.json when the file is absent. Reports
// whether it created the file.
func (c CodexSentinel) Ensure() (created bool, path string, err error) {
	path = c.AuthPath()
	if _, err := os.Stat(path); err == nil {
		return false, path, nil
	}
	if err := writeJSON(path, map[string]any{
		"OPENAI_API_KEY": Marker,
		"auth_mode":      "apikey",
	}); err != nil {
		return false, path, err
	}
	return true, path, nil
}

// IsOurs reports whether auth.json holds our sentinel rather than a real login.
func (c CodexSentinel) IsOurs() bool {
	data, err := readJSON(c.AuthPath())
	return err == nil &&
		data["auth_mode"] == "apikey" &&
		data["OPENAI_API_KEY"] == Marker
}

// Clear removes only Jevonian's own sentinel. A user login or API key written
// after the fact is left untouched.
func (c CodexSentinel) Clear() {
	if !c.IsOurs() {
		return
	}
	_ = os.Remove(c.AuthPath())
}

// ---------------------------------------------------------------------------
// Claude Code settings.json env
// ---------------------------------------------------------------------------

// ManagedEnvKeys are the env keys Jevonian owns inside Claude Code's settings
// `env` block (the ANTHROPIC_* remap plus telemetry opt-outs).
var ManagedEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME",
	"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION",
	"ANTHROPIC_CUSTOM_MODEL_OPTION",
	"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
	"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
	"CLAUDE_CODE_ATTRIBUTION_HEADER",
	"DISABLE_ERROR_REPORTING",
	"DISABLE_FEEDBACK_COMMAND",
	"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY",
}

// ModelLabel renders `jevonian/auto` as "Jevonian Auto" — the picker label
// Claude Code shows instead of the raw id.
func ModelLabel(model string) string {
	tail := model
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		tail = model[i+1:]
	}
	words := regexp.MustCompile(`[-_.\s]+`).Split(tail, -1)
	var parts []string
	for _, w := range words {
		if w == "" {
			continue
		}
		parts = append(parts, strings.ToUpper(w[:1])+w[1:])
	}
	return strings.Join(append([]string{"Jevonian"}, parts...), " ")
}

// ModelRouting is the primary/fast pair Claude Code's tier env vars resolve to.
type ModelRouting struct {
	// Primary is the Opus / Sonnet / subagent / custom picker entry.
	Primary string
	// Fast is the Haiku / background model.
	Fast string
}

// ResolveModels picks the primary and fast models, mirroring Ollama: one
// selected model for opus/sonnet/subagent, and a lighter alias for haiku when
// `jevonian/utility` is in the offer list.
func ResolveModels(models []string, override string) ModelRouting {
	primary := strings.TrimSpace(override)
	if primary == "" && len(models) > 0 {
		primary = strings.TrimSpace(models[0])
	}
	if primary == "" {
		primary = "jevonian/auto"
	}
	fast := primary
	for _, m := range models {
		if m == "jevonian/utility" || strings.HasSuffix(m, "/utility") {
			fast = m
			break
		}
	}
	return ModelRouting{Primary: primary, Fast: fast}
}

// BaseURL is the loopback URL Claude Code points at.
func BaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// Env is the map Claude Code needs to talk to Jevonian — same shape as
// `ollama launch claude`, plus picker labels so the CLI shows Jevonian names.
func Env(port int, models []string, model string) map[string]string {
	route := ResolveModels(models, model)
	primaryLabel := ModelLabel(route.Primary)
	fastLabel := ModelLabel(route.Fast)
	return map[string]string{
		"ANTHROPIC_BASE_URL":                         BaseURL(port),
		"ANTHROPIC_AUTH_TOKEN":                       Marker,
		"ANTHROPIC_API_KEY":                          "", // clears a shell-exported key so AUTH_TOKEN wins
		"ANTHROPIC_DEFAULT_OPUS_MODEL":               route.Primary,
		"ANTHROPIC_DEFAULT_SONNET_MODEL":             route.Primary,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":              route.Fast,
		"CLAUDE_CODE_SUBAGENT_MODEL":                 route.Primary,
		"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME":          primaryLabel,
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME":        primaryLabel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME":         fastLabel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION":   "Routed by Jevonian",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION": "Routed by Jevonian",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION":  "Routed by Jevonian",
		// Dedicated picker row so `/model` lists Jevonian even when aliases stay.
		"ANTHROPIC_CUSTOM_MODEL_OPTION":             route.Primary,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":        primaryLabel,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Routed by Jevonian",
		"CLAUDE_CODE_ATTRIBUTION_HEADER":            "0",
		"DISABLE_ERROR_REPORTING":                   "1",
		"DISABLE_FEEDBACK_COMMAND":                  "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":       "1",
	}
}

// SettingsPath is ~/.claude/settings.json (or $CLAUDE_CONFIG_DIR/settings.json).
func SettingsPath() string {
	base := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "settings.json")
}

// ClaudeApplyResult is what a connect reported back.
type ClaudeApplyResult struct {
	Status  ClaudeStatus
	Written []string
}

// ClaudeStatus is the state of the Claude Code integration.
type ClaudeStatus struct {
	Installed  bool
	Status     string // "connected" | "disconnected" | "unavailable"
	ConfigPath string
	BaseURL    string
	Reason     string
}

// RestoreState is the files' pre-connect contents, keyed by path. Written once
// under dataDir/clients so a disconnect always has the original to restore.
type RestoreState struct {
	DataDir string
	Client  string
}

func (s RestoreState) path() string {
	return filepath.Join(s.DataDir, "clients", s.Client+"-restore.json")
}

// Save persists the first connect's originals; later connects do not overwrite.
func (s RestoreState) Save(files map[string]string) {
	path := s.path()
	if _, err := os.Stat(path); err == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = writeJSON(path, map[string]any{
		"client":  s.Client,
		"savedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"files":   files,
	})
}

// Load returns the captured originals, or nil when none are recorded.
func (s RestoreState) Load() map[string]string {
	data, err := readJSON(s.path())
	if err != nil {
		return nil
	}
	files, ok := data["files"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(files))
	for path, v := range files {
		if str, ok := v.(string); ok {
			out[path] = str
		}
	}
	return out
}

// Clear backs the state file up and removes it.
func (s RestoreState) Clear() { backupFile(s.path()) }

// ApplyClaudeCode writes the managed env into ~/.claude/settings.json,
// surgically: only the managed keys change; comments, key order, and
// formatting survive. Creates a plain JSON file when absent.
func ApplyClaudeCode(port int, models []string, state RestoreState) (ClaudeApplyResult, error) {
	if len(models) == 0 {
		return ClaudeApplyResult{}, fmt.Errorf("select at least one model before connecting Claude Code")
	}
	settingsPath := SettingsPath()
	original := readText(settingsPath)
	state.Save(map[string]string{settingsPath: original})

	managed := Env(port, models, "")
	var text string
	if original == "" {
		data, _ := json.MarshalIndent(map[string]any{"env": managed}, "", "  ")
		text = string(data) + "\n"
	} else {
		edits := make([]jsoncedit.Edit, 0, len(managed)+1)
		for key, value := range managed {
			edits = append(edits, jsoncedit.Edit{Path: []string{"env", key}, Value: value})
		}
		if !jsoncedit.PathExists(original, []string{"env"}) {
			edits = append([]jsoncedit.Edit{{Path: []string{"env"}, Value: map[string]any{}}}, edits...)
		}
		text = jsoncedit.ApplyEdits(original, edits)
	}

	if original != "" {
		backupFile(settingsPath)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return ClaudeApplyResult{}, err
	}
	if err := os.WriteFile(settingsPath, []byte(text), 0o644); err != nil {
		return ClaudeApplyResult{}, err
	}
	return ClaudeApplyResult{
		Status:  claudeStatus(port),
		Written: []string{settingsPath},
	}, nil
}

// RestoreClaudeCode undoes an apply: restores the captured original verbatim,
// or — when no state was captured — strips only the managed env keys.
func RestoreClaudeCode(state RestoreState) ClaudeStatus {
	settingsPath := SettingsPath()
	saved := state.Load()
	original, ok := saved[settingsPath]

	if ok {
		if original == "" {
			// File did not exist before Connect — remove what we created rather
			// than leaving an empty stub Claude Code would try to parse.
			backupFile(settingsPath)
		} else {
			backupFile(settingsPath)
			_ = os.MkdirAll(filepath.Dir(settingsPath), 0o755)
			_ = os.WriteFile(settingsPath, []byte(original), 0o644)
		}
	} else if fileExists(settingsPath) {
		// Strip only the env keys this integration manages; everything else —
		// other env vars, comments, formatting — is left exactly as the user wrote
		// it.
		text := readText(settingsPath)
		edits := make([]jsoncedit.Edit, 0, len(ManagedEnvKeys))
		for _, key := range ManagedEnvKeys {
			edits = append(edits, jsoncedit.Edit{Path: []string{"env", key}, Remove: true})
		}
		next := jsoncedit.ApplyEdits(text, edits)
		// If env holds nothing but the keys we removed, drop the empty object too.
		if keys := jsoncedit.ObjectKeys(next, []string{"env"}); keys != nil && len(keys) == 0 {
			next = jsoncedit.ApplyEdits(next, []jsoncedit.Edit{{Path: []string{"env"}, Remove: true}})
		}
		backupFile(settingsPath)
		_ = os.WriteFile(settingsPath, []byte(next), 0o644)
	}

	state.Clear()
	return claudeStatus(0)
}

// claudeStatus reports the connection state from settings.json.
func claudeStatus(port int) ClaudeStatus {
	settingsPath := SettingsPath()
	installed := claudeInstalled()
	env := settingsEnv(settingsPath)
	baseURL := env["ANTHROPIC_BASE_URL"]
	connected := baseURL != "" && strings.Contains(baseURL, fmt.Sprintf(":%d", port)) &&
		env["ANTHROPIC_AUTH_TOKEN"] == Marker

	status := "disconnected"
	if !installed {
		status = "unavailable"
	} else if connected {
		status = "connected"
	}
	out := ClaudeStatus{
		Installed:  installed,
		Status:     status,
		ConfigPath: settingsPath,
	}
	if connected {
		out.BaseURL = baseURL
	}
	if !installed {
		out.Reason = "Claude Code CLI was not found. Install it, then Connect or run `jevonian launch claude`."
	}
	return out
}

// ClaudeCodeStatus is the read-only connection state for port.
func ClaudeCodeStatus(port int) ClaudeStatus { return claudeStatus(port) }

// FindClaudeCode locates the `claude` binary: PATH first, then the two common
// install locations.
func FindClaudeCode(lookPath func(string) (string, error)) string {
	if lookPath == nil {
		lookPath = defaultLookPath
	}
	if p, err := lookPath("claude"); err == nil && p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	for _, candidate := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".claude", "local", name),
	} {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

func defaultLookPath(name string) (string, error) { return exec.LookPath(name) }

func claudeInstalled() bool {
	if FindClaudeCode(nil) != "" {
		return true
	}
	base := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".claude")
	}
	return fileExists(base)
}

func settingsEnv(settingsPath string) map[string]string {
	data, err := readJSON(settingsPath)
	if err != nil {
		return map[string]string{}
	}
	raw, ok := data["env"].(map[string]any)
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// LaunchSpec is how to spawn Claude Code with Jevonian's env, Ollama-style
// (`jevonian launch claude`): no disk rewrite, one-shot env.
type LaunchSpec struct {
	Path string
	Args []string
	// Env is the full environment (base + managed overrides) as KEY=value.
	Env []string
}

// PrepareLaunch builds the launch spec. base is the parent environment
// (os.Environ()); model is an optional --model override; extra is forwarded.
func PrepareLaunch(port int, models []string, model string, extra []string, base []string, lookPath func(string) (string, error)) (LaunchSpec, error) {
	path := FindClaudeCode(lookPath)
	if path == "" {
		return LaunchSpec{}, fmt.Errorf("claude binary not found. Install Claude Code, then re-run `jevonian launch claude`")
	}
	route := ResolveModels(models, model)
	var args []string
	if strings.TrimSpace(model) != "" || len(models) > 0 {
		args = append(args, "--model", route.Primary)
	}
	args = append(args, extra...)
	managed := Env(port, models, model)
	env := make([]string, 0, len(base)+len(managed))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, ours := managed[key]; ours {
			continue
		}
		env = append(env, kv)
	}
	for _, key := range ManagedEnvKeys {
		if v, ok := managed[key]; ok {
			env = append(env, key+"="+v)
		}
	}
	return LaunchSpec{Path: path, Args: args, Env: env}, nil
}

// EnvAssignments is Env as sorted-by-managed-order `KEY=value` lines for shell
// exports.
func EnvAssignments(port int, models []string, model string) []string {
	managed := Env(port, models, model)
	out := make([]string, 0, len(managed))
	for _, key := range ManagedEnvKeys {
		if v, ok := managed[key]; ok {
			out = append(out, key+"="+v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Claude Desktop third-party gateway profile
// ---------------------------------------------------------------------------

// ClaudeDesktopPaths is where Claude Desktop keeps the third-party profile.
type ClaudeDesktopPaths struct {
	NormalConfig  string // ~/Library/Application Support/Claude/claude_desktop_config.json
	DesktopConfig string // Claude-3p/claude_desktop_config.json
	Meta          string // Claude-3p/configLibrary/_meta.json
	Profile       string // Claude-3p/configLibrary/<profileId>.json
	ProfileID     string
}

// ClaudeDesktopPathsFor returns the standard macOS layout.
func ClaudeDesktopPathsFor(home string) ClaudeDesktopPaths {
	base := filepath.Join(home, "Library", "Application Support")
	thirdParty := filepath.Join(base, "Claude-3p")
	profileID := "00000000-0000-4000-8000-000000000114"
	return ClaudeDesktopPaths{
		NormalConfig:  filepath.Join(base, "Claude", "claude_desktop_config.json"),
		DesktopConfig: filepath.Join(thirdParty, "claude_desktop_config.json"),
		Meta:          filepath.Join(thirdParty, "configLibrary", "_meta.json"),
		Profile:       filepath.Join(thirdParty, "configLibrary", profileID+".json"),
		ProfileID:     profileID,
	}
}

// ClaudeDesktopInstalled reports whether the macOS app layout is present.
func ClaudeDesktopInstalled(home string) bool {
	return runtime.GOOS == "darwin" &&
		fileExists(filepath.Join(home, "Library", "Application Support", "Claude"))
}

// GatewayProfileFields are the Claude Desktop profile fields Jevonian owns —
// including the placeholder inferenceGatewayApiKey. Write them with the
// caller's JSONC editor so the user's file keeps its formatting.
func GatewayProfileFields(baseURL string, models []map[string]any) map[string]any {
	return map[string]any{
		"inferenceProvider":            "gateway",
		"inferenceGatewayBaseUrl":      baseURL,
		"inferenceGatewayApiKey":       Marker,
		"inferenceGatewayAuthScheme":   "bearer",
		"deploymentDisplayName":        "Jevonian",
		"chatTabEnabled":               true,
		"inferenceModels":              models,
		"modelDiscoveryEnabled":        false,
		"disableDeploymentModeChooser": true,
		"coworkEgressAllowedHosts":     []any{"*"},
		"disableEssentialTelemetry":    true,
		"disableNonessentialTelemetry": true,
	}
}

// ---------------------------------------------------------------------------
// Small file helpers
// ---------------------------------------------------------------------------

// backupFile renames path to a timestamped sibling so a bad apply is always
// recoverable by hand. Best-effort.
func backupFile(path string) {
	if !fileExists(path) {
		return
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format(time.RFC3339Nano))
	_ = os.Rename(path, path+".jevonian-backup-"+stamp)
}

func readJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
