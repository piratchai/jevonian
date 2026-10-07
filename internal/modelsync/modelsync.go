// Package modelsync is the background model-discovery scheduler plus the
// admin /model-sync endpoints. Port of src/model-sync.ts.
//
// Discovery is append-only: it never removes, reorders, or rewrites an
// existing model entry, so per-model wire pins, manual removals
// (excludeModels), and operator ordering all survive a sync.
package modelsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/paths"
)

// MinIntervalMinutes mirrors src/config.ts MIN_MODEL_SYNC_INTERVAL_MINUTES.
const MinIntervalMinutes = 15

// PollInterval is how often serve wakes to ask "is a pass due?".
const PollInterval = MinIntervalMinutes * time.Minute

// ProviderSyncResult is one provider's outcome in a sync pass.
type ProviderSyncResult struct {
	Provider string   `json:"provider"`
	Added    []string `json:"added"`
	Skipped  string   `json:"skipped,omitempty"` // "opted-out" | "default-off"
	Error    string   `json:"error,omitempty"`
}

// Result is a completed sync pass (state file + POST /model-sync/run payload).
type Result struct {
	CheckedAt string               `json:"checkedAt"`
	Providers []ProviderSyncResult `json:"providers"`
	Added     int                  `json:"added"`
	Changed   bool                 `json:"changed"`
}

// Discovered is one provider's discovery result.
type Discovered struct {
	Models []string
	Error  string
}

// DefaultSources are the OAuth sources whose discovered catalog is the
// intended full list, so they sync without an explicit syncModels.
// src/config.ts MODEL_SYNC_DEFAULT_SOURCES.
var DefaultSources = []config.OAuthSource{
	config.OAuthCodex,
	config.OAuthClaudeCode,
	config.OAuthAntigravity,
	config.OAuthDevin,
	config.OAuthCursor,
	config.OAuthWorkbuddyAI,
}

// SyncsByDefault mirrors providerSyncsByDefault in src/config.ts.
func SyncsByDefault(p config.Provider) bool {
	if p.Type == config.ProviderTypeChatGPTWeb {
		return true
	}
	for _, s := range DefaultSources {
		if p.OAuthSource == s {
			return true
		}
	}
	return false
}

// Syncable mirrors providerSyncsModels in src/config.ts: an explicit
// syncModels wins; otherwise only the default OAuth sources sync. A provider
// that merely has an API key (or a static OAuth login) does not sync by
// default, because discovery would replace a deliberately curated list.
func Syncable(p config.Provider) bool {
	if p.SyncModels != nil {
		return *p.SyncModels
	}
	return SyncsByDefault(p)
}

// appendDiscovered returns the provider with new ids appended (dedup, honor
// excludeModels), plus the ids that landed. Pure — the input is not mutated.
func appendDiscovered(p config.Provider, discovered []string) (config.Provider, []string) {
	excluded := map[string]bool{}
	for _, id := range p.ExcludeModels {
		excluded[id] = true
	}
	next := p
	next.Models = append([]config.ModelEntry{}, p.Models...)
	var added []string
	for _, id := range discovered {
		if id == "" || excluded[id] || config.ProviderHasModel(next, id) {
			continue
		}
		next.Models = append(next.Models, config.ModelEntry{ID: id})
		added = append(added, id)
	}
	return next, added
}

// Deps wires the scheduler into serve: immutable snapshots in, atomic config
// saves out. Discover is injectable for tests; nil uses the CLI catalog probe.
type Deps struct {
	// Config returns the current immutable snapshot.
	Config func() *config.Config
	// Reload re-reads config from disk, merges, persists, and swaps the
	// snapshot — the same contract admin mutations use (admin.Deps.Reload).
	// It must save the merged config and store the new snapshot.
	Reload func(*config.Config)
	// SaveConfig persists a merged config to disk without swapping the
	// snapshot (admin does both via Reload; the scheduler needs the two
	// steps separately so a dashboard edit mid-probe is never clobbered).
	SaveConfig func(*config.Config) error
	// Discover probes one provider's live model list.
	Discover func(config.Provider) Discovered
	// Log receives human-readable progress (serve stderr).
	Log func(string)
	Now func() time.Time
	// StatePath overrides paths.ModelSyncStatePath (tests).
	StatePath string
	// Poll overrides PollInterval (tests).
	Poll time.Duration

	mu       sync.Mutex
	inFlight bool
	last     *Result
}

// state is the on-disk model-sync.json shape.
type state struct {
	CheckedAt string               `json:"checkedAt"`
	Added     int                  `json:"added"`
	Providers []ProviderSyncResult `json:"providers"`
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}
func (d *Deps) statePath() string {
	if d.StatePath != "" {
		return d.StatePath
	}
	return paths.ModelSyncStatePath()
}
func (d *Deps) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(fmt.Sprintf(format, args...))
	}
}

// LoadState reads model-sync.json (nil when absent/corrupt).
func (d *Deps) LoadState() *Result {
	b, err := os.ReadFile(d.statePath())
	if err != nil {
		return nil
	}
	var s state
	if json.Unmarshal(b, &s) != nil || s.CheckedAt == "" {
		return nil
	}
	return &Result{CheckedAt: s.CheckedAt, Added: s.Added, Providers: s.Providers}
}

func (d *Deps) saveState(r *Result) {
	path := d.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(state{CheckedAt: r.CheckedAt, Added: r.Added, Providers: r.Providers}, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, append(b, '\n'), 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// Fresh reports whether the last pass is inside the configured interval.
func (d *Deps) Fresh(intervalMinutes int) bool {
	if intervalMinutes < MinIntervalMinutes {
		intervalMinutes = MinIntervalMinutes
	}
	s := d.LoadState()
	if s == nil {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, s.CheckedAt)
	return err == nil && d.now().Sub(at) < time.Duration(intervalMinutes)*time.Minute
}

// Run executes one pass: probe planned providers, merge onto a fresh disk
// read, persist when something landed, then record state. Mirrors
// runModelSync's two-read merge so a dashboard edit mid-probe is preserved.
func (d *Deps) Run(ctx context.Context) (*Result, error) {
	d.mu.Lock()
	if d.inFlight {
		d.mu.Unlock()
		return nil, fmt.Errorf("a model sync pass is already running")
	}
	d.inFlight = true
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.inFlight = false; d.mu.Unlock() }()

	planned := d.Config()
	if planned == nil || len(planned.Providers) == 0 {
		return nil, nil
	}
	discover := d.Discover
	if discover == nil {
		return nil, fmt.Errorf("model discovery is not integrated")
	}

	results := make([]ProviderSyncResult, 0, len(planned.Providers))
	discovered := map[string][]string{}
	for _, p := range planned.Providers {
		if !Syncable(p) {
			skipped := "default-off"
			if p.SyncModels != nil && !*p.SyncModels {
				skipped = "opted-out"
			}
			results = append(results, ProviderSyncResult{Provider: p.Name, Added: []string{}, Skipped: skipped})
			continue
		}
		entry := discover(p)
		if entry.Error != "" {
			results = append(results, ProviderSyncResult{Provider: p.Name, Added: []string{}, Error: entry.Error})
			continue
		}
		merged, added := appendDiscovered(p, entry.Models)
		discovered[p.Name] = added
		results = append(results, ProviderSyncResult{Provider: p.Name, Added: added})
		_ = merged
	}

	// Second read: merge only the ids that still land on fresh state, so a
	// provider edited or deleted while probes ran is never resurrected.
	fresh := d.Config()
	if fresh == nil {
		fresh = planned
	}
	next := *fresh
	next.Providers = make([]config.Provider, len(fresh.Providers))
	copy(next.Providers, fresh.Providers)
	landed := map[string][]string{}
	for i, p := range next.Providers {
		ids := discovered[p.Name]
		if len(ids) == 0 {
			continue
		}
		merged, added := appendDiscovered(p, ids)
		next.Providers[i] = merged
		landed[p.Name] = added
	}
	added := 0
	for i := range results {
		results[i].Added = landed[results[i].Provider]
		if results[i].Added == nil {
			results[i].Added = []string{}
		}
		added += len(results[i].Added)
	}
	result := &Result{
		CheckedAt: d.now().Format(time.RFC3339Nano),
		Providers: results,
		Added:     added,
		Changed:   added > 0,
	}
	if result.Changed {
		if d.SaveConfig != nil {
			if err := d.SaveConfig(&next); err != nil {
				return nil, err
			}
		}
		if d.Reload != nil {
			d.Reload(&next)
		}
	}
	d.saveState(result)
	d.mu.Lock()
	d.last = result
	d.mu.Unlock()
	return result, nil
}

// Tick runs one scheduler wakeup (boot or poll).
func (d *Deps) Tick(ctx context.Context, boot bool) {
	cfg := d.Config()
	if cfg == nil {
		return
	}
	if !cfg.ModelSync.Enabled {
		if boot {
			d.logf("models: auto-sync disabled")
		}
		return
	}
	if d.Fresh(cfg.ModelSync.IntervalMinutes) {
		if boot {
			d.logf("models: up to date")
		}
		return
	}
	result, err := d.Run(ctx)
	if err != nil {
		d.logf("models: sync failed (%s)", err)
		return
	}
	if result == nil {
		return
	}
	for _, p := range result.Providers {
		switch {
		case p.Error != "":
			d.logf("models: %s discovery failed (%s)", p.Provider, p.Error)
		case len(p.Added) > 0:
			d.logf("models: %s +%d (%s)", p.Provider, len(p.Added), strings.Join(p.Added, ", "))
		}
	}
	if !result.Changed && boot {
		d.logf("models: up to date")
	}
}

// Start launches the serve-time scheduler. It ticks immediately (boot) then
// every Poll until ctx is cancelled. Returns immediately; use Ready to wait
// for the boot tick in tests.
func (d *Deps) Start(ctx context.Context) (ready <-chan struct{}) {
	done := make(chan struct{})
	poll := d.Poll
	if poll <= 0 {
		poll = PollInterval
	}
	go func() {
		defer close(done)
		d.Tick(ctx, true)
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.Tick(ctx, false)
			}
		}
	}()
	return done
}

// Handler serves GET /model-sync and POST /model-sync/run for admin
// Extensions. PUT stays on the admin's saveSection path (config mutation).
type Handler struct {
	Deps *Deps
	// Config returns the current snapshot for the GET payload's config field.
	Config func() *config.Config
}

func NewHandler(d *Deps, cfg func() *config.Config) *Handler {
	return &Handler{Deps: d, Config: cfg}
}

// Extensions plugs into admin.Deps.Extensions.
func (h *Handler) Extensions() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /model-sync":      h,
		"POST /model-sync/run": h,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fail(w, 403, "Admin API is only available on loopback.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/model-sync" && r.Method == http.MethodGet:
		h.sendStatus(w)
	case path == "/model-sync/run" && r.Method == http.MethodPost:
		if h.Deps == nil {
			fail(w, 503, "Model sync is not running in this process.")
			return
		}
		result, err := h.Deps.Run(r.Context())
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		h.sendResult(w, result)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) payload(result *Result) map[string]any {
	var c *config.Config
	if h.Config != nil {
		c = h.Config()
	}
	cfg := config.DefaultModelSync
	if c != nil {
		cfg = c.ModelSync
	}
	last := result
	if last == nil && h.Deps != nil {
		last = h.Deps.LoadState()
	}
	out := map[string]any{"config": cfg, "last": last}
	return out
}

func (h *Handler) sendStatus(w http.ResponseWriter) { send(w, 200, h.payload(nil)) }
func (h *Handler) sendResult(w http.ResponseWriter, r *Result) {
	send(w, 200, h.payload(r))
}

func send(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
func fail(w http.ResponseWriter, status int, msg string) {
	send(w, status, map[string]any{"error": msg})
}
