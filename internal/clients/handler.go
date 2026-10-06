package clients

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/routing"
)

// HandlerOptions supplies immutable snapshots at request time so reloaded model
// offers/ports are respected. Manager may be injected to isolate paths and app
// process hooks. Handler is loopback-only even if mounted without admin's guard.
type HandlerOptions struct {
	Config  func() *config.Config
	Manager *Manager
}
type Handler struct {
	opts HandlerOptions
	mu   sync.Mutex
}

func NewHandler(opts HandlerOptions) *Handler {
	if opts.Manager == nil {
		opts.Manager = New(Options{})
	}
	return &Handler{opts: opts}
}

// Extensions returns the exact relative route patterns accepted by admin.Deps.
// The same Handler also accepts /api/clients and /api/clients/:id directly.
func (h *Handler) Extensions() map[string]http.Handler {
	return map[string]http.Handler{"GET /clients": h, "POST /clients/{id}": h, "DELETE /clients/{id}": h}
}
func clientJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func clientFailure(w http.ResponseWriter, status int, message string) {
	clientJSON(w, status, map[string]any{"error": message})
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		clientFailure(w, 403, "Admin API is only available on loopback.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	if path != "/clients" && !strings.HasPrefix(path, "/clients/") {
		clientFailure(w, 404, "Not found")
		return
	}
	if h.opts.Config == nil {
		clientFailure(w, 503, "Configuration is unavailable.")
		return
	}
	cfg := h.opts.Config()
	if cfg == nil {
		clientFailure(w, 503, "Configuration is unavailable.")
		return
	}
	manager := h.opts.Manager
	if path == "/clients" && r.Method == http.MethodGet {
		clientJSON(w, 200, map[string]any{"clients": manager.Targets(cfg.Listen.Port), "hostname": manager.opts.Hostname, "platform": manager.opts.Platform})
		return
	}
	if !strings.HasPrefix(path, "/clients/") || r.Method != http.MethodPost && r.Method != http.MethodDelete {
		clientFailure(w, 405, "Method not allowed")
		return
	}
	id := ClientID(strings.TrimPrefix(path, "/clients/"))
	if !validID(id) {
		clientFailure(w, 400, "Unknown client.")
		return
	}
	// TS treats malformed/non-object JSON as {}, with restart only when === true.
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}
	restart := body["restart"] == true
	h.mu.Lock()
	defer h.mu.Unlock()
	models := routing.DesktopModels(cfg)
	codeModels := routing.ClaudeCodeModels(cfg)
	if r.Method == http.MethodPost && len(models) == 0 && (id != Claude || len(codeModels) == 0) {
		clientFailure(w, 400, "Configure at least one routed model before connecting.")
		return
	}
	running := manager.IsClientRunning(id)
	if running && !restart {
		message := "The app is running. Confirm the restart to apply the Jevonian profile."
		if r.Method == http.MethodDelete {
			message = "The app is running. Confirm the restart to restore its normal profile."
		}
		clientJSON(w, 409, map[string]any{"error": "restart-required", "running": true, "client": id, "message": message})
		return
	}
	if r.Method == http.MethodPost {
		if len(models) == 0 {
			models = codeModels
		}
		result, err := manager.Apply(id, ApplyOptions{Port: cfg.Listen.Port, Models: models, CodeModels: codeModels})
		if err != nil {
			clientFailure(w, 500, err.Error())
			return
		}
		if running && restart {
			if err = manager.RestartClient(r.Context(), id); err != nil {
				clientFailure(w, 500, err.Error())
				return
			}
			result.Restarted = true
			for _, target := range manager.Targets(cfg.Listen.Port) {
				if target.ID == id {
					result.Target = target
					break
				}
			}
		}
		clientJSON(w, 200, map[string]any{"result": result})
		return
	}
	if _, err := manager.Restore(id); err != nil {
		clientFailure(w, 500, err.Error())
		return
	}
	if running && restart {
		if err := manager.RestartClient(r.Context(), id); err != nil {
			clientFailure(w, 500, err.Error())
			return
		}
	}
	clientJSON(w, 200, map[string]any{"clients": manager.Targets(cfg.Listen.Port)})
}
