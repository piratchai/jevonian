package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

func sandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("JEVONIAN_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("JEVONIAN_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("JEVONIAN_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	t.Setenv("JEVONIAN_LEDGER", filepath.Join(dir, "ledger.jsonl"))
	t.Setenv("JEVONIAN_LEDGER_DB", filepath.Join(dir, "ledger.sqlite"))
	t.Setenv("JEVONIAN_SERVICE_PLIST", filepath.Join(dir, "never-live.plist"))
	t.Setenv("JEVONIAN_SYSTEM_PROXY", "off")
	t.Setenv("JEVONIAN_NO_OPEN", "1")
	t.Setenv("JEVONIAN_PORT", "")
}
func invoke(args ...string) (int, string, string) {
	var out, err bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &err)
	return code, out.String(), err.String()
}
func TestDispatch(t *testing.T) {
	sandbox(t)
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}, {"--version"}} {
		code, out, err := invoke(args...)
		if code != 0 || out == "" || err != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, err)
		}
	}
	code, _, err := invoke("misspelled")
	if code != 1 || !strings.Contains(err, "Usage:") {
		t.Fatalf("unknown: %d %s", code, err)
	}
	code, out, _ := invoke("update", "--check")
	if code != 0 && !strings.Contains(out, "jevonian") {
		t.Fatalf("update check failed: %d %s", code, out)
	}
}
func TestParseArgs(t *testing.T) {
	a, err := parseArgs([]string{"custom", "--name=x", "--yes", "--models", "a,b", "--", "--resume", "session"})
	if err != nil || a.flags["name"] != "x" || !a.has("yes") || len(a.positionals) != 1 || strings.Join(a.passthrough, " ") != "--resume session" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := parseArgs([]string{"--name"}); err == nil {
		t.Fatal("missing flag value accepted")
	}
}
func TestInitSerializesModelsAndMode(t *testing.T) {
	sandbox(t)
	code, _, err := invoke("init")
	if code != 0 {
		t.Fatal(err)
	}
	cfg, _, e := config.Load()
	if e != nil || len(cfg.Providers) != 1 || cfg.Providers[0].Models[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("%+v %v", cfg, e)
	}
	b, _ := os.ReadFile(paths.ConfigPath())
	if strings.Contains(string(b), `"ID"`) || strings.Contains(string(b), `"Wire"`) {
		t.Fatalf("wrong JSON model shape: %s", b)
	}
	st, _ := os.Stat(paths.ConfigPath())
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	cfg.Providers[0].Models[0].Wire = []config.UpstreamWire{config.WireOpenAI, config.WireAnthropic}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, e = config.Load()
	if e != nil || len(cfg.Providers[0].Models[0].Wire) != 2 {
		t.Fatalf("wire pins lost %v", e)
	}
}
func TestAddRemoveAndCredentialIsolation(t *testing.T) {
	sandbox(t)
	code, out, err := invoke("add", "custom", "--base-url", "http://localhost:9999/v1", "--type", "both", "--name", "test", "--key", "secret-never-inline", "--models", "a,b,a")
	if code != 0 {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 models") {
		t.Fatal(out)
	}
	cfg, _, e := config.Load()
	if e != nil || len(cfg.Providers) != 1 || cfg.DefaultProvider != "test" {
		t.Fatalf("%+v %v", cfg, e)
	}
	b, _ := os.ReadFile(paths.ConfigPath())
	if bytes.Contains(b, []byte("secret-never-inline")) {
		t.Fatal("key leaked into config")
	}
	if multiacct.DefaultStore().Get("test") != "secret-never-inline" {
		t.Fatal("key missing")
	}
	code, out, err = invoke("providers")
	if code != 0 || !strings.Contains(out, "key=credentials") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	code, _, err = invoke("remove", "test", "--keep-key")
	if code != 0 {
		t.Fatal(err)
	}
	if multiacct.DefaultStore().Get("test") == "" {
		t.Fatal("keep-key failed")
	}
	code, _, err = invoke("remove", "missing")
	if code == 0 {
		t.Fatal("missing removal succeeded")
	}
}
func TestAddLoginAndPairing(t *testing.T) {
	sandbox(t)
	home := t.TempDir()
	code, _, err := invoke("add", "cursor-subscription", "--name", "cursor-work", "--models", "sonnet", "--login-home", home, "--login-label", "Work")
	if code != 0 {
		t.Fatal(err)
	}
	cfg, _, e := config.Load()
	if e != nil || cfg.Providers[0].Login.Home != home || cfg.Providers[0].Type != config.ProviderTypeCursor {
		t.Fatalf("%+v %v", cfg, e)
	}
	_, out, _ := invoke("providers")
	if !strings.Contains(out, "account=Work") {
		t.Fatal(out)
	}
	before, _ := os.ReadFile(paths.ConfigPath())
	code, _, err = invoke("add", "cursor-subscription", "--type", "openai", "--models", "a")
	if code == 0 || !strings.Contains(err, "require --type cursor") {
		t.Fatalf("%d %s", code, err)
	}
	after, _ := os.ReadFile(paths.ConfigPath())
	if !bytes.Equal(before, after) {
		t.Fatal("failed add changed config")
	}
	code, _, _ = invoke("add", "openai", "--models", "a", "--login-home", home)
	if code == 0 {
		t.Fatal("API key login allowed")
	}
}
func TestAddPreservesSyncExclusions(t *testing.T) {
	sandbox(t)
	code, _, err := invoke("add", "ollama", "--models", "a,b")
	if code != 0 {
		t.Fatal(err)
	}
	code, _, err = invoke("add", "ollama", "--models", "b,c")
	if code != 0 {
		t.Fatal(err)
	}
	cfg, _, e := config.Load()
	if e != nil {
		t.Fatal(e)
	}
	p := cfg.Providers[0]
	if p.SyncModels == nil || !*p.SyncModels || strings.Join(p.ExcludeModels, ",") != "a" {
		t.Fatalf("%+v", p)
	}
}
func TestModelsDiscoverAndSync(t *testing.T) {
	sandbox(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("keyless discovery sent auth")
		}
		fmt.Fprint(w, `{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)
	}))
	defer server.Close()
	code, _, err := invoke("add", "ollama", "--base-url", server.URL+"/v1", "--models", "a,b")
	if code != 0 {
		t.Fatal(err)
	}
	cfg, _, _ := config.Load()
	cfg.Providers[0].ExcludeModels = []string{"c"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	code, out, err := invoke("models", "--refresh")
	if code != 0 || !strings.Contains(out, "3 models") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	code, _, err = invoke("models", "--sync")
	if code != 0 {
		t.Fatal(err)
	}
	cfg, _, _ = config.Load()
	if len(cfg.Providers[0].Models) != 2 {
		t.Fatal("sync resurrected excluded model")
	}
}
func TestKeyCRUD(t *testing.T) {
	sandbox(t)
	code, out, err := invoke("keys", "create", "Work", "--limit-usd", "5", "--json")
	if code != 0 {
		t.Fatal(err)
	}
	var result struct {
		Key    string       `json:"key"`
		Record keys.Summary `json:"record"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Key, "sk-jev-") {
		t.Fatal(out)
	}
	stored, _ := os.ReadFile(keys.Path(paths.DataDir()))
	if bytes.Contains(stored, []byte(result.Key)) {
		t.Fatal("plaintext stored")
	}
	code, _, err = invoke("keys", "update", result.Record.ID, "--name", "Renamed", "--limit-usd", "0")
	if code != 0 {
		t.Fatal(err)
	}
	list, e := keys.Open(paths.DataDir(), nil).List()
	if e != nil || list[0].Name != "Renamed" || list[0].LimitUSD != nil {
		t.Fatalf("%+v %v", list, e)
	}
	code, _, err = invoke("keys", "remove", result.Record.ID)
	if code != 0 {
		t.Fatal(err)
	}
	code, _, _ = invoke("keys", "create", "--limit-usd", "NaN")
	if code == 0 {
		t.Fatal("NaN accepted")
	}
}
func TestReportReadsGoLedger(t *testing.T) {
	sandbox(t)
	db, e := ledger.Open(paths.LedgerDBPath())
	if e != nil {
		t.Fatal(e)
	}
	cost := .4
	saved := 200
	if err := db.Append(ledger.Record{TS: time.Now(), Model: "deepseek-v4-pro", Session: "one", PromptTokens: 1000, CompletionTokens: 50, CacheReadTokens: 1000, CostUSD: &cost, SavedTokens: &saved, Brain: "jev", Phase: "plan", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, out, err := invoke("report")
	if code != 0 {
		t.Fatal(err)
	}
	for _, want := range []string{"1 requests, 1 sessions", "cache hits: 50.0%", "~200 tokens", "brain-decided: 1", "by thinking effort:", "$0.4000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}
func TestDoctorMissingAndServiceGuard(t *testing.T) {
	sandbox(t)
	code, out, err := invoke("doctor")
	if code != 0 || !strings.Contains(out, "missing") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	for _, command := range []string{"start", "stop", "restart"} {
		code, _, err := invoke(command)
		if code == 0 || !strings.Contains(err, "JEVONIAN_SERVICE_PLIST") {
			t.Fatalf("%s: %d %s", command, code, err)
		}
	}
}
func TestLaunchClaudeOneShot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	sandbox(t)
	if code, _, err := invoke("init"); code != 0 {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fixture := filepath.Join(bin, "claude")
	if err := os.WriteFile(fixture, []byte("#!/bin/sh\nprintf '%s\\n' \"$ANTHROPIC_BASE_URL\" \"$ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_API_KEY\" \"$ANTHROPIC_DEFAULT_OPUS_MODEL\" \"$ANTHROPIC_DEFAULT_HAIKU_MODEL\" \"$@\"\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_API_KEY", "shell-secret")
	before, _ := os.ReadFile(paths.ConfigPath())
	code, out, err := invoke("launch", "claude", "--model", "custom-model", "--", "--resume", "session")
	if code != 7 || err != "" {
		t.Fatalf("%d %s %s", code, out, err)
	}
	for _, want := range []string{"http://127.0.0.1:8787", "jevonian-local", "custom-model", "jevonian/utility", "--resume", "session"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, "shell-secret") {
		t.Fatal("shell API key survived remap")
	}
	after, _ := os.ReadFile(paths.ConfigPath())
	if !bytes.Equal(before, after) {
		t.Fatal("launch modified config")
	}
}
func TestPricingRefreshSnapshot(t *testing.T) {
	sandbox(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openai":{"name":"OpenAI","api":"https://api.openai.com/v1","env":["OPENAI_API_KEY"],"models":{"gpt-test":{"name":"GPT Test","cost":{"input":2,"output":4},"limit":{"context":1000,"output":200},"reasoning":["low","high"]}}}}`)
	}))
	defer server.Close()
	t.Setenv("JEVONIAN_MODELS_DEV_URL", server.URL)
	code, out, err := invoke("pricing", "--refresh")
	if code != 0 {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pricing: saved") {
		t.Fatal(out)
	}
	s := loadPricing()
	if s.Models["openai/gpt-test"].Input != 2 {
		t.Fatalf("%+v", s)
	}
	// Capabilities live in catalogsync's half of the snapshot, not the price
	// view; read the persisted file for them.
	raw, readErr := os.ReadFile(pricingPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	var file struct {
		Capabilities map[string]map[string]any `json:"capabilities"`
	}
	_ = json.Unmarshal(raw, &file)
	if file.Capabilities["gpt-test"]["contextWindow"] != float64(1000) {
		t.Fatalf("capabilities = %+v", file.Capabilities)
	}
}
