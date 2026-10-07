package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/brain"
	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/provider/cursor"
	"github.com/xinyao27/jevonian/internal/provider/devin"
	"github.com/xinyao27/jevonian/internal/provider/freebuff"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/tunnel"
)

func (h *Handler) providerPayload(b map[string]any, previous *config.Provider) (config.Provider, error) {
	name := strings.TrimSpace(text(b["name"]))
	base := strings.TrimRight(strings.TrimSpace(text(b["baseUrl"])), "/")
	if name == "" {
		name = "probe"
	}
	if base == "" {
		return config.Provider{}, errors.New("baseUrl is required")
	}
	p := map[string]any{"name": name, "baseUrl": base, "type": "openai", "auth": "api-key", "billing": "api", "models": b["models"], "injectStreamUsage": true}
	validType := map[string]bool{"openai": true, "anthropic": true, "responses": true, "both": true, "gemini": true, "devin": true, "cursor": true}
	if validType[text(b["type"])] {
		p["type"] = b["type"]
	}
	if b["auth"] == "oauth" {
		p["auth"] = "oauth"
		p["oauthSource"] = oauth.ParseSource(b["oauthSource"])
	}
	if b["billing"] == "subscription" {
		p["billing"] = "subscription"
	}
	for _, k := range []string{"quota", "login", "apiKeyEnv", "noKey", "syncModels", "excludeModels"} {
		if x, ok := b[k]; ok {
			p[k] = x
		}
	}
	// Login is a partial form exception: omission retains the stored account.
	if previous != nil {
		if _, ok := b["login"]; !ok {
			p["login"] = toMap(previous.Login)
		}
		if _, ok := b["noKey"]; !ok && previous.NoKey {
			p["noKey"] = true
		}
		if _, ok := b["syncModels"]; !ok && previous.SyncModels != nil {
			p["syncModels"] = *previous.SyncModels
		}
	}
	if strings.TrimSpace(text(b["apiKey"])) != "" {
		delete(p, "apiKeyEnv")
	}
	forced, err := oauth.WireType(text(p["oauthSource"]), text(p["type"]), b["type"] != nil)
	if err != nil {
		return config.Provider{}, err
	}
	p["type"] = forced
	if mismatch := oauth.WireMismatch(forced, text(p["auth"]), text(p["oauthSource"])); mismatch != "" {
		return config.Provider{}, errors.New(mismatch)
	}
	parsed, err := config.ParseConfig(map[string]any{"providers": []any{p}})
	if err != nil {
		return config.Provider{}, err
	}
	out := parsed.Providers[0]
	selected := map[string]bool{}
	for i, m := range out.Models {
		selected[m.ID] = true
		if len(m.Wire) == 0 && previous != nil {
			for _, old := range previous.Models {
				if old.ID == m.ID {
					out.Models[i].Wire = old.Wire
					break
				}
			}
		}
	}
	excluded := []string{}
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !selected[s] && !seen[s] {
			seen[s] = true
			excluded = append(excluded, s)
		}
	}
	if x, ok := b["excludeModels"].([]any); ok {
		for _, id := range x {
			add(text(id))
		}
	} else if previous != nil {
		for _, id := range previous.ExcludeModels {
			add(id)
		}
		for _, m := range previous.Models {
			add(m.ID)
		}
	}
	out.ExcludeModels = excluded
	return out, nil
}
func providerAt(c *config.Config, name string) *config.Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}
func (h *Handler) providers(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(value, b map[string]any) (any, int, error) {
		c, err := config.ParseConfig(value)
		if err != nil {
			return nil, 500, err
		}
		if r.Method == "DELETE" {
			name := r.PathValue("name")
			found := false
			ps := []config.Provider{}
			for _, p := range c.Providers {
				if p.Name == name {
					found = true
					continue
				}
				ps = append(ps, p)
			}
			if !found {
				return nil, 404, fmt.Errorf("provider %q not found", name)
			}
			// The name is no longer in use. Remove only its stored API credential,
			// never a shared OAuth login file or keychain item.
			if err := h.deps.Credentials.Remove(name); err != nil {
				return nil, 500, err
			}
			c.Providers = ps
			if c.DefaultProvider == name {
				c.DefaultProvider = ""
				if len(ps) > 0 {
					c.DefaultProvider = ps[0].Name
				}
			}
			v, _ := config.JSONValue(&c)
			next, err := h.persist(v)
			if err != nil {
				return nil, 500, err
			}
			out := map[string]any{"config": publicConfig(next), "tiers": h.routingPayload(next)["tiers"]}
			return out, 200, nil
		}
		name := strings.TrimSpace(text(b["name"]))
		if name == "" || strings.TrimSpace(text(b["baseUrl"])) == "" {
			return nil, 400, errors.New("name and baseUrl are required")
		}
		previous := providerAt(&c, name)
		p, err := h.providerPayload(b, previous)
		if err != nil {
			return nil, 400, err
		}
		key := strings.TrimSpace(text(b["apiKey"]))
		if key != "" {
			if err := h.deps.Credentials.Set(name, key); err != nil {
				return nil, 500, err
			}
		}
		signed := ""
		if p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthFreebuff && !freebuff.HasCredential(p.Login) {
			result, err := h.deps.Freebuff.SignIn(r.Context(), freebuff.EndpointFromBaseURL(p.BaseURL), p.Login)
			if err != nil {
				return nil, 400, fmt.Errorf("Freebuff sign-in failed: %w", err)
			}
			signed = result.Email
		}
		// Saving a provider must not launch a browser. This request only persists
		// provider settings; the user can start sign-in from the dedicated action.
		if len(p.Models) == 0 {
			probe := p
			probe.APIKey = key
			if probe.APIKey == "" {
				probe.APIKey = h.deps.Credentials.Get(p.Name)
			}
			if p.Auth == config.AuthOAuth || p.NoKey || probe.APIKey != "" || config.ResolveAPIKey(p) != "" {
				ids, _ := h.discover(r.Context(), probe)
				for _, id := range ids {
					p.Models = append(p.Models, config.ModelEntry{ID: id})
				}
			}
		}
		replaced := false
		for i, old := range c.Providers {
			if old.Name == name {
				c.Providers[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			c.Providers = append(c.Providers, p)
		}
		if c.DefaultProvider == "" {
			c.DefaultProvider = name
		}
		// Provider catalog changes must not pin auto-derived route model lists. Keep
		// empty Models in configuration; routingPayload derives its live preview.
		derived := deriveRoutings(&c, h.deps.Prices)
		materialized := append([]config.RoutingEntry(nil), c.Routing.Routings...)
		for i, entry := range materialized {
			if len(entry.Models) == 0 && i < len(derived) {
				materialized[i] = derived[i]
			}
		}
		c.Routing.Tiers = tiers(materialized)
		if c.Routing.BaselineModel == "" && len(c.Routing.Tiers.Plan) > 0 {
			c.Routing.BaselineModel = c.Routing.Tiers.Plan[0]
		}
		v, _ := config.JSONValue(&c)
		next, err := h.persist(v)
		if err != nil {
			return nil, 500, err
		}
		out := h.routingPayload(next)
		out["config"] = publicConfig(next)
		if signed != "" {
			out["signedInAs"] = signed
		}
		return out, 200, nil
	})
}
func publicConfig(c *config.Config) map[string]any {
	v, _ := config.JSONValue(c)
	for i, raw := range slice(v["providers"]) {
		p := object(raw)
		delete(p, "apiKey")
		// Admin mutations return the runtime model schema, not the compact disk schema.
		models := []any{}
		for _, model := range c.Providers[i].Models {
			row := map[string]any{"id": model.ID}
			if len(model.Wire) > 0 {
				row["wire"] = model.Wire
			}
			models = append(models, row)
		}
		p["models"] = models
	}
	return v
}
func sortedIDs(ids []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
func (h *Handler) discover(ctx context.Context, p config.Provider) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if p.Type == config.ProviderTypeCursor {
		raw, err := cursor.NewProvider(p, h.deps.HTTP).Models(ctx)
		ids := []string{}
		for _, m := range raw {
			ids = append(ids, m.ID)
		}
		return sortedIDs(ids), err
	}
	if p.Type == config.ProviderTypeDevin {
		auth, err := h.deps.OAuth.ResolveProviderAuth(ctx, p, oauth.WireKindOpenAI, "")
		if err != nil {
			return nil, err
		}
		raw, err := devin.FetchModels(ctx, h.deps.HTTP, auth.Token, p.BaseURL)
		if err != nil {
			return nil, err
		}
		usable := []devin.Model{}
		ids := []string{}
		for _, m := range raw {
			if devin.IsRoutable(m) {
				usable = append(usable, m)
				ids = append(ids, m.ID)
			}
		}
		devin.SaveModelMeta(usable)
		return sortedIDs(ids), nil
	}
	if p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthFreebuff {
		return freebuff.ModelIDs(), nil
	}
	if p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthWorkbuddyAI {
		endpoint := workbuddy.EndpointFromBaseURL(p.BaseURL)
		creds, err := h.deps.Workbuddy.Resolve(ctx, p.Login, endpoint)
		if err != nil {
			return nil, err
		}
		ids, err := h.deps.Workbuddy.FetchModels(ctx, creds, endpoint)
		return sortedIDs(ids), err
	}
	if p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthCodex {
		base := os.Getenv("CODEX_HOME")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".codex")
		}
		data, err := os.ReadFile(filepath.Join(base, "models_cache.json"))
		if err != nil {
			return nil, errors.New("No Codex model cache found. Run `codex` once to refresh ~/.codex/models_cache.json, or add model ids manually.")
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, err
		}
		ids := []string{}
		for _, x := range slice(payload["models"]) {
			m := object(x)
			if m["visibility"] != "hidden" && m["supported_in_api"] != false {
				ids = append(ids, text(m["slug"]))
			}
		}
		return sortedIDs(ids), nil
	}
	if p.Type == config.ProviderTypeResponses && p.Auth != config.AuthAPIKey {
		return nil, errors.New("this endpoint has no /models route; add model ids manually")
	}
	auth, err := h.deps.OAuth.ResolveProviderAuth(ctx, p, oauth.WireKindOpenAI, "")
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/models"
	method := "GET"
	var data []byte
	if p.Type == config.ProviderTypeGemini || p.OAuthSource == config.OAuthAntigravity {
		endpoint = strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/v1internal") + "/v1internal:fetchAvailableModels"
		method = "POST"
		data, _ = json.Marshal(map[string]any{"project": auth.Project})
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range auth.Headers {
		req.Header.Set(k, v)
	}
	resp, err := h.deps.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	ids := []string{}
	if method == "POST" {
		for id, x := range object(payload["models"]) {
			if !strings.HasPrefix(id, "tab_") && !strings.HasPrefix(id, "chat_") && object(x)["isInternal"] != true {
				ids = append(ids, id)
			}
		}
	} else {
		for _, x := range slice(payload["data"]) {
			ids = append(ids, text(object(x)["id"]))
		}
	}
	return sortedIDs(ids), nil
}
func (h *Handler) discoverAPI(w http.ResponseWriter, r *http.Request) {
	b := body(r)
	name := text(b["name"])
	p, err := h.providerPayload(b, providerAt(h.current(), name))
	if err != nil {
		send(w, 400, map[string]any{"models": []string{}, "error": err.Error()})
		return
	}
	p.APIKey = strings.TrimSpace(text(b["apiKey"]))
	if p.APIKey == "" {
		if old := providerAt(h.current(), name); old != nil {
			p.APIKey = old.APIKey
			if p.APIKey == "" {
				p.APIKey = h.deps.Credentials.Get(name)
			}
			if p.APIKey == "" {
				p.APIKey = config.ResolveAPIKey(*old)
			}
		}
	}
	ids, err := h.discover(r.Context(), p)
	out := map[string]any{"models": sortedIDs(ids)}
	// Discovery errors can mean quota exhaustion or a network problem. Never
	// start an interactive browser sign-in as a side effect of discovery.
	if err != nil {
		out["error"] = err.Error()
	}
	send(w, 200, out)
}
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request) {
	b := body(r)
	p, err := h.providerPayload(map[string]any{"baseUrl": workbuddy.Endpoint, "login": b["login"]}, nil)
	if err != nil {
		failure(w, 400, err.Error())
		return
	}
	result, err := h.deps.Workbuddy.SignIn(r.Context(), workbuddy.Endpoint, p.Login)
	if err != nil {
		failure(w, 400, err.Error())
		return
	}
	send(w, 200, map[string]any{"ok": true, "user": result.User})
}
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	c := h.current()
	models := []any{}
	seen := map[string]bool{}
	priceRow := func(input, output any, cacheRead, cacheWrite any) map[string]any {
		pm := map[string]any{"input": input, "output": output}
		if cacheRead != nil {
			pm["cacheRead"] = cacheRead
		}
		if cacheWrite != nil {
			pm["cacheWrite"] = cacheWrite
		}
		return pm
	}
	ids := map[string]bool{}
	for _, p := range c.Providers {
		for _, m := range p.Models {
			key := p.Name + "/" + m.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			id := canonical(m.ID)
			if id != "unknown" {
				ids[id] = true
			}
			row := map[string]any{"id": m.ID, "provider": p.Name, "configured": true, "canonical": id}
			if h.deps.Prices != nil {
				if price := h.deps.Prices(m.ID, p.Name); price != nil {
					var cr, cw any
					if price.HasCacheRd {
						cr = price.CacheRead
					}
					if price.HasCacheWr {
						cw = price.CacheWrite
					}
					row["price"] = priceRow(price.Input, price.Output, cr, cw)
				}
			}
			models = append(models, row)
		}
	}
	// Snapshot rows for providers the user has configured but models they have not added yet.
	snapshot := pricingSnapshotModels()
	for _, p := range c.Providers {
		prefix := p.Name + "/"
		keys := make([]string, 0)
		for key := range snapshot {
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			id := strings.TrimPrefix(key, prefix)
			if seen[key] {
				continue
			}
			seen[key] = true
			price := snapshot[key]
			row := map[string]any{"id": id, "provider": p.Name, "configured": false, "canonical": canonical(id)}
			var cr, cw any
			if v, ok := price["cacheRead"]; ok {
				cr = v
			}
			if v, ok := price["cacheWrite"]; ok {
				cw = v
			}
			row["price"] = priceRow(price["input"], price["output"], cr, cw)
			models = append(models, row)
		}
	}
	for canonicalID := range c.ModelAliases {
		if key := routing.CanonicalModelID(strings.TrimSpace(canonicalID)); key != "" {
			ids[key] = true
		}
	}
	sortedCanonical := make([]string, 0, len(ids))
	for id := range ids {
		sortedCanonical = append(sortedCanonical, id)
	}
	sort.Strings(sortedCanonical)
	identity := catalogsync.NewIdentity()
	named := catalogsync.Identity{}
	canonicals := []any{}
	for _, id := range sortedCanonical {
		variants := routing.CanonicalVariants(c, id, "", identity)
		if len(variants) == 0 {
			continue
		}
		rows := make([]any, 0, len(variants))
		for _, v := range variants {
			row := map[string]any{"provider": v.Provider, "model": v.Model}
			if v.ViaIdentity {
				row["viaIdentity"] = true
			}
			if v.Official {
				row["official"] = true
			}
			rows = append(rows, row)
		}
		entry := map[string]any{"id": id, "variants": rows}
		ident := named.Of(id)
		if ident.DisplayName != "" {
			entry["name"] = ident.DisplayName
		}
		if ident.Family != "" {
			entry["family"] = ident.Family
		}
		canonicals = append(canonicals, entry)
	}
	send(w, 200, map[string]any{"models": models, "canonicals": canonicals})
}

// pricingSnapshotModels reads the `models` table of the models.dev snapshot (empty when absent).
func pricingSnapshotModels() map[string]map[string]any {
	data, err := os.ReadFile(filepath.Join(paths.DataDir(), "pricing.json"))
	if err != nil {
		return nil
	}
	var snapshot struct {
		Models map[string]map[string]any `json:"models"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return nil
	}
	return snapshot.Models
}
func (h *Handler) testBrain(w http.ResponseWriter, r *http.Request) {
	b := body(r)
	saved := config.DefaultBrain
	if bs := h.current().Routing.Brains; len(bs) > 0 {
		saved = bs[0]
	}
	current := toMap(saved)
	// Test credentials are deliberately not persisted.
	key := strings.TrimSpace(text(b["apiKey"]))
	delete(b, "apiKey")
	for _, k := range []string{"channel", "baseUrl", "accountId", "apiKeyEnv", "model"} {
		if s, ok := b[k].(string); ok && (s != "" || k == "accountId") {
			current[k] = strings.TrimSpace(s)
		}
	}
	if n, ok := b["timeoutMs"].(float64); ok && n > 0 {
		current["timeoutMs"] = n
	}
	cfg, err := config.ParseConfig(map[string]any{"routing": map[string]any{"brains": []any{current}}})
	if err != nil {
		failure(w, 400, err.Error())
		return
	}
	selected := cfg.Routing.Brains[0]
	channel := brain.FindChannel(selected.Channel)
	base := selected.BaseURL
	if base == "" && channel != nil {
		base = channel.BaseURL
	}
	if selected.Channel == "cloudflare" && selected.AccountID == "" {
		send(w, 200, map[string]any{"ok": false, "error": "Set a Cloudflare account ID for this channel."})
		return
	}
	if selected.Channel != "cloudflare" && selected.Channel != "vercel" && base == "" {
		send(w, 200, map[string]any{"ok": false, "error": "Set a base URL for this channel."})
		return
	}
	if key == "" && h.brainSource(selected) == "none" {
		send(w, 200, map[string]any{"ok": false, "error": "Add an API key for this channel."})
		return
	}
	state := map[string]any{"last_user_message": "Design a caching layer for the settings page.", "recent_tool_results": []any{}, "has_tool_results": false, "consecutive_failures": 0, "session_turns": 1, "routings": []any{map[string]any{"id": "plan", "label": "Plan", "description": "planning, coordination, review", "models": []any{map[string]any{"model": "claude-opus-4-6", "provider": "anthropic"}, map[string]any{"model": "gpt-5.6", "provider": "openai"}}}, map[string]any{"id": "execute", "label": "Execute", "description": "implementation, debugging, tool loops", "models": []any{map[string]any{"model": "deepseek-v4.1-flash", "provider": "deepseek"}}}}, "candidates": []any{map[string]any{"model": "claude-opus-4-6", "provider": "anthropic"}, map[string]any{"model": "gpt-5.6", "provider": "openai"}, map[string]any{"model": "deepseek-v4.1-flash", "provider": "deepseek"}}}
	started := time.Now()
	outcome := h.deps.Brain.Ask(r.Context(), brain.Input{Brain: selected, APIKey: key, State: state})
	out := map[string]any{"ok": outcome.Verdict != nil, "latencyMs": time.Since(started).Milliseconds()}
	if outcome.Verdict == nil {
		message := "No verdict. Check the endpoint, model, and key."
		if outcome.Failure != nil && outcome.Failure.Error != "" {
			message = "No verdict (" + outcome.Failure.Error + "). Check the endpoint, model, and key."
		}
		out["error"] = message
	} else {
		out["verdict"] = outcome.Verdict
		label := selected.Channel
		model := selected.Model
		if channel != nil {
			label = channel.Label
			if model == "" {
				model = channel.Model
			}
		}
		out["channel"] = label
		out["model"] = model
	}
	send(w, 200, out)
}
func (h *Handler) lanAPI(w http.ResponseWriter, r *http.Request) {
	payload := func(c *config.Config, changed bool) map[string]any {
		return map[string]any{"config": c.Lan, "port": tunnel.LanPort(c), "bindHost": tunnel.LanBindHost(c.Lan), "urls": tunnel.LanBaseURLs(c, tunnel.LanIPv4Addresses(nil)), "restartRequired": changed}
	}
	if r.Method == "GET" {
		send(w, 200, payload(h.current(), false))
		return
	}
	h.mutate(w, r, func(value, b map[string]any) (any, int, error) {
		old := object(value["lan"])
		value["lan"] = old
		before, _ := json.Marshal(old)
		for _, k := range []string{"enabled", "host", "port"} {
			if x, ok := b[k]; ok {
				old[k] = x
			}
		}
		message := ""
		if old["enabled"] == true && (h.deps.Keys == nil || !h.deps.Keys.HasKeys()) {
			old["enabled"] = false
			message = "Create a Jevonian API key first — the LAN endpoint refuses unauthenticated traffic."
		}
		c, err := h.persist(value)
		if err != nil {
			return nil, 500, err
		}
		after, _ := json.Marshal(c.Lan)
		out := payload(c, string(before) != string(after))
		if message != "" {
			out["error"] = message
		}
		return out, 200, nil
	})
}
func (h *Handler) tunnelAPI(w http.ResponseWriter, r *http.Request) {
	payload := func(c *config.Config) map[string]any {
		out := map[string]any{"config": c.Tunnel, "tunnel": nil}
		if h.deps.Tunnel != nil {
			out["tunnel"] = h.deps.Tunnel.Status()
		}
		return out
	}
	if r.Method == "GET" {
		send(w, 200, payload(h.current()))
		return
	}
	h.mutate(w, r, func(value, b map[string]any) (any, int, error) {
		v := object(value["tunnel"])
		value["tunnel"] = v
		for _, k := range []string{"enabled", "provider", "command", "url", "publicPort"} {
			if x, ok := b[k]; ok {
				if k == "command" && strings.TrimSpace(text(x)) == "" {
					continue
				}
				v[k] = x
			}
		}
		message := ""
		if v["enabled"] == true && (h.deps.Keys == nil || !h.deps.Keys.HasKeys()) {
			v["enabled"] = false
			message = "Create a Jevonian API key first — the public endpoint refuses unauthenticated traffic."
		}
		c, err := h.persist(value)
		if err != nil {
			return nil, 500, err
		}
		if h.deps.Tunnel == nil {
			message = "Tunnel manager is not running in this process."
		} else {
			h.deps.Tunnel.Update(c.Tunnel, c.Listen.Port)
			if c.Tunnel.Enabled {
				h.deps.Tunnel.Start(context.Background())
			} else {
				h.deps.Tunnel.Stop()
			}
			if e := h.deps.Tunnel.Status().Error; e != "" {
				message = e
			}
		}
		out := payload(c)
		if message != "" {
			out["error"] = message
		}
		return out, 200, nil
	})
}
func (h *Handler) quotaResetAPI(w http.ResponseWriter, r *http.Request) {
	if h.deps.ResetQuota == nil {
		failure(w, 503, "Quota reset is unavailable.")
		return
	}
	var payload struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil || strings.TrimSpace(payload.Provider) == "" {
		failure(w, 400, "provider is required")
		return
	}
	if err := h.deps.ResetQuota(r.Context(), h.current(), strings.TrimSpace(payload.Provider)); err != nil {
		failure(w, 400, err.Error())
		return
	}
	send(w, 200, map[string]any{"ok": true})
}

func (h *Handler) quotaAPI(w http.ResponseWriter, r *http.Request) {
	c := h.current()
	if h.deps.Quotas == nil {
		failure(w, 503, "Live quota probes are unavailable; integrate Deps.Quotas.")
		return
	}
	qs, err := h.deps.Quotas(r.Context(), c, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		failure(w, 502, err.Error())
		return
	}
	health := []any{}
	for _, p := range c.Providers {
		row := map[string]any{"provider": p.Name, "billing": p.Billing, "status": "unknown"}
		if h.deps.Quota != nil {
			q := h.deps.Quota.ProviderHealth(p, quota.HealthOptions{LowPercent: &c.Routing.QuotaGuard.LowPercent})
			row["status"] = q.Status
			modelHealth := []any{}
			for _, model := range p.Models {
				q := h.deps.Quota.ModelHealth(p, model.ID)
				if q.Status == quota.StatusOK {
					continue
				}
				modelRow := map[string]any{"model": q.Model, "status": q.Status}
				if q.Reason != "" {
					modelRow["reason"] = q.Reason
				}
				if !q.ResetsAt.IsZero() {
					modelRow["resetsAt"] = q.ResetsAt.UTC().Format(time.RFC3339Nano)
				}
				modelHealth = append(modelHealth, modelRow)
			}
			row["modelHealth"] = modelHealth
			if q.Status != quota.StatusUnknown {
				row["usedPercent"] = q.UsedPercent
				row["remainingPercent"] = q.RemainingPercent
			}
			if q.Window != "" {
				row["window"] = q.Window
			}
			if q.ResetsAt != "" {
				row["resetsAt"] = q.ResetsAt
			}
			if q.RemainingUSD != nil {
				row["remainingUsd"] = q.RemainingUSD
			}
			if q.AvgRequestUSD != nil {
				row["avgRequestUsd"] = q.AvgRequestUSD
			}
			if q.Note != "" {
				row["note"] = q.Note
			}
		}
		health = append(health, row)
	}
	send(w, 200, map[string]any{"quotas": qs, "health": health, "guard": c.Routing.QuotaGuard})
}
