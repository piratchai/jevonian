package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/modelsync"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/provider/cursor"
	"github.com/xinyao27/jevonian/internal/provider/devin"
	"github.com/xinyao27/jevonian/internal/provider/freebuff"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
	"github.com/xinyao27/jevonian/internal/proxy"
)

func cliHTTP() *http.Client {
	transport, _ := proxy.UseSystemProxy()
	forceHTTP1(transport)
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

// forceHTTP1 keeps ALPN on http/1.1. Cloning http.DefaultTransport copies a TLS config that
// already offers "h2", so an h2-only host (models.dev) would answer HTTP/2 frames to a client
// whose TLSNextProto map has h2 disabled ("malformed HTTP response"). TS runs allowH2:false.
func forceHTTP1(t *http.Transport) {
	if t == nil {
		return
	}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
}
func sortedUnique(list []string) []string {
	set := map[string]bool{}
	for _, id := range list {
		if id != "" {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CatalogEntry is one provider's discovered model snapshot (models.json).
// Exported so admin's GET /catalog extension can serve it without cli internals.
type CatalogEntry struct {
	Provider  string   `json:"provider"`
	Models    []string `json:"models"`
	FetchedAt string   `json:"fetchedAt"`
	Error     string   `json:"error,omitempty"`
}
type catalogEntry = CatalogEntry

// LoadCatalog reads the cached discovery snapshot (nil when absent/corrupt).
func LoadCatalog() []CatalogEntry { return loadCatalog() }

// CatalogPath is the discovery snapshot location (admin catalog status).
func CatalogPath() string { return catalogPath() }

// RefreshModels discovers every provider's models and persists the catalog.
// The function value is shareable with the admin POST /catalog/refresh handler.
func RefreshModels(cfg *config.Config) ([]CatalogEntry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is unavailable")
	}
	entries := make([]CatalogEntry, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		entries = append(entries, DiscoverProvider(p))
	}
	return entries, writeJSONAtomic(catalogPath(), entries, 0o600)
}

// SyncModels appends newly discovered ids for providers whose sync is enabled.
// It never removes or reorders entries; excluded ids stay out. Returns the
// updated config (caller persists), discovery entries, and per-provider adds.
func SyncModels(cfg *config.Config) (*config.Config, []CatalogEntry, map[string][]string, error) {
	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("config is unavailable")
	}
	next := *cfg
	next.Providers = make([]config.Provider, len(cfg.Providers))
	for i, p := range cfg.Providers {
		next.Providers[i] = p
		next.Providers[i].Models = append([]config.ModelEntry{}, p.Models...)
		next.Providers[i].ExcludeModels = append([]string{}, p.ExcludeModels...)
	}
	entries := make([]CatalogEntry, 0, len(next.Providers))
	added := map[string][]string{}
	failed := 0
	for i, p := range next.Providers {
		if !modelsync.Syncable(p) {
			continue
		}
		entry := DiscoverProvider(p)
		entries = append(entries, entry)
		if entry.Error != "" {
			failed++
			continue
		}
		excluded := map[string]bool{}
		for _, id := range p.ExcludeModels {
			excluded[id] = true
		}
		for _, id := range entry.Models {
			if excluded[id] || config.ProviderHasModel(next.Providers[i], id) {
				continue
			}
			next.Providers[i].Models = append(next.Providers[i].Models, config.ModelEntry{ID: id})
			added[p.Name] = append(added[p.Name], id)
		}
	}
	_ = writeJSONAtomic(catalogPath(), entries, 0o600)
	if failed > 0 {
		return &next, entries, added, fmt.Errorf("%d model sync probe(s) failed", failed)
	}
	return &next, entries, added, nil
}

// DiscoverProvider performs one provider's live model discovery. Exported so
// the model-sync scheduler and admin refresh share the CLI's probe logic.
func DiscoverProvider(p config.Provider) CatalogEntry { return discoverProvider(p) }

func catalogPath() string { return filepath.Join(paths.DataDir(), "models.json") }
func loadCatalog() []catalogEntry {
	var entries []catalogEntry
	b, err := os.ReadFile(catalogPath())
	if err == nil {
		_ = json.Unmarshal(b, &entries)
	}
	return entries
}
func discoverProvider(p config.Provider) catalogEntry {
	e := catalogEntry{Provider: p.Name, Models: []string{}, FetchedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := cliHTTP()
	fail := func(err error) catalogEntry { e.Error = err.Error(); return e }
	if p.Type == config.ProviderTypeCursor || p.OAuthSource == config.OAuthCursor {
		raw, err := cursor.FetchModels(ctx)
		if err != nil {
			return fail(err)
		}
		catalog := cursor.SaveCatalog(raw)
		if catalog == nil || len(catalog.Models) == 0 {
			return fail(fmt.Errorf("cursor-agent listed no models; run cursor-agent login and cursor-agent models"))
		}
		for _, m := range catalog.Models {
			e.Models = append(e.Models, m.ID)
		}
		return e
	}
	if p.Type == config.ProviderTypeDevin || p.OAuthSource == config.OAuthDevin {
		models, err := devin.NewProvider(p, client).Models(ctx)
		if err != nil {
			return fail(err)
		}
		for _, m := range models {
			e.Models = append(e.Models, m.ID)
		}
		return e
	}
	if p.OAuthSource == config.OAuthFreebuff {
		// The session endpoint cannot list models without taking a session, and
		// taking one just to list would supersede a live chat. Offer the built-in table.
		e.Models = freebuff.ModelIDs()
		return e
	}
	if p.OAuthSource == config.OAuthWorkbuddyAI {
		wb := &workbuddy.Client{HTTP: client}
		endpoint := workbuddy.EndpointFromBaseURL(p.BaseURL)
		creds, err := wb.Resolve(ctx, p.Login, endpoint)
		if err != nil {
			return fail(err)
		}
		e.Models, err = wb.FetchModels(ctx, creds, endpoint)
		if err != nil {
			return fail(err)
		}
		return e
	}
	if p.OAuthSource == config.OAuthCodex {
		base := os.Getenv("CODEX_HOME")
		if p.Login != nil && p.Login.Home != "" {
			base = p.Login.Home
		}
		if base == "" {
			h, _ := os.UserHomeDir()
			base = filepath.Join(h, ".codex")
		}
		var cache struct {
			Models []struct {
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
				Supported  *bool  `json:"supported_in_api"`
				Priority   *int   `json:"priority"`
			} `json:"models"`
		}
		b, err := os.ReadFile(filepath.Join(base, "models_cache.json"))
		if err != nil {
			return fail(fmt.Errorf("no Codex model cache found; run codex once or add model ids manually"))
		}
		if err := json.Unmarshal(b, &cache); err != nil {
			return fail(err)
		}
		sort.SliceStable(cache.Models, func(i, j int) bool {
			a, b := int(^uint(0)>>1), int(^uint(0)>>1)
			if cache.Models[i].Priority != nil {
				a = *cache.Models[i].Priority
			}
			if cache.Models[j].Priority != nil {
				b = *cache.Models[j].Priority
			}
			return a < b
		})
		for _, m := range cache.Models {
			if m.Slug != "" && m.Visibility != "hidden" && (m.Supported == nil || *m.Supported) {
				e.Models = append(e.Models, m.Slug)
			}
		}
		if len(e.Models) == 0 {
			return fail(fmt.Errorf("no usable Codex models in cache"))
		}
		return e
	}
	resolver := oauth.Resolver{HTTP: client, CursorToken: cursor.Token}
	kind := oauth.WireKindOpenAI
	if p.Type == config.ProviderTypeAnthropic {
		kind = oauth.WireKindAnthropic
	}
	auth, err := resolver.ResolveProviderAuth(ctx, p, kind, "")
	if err != nil {
		return fail(err)
	}
	target := strings.TrimRight(p.BaseURL, "/") + "/models"
	method := http.MethodGet
	var body io.Reader
	gemini := p.Type == config.ProviderTypeGemini || p.OAuthSource == config.OAuthAntigravity
	if gemini {
		target = strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/v1internal") + "/v1internal:fetchAvailableModels"
		method = http.MethodPost
		b, _ := json.Marshal(map[string]string{"project": auth.Project})
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fail(err)
	}
	for k, v := range auth.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models map[string]struct {
			Internal bool `json:"isInternal"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return fail(err)
	}
	if gemini {
		for id, m := range payload.Models {
			if !m.Internal && !strings.HasPrefix(id, "tab_") && !strings.HasPrefix(id, "chat_") {
				e.Models = append(e.Models, id)
			}
		}
		sort.Strings(e.Models)
	} else {
		for _, m := range payload.Data {
			if m.ID != "" {
				e.Models = append(e.Models, m.ID)
			}
		}
	}
	return e
}
func (c commandContext) refreshModels(cfg config.Config) ([]catalogEntry, error) {
	return RefreshModels(&cfg)
}

// syncedProvider mirrors SyncModels' enable rule for output labeling.
func syncedProvider(p config.Provider) bool { return modelsync.Syncable(p) }
func (c commandContext) models(a arguments) error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("No providers configured. Run `jevonian add`.")
	}
	if a.has("sync") {
		return c.modelsSync()
	}
	entries := loadCatalog()
	if a.has("refresh") {
		entries, err = c.refreshModels(cfg)
		if err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		fmt.Fprintln(c.out, "No catalog yet. Run `jevonian models --refresh`.")
		return nil
	}
	failed := false
	for _, e := range entries {
		if e.Error != "" {
			fmt.Fprintf(c.out, "%s: error: %s\n", e.Provider, e.Error)
			failed = true
		} else {
			fmt.Fprintf(c.out, "%s: %d models\n", e.Provider, len(e.Models))
		}
		for _, id := range e.Models {
			fmt.Fprintln(c.out, "  "+id)
		}
	}
	if failed && a.has("refresh") {
		return fmt.Errorf("one or more model discovery probes failed")
	}
	return nil
}

// modelsSync runs one append-only discovery pass (src/model-sync.ts runModelSync): the
// config is re-read before merging so a concurrent dashboard edit is never clobbered.
func (c commandContext) modelsSync() error {
	deps := &modelsync.Deps{
		Config: func() *config.Config {
			cfg, _, err := config.Load()
			if err != nil {
				return nil
			}
			return &cfg
		},
		Discover: func(p config.Provider) modelsync.Discovered {
			e := discoverProvider(p)
			return modelsync.Discovered{Models: e.Models, Error: e.Error}
		},
		SaveConfig: func(next *config.Config) error { return saveConfig(*next) },
	}
	result, err := deps.Run(context.Background())
	if err != nil {
		return err
	}
	if result == nil {
		fmt.Fprintln(c.out, "models: nothing to sync")
		return nil
	}
	for _, entry := range result.Providers {
		switch {
		case entry.Skipped == "opted-out":
			fmt.Fprintf(c.out, "%s: skipped (syncModels: false)\n", entry.Provider)
		case entry.Skipped == "default-off":
			fmt.Fprintf(c.out, "%s: skipped (API/reseller; set syncModels: true to enable)\n", entry.Provider)
		case entry.Error != "":
			fmt.Fprintf(c.out, "%s: error: %s\n", entry.Provider, entry.Error)
		case len(entry.Added) > 0:
			fmt.Fprintf(c.out, "%s: +%d (%s)\n", entry.Provider, len(entry.Added), strings.Join(entry.Added, ", "))
		default:
			fmt.Fprintf(c.out, "%s: up to date\n", entry.Provider)
		}
	}
	return nil
}
