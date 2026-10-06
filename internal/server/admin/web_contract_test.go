package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/clients"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/modelsync"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// Every route + method that web/src/lib/api.ts (and pages/clients.tsx, hooks/use-log-stream.ts)
// calls must be registered. A 404 "Not found" means the route is missing.
func TestEveryWebRouteIsRegistered(t *testing.T) {
	x := setup(t, &logs{})
	routes := []struct{ method, path string }{
		{"GET", "/state"}, {"GET", "/models"}, {"GET", "/pricing?provider=x"}, {"GET", "/catalog"},
		{"GET", "/catalog/refresh"}, {"POST", "/catalog/refresh"}, {"GET", "/stats"}, {"GET", "/update"}, {"GET", "/update/check"}, {"POST", "/update/check"},
		{"POST", "/update/install"}, {"GET", "/clients"}, {"POST", "/clients/claude"}, {"DELETE", "/clients/claude"},
		{"GET", "/tunnel"}, {"PUT", "/tunnel"}, {"GET", "/lan"}, {"PUT", "/lan"}, {"GET", "/quota"},
		{"GET", "/logs"}, {"GET", "/logs/series"}, {"GET", "/logs/abc"},
		{"POST", "/brains"}, {"PUT", "/brains/0"}, {"DELETE", "/brains/0"}, {"POST", "/brains/0/move"},
		{"PUT", "/routing"}, {"POST", "/brain/test"}, {"POST", "/providers"}, {"DELETE", "/providers/none"},
		{"POST", "/providers/discover"}, {"POST", "/oauth/workbuddy-ai/signin"}, {"PUT", "/token-saver"},
		{"GET", "/model-sync"}, {"PUT", "/model-sync"}, {"POST", "/model-sync/run"},
		{"POST", "/keys"}, {"GET", "/keys"}, {"PUT", "/keys/none"}, {"DELETE", "/keys/none"},
		{"GET", "/activity"}, {"GET", "/tiers"},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, strings.NewReader("{}"))
		req.RemoteAddr = "127.0.0.1:1"
		rr := httptest.NewRecorder()
		x.h.ServeHTTP(rr, req)
		if rr.Code == 404 && strings.Contains(rr.Body.String(), `"Not found"`) || rr.Code == 405 {
			t.Errorf("%s %s not registered: %d %s", r.method, r.path, rr.Code, rr.Body.String())
		}
	}
	// The SSE stream blocks on a live ledger; a cancelled context returns after the first frame.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/logs/stream", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	x.h.ServeHTTP(rr, req)
	if rr.Code == 404 {
		t.Errorf("GET /logs/stream not registered")
	}
}

func TestStatePresetsAndPricingSourceMatchDashboard(t *testing.T) {
	x := setup(t, &logs{})
	code, out := request(t, x.h, "GET", "/state", nil)
	checkStatus(t, code, 200, out)
	presets, _ := out["presets"].([]any)
	if len(presets) != len(admin.DefaultPresets()) || len(presets) == 0 {
		t.Fatalf("state.presets not populated by default: %d", len(presets))
	}
	first := presets[0].(map[string]any)
	for _, k := range []string{"id", "name", "type", "baseUrl", "hint"} {
		if first[k] == nil {
			t.Fatalf("preset missing %s: %#v", k, first)
		}
	}
	price := out["pricing"].(map[string]any)
	if price["source"] != "bundled-fallback" || price["models"] == nil {
		t.Fatalf("pricing %#v", price)
	}
	for _, k := range []string{"config", "tiers", "routings", "keys", "brainChannels", "modelSyncDefaultSources"} {
		if out[k] == nil {
			t.Fatalf("state missing %s", k)
		}
	}
	cfg := out["config"].(map[string]any)
	for _, k := range []string{"listen", "providers", "routing", "modelSync", "tokenSaver", "tunnel", "lan"} {
		if cfg[k] == nil {
			t.Fatalf("state.config missing %s", k)
		}
	}
	lan := cfg["lan"].(map[string]any)
	for _, k := range []string{"enabled", "port", "bindHost", "urls"} {
		if _, ok := lan[k]; !ok {
			t.Fatalf("state.config.lan missing %s", k)
		}
	}
}

func TestBrainChannelsHideUnsupportedVercel(t *testing.T) {
	x := setup(t, &logs{})
	_, out := request(t, x.h, "GET", "/brains", nil)
	for _, raw := range out["brainChannels"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == "vercel" {
			t.Fatalf("unsupported channel exposed: %#v", c)
		}
	}
}

func TestBrainMoveUnknownIndexIs400(t *testing.T) {
	x := setup(t, &logs{})
	code, out := request(t, x.h, "POST", "/brains/7/move", map[string]any{"direction": "down"})
	checkStatus(t, code, 400, out)
	if out["error"] != "cannot move that brain" {
		t.Fatalf("%#v", out)
	}
	code, out = request(t, x.h, "PUT", "/brains/7", map[string]any{})
	checkStatus(t, code, 404, out)
}

func TestLegacyTiersPutRoutesModelsIntoBuiltinEntries(t *testing.T) {
	x := setup(t, &logs{})
	request(t, x.h, "POST", "/providers", map[string]any{"name": "p", "type": "openai", "baseUrl": "http://x/v1", "apiKey": "k", "models": []any{"m1", "m2"}})
	code, out := request(t, x.h, "PUT", "/routing", map[string]any{"tiers": map[string]any{"plan": []any{"m2"}}})
	checkStatus(t, code, 200, out)
	if got := x.config.Routing.Tiers.Plan; len(got) != 1 || got[0] != "m2" {
		t.Fatalf("legacy tiers PUT was dropped: %#v", x.config.Routing.Tiers)
	}
	plan := out["routing"].(map[string]any)["tiers"].(map[string]any)["plan"].([]any)
	if len(plan) != 1 || plan[0] != "m2" {
		t.Fatalf("response routing.tiers.plan %#v", plan)
	}
}

func TestModelSyncPayloadAndPut(t *testing.T) {
	x := setup(t, &logs{})
	request(t, x.h, "POST", "/providers", map[string]any{"name": "p", "type": "openai", "baseUrl": "http://x/v1", "apiKey": "k", "models": []any{"m1"}})
	dir := t.TempDir()
	deps := &modelsync.Deps{StatePath: filepath.Join(dir, "model-sync.json"), Config: func() *config.Config { return x.config }}
	exts := admin.BuildExtensions(admin.ExtensionOptions{Config: func() *config.Config { return x.config }, ModelSync: deps, Clients: clients.New(clients.Options{Home: dir, DataDir: dir})})
	h := admin.New(admin.Deps{Config: func() *config.Config { return x.config }, Reload: func(c *config.Config) { x.config = c }, ConfigPath: x.path, Credentials: x.creds, Extensions: exts})
	code, out := request(t, h, "GET", "/model-sync", nil)
	checkStatus(t, code, 200, out)
	for _, k := range []string{"config", "lastAdded", "providers", "providersSkipped"} {
		if out[k] == nil {
			t.Fatalf("GET /model-sync missing %s: %#v", k, out)
		}
	}
	// A plain API-key provider without syncModels is not auto-synced: listed as skipped.
	if skipped := out["providersSkipped"].([]any); len(skipped) != 1 || skipped[0] != "p" {
		t.Fatalf("providersSkipped %#v", skipped)
	}
	code, out = request(t, h, "PUT", "/model-sync", map[string]any{"enabled": false, "intervalMinutes": 30})
	checkStatus(t, code, 200, out)
	cfg := out["config"].(map[string]any)
	if cfg["enabled"] != false || cfg["intervalMinutes"] != float64(30) || out["providersSkipped"] == nil {
		t.Fatalf("PUT /model-sync %#v", out)
	}
	if x.config.ModelSync.Enabled || x.config.ModelSync.IntervalMinutes != 30 {
		t.Fatal("model-sync not persisted")
	}
}

func TestPricingEndpointReadsSnapshotByProvider(t *testing.T) {
	x := setup(t, &logs{})
	snapshot := `{"fetchedAt":"2026-10-05T00:00:00Z","models":{"deepseek/deepseek-v4":{"input":1,"output":2,"cacheRead":0.1},"openai/gpt-x":{"input":3,"output":4}}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("JEVONIAN_DATA_DIR"), "pricing.json"), []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	pricingStatus := func() map[string]any {
		return map[string]any{"present": true, "fresh": true, "fetchedAt": "2026-10-05T00:00:00Z", "models": 2}
	}
	exts := admin.BuildExtensions(admin.ExtensionOptions{Config: func() *config.Config { return x.config }, CatalogDeps: catalogsync.Deps{PricingStatus: pricingStatus}})
	h := admin.New(admin.Deps{Config: func() *config.Config { return x.config }, Extensions: exts})
	code, out := request(t, h, "GET", "/pricing?provider=deepseek", nil)
	checkStatus(t, code, 200, out)
	prices := out["prices"].(map[string]any)
	row := prices["deepseek-v4"].(map[string]any)
	if out["provider"] != "deepseek" || len(prices) != 1 || row["input"] != float64(1) || row["cacheRead"] != 0.1 {
		t.Fatalf("pricing %#v", out)
	}
	code, out = request(t, h, "GET", "/catalog", nil)
	checkStatus(t, code, 200, out)
	pricing := out["pricing"].(map[string]any)
	board := out["leaderboard"].(map[string]any)
	if pricing["present"] != true || pricing["fresh"] != true || pricing["ttlMs"] == nil || pricing["fetchedAt"] == nil {
		t.Fatalf("catalog pricing %#v", pricing)
	}
	if board["boards"] == nil || board["ttlMs"] == nil || board["present"] != false {
		t.Fatalf("catalog leaderboard %#v", board)
	}
}

func TestUpdateRoutesAnswerWithoutManager(t *testing.T) {
	x := setup(t, &logs{})
	h := admin.New(admin.Deps{Config: func() *config.Config { return x.config }, Extensions: admin.BuildExtensions(admin.ExtensionOptions{Config: func() *config.Config { return x.config }})})
	code, out := request(t, h, "GET", "/update", nil)
	checkStatus(t, code, 200, out)
	status := out["update"].(map[string]any)
	if status["current"] != "unknown" || status["updateAvailable"] != false || out["active"] != false {
		t.Fatalf("%#v", out)
	}
	code, out = request(t, h, "POST", "/update/check", nil)
	checkStatus(t, code, 503, out)
	code, out = request(t, h, "POST", "/update/install", nil)
	checkStatus(t, code, 503, out)
}

func TestModelsAndCanonicalsCarryCatalogFields(t *testing.T) {
	x := setup(t, &logs{})
	request(t, x.h, "POST", "/providers", map[string]any{"name": "a", "type": "openai", "baseUrl": "http://x/v1", "apiKey": "k", "models": []any{"vendor/foo-1"}})
	request(t, x.h, "POST", "/providers", map[string]any{"name": "b", "type": "openai", "baseUrl": "http://y/v1", "apiKey": "k", "models": []any{"foo-1"}})
	code, out := request(t, x.h, "GET", "/models", nil)
	checkStatus(t, code, 200, out)
	models := out["models"].([]any)
	if len(models) != 2 || models[0].(map[string]any)["configured"] != true || models[0].(map[string]any)["canonical"] == nil {
		t.Fatalf("models %#v", models)
	}
	canon := out["canonicals"].([]any)
	if len(canon) != 1 {
		t.Fatalf("one canonical expected: %#v", canon)
	}
	variants := canon[0].(map[string]any)["variants"].([]any)
	if len(variants) != 2 || variants[0].(map[string]any)["provider"] != "a" || variants[1].(map[string]any)["provider"] != "b" {
		t.Fatalf("variants %#v", variants)
	}
}

func TestProviderMutationModelSchemaAndCredentialCleanup(t *testing.T) {
	x := setup(t, &logs{})
	code, out := request(t, x.h, "POST", "/providers", map[string]any{"name": "p", "type": "openai", "baseUrl": "http://x/v1", "apiKey": "secret", "models": []any{"plain", map[string]any{"id": "pinned", "wire": "anthropic"}}})
	checkStatus(t, code, 200, out)
	provider := out["config"].(map[string]any)["providers"].([]any)[0].(map[string]any)
	models := provider["models"].([]any)
	if models[0].(map[string]any)["id"] != "plain" || models[1].(map[string]any)["wire"] == nil {
		t.Fatalf("runtime model schema %#v", models)
	}
	if provider["apiKey"] != nil || x.creds.Get("p") != "secret" {
		t.Fatal("credential was exposed or not stored")
	}
	if err := x.creds.Set("shared-login", "keep"); err != nil {
		t.Fatal(err)
	}
	code, out = request(t, x.h, "DELETE", "/providers/p", nil)
	checkStatus(t, code, 200, out)
	if x.creds.Get("p") != "" || x.creds.Get("shared-login") != "keep" {
		t.Fatal("deleted provider credential remains or unrelated credential removed")
	}
}

func TestAdminNeverServedOutsideLoopback(t *testing.T) {
	x := setup(t, &logs{})
	for _, addr := range []string{"10.0.0.5:1", "192.168.1.9:2", "[2001:db8::1]:3"} {
		req := httptest.NewRequest("PUT", "/token-saver", strings.NewReader(`{"enabled":false}`))
		req.RemoteAddr = addr
		rr := httptest.NewRecorder()
		x.h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s got %d", addr, rr.Code)
		}
	}
}
