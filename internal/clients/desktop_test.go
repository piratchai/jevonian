package clients

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/oauth/jsoncedit"
)

func TestClaudeDesktopJSONCProfileAndRestore(t *testing.T) {
	m := testManager(t, "darwin")
	p := m.ClaudeDesktopPaths()
	mkdir(t, m.opts.ClaudeConfigDir)
	originals := map[string]string{
		p.NormalConfig:  "{\n  // normal settings\n  \"deploymentMode\": \"1p\",\n  \"mcpServers\": {\"keep\": {\"command\": \"thing\"}}\n}\n",
		p.DesktopConfig: "{\n  // third party settings\n  \"userSetting\": true\n}\n",
		p.Profile:       "{\n  // profile notes\n  \"userSetting\": \"keep\",\n  \"inferenceGatewayApiKey\": \"prior-key\"\n}\n",
		p.Meta:          "{\n  // profiles\n  \"appliedId\": \"user-profile\",\n  \"entries\": [{\"id\": \"user-profile\", \"name\": \"Mine\"}],\n  \"keep\": true\n}\n",
	}
	for path, text := range originals {
		put(t, path, text)
	}
	applied := mustApply(t, m, Claude, ApplyOptions{Port: 8787, Models: []string{"jevonian/auto"}, CodeModels: []string{"jevonian/auto", "jevonian/utility"}})
	if applied.Target.Status != Connected || len(applied.Written) != 5 {
		t.Fatalf("%+v", applied)
	}
	profile := readObject(p.Profile)
	checks := map[string]any{"inferenceProvider": "gateway", "inferenceGatewayBaseUrl": "http://127.0.0.1:8787", "inferenceGatewayApiKey": "jevonian-local", "inferenceGatewayAuthScheme": "bearer", "deploymentDisplayName": "Jevonian", "chatTabEnabled": true, "modelDiscoveryEnabled": false, "disableDeploymentModeChooser": true, "disableEssentialTelemetry": true, "disableNonessentialTelemetry": true}
	for key, value := range checks {
		if profile[key] != value {
			t.Fatalf("%s = %v", key, profile[key])
		}
	}
	if !reflect.DeepEqual(profile["coworkEgressAllowedHosts"], []any{"*"}) {
		t.Fatal(profile)
	}
	rows := profile["inferenceModels"].([]any)
	expected := map[string]any{"name": "claude-sonnet-5", "labelOverride": "Jevonian Auto", "anthropicFamilyTier": "sonnet", "isFamilyDefault": true}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], expected) {
		t.Fatal(rows)
	}
	assertContains(t, get(t, p.NormalConfig), "// normal settings", `"mcpServers": {"keep": {"command": "thing"}}`, `"deploymentMode": "3p"`)
	assertContains(t, get(t, p.Profile), "// profile notes", `"userSetting": "keep"`)
	assertContains(t, get(t, p.Meta), "// profiles", `"keep": true`)
	meta := readObject(p.Meta)
	if meta["appliedId"] != p.ProfileID || len(meta["entries"].([]any)) != 2 {
		t.Fatal(meta)
	}
	// Reconnect changes offered models but keeps the first restore state.
	mustApply(t, m, Claude, ApplyOptions{Port: 9090, Models: []string{"jevonian/plan"}})
	mustRestore(t, m, Claude)
	for path, text := range originals {
		if get(t, path) != text {
			t.Fatalf("not exact: %s\n%s", path, get(t, path))
		}
	}
	if exists(m.ClaudeCodeSettingsPath()) {
		t.Fatal("fresh code settings not removed")
	}
}
func TestClaudeDesktopRestoreDoesNotDiscardNewProfilesOrUserSettings(t *testing.T) {
	m := testManager(t, "darwin")
	p := m.ClaudeDesktopPaths()
	put(t, p.NormalConfig, `{"deploymentMode":"1p","keep":true}`)
	put(t, p.Meta, `{"appliedId":"original","entries":[{"id":"original","name":"Mine"}]}`)
	if _, err := m.ApplyClaudeDesktop(ApplyOptions{Port: 8787, Models: []string{"jevonian/auto"}}); err != nil {
		t.Fatal(err)
	}
	profile := jsoncedit.ApplyEdits(get(t, p.Profile), []jsoncedit.Edit{{Path: []string{"newSetting"}, Value: "keep-after-connect"}})
	put(t, p.Profile, profile)
	normal := jsoncedit.ApplyEdits(get(t, p.NormalConfig), []jsoncedit.Edit{{Path: []string{"newSetting"}, Value: true}})
	put(t, p.NormalConfig, normal)
	meta := readObject(p.Meta)
	entries := meta["entries"].([]any)
	entries = append(entries, map[string]any{"id": "new-profile", "name": "New"})
	put(t, p.Meta, jsoncedit.ApplyEdits(get(t, p.Meta), []jsoncedit.Edit{{Path: []string{"entries"}, Value: entries}}))
	if err := m.RestoreClaudeDesktop(); err != nil {
		t.Fatal(err)
	}
	if readObject(p.Profile)["newSetting"] != "keep-after-connect" {
		t.Fatal("new profile setting clobbered")
	}
	for key := range readObject(p.Profile) {
		if strings.HasPrefix(key, "inference") {
			t.Fatal("managed profile setting left behind")
		}
	}
	if readObject(p.NormalConfig)["deploymentMode"] != "1p" || readObject(p.NormalConfig)["newSetting"] != true {
		t.Fatal("normal settings clobbered")
	}
	meta = readObject(p.Meta)
	if meta["appliedId"] != "original" || len(meta["entries"].([]any)) != 2 {
		t.Fatal(meta)
	}
	for _, e := range meta["entries"].([]any) {
		if e.(map[string]any)["id"] == p.ProfileID {
			t.Fatal("our meta row not removed")
		}
	}
}
func TestClaudeDesktopLostStateManagedKeysOnly(t *testing.T) {
	m := testManager(t, "darwin")
	p := m.ClaudeDesktopPaths()
	put(t, p.NormalConfig, "{\n// keep normal\n\"keep\":true\n}\n")
	put(t, p.Profile, "{\n// keep profile\n\"keep\":true\n}\n")
	put(t, p.Meta, `{"appliedId":"user","entries":[{"id":"user","name":"Mine"}],"keep":true}`)
	if _, err := m.ApplyClaudeDesktop(ApplyOptions{Port: 8787, Models: []string{"jevonian/auto"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.statePath("claude")); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreClaudeDesktop(); err != nil {
		t.Fatal(err)
	}
	assertContains(t, get(t, p.NormalConfig), "// keep normal", `"keep":true`)
	assertContains(t, get(t, p.Profile), "// keep profile", `"keep":true`)
	profile := readObject(p.Profile)
	if len(profile) != 1 {
		t.Fatal("managed-only cleanup incomplete", profile)
	}
	meta := readObject(p.Meta)
	if meta["keep"] != true || len(meta["entries"].([]any)) != 1 {
		t.Fatal(meta)
	}
	if _, ok := meta["appliedId"]; ok {
		t.Fatal(meta)
	}
	// TS's lost-state fallback restores Desktop's own deployment default, and
	// strips the managed key from the normal config.
	if readObject(p.DesktopConfig)["deploymentMode"] != "1p" {
		t.Fatal("desktop config not restored to 1p")
	}
	if _, ok := readObject(p.NormalConfig)["deploymentMode"]; ok {
		t.Fatal("managed deployment flag left behind")
	}
}
func TestClaudeAvailabilityAndAggregateStatus(t *testing.T) {
	m := testManager(t, "linux")
	target := m.ClaudeStatus(8787)
	if target.Installed || target.Status != Unavailable || len(target.Surfaces) != 2 {
		t.Fatal(target)
	}
	if target.Surfaces[0].Reason != "Claude Desktop integration is only supported on macOS." {
		t.Fatal(target)
	}
	if _, err := m.ApplyClaudeDesktop(ApplyOptions{Port: 8787, Models: []string{"jevonian/auto"}}); err == nil {
		t.Fatal("desktop allowed on Linux")
	}
	if _, err := m.Apply(Claude, ApplyOptions{Port: 8787, Models: []string{"jevonian/auto"}}); err == nil {
		t.Fatal("uninstalled surfaces connected")
	}
	mkdir(t, m.opts.ClaudeConfigDir)
	mustApply(t, m, Claude, ApplyOptions{Port: 8787, Models: []string{"jevonian/auto"}})
	if m.ClaudeStatus(8787).Status != Connected {
		t.Fatal("Linux CLI should connect")
	}
	// macOS installs both, and the aggregate is connected only when both are.
	m.opts.Platform = "darwin"
	mkdir(t, strings.TrimSuffix(m.ClaudeDesktopPaths().NormalConfig, "/claude_desktop_config.json"))
	if m.ClaudeStatus(8787).Status != Disconnected {
		t.Fatal("partial connection marked connected")
	}
}
func TestGatewayStandinsTSContract(t *testing.T) {
	models := []string{"jevonian/auto", "jevonian/plan", "jevonian/execute", "jevonian/utility", "surplus"}
	slots := ClaudeGatewaySlots(models)
	ids := []string{}
	for _, s := range slots {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"claude-sonnet-5", "claude-opus-5", "claude-sonnet-4-6", "claude-haiku-4-5-20251001"}) {
		t.Fatal(ids)
	}
	profile := ClaudeGatewayProfileModels(models)
	if len(profile) != 4 || profile[0]["isFamilyDefault"] != true || profile[1]["isFamilyDefault"] != true || profile[3]["isFamilyDefault"] != true {
		t.Fatal(profile)
	}
	if _, ok := profile[2]["isFamilyDefault"]; ok {
		t.Fatal("second sonnet incorrectly default")
	}
	page := ClaudeGatewayModels([]string{"jevonian/auto"})
	if page["first_id"] != "claude-sonnet-5" || page["last_id"] != "claude-sonnet-5" || page["has_more"] != false {
		t.Fatal(page)
	}
	empty := ClaudeGatewayModels(nil)
	if empty["first_id"] != nil || empty["last_id"] != nil || len(empty["data"].([]map[string]any)) != 0 {
		t.Fatal(empty)
	}
	pinned := func(string) bool { return true }
	if got := ResolveClaudeGatewayModel("claude-sonnet-5", []string{"jevonian/auto"}, true, false, pinned); got != "jevonian/auto" {
		t.Fatal(got)
	}
	if got := ResolveClaudeGatewayModel("claude-sonnet-5", []string{"deepseek-v4-1-flash"}, false, false, nil); got != "" {
		t.Fatal(got)
	}
	if got := ResolveClaudeGatewayModel("claude-sonnet-5", []string{"deepseek-v4-1-flash"}, false, true, nil); got != "deepseek-v4-1-flash" {
		t.Fatal(got)
	}
	if got := ResolveClaudeGatewayModel("claude-sonnet-5", []string{"deepseek-v4-1-flash"}, false, true, pinned); got != "" {
		t.Fatal(got)
	}
	if IsClaudeGatewayRequest("  ") || !IsClaudeGatewayRequest("2023-06-01") {
		t.Fatal("gateway header")
	}
}
