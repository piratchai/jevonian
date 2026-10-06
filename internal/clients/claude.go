package clients

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xinyao27/jevonian/internal/oauth/jsoncedit"
	"github.com/xinyao27/jevonian/internal/oauth/sentinel"
)

func (m *Manager) ClaudeDesktopPaths() sentinel.ClaudeDesktopPaths {
	return sentinel.ClaudeDesktopPathsFor(m.opts.Home)
}
func (m *Manager) desktopInstalled() bool {
	return m.opts.Platform == "darwin" && exists(filepath.Dir(m.ClaudeDesktopPaths().NormalConfig))
}
func connectionStatus(installed, connected bool) Status {
	if !installed {
		return Unavailable
	}
	if connected {
		return Connected
	}
	return Disconnected
}
func (m *Manager) codeSurface(port int) Surface {
	installed := m.FindClaudeCodePath() != "" || exists(m.opts.ClaudeConfigDir)
	data := readObject(m.ClaudeCodeSettingsPath())
	env, _ := data["env"].(map[string]any)
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	// Exact URLs avoid treating :87870 or an external host as loopback :8787.
	connected := base == ClaudeCodeBaseURL(port) && env["ANTHROPIC_AUTH_TOKEN"] == ManagedMarker
	s := Surface{ID: "cli", Label: "Claude Code", Status: connectionStatus(installed, connected), ConfigPath: m.ClaudeCodeSettingsPath()}
	if connected && installed {
		s.BaseURL = base
	}
	if !installed {
		s.Reason = "Claude Code CLI was not found. Install it, then Connect or run `jevonian launch claude`."
	}
	return s
}
func (m *Manager) desktopSurface(port int) Surface {
	s := Surface{ID: "desktop", Label: "Desktop", Status: Unavailable}
	if m.opts.Platform != "darwin" {
		s.Reason = "Claude Desktop integration is only supported on macOS."
		return s
	}
	s.ConfigPath = m.ClaudeDesktopPaths().Profile
	if !m.desktopInstalled() {
		s.Reason = "Claude Desktop was not found on this machine."
		return s
	}
	data := readObject(s.ConfigPath)
	base, _ := data["inferenceGatewayBaseUrl"].(string)
	connected := data["inferenceProvider"] == "gateway" && base == ClaudeCodeBaseURL(port)
	s.Status = connectionStatus(true, connected)
	if connected {
		s.BaseURL = base
	}
	return s
}
func (m *Manager) claudeStatus(port int) Target {
	surfaces := []Surface{m.desktopSurface(port), m.codeSurface(port)}
	t := Target{ID: Claude, Label: "Claude", Logo: "claude", Status: Unavailable, Surfaces: surfaces}
	available := []Surface{}
	for _, s := range surfaces {
		if s.Status != Unavailable {
			available = append(available, s)
		}
	}
	if len(available) == 0 {
		for _, s := range surfaces {
			if s.Reason != "" {
				t.Reason = s.Reason
				break
			}
		}
		return t
	}
	t.Installed = true
	t.Status = Connected
	primary := available[0]
	for _, s := range available {
		if s.Status != Connected {
			t.Status = Disconnected
		} else {
			primary = s
		}
	}
	t.ConfigPath = primary.ConfigPath
	t.BaseURL = primary.BaseURL
	return t
}
func (m *Manager) ClaudeStatus(port int) Target {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claudeStatus(port)
}
func (m *Manager) ClaudeCodeStatus(port int) Surface {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.codeSurface(port)
}
func (m *Manager) ApplyClaudeCode(opts ApplyOptions) (ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	written, err := m.applyCode(opts)
	return ApplyResult{Target: m.claudeStatus(opts.Port), Written: written}, err
}
func (m *Manager) applyCode(opts ApplyOptions) ([]string, error) {
	if len(opts.Models) == 0 {
		return nil, fmt.Errorf("Select at least one model before connecting Claude Code.")
	}
	path := m.ClaudeCodeSettingsPath()
	original, err := readText(path)
	if err != nil {
		return nil, err
	}
	edits := []jsoncedit.Edit{}
	if !jsoncedit.PathExists(original, []string{"env"}) {
		edits = append(edits, jsoncedit.Edit{Path: []string{"env"}, Value: map[string]any{}})
	}
	env := ClaudeCodeEnv(opts.Port, opts.Models, "")
	for _, key := range sentinel.ManagedEnvKeys {
		edits = append(edits, jsoncedit.Edit{Path: []string{"env", key}, Value: env[key]})
	}
	next, err := editedObject(original, edits)
	if err != nil {
		return nil, err
	}
	if err = m.saveState("claude-code", []string{path}); err != nil {
		return nil, err
	}
	if err = m.replace(path, next); err != nil {
		return nil, err
	}
	written := []string{path}
	return written, m.recordApplied("claude-code", written)
}
func (m *Manager) ApplyClaudeDesktop(opts ApplyOptions) (ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	written, err := m.applyDesktop(opts)
	return ApplyResult{Target: m.claudeStatus(opts.Port), Written: written}, err
}
func (m *Manager) applyDesktop(opts ApplyOptions) ([]string, error) {
	if m.opts.Platform != "darwin" {
		return nil, fmt.Errorf("Claude Desktop integration is only supported on macOS.")
	}
	if !m.desktopInstalled() {
		return nil, fmt.Errorf("Claude Desktop was not found on this machine.")
	}
	p := m.ClaudeDesktopPaths()
	files := []string{p.DesktopConfig, p.Meta, p.Profile, p.NormalConfig}
	// Validate and stage every JSONC change before writing any client files.
	staged := map[string]string{}
	for _, path := range []string{p.DesktopConfig, p.NormalConfig} {
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		next, err := editedObject(text, jsonEdits(map[string]any{"deploymentMode": "3p"}))
		if err != nil {
			return nil, err
		}
		staged[path] = next
	}
	text, err := readText(p.Meta)
	if err != nil {
		return nil, err
	}
	meta, err := objectText(text)
	if err != nil {
		return nil, err
	}
	entries := []any{}
	raw, _ := meta["entries"].([]any)
	for _, e := range raw {
		obj, _ := e.(map[string]any)
		if obj["id"] != p.ProfileID {
			entries = append(entries, e)
		}
	}
	entries = append(entries, map[string]any{"id": p.ProfileID, "name": "Jevonian"})
	next, err := editedObject(text, jsonEdits(map[string]any{"appliedId": p.ProfileID, "entries": entries}))
	if err != nil {
		return nil, err
	}
	staged[p.Meta] = next
	text, err = readText(p.Profile)
	if err != nil {
		return nil, err
	}
	next, err = editedObject(text, jsonEdits(sentinel.GatewayProfileFields(ClaudeCodeBaseURL(opts.Port), ClaudeGatewayProfileModels(opts.Models))))
	if err != nil {
		return nil, err
	}
	staged[p.Profile] = next
	if err = m.saveState("claude", files); err != nil {
		return nil, err
	}
	for _, path := range files {
		if err = m.replace(path, staged[path]); err != nil {
			return nil, err
		}
	}
	return files, m.recordApplied("claude", files)
}
func (m *Manager) applyClaude(opts ApplyOptions) (ApplyResult, error) {
	desktop, code := m.desktopSurface(opts.Port), m.codeSurface(opts.Port)
	if desktop.Status == Unavailable && code.Status == Unavailable {
		reason := code.Reason
		if reason == "" {
			reason = desktop.Reason
		}
		return ApplyResult{}, fmt.Errorf("%s", reason)
	}
	written := []string{}
	if desktop.Status != Unavailable {
		files, err := m.applyDesktop(opts)
		if err != nil {
			return ApplyResult{}, err
		}
		written = append(written, files...)
	}
	if code.Status != Unavailable {
		if opts.CodeModels != nil {
			opts.Models = opts.CodeModels
		}
		files, err := m.applyCode(opts)
		if err != nil {
			return ApplyResult{}, err
		}
		written = append(written, files...)
	}
	return ApplyResult{Target: m.claudeStatus(opts.Port), Written: written}, nil
}
func topPaths(keys []string) [][]string {
	paths := make([][]string, 0, len(keys))
	for _, k := range keys {
		paths = append(paths, []string{k})
	}
	return paths
}
func (m *Manager) restoreCode() error {
	s, err := m.loadState("claude-code")
	if err != nil {
		return err
	}
	paths := [][]string{}
	for _, k := range sentinel.ManagedEnvKeys {
		paths = append(paths, []string{"env", k})
	}
	if err = m.restoreJSON(s, m.ClaudeCodeSettingsPath(), paths, true); err != nil {
		return err
	}
	return m.clearState("claude-code")
}
func (m *Manager) RestoreClaudeCode() (Surface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.restoreCode()
	return m.codeSurface(0), err
}
func (m *Manager) restoreDesktop() error {
	s, err := m.loadState("claude")
	if err != nil {
		return err
	}
	p := m.ClaudeDesktopPaths()
	for _, path := range []string{p.DesktopConfig, p.NormalConfig} {
		if s == nil {
			// Lost-state fallback (src/clients.ts): desktop's own config returns to
			// Claude's 1p default; the normal config only loses the managed key.
			if !exists(path) {
				continue
			}
			text, err := readText(path)
			if err != nil {
				return err
			}
			edit := jsoncedit.Edit{Path: []string{"deploymentMode"}, Remove: true}
			if path == p.DesktopConfig {
				edit = jsoncedit.Edit{Path: []string{"deploymentMode"}, Value: "1p"}
			}
			next, err := editedObject(text, []jsoncedit.Edit{edit})
			if err != nil {
				return err
			}
			if err = m.replace(path, next); err != nil {
				return err
			}
			continue
		}
		if err = m.restoreJSON(s, path, [][]string{{"deploymentMode"}}, false); err != nil {
			return err
		}
	}
	keys := []string{}
	for k := range sentinel.GatewayProfileFields("", nil) {
		keys = append(keys, k)
	}
	if err = m.restoreJSON(s, p.Profile, topPaths(keys), false); err != nil {
		return err
	}
	if done, err := m.exactRestore(s, p.Meta); err != nil {
		return err
	} else if !done && exists(p.Meta) {
		// Meta entries belong to other profiles as well. Restore only our appliedId
		// and remove our own row, never replace the whole current entry list.
		text, err := readText(p.Meta)
		if err != nil {
			return err
		}
		meta, err := objectText(text)
		if err != nil {
			return err
		}
		raw, _ := meta["entries"].([]any)
		entries := []any{}
		for _, entry := range raw {
			obj, _ := entry.(map[string]any)
			if obj["id"] != p.ProfileID {
				entries = append(entries, entry)
			}
		}
		original, known, _ := originalFile(s, p.Meta)
		before := map[string]any{}
		if known {
			before, err = objectText(original)
			if err != nil {
				return err
			}
		}
		prior, _ := before["entries"].([]any)
		for _, entry := range prior {
			obj, _ := entry.(map[string]any)
			if obj["id"] == p.ProfileID {
				entries = append(entries, entry)
			}
		}
		edits := []jsoncedit.Edit{{Path: []string{"entries"}, Value: entries}}
		if meta["appliedId"] == p.ProfileID {
			v, ok := before["appliedId"]
			edits = append(edits, jsoncedit.Edit{Path: []string{"appliedId"}, Value: v, Remove: !ok})
		}
		next, err := editedObject(text, edits)
		if err != nil {
			return err
		}
		if err = m.replace(p.Meta, next); err != nil {
			return err
		}
	}
	return m.clearState("claude")
}
func (m *Manager) RestoreClaudeDesktop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restoreDesktop()
}
func (m *Manager) restoreClaude() (Target, error) {
	// TS restoreClaude catches each surface so a malformed/absent Desktop config
	// cannot leave the Claude Code env connected forever.
	var errs []string
	for _, restore := range []func() error{m.restoreDesktop, m.restoreCode} {
		if err := restore(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return Target{}, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return m.claudeStatus(0), nil
}

// GatewaySlot mirrors src/claude-gateway.ts without depending on server.
type GatewaySlot struct{ ID, Model, Label, Family, CreatedAt string }

var gatewayTemplates = []GatewaySlot{
	{ID: "claude-sonnet-5", Family: "sonnet", CreatedAt: "2026-06-30T00:00:00Z"},
	{ID: "claude-opus-5", Family: "opus", CreatedAt: "2026-07-24T00:00:00Z"},
	{ID: "claude-sonnet-4-6", Family: "sonnet", CreatedAt: "2025-11-18T00:00:00Z"},
	{ID: "claude-haiku-4-5-20251001", Family: "haiku", CreatedAt: "2025-10-01T00:00:00Z"},
}

func ClaudeGatewaySlots(models []string) []GatewaySlot {
	slots := make([]GatewaySlot, 0, min(len(models), len(gatewayTemplates)))
	for i, model := range models {
		if i >= len(gatewayTemplates) {
			break
		}
		slot := gatewayTemplates[i]
		slot.Model = model
		slot.Label = ClaudeCodeModelLabel(model)
		slots = append(slots, slot)
	}
	return slots
}
func ClaudeGatewayProfileModels(models []string) []map[string]any {
	rows := []map[string]any{}
	defaults := map[string]bool{}
	for _, slot := range ClaudeGatewaySlots(models) {
		row := map[string]any{"name": slot.ID, "labelOverride": slot.Label, "anthropicFamilyTier": slot.Family}
		if !defaults[slot.Family] {
			row["isFamilyDefault"] = true
			defaults[slot.Family] = true
		}
		rows = append(rows, row)
	}
	return rows
}
func ClaudeGatewayModels(models []string) map[string]any {
	data := []map[string]any{}
	for _, s := range ClaudeGatewaySlots(models) {
		data = append(data, map[string]any{"id": s.ID, "type": "model", "display_name": s.Label, "created_at": s.CreatedAt, "anthropic_family_tier": s.Family})
	}
	var first, last any
	if len(data) > 0 {
		first = data[0]["id"]
		last = data[len(data)-1]["id"]
	}
	return map[string]any{"data": data, "first_id": first, "last_id": last, "has_more": false}
}
func IsClaudeGatewayRequest(version string) bool { return strings.TrimSpace(version) != "" }
func ResolveClaudeGatewayModel(requested string, models []string, autoMode, fromGateway bool, pinned func(string) bool) string {
	for _, s := range ClaudeGatewaySlots(models) {
		if s.ID != requested {
			continue
		}
		if autoMode {
			return s.Model
		}
		if !fromGateway || pinned != nil && pinned(requested) {
			return ""
		}
		return s.Model
	}
	return ""
}
