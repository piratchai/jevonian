package update

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Lifecycle is implemented by the integration owner, shared with all listeners.
// RestartAfterDrain must drain requests (with a bounded timeout), close listeners,
// then invoke restart. This module never exits or starts the serving process.
type Lifecycle interface {
	Draining() bool
	ActiveRequests() int
	RestartAfterDrain(context.Context, func(context.Context) error) error
	Resume()
}

type HandlerOptions struct {
	Manager        *Manager
	Lifecycle      Lifecycle
	Restart        func(context.Context) error
	Context        context.Context // Server lifetime, not the accepted request lifetime.
	InstallTimeout time.Duration
}
type Handler struct {
	options    HandlerOptions
	mu         sync.Mutex
	installing bool
	lastError  string
}

func NewHandler(o HandlerOptions) *Handler {
	if o.Context == nil {
		o.Context = context.Background()
	}
	if o.InstallTimeout <= 0 {
		o.InstallTimeout = 10 * time.Minute
	}
	return &Handler{options: o}
}

// Extensions plugs directly into admin.Deps.Extensions (which strips /api).
func (h *Handler) Extensions() map[string]http.Handler {
	return map[string]http.Handler{"GET /update": h, "POST /update/check": h, "POST /update/install": h}
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		send(w, 403, map[string]any{"error": "Admin API is only available on loopback."})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/update" && r.Method == "GET":
		status := Status{Current: "unknown", Installed: "unknown", Channel: Unknown}
		if h.options.Manager != nil {
			status = h.options.Manager.Check(r.Context(), false)
		}
		h.mu.Lock()
		lastError := h.lastError
		h.mu.Unlock()
		payload := map[string]any{"update": status, "active": h.draining(), "activeRequests": h.activeRequests()}
		if lastError != "" {
			payload["error"] = lastError
		}
		send(w, 200, payload)
	case (path == "/update/check") && (r.Method == "POST" || r.Method == "GET"):
		if h.options.Manager == nil {
			send(w, 503, map[string]any{"error": "Update checking is unavailable."})
			return
		}
		h.mu.Lock()
		h.lastError = ""
		h.mu.Unlock()
		send(w, 200, map[string]any{"update": h.options.Manager.Check(r.Context(), true), "active": h.draining()})
	case path == "/update/install" && r.Method == "POST":
		h.install(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (h *Handler) draining() bool {
	return h.options.Lifecycle != nil && h.options.Lifecycle.Draining()
}
func (h *Handler) activeRequests() int {
	if h.options.Lifecycle == nil {
		return 0
	}
	return h.options.Lifecycle.ActiveRequests()
}
func (h *Handler) install(w http.ResponseWriter, _ *http.Request) {
	if h.options.Manager == nil || h.options.Lifecycle == nil || h.options.Restart == nil {
		send(w, 503, map[string]any{"error": "Updates can only be installed from a running server."})
		return
	}
	h.mu.Lock()
	if h.installing || h.draining() {
		h.mu.Unlock()
		send(w, 409, map[string]any{"error": "An update is already in progress."})
		return
	}
	status := h.options.Manager.Status()
	if !status.UpdateAvailable && !status.RestartRequired {
		message := "No update check has completed yet."
		if status.Latest != "" {
			message = "Jevonian " + status.Current + " is up to date."
		}
		h.mu.Unlock()
		send(w, 409, map[string]any{"error": message})
		return
	}
	if status.UpdateAvailable && !status.RestartRequired && status.InstallCommand == "" {
		h.mu.Unlock()
		send(w, 409, map[string]any{"error": "This installation is not managed by npm, pnpm, or a direct binary updater."})
		return
	}
	h.installing, h.lastError = true, ""
	h.mu.Unlock()
	// Write acceptance before launching; package installation occurs while serving.
	send(w, 202, map[string]any{"accepted": true, "update": status, "active": false})
	go func() {
		ctx, cancel := context.WithTimeout(h.options.Context, h.options.InstallTimeout)
		defer cancel()
		_, err := h.options.Manager.Install(ctx)
		if err == nil {
			err = h.options.Lifecycle.RestartAfterDrain(ctx, h.options.Restart)
		}
		if err != nil {
			h.options.Lifecycle.Resume()
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.installing = false
		if err != nil {
			h.lastError = err.Error()
		}
	}()
}
func send(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
