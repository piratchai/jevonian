package admin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/clients"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/modelsync"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/update"
)

// ExtensionOptions bundles the optional sub-system handlers that plug into
// admin.Deps.Extensions.
type ExtensionOptions struct {
	Config      func() *config.Config
	Reload      func(*config.Config)
	Updater     *update.Manager
	Lifecycle   update.Lifecycle
	Restart     func(context.Context) error
	Clients     *clients.Manager
	ModelSync   *modelsync.Deps
	CatalogDeps catalogsync.Deps
}

// BuildExtensions creates the extension handlers matching the 10 routes in
// Handler.routes():
//
//	GET /update, POST /update/check, POST /update/install
//	GET /clients, POST /clients/{id}, DELETE /clients/{id}
//	GET /pricing, GET /catalog, POST /catalog/refresh
//	GET /model-sync, POST /model-sync/run
func BuildExtensions(opts ExtensionOptions) map[string]http.Handler {
	m := make(map[string]http.Handler)

	// 1. Updater. Without a manager the routes still answer like the TS admin app does outside a
	// running server: GET /update reports "unknown", check/install return 503.
	u := update.NewHandler(update.HandlerOptions{
		Manager:   opts.Updater,
		Lifecycle: opts.Lifecycle,
		Restart:   opts.Restart,
	})
	for pattern, h := range u.Extensions() {
		m[pattern] = h
	}
	m["GET /update/check"] = u

	// 2. Client connectors (ChatGPT / Claude)
	clientH := clients.NewHandler(clients.HandlerOptions{
		Config:  opts.Config,
		Manager: opts.Clients,
	})
	for pattern, h := range clientH.Extensions() {
		m[pattern] = h
	}

	// 3. Catalog & Pricing
	catalogH := &catalogHandler{opts: opts}
	m["GET /catalog"] = catalogH
	m["GET /catalog/refresh"] = catalogH
	m["POST /catalog/refresh"] = catalogH
	m["GET /pricing"] = catalogH

	// 4. Model Sync (dashboard payload: config + last pass + providers not syncing)
	msH := &modelSyncHandler{opts: opts}
	m["GET /model-sync"] = msH
	m["POST /model-sync/run"] = msH

	return m
}

type catalogHandler struct {
	opts ExtensionOptions
}

func (h *catalogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		extFail(w, 403, "Admin API is only available on loopback.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/catalog" && r.Method == http.MethodGet:
		extSend(w, 200, h.status())
	case path == "/catalog/refresh" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		result := h.opts.CatalogDeps.Refresh(r.Context(), true)
		extSend(w, 200, map[string]any{
			"pricing":     result.Pricing,
			"leaderboard": result.Leaderboard,
			"status":      h.status(),
		})
	case path == "/pricing" && r.Method == http.MethodGet:
		provider := r.URL.Query().Get("provider")
		extSend(w, 200, map[string]any{"provider": provider, "prices": providerPrices(provider)})
	default:
		http.NotFound(w, r)
	}
}

// status is the catalogStatus() payload of src/catalog-sync.ts: pricing {present, fresh,
// fetchedAt?, ttlMs} and leaderboard {present, fresh, fetchedAt?, boards[], models, ttlMs}.
func (h *catalogHandler) status() map[string]any {
	status := catalogsync.Status(h.now(), h.pricingStatus())
	pricing := map[string]any{"present": false, "fresh": false, "ttlMs": (12 * time.Hour).Milliseconds()}
	if raw, ok := status["pricing"].(map[string]any); ok {
		for _, k := range []string{"present", "fresh", "ttlMs"} {
			if v, ok := raw[k]; ok {
				pricing[k] = v
			}
		}
		if s, _ := raw["fetchedAt"].(string); s != "" {
			pricing["fetchedAt"] = s
		}
	}
	status["pricing"] = pricing
	if board, ok := status["leaderboard"].(map[string]any); ok {
		if ids, _ := board["boards"].([]string); ids == nil {
			board["boards"] = []string{}
		}
		if s, _ := board["fetchedAt"].(string); s == "" {
			delete(board, "fetchedAt")
		}
	}
	return status
}

// providerPrices mirrors GET /api/pricing: the snapshot's `provider/model` rows for one provider,
// keyed by the bare model id. An empty provider yields an empty table.
func providerPrices(provider string) map[string]any {
	out := map[string]any{}
	if provider == "" {
		return out
	}
	data, err := os.ReadFile(filepath.Join(paths.DataDir(), "pricing.json"))
	if err != nil {
		return out
	}
	var snapshot struct {
		Models map[string]map[string]any `json:"models"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return out
	}
	prefix := provider + "/"
	for key, price := range snapshot.Models {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		row := map[string]any{"input": price["input"], "output": price["output"]}
		for _, k := range []string{"cacheRead", "cacheWrite"} {
			if v, ok := price[k]; ok {
				row[k] = v
			}
		}
		out[strings.TrimPrefix(key, prefix)] = row
	}
	return out
}

func (h *catalogHandler) now() time.Time {
	if h.opts.CatalogDeps.Now != nil {
		return h.opts.CatalogDeps.Now()
	}
	return time.Now().UTC()
}

func (h *catalogHandler) pricingStatus() map[string]any {
	if h.opts.CatalogDeps.PricingStatus != nil {
		return h.opts.CatalogDeps.PricingStatus()
	}
	return map[string]any{"present": false, "fresh": false}
}

func extSend(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
func extFail(w http.ResponseWriter, status int, msg string) {
	extSend(w, status, map[string]any{"error": msg})
}

// modelSyncPayload mirrors modelSyncPayload() in src/admin.ts: the config, the last pass
// recorded on disk, and the providers that are not auto-syncing.
func modelSyncPayload(cfg *config.Config, load func() *modelsync.Result) map[string]any {
	out := map[string]any{"config": cfg.ModelSync, "lastAdded": 0, "providers": []modelsync.ProviderSyncResult{}}
	if last := load(); last != nil {
		out["lastCheckedAt"] = last.CheckedAt
		out["lastAdded"] = last.Added
		if last.Providers != nil {
			out["providers"] = last.Providers
		}
	}
	skipped := []string{}
	for _, p := range cfg.Providers {
		if !modelsync.Syncable(p) {
			skipped = append(skipped, p.Name)
		}
	}
	out["providersSkipped"] = skipped
	return out
}

type modelSyncHandler struct{ opts ExtensionOptions }

func (h *modelSyncHandler) load() *modelsync.Result {
	d := h.opts.ModelSync
	if d == nil {
		d = &modelsync.Deps{}
	}
	return d.LoadState()
}

func (h *modelSyncHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		extFail(w, 403, "Admin API is only available on loopback.")
		return
	}
	cfg := h.opts.Config()
	if cfg == nil {
		extFail(w, 503, "Configuration is unavailable.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/model-sync" && r.Method == http.MethodGet:
		extSend(w, 200, modelSyncPayload(cfg, h.load))
	case path == "/model-sync/run" && r.Method == http.MethodPost:
		if h.opts.ModelSync == nil {
			extFail(w, 503, "Model sync is not running in this process.")
			return
		}
		result, err := h.opts.ModelSync.Run(r.Context())
		if err != nil {
			extFail(w, 409, err.Error())
			return
		}
		if fresh := h.opts.Config(); fresh != nil {
			cfg = fresh
		}
		payload := modelSyncPayload(cfg, h.load)
		if result != nil {
			payload["result"] = result
		}
		extSend(w, 200, payload)
	default:
		http.NotFound(w, r)
	}
}
