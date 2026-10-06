package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinyao27/jevonian/internal/brain"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/guard"
	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/server/admin"
	"github.com/xinyao27/jevonian/web"
)

// Deps are process-wide resources the HTTP surface needs. serve.go assembles
// them; tests inject fakes.
type Deps struct {
	// Config is the initial snapshot; Reload swaps it atomically.
	Config *config.Config
	Ledger *ledger.DB
	// Client is the egress HTTP client (system proxy / hardened transport).
	Client *http.Client
	// Guard is the per-provider concurrency cap + breaker. nil disables
	// gating (every candidate is tried in order).
	Guard *guard.Guard
	// Quota records spend refusals; nil means a fresh tracker over Ledger.
	Quota *quota.Tracker
	// Keys gates /v1; nil or empty means open mode (first-run).
	Keys *keys.Store
	// Store is the per-session routing memory.
	Store *routing.SessionStore
	// Routing is Decide's dependencies beyond the per-request fields.
	Routing routing.Deps
	// Brain answers compaction's raw questions (internal/compaction's
	// BrainAsker). serve.go passes the same client routing scores with so
	// stored brain credentials resolve; nil builds a default over Client.
	Brain *brain.Client
	// SameHostRetries overrides JEVONIAN_SAME_HOST_RETRIES when non-nil.
	SameHostRetries *int
	// Sleep backs off between same-host retries (tests inject a no-op).
	Sleep func(time.Duration)
	// Now overrides the wall clock for ledger timestamps (tests).
	Now func() time.Time
	// Public marks the tunnel/LAN surface: no loopback sentinel auth, a
	// minimal /healthz, no admin routes.
	Public bool
	Auth   *oauth.Resolver
	Admin  *admin.Deps
	// ConfigSource lets public listeners read the main server's immutable snapshot.
	ConfigSource func() *config.Config
	// OnReload reconciles process resources after swapping the snapshot.
	OnReload func(*config.Config)
}

// Server is the Jevonian HTTP surface (/v1 + health/stats).
type Server struct {
	addr string
	deps Deps

	cfg atomic.Pointer[config.Config]

	runner *Runner
	life   lifecycle
	admin  http.Handler
}

// lifecycle tracks in-flight requests so shutdown drains politely and a
// restart can answer 503 while draining.
type lifecycle struct {
	mu       sync.Mutex
	inFlight int
	draining bool
	cancels  map[*http.Request]context.CancelFunc
}

func (l *lifecycle) begin(r *http.Request, cancel context.CancelFunc) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.draining {
		return false
	}
	if l.cancels == nil {
		l.cancels = map[*http.Request]context.CancelFunc{}
	}
	l.cancels[r] = cancel
	l.inFlight++
	return true
}

func (l *lifecycle) end(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cancel, ok := l.cancels[r]; ok {
		delete(l.cancels, r)
		l.inFlight--
		cancel()
	}
}

// cancelActive releases lifecycle slots before restart and stops upstream IO.
func (s *Server) cancelActive() {
	s.life.mu.Lock()
	cancels := s.life.cancels
	s.life.cancels = map[*http.Request]context.CancelFunc{}
	s.life.inFlight = 0
	s.life.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

const DefaultDrainTimeout = 60 * time.Second

// Drain marks the server draining (in-flight requests finish; new /v1 calls
// get 503 retry-after).
func (s *Server) Drain() {
	s.life.mu.Lock()
	s.life.draining = true
	s.life.mu.Unlock()
}

// Draining reports whether the server is draining.
func (s *Server) Draining() bool {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	return s.life.draining
}

// ActiveRequests returns the count of in-flight requests.
func (s *Server) ActiveRequests() int {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	return s.life.inFlight
}

// Resume clears the draining state so requests are accepted again if an
// update fails before restart.
func (s *Server) Resume() {
	s.life.mu.Lock()
	s.life.draining = false
	s.life.mu.Unlock()
}

// RestartAfterDrain marks the server draining, waits for in-flight requests to
// finish (or ctx deadline), then calls restart.
func (s *Server) RestartAfterDrain(ctx context.Context, restart func(context.Context) error) error {
	s.Drain()
	deadline := time.Now().Add(DefaultDrainTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for time.Now().Before(deadline) && s.ActiveRequests() > 0 {
		select {
		case <-ctx.Done():
			s.cancelActive()
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s.ActiveRequests() > 0 {
		s.cancelActive()
	}
	if restart != nil {
		return restart(ctx)
	}
	return nil
}

// New builds the main (loopback) server.
func New(addr string, deps Deps) *Server {
	if deps.Client == nil {
		deps.Client = http.DefaultClient
	}
	if deps.Quota == nil {
		deps.Quota = quota.New(deps.Ledger)
	}
	if deps.Store == nil {
		deps.Store = routing.NewSessionStore(0)
	}
	if deps.Guard == nil {
		deps.Guard = guard.New(guard.OptionsFromEnv())
	}
	s := &Server{addr: addr, deps: deps}
	if deps.Config != nil {
		s.cfg.Store(deps.Config)
	}
	s.runner = NewRunner(ForwardDeps{
		HTTP:     deps.Client,
		Guard:    deps.Guard,
		Quota:    deps.Quota,
		SameHost: deps.SameHostRetries,
		Sleep:    deps.Sleep,
		Auth:     deps.Auth,
	})
	if deps.Admin != nil && !deps.Public {
		ad := *deps.Admin
		ad.Config = s.Config
		ad.Reload = s.Reload
		if ad.HTTP == nil {
			ad.HTTP = deps.Client
		}
		if ad.Quota == nil {
			ad.Quota = deps.Quota
		}
		if ad.OAuth == nil {
			ad.OAuth = deps.Auth
		}
		s.admin = admin.New(ad)
	}
	return s
}

// NewPublicServer builds the tunnel/LAN surface: /v1 requires real keys, the
// loopback desktop sentinel is never honored, and /healthz stays minimal.
func NewPublicServer(addr string, deps Deps) *Server {
	deps.Public = true
	return New(addr, deps)
}

// Config returns the current config snapshot.
func (s *Server) Config() *config.Config {
	if s.deps.ConfigSource != nil {
		return s.deps.ConfigSource()
	}
	return s.cfg.Load()
}

// Reload swaps the config snapshot; in-flight requests keep their copy.
func (s *Server) Reload(cfg *config.Config) {
	s.cfg.Store(cfg)
	if s.deps.OnReload != nil {
		s.deps.OnReload(cfg)
	}
}

// Handler returns the HTTP mux (for ListenAndServe and httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /health", s.handleHealthz)
	if !s.deps.Public {
		mux.HandleFunc("GET /stats", s.handleStats)
		if s.admin != nil {
			mux.Handle("/api/", http.StripPrefix("/api", s.admin))
		}
	}
	// /v1 goes through lifecycle + auth + the probe/upgraded guards.
	mux.Handle("/v1/", http.HandlerFunc(s.serveV1))
	if s.deps.Public {
		mux.HandleFunc("/", http.NotFound)
		// src/server.ts createPublicApp authenticates "*" before any route, so
		// /healthz and unknown paths demand a real key too (401, never a 404
		// that would reveal what exists). Authenticate once and hand the key
		// identity to serveV1 so request counters are not bumped twice.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keyID, keyName, deny := s.authorize(r)
			if deny != nil {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(deny.status)
				_ = json.NewEncoder(w).Encode(deny.body)
				return
			}
			ctx := context.WithValue(r.Context(), authedKey{}, [2]string{keyID, keyName})
			mux.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	mux.Handle("/", web.Handler())
	return mux
}

// authedKey carries the identity the public wrapper already authenticated.
type authedKey struct{}

// serveV1 is the single entry for every /v1 path: drain check, auth, WS
// upgrade, account probes, then routing.
func (s *Server) serveV1(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	if !s.life.begin(r, cancel) {
		cancel()
		w.Header().Set("retry-after", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "server_error",
			"Jevonian is restarting after an update. Retry shortly.")
		return
	}
	defer s.life.end(r)

	var keyID, keyName string
	if pre, ok := r.Context().Value(authedKey{}).([2]string); ok {
		keyID, keyName = pre[0], pre[1]
	} else {
		var deny *denyReply
		keyID, keyName, deny = s.authorize(r)
		if deny != nil {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(deny.status)
			_ = json.NewEncoder(w).Encode(deny.body)
			return
		}
	}

	// Codex prefers a WebSocket transport for /v1/responses; 426 is the signal
	// it reads as "fall back to HTTP for the whole session". GET /v1/models is
	// registered ahead of the upgrade guard in src/server.ts, so it never sees it.
	// The public (tunnel/LAN) app has no upgrade guard in src/server.ts.
	if !s.deps.Public && IsWebSocketUpgrade(r.Header) && !(r.Method == http.MethodGet && r.URL.Path == "/v1/models") {
		writeJSONError(w, http.StatusUpgradeRequired, "invalid_request_error",
			"Jevonian uses the HTTP Responses transport.")
		return
	}

	// Account/session probes must reach the real backend — answering them
	// leaves desktop clients stuck on sign-in.
	if IsAccountProbe(r) {
		if ProxyAccountProbe(w, r, s.deps.Client) {
			return
		}
	}

	path := r.URL.Path
	switch {
	case path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.handleChat(w, r, keyID, keyName, KindOpenAI)
	case path == "/v1/messages" && r.Method == http.MethodPost:
		s.handleChat(w, r, keyID, keyName, KindAnthropic)
	case path == "/v1/messages/count_tokens" && r.Method == http.MethodPost && !s.deps.Public:
		s.handleCountTokens(w, r)
	case path == "/v1/responses" && r.Method == http.MethodPost:
		s.handleChat(w, r, keyID, keyName, KindResponses)
	case path == "/v1/models" && r.Method == http.MethodGet:
		s.handleModels(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	if s.deps.Public {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "public": true})
		return
	}
	cfg := s.Config()
	payload := map[string]any{"ok": true, "sessions": s.deps.Store.Size()}
	if cfg != nil {
		payload["routing"] = cfg.Routing.Mode
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	// Same shape as src/server.ts GET /stats: request count, sessions, cost,
	// cache-read and prompt tokens over every ledger row.
	payload := map[string]any{
		"requests":        int64(0),
		"sessions":        s.deps.Store.Size(),
		"costUsd":         0.0,
		"cacheReadTokens": int64(0),
		"promptTokens":    int64(0),
	}
	if s.deps.Ledger != nil {
		if t, err := s.deps.Ledger.Totals(); err == nil {
			payload["requests"] = t.Requests
			payload["costUsd"] = math.Round(t.CostUSD*1e6) / 1e6
			payload["cacheReadTokens"] = t.CacheReadTokens
			payload["promptTokens"] = t.PromptTokens
		}
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// handleCountTokens answers Anthropic's token counter with an estimate so
// Claude Desktop's context meter keeps working.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	// Any JSON value is accepted (c.req.json() in TS), and it is re-serialized
	// without HTML escaping so "<", ">" and "&" tokenize like JSON.stringify.
	var body any
	decoded, err := readRequestBody(r)
	if err != nil || json.Unmarshal(decoded, &body) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body")
		return
	}
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"input_tokens": routing.EstimateTokens(strings.TrimSuffix(raw.String(), "\n")),
	})
}

// handleModels answers in the dialect the caller speaks: the Anthropic page
// when the request carries anthropic-version, else the OpenAI list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	if cfg == nil {
		writeJSONError(w, http.StatusInternalServerError, "jevonian_error", "server config is not loaded")
		return
	}
	w.Header().Set("content-type", "application/json")
	if isClaudeGatewayRequest(r.Header) {
		_ = json.NewEncoder(w).Encode(claudeGatewayModels(routing.DesktopModels(cfg)))
		return
	}
	models := routing.ClientModels(cfg)
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "jevonian"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// authorize enforces the Jevonian API-key gate on /v1, mirroring the auth
// middleware in src/server.ts: open mode when no keys exist, loopback desktop
// sentinels / a ChatGPT account session on the loopback surface, 401 on a bad
// or missing token, 429 when the key's credit limit is reached.
func (s *Server) authorize(r *http.Request) (keyID, keyName string, deny *denyReply) {
	cfg := s.Config()
	if s.deps.Keys == nil || !s.deps.Keys.HasKeys() {
		if s.deps.Public {
			return "", "", &denyReply{http.StatusUnauthorized, errorBody("invalid_request_error",
				"No Jevonian API key exists yet. Create one in the dashboard first.")}
		}
		return "unauthenticated", "unauthenticated", nil
	}
	token := keys.TokenFromHeaders(r.Header.Get("authorization"), r.Header.Get("x-api-key"))
	decision, err := s.deps.Keys.AllowKey(token)
	if err != nil {
		return "", "", &denyReply{500, errorBody("jevonian_error", "key validation failed")}
	}
	if decision.Allowed {
		if decision.Key != nil {
			return decision.Key.ID, decision.Key.Name, nil
		}
		return "unauthenticated", "unauthenticated", nil
	}
	// A recognised key over its limit is answered 429 outright; only an
	// unrecognised credential falls through to the desktop-client check
	// (verifyKey succeeds before isLocalClientRequest in src/server.ts).
	if decision.Reason == keys.DenyCreditLimit && decision.Key != nil {
		limit := 0.0
		if decision.Key.LimitUSD != nil {
			limit = *decision.Key.LimitUSD
		}
		return "", "", &denyReply{429, creditLimitBody(limit, decision.Key.Name)}
	}
	// Desktop clients (Codex, Claude Desktop) present their own credential
	// rather than a Jevonian key; loopback-only so it never unlocks a tunnel.
	if !s.deps.Public && IsLocalClientRequest(r, cfg) {
		return "local", "local desktop client", nil
	}
	if s.deps.Public {
		return "", "", &denyReply{401, errorBody("invalid_request_error", "Invalid API key.")}
	}
	port := 8787
	if cfg != nil && cfg.Listen.Port > 0 {
		port = cfg.Listen.Port
	}
	return "", "", &denyReply{401, errorBody("invalid_request_error",
		"Invalid API key. Create one at http://127.0.0.1:"+itoa(port)+"/keys")}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return s.serve(ctx, httpServer, ln)
}

// Serve accepts an already-bound listener so callers can report bind failures
// before publishing tunnel URLs or starting other process resources.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpServer := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return s.serve(ctx, httpServer, ln)
}

func (s *Server) serve(ctx context.Context, httpServer *http.Server, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), DefaultDrainTimeout)
		defer cancel()
		s.Drain()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			// Shutdown does not close active sockets when its budget expires.
			// Force close before resource owners release keys/ledger/transport.
			s.cancelActive()
			_ = httpServer.Close()
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
