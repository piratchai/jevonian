package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var tomlAssignment = regexp.MustCompile(`^([A-Za-z0-9_-]+)\s*=\s*(.*)$`)

func tomlRootRaw(text, key string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return "", false
		}
		match := tomlAssignment.FindStringSubmatch(trimmed)
		if len(match) > 0 && match[1] == key {
			return match[2], true
		}
	}
	return "", false
}
func tomlRootString(text, key string) string {
	raw, ok := tomlRootRaw(text, key)
	if !ok {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		var value string // Parse the quoted value without an optional trailing comment.
		for i := 1; i < len(raw); i++ {
			if raw[i] == '\\' {
				i++
				continue
			}
			if raw[i] == '"' {
				if json.Unmarshal([]byte(raw[:i+1]), &value) == nil {
					return value
				}
				break
			}
		}
	}
	if strings.HasPrefix(raw, "'") {
		if i := strings.Index(raw[1:], "'"); i >= 0 {
			return raw[1 : 1+i]
		}
	}
	return ""
}
func setTomlRootRaw(text, key, raw string) string {
	lines := strings.Split(text, "\n")
	first := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			first = i
			break
		}
		match := tomlAssignment.FindStringSubmatch(trimmed)
		if len(match) > 0 && match[1] == key {
			lines[i] = key + " = " + raw
			return strings.Join(lines, "\n")
		}
	}
	root := lines[:first]
	for len(root) > 0 && strings.TrimSpace(root[len(root)-1]) == "" {
		root = root[:len(root)-1]
	}
	out := append(append([]string{}, root...), key+" = "+raw, "")
	out = append(out, lines[first:]...)
	return strings.Join(out, "\n")
}
func setTomlRootString(text, key, value string) string {
	raw, _ := json.Marshal(value)
	return setTomlRootRaw(text, key, string(raw))
}
func removeTomlRoot(text, key string) string {
	kept := []string{}
	reachedTable := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			reachedTable = true
		}
		match := tomlAssignment.FindStringSubmatch(trimmed)
		if !reachedTable && len(match) > 0 && match[1] == key {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
func (m *Manager) CodexCatalogPath() string {
	return filepath.Join(m.opts.CodexHome, "jevonian-models.json")
}
func (m *Manager) CodexRoutingCatalogPath() string {
	return filepath.Join(m.opts.CodexHome, "jevonian-codex-routing.json")
}
func (m *Manager) CodexModelsCachePath() string {
	return filepath.Join(m.opts.CodexHome, "models_cache.json")
}
func (m *Manager) ReadNativeCodexCatalogEntries() []map[string]any {
	raw, _ := readObject(m.CodexModelsCachePath())["models"].([]any)
	entries := []map[string]any{}
	for _, entry := range raw {
		row, ok := entry.(map[string]any)
		slug, _ := row["slug"].(string)
		if ok && slug != "" {
			entries = append(entries, row)
		}
	}
	return entries
}
func catalogKey(slug string) string { return strings.ToLower(strings.TrimSpace(slug)) }

var reasoningDescriptions = []string{
	"Turn thinking off", "Minimal thinking for the fastest responses", "Fast responses with lighter thinking", "Balances speed and thinking depth for everyday tasks", "Greater thinking depth for complex tasks", "Extra high thinking depth for demanding tasks", "Maximum thinking depth for the hardest tasks", "Highest available thinking depth",
}

// CodexCatalog keeps the complete parser-required TS entry schema, with injected
// rows first and native rows copied (including unknown fields) as ChatGPT-only.
func CodexCatalog(models []string, native []map[string]any) map[string]any {
	instructions := "You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user's goals."
	priority := float64(1)
	found := false
	for _, row := range native {
		if text, ok := row["base_instructions"].(string); ok && strings.TrimSpace(text) != "" {
			instructions = text
			break
		}
	}
	for _, row := range native {
		var p float64
		ok := true
		switch n := row["priority"].(type) {
		case float64:
			p = n
		case int:
			p = float64(n)
		default:
			ok = false
		}
		if ok && (!found || p < priority) {
			priority = p
			found = true
		}
	}
	if found {
		priority -= float64(len(models))
	}
	levels := []map[string]any{}
	for i, e := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		levels = append(levels, map[string]any{"effort": e, "description": reasoningDescriptions[i]})
	}
	seen := map[string]bool{}
	rows := []map[string]any{}
	for i, model := range models {
		key := catalogKey(model)
		if seen[key] {
			continue
		}
		seen[key] = true
		display := model
		if model == "jevonian/auto" {
			display = "Jevonian Auto"
		}
		rows = append(rows, map[string]any{
			"slug": model, "display_name": display, "description": "Jevonian routed model",
			"default_reasoning_level": "medium", "supported_reasoning_levels": levels,
			"shell_type": "unified_exec", "visibility": "list", "supported_in_api": true, "priority": priority + float64(i),
			"additional_speed_tiers": []any{}, "service_tiers": []any{}, "default_service_tier": nil, "availability_nux": nil, "upgrade": nil,
			"base_instructions": instructions, "model_messages": nil,
			"include_skills_usage_instructions": true, "include_plugin_usage_instructions": true, "include_apps_usage_instructions": true,
			"supports_reasoning_summary_parameter": false, "supports_reasoning_summaries": false, "default_reasoning_summary": "auto",
			"support_verbosity": false, "default_verbosity": nil, "apply_patch_tool_type": nil, "web_search_tool_type": "text",
			"truncation_policy": map[string]any{"mode": "tokens", "limit": 10000}, "supports_parallel_tool_calls": true, "supports_image_detail_original": true,
			"context_window": 128000, "max_context_window": 128000, "auto_compact_token_limit": nil, "effective_context_window_percent": 95,
			"experimental_supported_tools": []any{}, "input_modalities": []string{"text", "image"}, "supports_search_tool": true,
		})
	}
	for _, entry := range native {
		slug, _ := entry["slug"].(string)
		key := catalogKey(slug)
		if slug == "" || seen[key] {
			continue
		}
		seen[key] = true
		row := map[string]any{}
		for k, v := range entry {
			row[k] = v
		}
		row["supported_in_api"] = false
		rows = append(rows, row)
	}
	return map[string]any{"models": rows}
}
func (m *Manager) chatGPTStatus(port int) Target {
	path := m.CodexConfigPath()
	installed := exists(path)
	for _, app := range []string{"/Applications/ChatGPT.app", "/Applications/Codex.app", filepath.Join(m.opts.Home, "Applications", "ChatGPT.app")} {
		installed = installed || exists(app)
	}
	text, _ := readText(path)
	base := tomlRootString(text, "openai_base_url")
	connected := base == CodexBaseURL(port) && tomlRootString(text, "model_catalog_json") == m.CodexCatalogPath()
	t := Target{ID: ChatGPT, Label: "ChatGPT", Installed: installed, Status: connectionStatus(installed, connected), ConfigPath: path, Logo: "openai"}
	if !installed {
		t.Reason = "ChatGPT / Codex app was not found on this machine."
	} else {
		if connected {
			t.BaseURL = base
		}
		t.Surfaces = []Surface{{ID: "desktop", Label: "Desktop (Codex)", Status: t.Status, ConfigPath: path, BaseURL: t.BaseURL}}
	}
	return t
}
func (m *Manager) ChatGPTStatus(port int) Target {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatGPTStatus(port)
}

// EnsureCodexSentinel uses O_EXCL, not a stat-then-write sequence: even a login
// created concurrently by Codex must never be replaced.
func (m *Manager) EnsureCodexSentinel() (bool, string, error) {
	path := filepath.Join(m.opts.CodexHome, "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, path, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return false, path, nil
	}
	if err != nil {
		return false, path, err
	}
	text, _ := jsonText(map[string]any{"OPENAI_API_KEY": ManagedMarker, "auth_mode": "apikey"})
	_, err = f.WriteString(text)
	ce := f.Close()
	if err == nil {
		err = ce
	}
	return true, path, err
}
func (m *Manager) clearCodexSentinel() error {
	path := filepath.Join(m.opts.CodexHome, "auth.json")
	obj := readObject(path)
	if obj["auth_mode"] != "apikey" || obj["OPENAI_API_KEY"] != ManagedMarker {
		return nil
	}
	// Only the exact sentinel credential is managed. Do not unlink a mixed file
	// containing newly saved login tokens or additional authentication fields.
	if len(obj) != 2 {
		return nil
	}
	return os.Remove(path)
}
func (m *Manager) applyChatGPT(opts ApplyOptions) (ApplyResult, error) {
	path := m.CodexConfigPath()
	original, err := readText(path)
	if err != nil {
		return ApplyResult{}, err
	}
	files := []string{path, m.CodexCatalogPath(), m.CodexRoutingCatalogPath()}
	if err = m.saveState("chatgpt", files); err != nil {
		return ApplyResult{}, err
	}
	catalog, err := jsonText(CodexCatalog(opts.Models, m.ReadNativeCodexCatalogEntries()))
	if err != nil {
		return ApplyResult{}, err
	}
	routes := []map[string]any{}
	for _, slug := range opts.Models {
		routes = append(routes, map[string]any{"slug": slug})
	}
	routing, err := jsonText(map[string]any{"models": routes})
	if err != nil {
		return ApplyResult{}, err
	}
	text := setTomlRootString(original, "model", opts.Models[0])
	text = removeTomlRoot(text, "model_provider")
	text = setTomlRootString(text, "model_catalog_json", m.CodexCatalogPath())
	text = setTomlRootString(text, "openai_base_url", CodexBaseURL(opts.Port))
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	for _, pair := range [][2]string{{m.CodexCatalogPath(), catalog}, {m.CodexRoutingCatalogPath(), routing}, {path, text}} {
		if err = m.replace(pair[0], pair[1]); err != nil {
			return ApplyResult{}, err
		}
	}
	_, auth, err := m.EnsureCodexSentinel()
	if err != nil {
		return ApplyResult{}, err
	}
	if err = m.recordApplied("chatgpt", files); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Target: m.chatGPTStatus(opts.Port), Written: append(files, auth)}, nil
}
func (m *Manager) restoreChatGPT() (Target, error) {
	s, err := m.loadState("chatgpt")
	if err != nil {
		return Target{}, err
	}
	path := m.CodexConfigPath()
	if done, err := m.exactRestore(s, path); err != nil {
		return Target{}, err
	} else if !done && exists(path) {
		text, err := readText(path)
		if err != nil {
			return Target{}, err
		}
		original, known, _ := originalFile(s, path)
		keys := []string{"openai_base_url", "model_catalog_json"}
		if known {
			keys = append(keys, "model", "model_provider")
		}
		for _, key := range keys {
			if raw, ok := tomlRootRaw(original, key); known && ok {
				text = setTomlRootRaw(text, key, raw)
			} else {
				text = removeTomlRoot(text, key)
			}
		}
		if err = m.replace(path, text); err != nil {
			return Target{}, err
		}
	}
	if err = m.clearCodexSentinel(); err != nil && !os.IsNotExist(err) {
		return Target{}, err
	}
	for _, p := range []string{m.CodexCatalogPath(), m.CodexRoutingCatalogPath()} {
		if done, err := m.exactRestore(s, p); err != nil {
			return Target{}, err
		} else if !done {
			if err = m.removeBackedUp(p); err != nil {
				return Target{}, err
			}
		}
	}
	if err = m.clearState("chatgpt"); err != nil {
		return Target{}, err
	}
	return m.chatGPTStatus(0), nil
}

// ApplyChatGPT is the explicit Codex entry point.
func (m *Manager) ApplyChatGPT(opts ApplyOptions) (ApplyResult, error) { return m.Apply(ChatGPT, opts) }
func (m *Manager) RestoreChatGPT() (Target, error)                     { return m.Restore(ChatGPT) }
