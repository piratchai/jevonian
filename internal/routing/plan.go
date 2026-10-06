// Package routing ports the candidate-plan logic of src/routing.ts: virtual
// model resolution, pre-brain candidate narrowing, the heuristic phase
// classifier, session/cache affinity, and the x-jevonian-reason semantics the
// server stamps on responses.
//
// The brain HTTP call itself lives behind the Scorer seam (internal/brain
// provides the production implementation); Decide never speaks to a channel
// directly.
package routing

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/quota"
)

// RequestKind is the client wire a request arrived on.
// src/routing.ts RequestKind.
type RequestKind string

const (
	KindOpenAI    RequestKind = "openai"
	KindAnthropic RequestKind = "anthropic"
	KindResponses RequestKind = "responses"
)

// ReasoningEffort is a thinking level in catalog vocabulary.
// src/capabilities.ts ReasoningEffort.
type ReasoningEffort = string

var effortDepth = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3,
	"high": 4, "xhigh": 5, "max": 6, "ultra": 8,
}

// ReasoningEfforts lists every level, cheapest first.
// src/capabilities.ts REASONING_EFFORTS.
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// IsReasoningEffort reports whether value names a thinking level.
// src/capabilities.ts isReasoningEffort.
func IsReasoningEffort(value string) bool {
	_, ok := effortDepth[value]
	return ok
}

// EffortRank is how deep a level thinks. src/capabilities.ts effortRank.
func EffortRank(effort string) int {
	return effortDepth[effort]
}

// ModelCapabilities is what a model can actually do.
// src/capabilities.ts ModelCapabilities.
type ModelCapabilities struct {
	// ContextWindow in tokens; 0 means "unknown", not "unlimited".
	ContextWindow int
	// MaxOutput tokens per response, when stated.
	MaxOutput int
	// Efforts the model accepts, cheapest first; empty means "unknown".
	Efforts []string
}

// CapabilitySource resolves stated limits for a model id (models.dev snapshot,
// provider catalogs). src/capabilities.ts modelCapabilities. Injected because
// the Go catalog snapshot is deferred; config.routing.capacities still
// override per field inside Decide.
type CapabilitySource func(model string) ModelCapabilities

// EffectiveCapabilities layers a config capacities override over the stated
// catalog values field by field. src/capabilities.ts effectiveCapabilities.
func EffectiveCapabilities(model string, stated ModelCapabilities, override *config.ModelCapacityConfig) ModelCapabilities {
	out := stated
	if override == nil {
		return out
	}
	if override.ContextWindow != nil {
		out.ContextWindow = *override.ContextWindow
	}
	if override.MaxOutput != nil {
		out.MaxOutput = *override.MaxOutput
	}
	if len(override.Efforts) > 0 {
		filtered := override.Efforts[:0]
		for _, e := range override.Efforts {
			if IsReasoningEffort(e) {
				filtered = append(filtered, e)
			}
		}
		out.Efforts = append([]string{}, filtered...)
	}
	return out
}

// ClampEffort picks the thinking level to actually use: the shallowest
// supported level at least as deep as the request, else the deepest the model
// has, else the middle when nothing was asked. No stated levels passes the
// request through untouched. src/capabilities.ts clampEffort.
func ClampEffort(requested string, supports []string) string {
	if len(supports) == 0 {
		return requested
	}
	for _, s := range supports {
		if s == requested && requested != "" {
			return requested
		}
	}
	if requested == "" {
		return supports[(len(supports)-1)/2]
	}
	target := EffortRank(requested)
	best := ""
	for _, s := range supports {
		if EffortRank(s) >= target && (best == "" || EffortRank(s) < EffortRank(best)) {
			best = s
		}
	}
	if best != "" {
		return best
	}
	for _, s := range supports {
		if best == "" || EffortRank(s) > EffortRank(best) {
			best = s
		}
	}
	return best
}

// FitsContext reports whether the stated window can hold tokens. Unknown
// windows always fit; the 90% factor leaves headroom for the response and for
// the estimate being approximate. src/capabilities.ts fitsContext.
func FitsContext(window, tokens int) bool {
	if window <= 0 {
		return true
	}
	return tokens <= int(float64(window)*0.9)
}

// ---- virtual model ids (src/routing.ts) ----

// VirtualModels are the `jevonian/*` aliases clients can request.
// src/routing.ts virtualModels.
func VirtualModels(cfg *config.Config) []string {
	out := []string{"jevonian/auto"}
	for _, entry := range cfg.Routing.Routings {
		out = append(out, "jevonian/"+entry.ID)
	}
	return out
}

// ClientModels is what /v1/models advertises.
// src/routing.ts clientModels.
func ClientModels(cfg *config.Config) []string {
	if cfg.Routing.Mode == "auto" {
		return VirtualModels(cfg)
	}
	if cfg.Routing.BaselineModel != "" {
		return []string{cfg.Routing.BaselineModel}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			if m.ID != "" && !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m.ID)
			}
		}
	}
	return out
}

// DesktopModels is what ChatGPT/Claude Desktop pickers inject.
// src/routing.ts desktopModels.
func DesktopModels(cfg *config.Config) []string {
	if cfg.Routing.Mode == "auto" {
		return []string{"jevonian/auto"}
	}
	return ClientModels(cfg)
}

// ClaudeCodeModels is what Claude Code's tier remaps can point at.
// src/routing.ts claudeCodeModels.
func ClaudeCodeModels(cfg *config.Config) []string {
	return ClientModels(cfg)
}

// IsDesktopRoutedModel reports whether a desktop request should be served by
// Jevonian rather than the native upstream. src/routing.ts isDesktopRoutedModel.
func IsDesktopRoutedModel(model string) bool {
	id := strings.TrimSpace(model)
	if id == "" {
		return false
	}
	return id == "jevonian/auto" || id == "auto" || strings.HasPrefix(id, "jevonian/")
}

// IsVirtualModel reports whether a requested model id is virtual.
// Virtual ids always win over a provider that catalogs the same bare name.
// src/routing.ts isVirtualModel.
func IsVirtualModel(model string, cfg *config.Config) bool {
	id := strings.TrimPrefix(model, "jevonian/")
	if id == "auto" {
		return true
	}
	if cfg != nil {
		for _, entry := range cfg.Routing.Routings {
			if entry.ID == id {
				return true
			}
		}
		return false
	}
	for _, b := range config.BuiltinRoutingIDs {
		if b == id {
			return true
		}
	}
	return false
}

// ---- wire helpers (src/wire.ts, minimal port for candidate filtering) ----

var nativeResponsesHosts = []string{"opencode.ai"}
var messagesClaudeOnlyHosts = []string{"opencode.ai", "commandcode.ai"}

func hostMatches(baseURL string, suffixes []string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, suffix := range suffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// IsNativeResponsesHost reports whether the host accepts /responses natively.
// src/wire.ts isNativeResponsesHost.
func IsNativeResponsesHost(baseURL string) bool {
	return hostMatches(baseURL, nativeResponsesHosts)
}

// MessagesRejectsNonClaude reports whether a `both` host rejects non-Claude
// ids on /messages. src/wire.ts messagesRejectsNonClaude.
func MessagesRejectsNonClaude(p config.Provider) bool {
	return hostMatches(p.BaseURL, messagesClaudeOnlyHosts)
}

// ProviderSpeaks reports native wire support — no translation.
// src/wire.ts providerSpeaks.
func ProviderSpeaks(p config.Provider, wire config.ProviderType) bool {
	if p.Type == config.ProviderTypeBoth {
		return wire == config.ProviderTypeOpenAI || wire == config.ProviderTypeAnthropic
	}
	// Gemini, Devin and Cursor wrap their own envelopes around a Chat
	// Completions body. src/wire.ts.
	if p.Type == config.ProviderTypeGemini || p.Type == config.ProviderTypeDevin || p.Type == config.ProviderTypeCursor {
		return wire == config.ProviderTypeOpenAI
	}
	if wire == config.ProviderTypeOpenAI {
		return p.Type == config.ProviderTypeOpenAI || p.Type == config.ProviderTypeResponses
	}
	return p.Type == wire
}

// CanServeClient is the routing filter: may this provider serve a client on
// this wire, including bridgeable hosts? src/wire.ts canServeClient.
func CanServeClient(p config.Provider, client RequestKind) bool {
	if ProviderSpeaks(p, config.ProviderType(client)) {
		return true
	}
	// Devin/Cursor take a Chat Completions body; every client folds onto it.
	if p.Type == config.ProviderTypeDevin || p.Type == config.ProviderTypeCursor {
		return true
	}
	if client == KindAnthropic && p.Type == config.ProviderTypeOpenAI {
		return true
	}
	if client == KindOpenAI && p.Type == config.ProviderTypeAnthropic {
		return true
	}
	if client == KindResponses &&
		(p.Type == config.ProviderTypeOpenAI || p.Type == config.ProviderTypeBoth ||
			p.Type == config.ProviderTypeGemini || p.Type == config.ProviderTypeAnthropic) {
		return true
	}
	return false
}

// ---- model id helpers (src/model-id.ts, src/models.ts) ----

// BareModelID is the model segment after the last `/`.
// src/model-id.ts bareModelId.
func BareModelID(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

var (
	dateSuffix       = regexp.MustCompile(`-\d{8}$`)
	dashedDateSuffix = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)
	serviceTierTail  = regexp.MustCompile(`-tiered$`)
	multiDash        = regexp.MustCompile(`-+`)
	edgeDashes       = regexp.MustCompile(`^-+|-+$`)
)

// CanonicalModelID normalizes a model id for cross-provider comparison.
// src/models.ts canonicalModelId.
func CanonicalModelID(model string) string {
	tail := strings.ToLower(BareModelID(model))
	tail = strings.ReplaceAll(tail, ".", "-")
	tail = dateSuffix.ReplaceAllString(tail, "")
	tail = dashedDateSuffix.ReplaceAllString(tail, "")
	tail = serviceTierTail.ReplaceAllString(tail, "")
	tail = multiDash.ReplaceAllString(tail, "-")
	return edgeDashes.ReplaceAllString(tail, "")
}

// ModelVariant is one provider/model pair a requested id resolves to.
// src/models.ts ModelVariant.
type ModelVariant struct {
	Provider string
	Model    string
	// ViaIdentity marks a catalog-identity (not spelling) match. Surfaced for
	// callers that care; Decide treats both the same.
	ViaIdentity bool
	// Official marks the configured provider as the vendor that owns this
	// model, not a reseller. Requires an IdentityIndex; always false when nil.
	Official bool
}

// IdentityIndex resolves cross-spelling same-model matches. src/models.ts
// identityOf / identityKeyOf / isOfficial. Injected because the Go catalog
// snapshot is deferred; when nil, canonical-variant widening still works on
// canonical-id spelling, and no provider is ever marked official.
type IdentityIndex interface {
	// IdentityKey returns the comparable key for a model id, or "" when the
	// catalog does not name it. Two ids with the same key are the same model.
	IdentityKey(model string) string
	// IsOfficial reports whether providerName is the vendor that owns
	// requested's identity.
	IsOfficial(cfg *config.Config, providerName, requested string) bool
}

// CanonicalVariants widens a requested model id across aliases and
// same-canonical / same-identity spellings. src/models.ts canonicalVariants.
func CanonicalVariants(cfg *config.Config, requested string, kind RequestKind, idx IdentityIndex) []ModelVariant {
	key := CanonicalModelID(requested)
	if key == "" {
		return nil
	}
	identity := ""
	if idx != nil {
		identity = idx.IdentityKey(requested)
	}
	variants := []ModelVariant{}
	seen := map[string]bool{}
	push := func(p config.Provider, model string, viaIdentity bool) {
		if kind != "" && !CanServeClient(p, kind) {
			return
		}
		id := p.Name + "/" + model
		if seen[id] {
			return
		}
		seen[id] = true
		v := ModelVariant{Provider: p.Name, Model: model, ViaIdentity: viaIdentity}
		if idx != nil && idx.IsOfficial(cfg, p.Name, requested) {
			v.Official = true
		}
		variants = append(variants, v)
	}

	// modelAliases entries pin a canonical id to provider/model spellings.
	// src/models.ts aliasIndex.
	for canonical, entries := range cfg.ModelAliases {
		if CanonicalModelID(canonical) != key {
			continue
		}
		for _, entry := range entries {
			slash := strings.Index(entry, "/")
			if slash > 0 {
				pname := entry[:slash]
				if p := providerByName(cfg, pname); p != nil {
					push(*p, entry[slash+1:], false)
					continue
				}
			}
			for _, p := range cfg.Providers {
				if config.ProviderHasModel(p, entry) {
					push(p, entry, false)
				}
			}
		}
	}

	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			if CanonicalModelID(m.ID) == key {
				push(p, m.ID, false)
			} else if identity != "" && idx != nil && idx.IdentityKey(m.ID) == identity {
				push(p, m.ID, true)
			}
		}
	}
	return variants
}

func providerByName(cfg *config.Config, name string) *config.Provider {
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			return &cfg.Providers[i]
		}
	}
	return nil
}

// ---- pricing seam ----

// Price is a per-million-token rate card. src/pricing.ts ModelPrice.
type Price struct {
	Provider   string
	PeakRule   string // "deepseek"
	Input      float64
	Output     float64
	CacheRead  float64
	HasCacheRd bool
	CacheWrite float64
	HasCacheWr bool
	Peak       *Price
}

// PriceSource resolves the rate for a model on a provider.
// src/pricing.ts priceFor. Injected because the Go price table is deferred;
// when nil every model is unpriced and deriveRoutings' cheap/expensive
// ranking degenerates to the unpriced tier, matching TS with an empty table.
type PriceSource func(model, provider string) *Price

// Usage is token accounting for one call. src/pricing.ts Usage.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// CostOf prices usage at the given rates. src/pricing.ts costOf.
func CostOf(price *Price, usage Usage, at time.Time) (usd float64, known bool) {
	if price == nil {
		return 0, false
	}
	rates := price
	if price.PeakRule == "deepseek" && IsDeepSeekPeak(at) && price.Peak != nil {
		rates = price.Peak
	}
	perMillion := float64(usage.Input)*rates.Input +
		float64(usage.Output)*rates.Output +
		float64(usage.CacheRead)*rates.CacheRead +
		float64(usage.CacheWrite)*rates.CacheWrite
	return perMillion / 1_000_000, true
}

// IsDeepSeekPeak reports the DeepSeek off-peak inverse window.
// src/pricing.ts isDeepSeekPeak.
func IsDeepSeekPeak(at time.Time) bool {
	at = at.UTC()
	day := at.Weekday()
	if day == time.Sunday || day == time.Saturday {
		return false
	}
	hour := at.Hour()
	return (hour >= 1 && hour < 4) || (hour >= 6 && hour < 10)
}

// ---- sessions (src/routing.ts SessionStore) ----

// CacheObservation is what one answered request told us about the vendor's
// prompt cache. src/routing.ts CacheObservation.
type CacheObservation struct {
	Provider            string
	Model               string
	At                  int64 // epoch ms
	UncachedInputTokens int
	CacheReadTokens     int
	CacheWriteTokens    int
	// A successful request is required: failed usage cannot establish a
	// reusable prefix.
	Success bool
	// UsageKnown distinguishes an explicit zero cache count from missing usage.
	UsageKnown  bool
	InputTokens int
	// Scope separates provider configuration and client protocol generations.
	Scope  string
	Prefix CachePrefix
}

// SessionState is what one conversation remembers between turns.
// src/routing.ts SessionState.
type SessionState struct {
	Phase     string
	Model     string
	Provider  string
	Turns     int
	UpdatedAt int64 // epoch ms
	Cache     *CacheObservation
	// Caches retains the latest successful observation for each serving target.
	Caches map[string]CacheObservation
}

// SessionStore keeps per-session routing memory with a TTL.
// src/routing.ts SessionStore. Safe for concurrent use.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]SessionState
	ttlMs    int64
}

// NewSessionStore returns a store with ttl in milliseconds.
func NewSessionStore(ttlMs int64) *SessionStore {
	return &SessionStore{sessions: map[string]SessionState{}, ttlMs: ttlMs}
}

// Get returns the session state, expiring it past the TTL.
func (s *SessionStore) Get(key string, now int64) (SessionState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sessions[key]
	if !ok {
		return SessionState{}, false
	}
	if now-state.UpdatedAt > s.ttlMs {
		delete(s.sessions, key)
		return SessionState{}, false
	}
	return cloneSessionState(state), true
}

// Set records the session state.
func (s *SessionStore) Set(key string, state SessionState) {
	state = cloneSessionState(state)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Merge history under the lock so an in-flight completion is not lost.
	if old, ok := s.sessions[key]; ok && (s.ttlMs <= 0 || state.UpdatedAt-old.UpdatedAt <= s.ttlMs) {
		if state.Caches == nil {
			state.Caches = map[string]CacheObservation{}
		}
		for target, observation := range old.Caches {
			if current, exists := state.Caches[target]; !exists || current.At < observation.At {
				state.Caches[target] = cloneCacheObservation(observation)
			}
		}
		if old.Cache != nil && (state.Cache == nil || state.Cache.At < old.Cache.At) {
			copied := cloneCacheObservation(*old.Cache)
			state.Cache = &copied
		}
		if state.Cache != nil {
			if current, ok := state.Caches[PlanKey(state.Cache.Provider, state.Cache.Model)]; ok && current.At > state.Cache.At {
				copied := cloneCacheObservation(current)
				state.Cache = &copied
			}
		}
		if state.Provider != "" && state.Model != "" {
			if current, ok := state.Caches[PlanKey(state.Provider, state.Model)]; ok && (state.Cache == nil || state.Cache.Provider != state.Provider || state.Cache.Model != state.Model || state.Cache.At < current.At) {
				copied := cloneCacheObservation(current)
				state.Cache = &copied
			}
		}
	}
	s.sessions[key] = cloneSessionState(state)
}

// Retarget re-points a session at the target a failover moved the turn to.
// src/routing.ts SessionStore.retarget.
func (s *SessionStore) Retarget(key string, provider, model string, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sessions[key]
	if !ok || now-state.UpdatedAt > s.ttlMs {
		if ok {
			delete(s.sessions, key)
		}
		return
	}
	state.Provider = provider
	state.Model = model
	state.UpdatedAt = now
	s.sessions[key] = state
}

// ObserveCache retains successful usage by serving target. A delayed
// completion updates only that target's history, not the active target.
// src/routing.ts SessionStore.observeCache.
func (s *SessionStore) ObserveCache(key string, obs CacheObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sessions[key]
	if !ok || !obs.Success {
		return
	}
	if obs.At-state.UpdatedAt > s.ttlMs || obs.Provider == "" || obs.Model == "" {
		return
	}
	if !obs.UsageKnown && obs.InputTokens == 0 && obs.UncachedInputTokens == 0 && obs.CacheReadTokens == 0 && obs.CacheWriteTokens == 0 {
		// No token usage is different from a reported cache miss. A missing-
		// usage snapshot refreshes metadata of the target's existing evidence
		// without overwriting its token counts; with no evidence yet, the
		// observation is still recorded so the target reads as "warm".
		if old, exists := state.Caches[PlanKey(obs.Provider, obs.Model)]; exists {
			if obs.At >= old.At {
				old.At, old.Prefix, old.Scope = obs.At, obs.Prefix, obs.Scope
				state.Caches[PlanKey(obs.Provider, obs.Model)] = old
				if state.Provider == obs.Provider && state.Model == obs.Model && state.Cache != nil {
					c := cloneCacheObservation(old)
					state.Cache = &c
				}
				s.sessions[key] = cloneSessionState(state)
			}
			return
		}
	}
	if state.Caches == nil {
		state.Caches = map[string]CacheObservation{}
	}
	target := PlanKey(obs.Provider, obs.Model)
	if old, exists := state.Caches[target]; exists && old.At > obs.At {
		return
	}
	state.Caches[target] = cloneCacheObservation(obs)
	// Bound memory even when a conversation visits many targets.
	if len(state.Caches) > 32 {
		oldest := ""
		var at int64
		for key, observation := range state.Caches {
			if key != target && (oldest == "" || observation.At < at) {
				oldest, at = key, observation.At
			}
		}
		delete(state.Caches, oldest)
	}
	if state.Provider == obs.Provider && state.Model == obs.Model && (state.Cache == nil || state.Cache.At <= obs.At) {
		o := cloneCacheObservation(state.Caches[target])
		state.Cache = &o
	}
	s.sessions[key] = cloneSessionState(state)
}

func cloneCacheObservation(c CacheObservation) CacheObservation {
	c.Prefix.MessageHash = append([]string(nil), c.Prefix.MessageHash...)
	return c
}

func cloneSessionState(state SessionState) SessionState {
	if state.Cache != nil {
		c := cloneCacheObservation(*state.Cache)
		state.Cache = &c
	}
	if state.Caches != nil {
		copied := make(map[string]CacheObservation, len(state.Caches))
		for key, observation := range state.Caches {
			copied[key] = cloneCacheObservation(observation)
		}
		state.Caches = copied
	}
	return state
}

// Size reports live session count (diagnostics).
func (s *SessionStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// SessionFingerprint hashes the stable conversation head when no explicit
// session id exists. src/session.ts sessionFingerprint.
func SessionFingerprint(system, tools, messages any) string {
	var firstUser any
	if arr, ok := messages.([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok && m["role"] == "user" {
				firstUser = item
				break
			}
		}
	}
	// JS JSON.stringify emits system, tools, firstUser in that order and
	// never HTML-escapes; json.Marshal on a map would sort keys and escape
	// <, >, &. Nested object key order still follows Go's sorted order (the
	// decoded body has no insertion order), so hashes match TS only for
	// alphabetically ordered payloads — the fingerprint is process-local.
	payload := orderedJSON([]kv{{"system", system}, {"tools", tools}, {"firstUser", firstUser}})
	sum := sha1.Sum([]byte(payload))
	return hex.EncodeToString(sum[:])[:16]
}

// ---- decisions ----

// BrainSource names who decided. src/routing.ts BrainSource.
type BrainSource string

const (
	BrainJev              BrainSource = "jev"
	BrainJevLowConfidence BrainSource = "jev-low-confidence"
	BrainHeuristic        BrainSource = "heuristic"
)

// WeighedCandidate is one candidate as routing weighed it — the failover plan
// entry. src/trace.ts WeighedCandidate.
type WeighedCandidate struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Canonical string `json:"canonical,omitempty"`
	// Rank 0 is the candidate routing put first.
	Rank       int       `json:"rank"`
	Quota      string    `json:"quota,omitempty"`
	CacheState string    `json:"cacheState,omitempty"`
	Skipped    *SkipNote `json:"skipped,omitempty"`
}

// SkipNote records why a candidate was withheld.
type SkipNote struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// RouteSkip is a model withheld from the brain's choice, with the reason.
// src/routing.ts RouteSkip.
type RouteSkip struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	// Reason is "context" or "effort".
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// CacheAffinityState is the measured warmth of the candidate's vendor cache.
// src/routing.ts CacheAffinityState.
type CacheAffinityState string

const (
	CacheHot     CacheAffinityState = "hot"
	CacheWarm    CacheAffinityState = "warm"
	CacheStale   CacheAffinityState = "stale"
	CacheUnknown CacheAffinityState = "unknown"
)

// CacheAffinity is the cache evidence for one candidate.
// src/routing.ts CacheAffinity.
type CacheAffinity struct {
	State                  CacheAffinityState `json:"state"`
	PrefixMatch            string             `json:"prefixMatch"` // "extends" | "changed" | "unknown"
	ObservedHitRatio       float64            `json:"observedHitRatio"`
	ExpectedReadTokens     int                `json:"expectedReadTokens"`
	ExpectedUncachedTokens int                `json:"expectedUncachedTokens"`
	EffectiveInputCostUSD  *float64           `json:"effectiveInputCostUsd"`
	CostKnown              bool               `json:"costKnown"`
	Confidence             float64            `json:"confidence"`
}

// CacheKeepReason is why a conversation was kept where it was, or not.
// src/routing.ts CacheKeepReason.
type CacheKeepReason string

const (
	KeepOff     CacheKeepReason = "off"
	KeepFirst   CacheKeepReason = "first"
	KeepGone    CacheKeepReason = "gone"
	KeepSession CacheKeepReason = "session"
	KeepTurn    CacheKeepReason = "turn"
	KeepCache   CacheKeepReason = "cache"
	KeepNewTurn CacheKeepReason = "new-turn"
	KeepNoCache CacheKeepReason = "no-cache"
	KeepCold    CacheKeepReason = "cold"
)

// Decision is the routed target for one turn. src/routing.ts RouteDecision.
type Decision struct {
	Model          string
	Provider       string
	Phase          string
	RequestedModel string
	// RequestID joins ledger row, body capture, and trace.
	RequestID string
	Canonical string
	Virtual   bool
	Routed    bool
	// Reason is the x-jevonian-reason string (see Reason* builders).
	Reason        string
	Session       string
	Brain         BrainSource
	BrainChannel  string
	Confidence    float64
	HasConfidence bool
	// Effort is the thinking level to send upstream, already clamped.
	Effort string
	// EffortNote is why the level was reduced from what was asked.
	EffortNote string
	Skipped    []RouteSkip
	// ContextOverflow means no candidate's window could hold the
	// conversation; the caller should compact and route again.
	ContextOverflow bool
	Cache           *CacheAffinity
	// SwitchPenaltyUSD is the difference from the previous turn's model's
	// effective input cost; positive means switching costs more.
	SwitchPenaltyUSD *float64
	CacheKeep        CacheKeepReason
	// Order is the full ranked failover plan, chosen target at rank 0 (kept
	// for trace parity with TS). Failover walks it via NextFromPlan, whose
	// caller seeds `exclude` with the tried target — so rank 0 is skipped the
	// same way TS nextFromPlan skips it. src/routing.ts RouteDecision.order.
	Order []WeighedCandidate
}

// NextFromPlan returns the next failover target in the decision's plan, or
// nil. src/routing.ts nextFromPlan.
func (d Decision) NextFromPlan(exclude map[string]bool) *Decision {
	for _, candidate := range d.Order {
		key := PlanKey(candidate.Provider, candidate.Model)
		if exclude[key] {
			continue
		}
		next := d
		next.Model = candidate.Model
		next.Provider = candidate.Provider
		next.Canonical = candidate.Canonical
		remaining := []WeighedCandidate{}
		for _, c := range d.Order {
			k := PlanKey(c.Provider, c.Model)
			if exclude[k] || k == key {
				continue
			}
			remaining = append(remaining, c)
		}
		next.Order = remaining
		return &next
	}
	return nil
}

// PlanKey identifies a failover target inside a decision's order.
// src/routing.ts planKey.
func PlanKey(provider, model string) string {
	return provider + "/" + model
}

// RouteError is Decide's failure shape: a message and the HTTP status the
// server should return. src/routing.ts `{ error, status? }`.
type RouteError struct {
	Message string
	Status  int
}

func (e *RouteError) Error() string { return e.Message }

// ---- the seam to internal/brain ----

// Choice is the scorer's verdict for one routing decision.
// src/brain.ts BrainVerdict, narrowed to what routing reads.
type Choice struct {
	// Model is the chosen routing id (or "none_of_the_above").
	Model               string
	Confidence          float64
	Probabilities       map[string]float64
	Effort              string
	EffortProbabilities map[string]float64
	// ModelName is the decision model's own id (ledger model field).
	ModelName string
	Usage     *Usage
}

// Failure is why a scoring call produced no verdict.
// src/brain.ts AskJevFailure.
type Failure struct {
	// Status is the HTTP status when there was a response (402 marks a
	// linked provider spent; 402/403 skip the channel for the turn).
	Status int
	Error  string
}

// AskResult is either a choice or a failure. src/brain.ts askJevOutcome.
type AskResult struct {
	Choice  *Choice
	Failure *Failure
}

// Scorer is the seam internal/brain implements: one call asks one configured
// brain channel to score the prepared state. Returning a Failure never aborts
// Decide — the router walks the next channel and, when every channel fails,
// falls back to the heuristic. src/routing.ts the askJevOutcome call site.
type Scorer interface {
	// Score asks one brain channel for a verdict on state. modelOnly tells
	// the brain not to answer the effort question (brainPicksEffort off).
	Score(ctx context.Context, brain config.BrainConfig, state map[string]any, modelOnly bool) AskResult
}

// ScorerFunc adapts a function to Scorer.
type ScorerFunc func(ctx context.Context, brain config.BrainConfig, state map[string]any, modelOnly bool) AskResult

// Score calls f.
func (f ScorerFunc) Score(ctx context.Context, brain config.BrainConfig, state map[string]any, modelOnly bool) AskResult {
	return f(ctx, brain, state, modelOnly)
}

// QuotaView is the standing routing reads for one provider/model.
// src/routing.ts the quota.ts reads it merges in standingOf.
type QuotaView struct {
	// Status is the provider's quota health verdict.
	Status quota.Status
	// UsedPercent is the fullest account window's used share, 0-100.
	UsedPercent float64
	// Renews lists when the account windows refill, longest window first,
	// epoch ms; 0 when not stated.
	Renews []int64
	// ModelExhausted means this exact model's window/cooldown is spent.
	ModelExhausted bool
}

// QuotaSource answers quota standing for one provider/model.
// src/routing.ts standingOf's inputs (providerQuotaHealth, accountWindows,
// providerModelExhausted). Production wires internal/quota.Tracker in.
type QuotaSource interface {
	Standing(provider config.Provider, model string, now int64, lowPercent float64) QuotaView
}

// QuotaSourceFunc adapts a function to QuotaSource.
type QuotaSourceFunc func(provider config.Provider, model string, now int64, lowPercent float64) QuotaView

// Standing calls f.
func (f QuotaSourceFunc) Standing(provider config.Provider, model string, now int64, lowPercent float64) QuotaView {
	return f(provider, model, now, lowPercent)
}

// TrackerStanding adapts internal/quota.Tracker to QuotaSource. The Go
// tracker has no account-window reset data, so Renews stays empty (reset-aware
// ordering degenerates to config order within buckets — noted in the report).
func TrackerStanding(t *quota.Tracker) QuotaSource {
	return QuotaSourceFunc(func(provider config.Provider, model string, now int64, lowPercent float64) QuotaView {
		opts := quota.HealthOptions{LowPercent: &lowPercent}
		health := t.ProviderHealth(provider, opts)
		return QuotaView{
			Status:         health.Status,
			UsedPercent:    health.UsedPercent,
			ModelExhausted: t.ProviderModelExhausted(provider, model, opts),
		}
	})
}

// GuardView is the provider-guard read: breaker open or in-flight saturated
// counts a provider as spent for ordering. src/routing.ts standingOf's
// provider-guard half. Injected; nil means no guard.
type GuardView interface {
	// Spent reports whether the provider cannot accept a turn right now.
	Spent(provider string, now int64) bool
}

// GuardViewFunc adapts a function to GuardView.
type GuardViewFunc func(provider string, now int64) bool

// Spent calls f.
func (f GuardViewFunc) Spent(provider string, now int64) bool { return f(provider, now) }

// ProviderLookup maps a benchmark model id to its leaderboard view.
// src/leaderboard.ts leaderboardViewFor. Injected; nil omits benchmark fields.
type ProviderLookup func(model string) map[string]any

// Deps are the dependencies Decide needs beyond the request itself.
type Deps struct {
	// Scorer asks brain channels; required when routing.mode is auto and
	// brains are configured. nil means every brain call fails (heuristic).
	Scorer Scorer
	// Quota answers provider standing; nil means "unknown" (never blocks).
	Quota QuotaSource
	// Guard reports breaker/saturation; nil means providers are never
	// guard-spent.
	Guard GuardView
	// Capabilities resolves stated model limits; nil means all capabilities
	// are unknown (context/effort filters never withhold).
	Capabilities CapabilitySource
	// Prices resolves rate cards for deriveRoutings ordering and cache cost
	// math; nil means unpriced.
	Prices PriceSource
	// Identity resolves catalog same-model keys; nil disables
	// identity-widening and official-provider preference.
	Identity IdentityIndex
	// LeaderboardView returns a model's benchmark view for the brain state;
	// nil omits benchmark soft evidence entirely.
	LeaderboardView ProviderLookup
	// BrainBreakerOpen reports the cross-channel brain breaker; nil means
	// closed. When open, Decide skips the scorer round.
	BrainBreakerOpen func() bool
	// RecordBrainOutcome feeds the round result back to the breaker; nil
	// means no breaker bookkeeping.
	RecordBrainOutcome func(ok bool)
	// RecordBrainCall logs one channel call (ledger "brain" row + body).
	// nil means no per-call reporting.
	RecordBrainCall func(BrainCallRecord)
	// CaptureUsageLimit marks a linked provider spent on a brain 402.
	// src/routing.ts the captureUsageLimit call.
	CaptureUsageLimit func(provider config.Provider, status int, errText string)
	// Now returns epoch ms; nil means wall clock.
	Now func() int64
	// CacheTTL is the vendor cache TTL estimate; 0 uses the conservative
	// default. src/routing.ts RouteInput.cacheTtlMs.
	CacheTTL time.Duration
	// CacheEvidence supplies conservative prepared-body prefix evidence for a target.
	// It does not prove the vendor has retained a cache entry.
	CacheEvidence func(provider, model string) (string, CachePrefix)
	// Sleep backs off between brain rounds; nil means a real sleep.
	Sleep func(time.Duration)
}

// BrainCallRecord is the ledger/body payload for one brain channel call.
// src/routing.ts recordBrainCall's inputs, minus the reporting internals.
type BrainCallRecord struct {
	Brain     config.BrainConfig
	Session   string
	RequestID string
	KeyID     string
	KeyName   string
	Started   int64 // epoch ms
	State     map[string]any
	Verdict   *Choice
	Failure   *Failure
	LatencyMs int64
}

// Input is one routing request. src/routing.ts RouteInput.
type Input struct {
	Config *config.Config
	// Body is the decoded request body (OpenAI chat / Anthropic messages /
	// Responses shapes).
	Body map[string]any
	// Headers is the request's header map, keys already lower-cased.
	Headers   map[string]string
	Store     *SessionStore
	Kind      RequestKind
	RequestID string
	KeyID     string
	KeyName   string
	// Now overrides Deps.Now for this call (epoch ms). 0 uses Deps.Now.
	Now int64
	// CacheTTL overrides Deps.CacheTTL for this call.
	CacheTTL time.Duration
}

const defaultCacheTTLMs int64 = 5 * 60_000

// CacheWorthTokens is the vendor cache read that makes staying worthwhile.
// src/routing.ts CACHE_WORTH_TOKENS.
const CacheWorthTokens = 1024

// Decide routes one turn. src/routing.ts decideRoute.
func Decide(ctx context.Context, deps Deps, input Input) (*Decision, error) {
	cfg := input.Config
	if cfg == nil {
		return nil, &RouteError{Message: "routing: config is nil", Status: 500}
	}
	now := input.Now
	if now == 0 {
		if deps.Now != nil {
			now = deps.Now()
		} else {
			now = time.Now().UnixMilli()
		}
	}
	requestedRaw, _ := input.Body["model"].(string)
	requestedModel := strings.TrimPrefix(requestedRaw, "jevonian/")
	requestID := input.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	virtual := IsVirtualModel(requestedModel, cfg)
	session := ResolveSessionKey(input.Body, input.Headers)
	guard := cfg.Routing.QuotaGuard
	standCache := map[string]quotaStanding{}
	resetOrder := func(picks []TierPick) []TierPick {
		if guard.Enabled && guard.ResetAware {
			return OrderByQuotaReset(picks, cfg, deps, orderOptions{now: now, cache: standCache})
		}
		return picks
	}

	if !virtual {
		previous, hadPrevious := SessionState{}, false
		if input.Store != nil {
			previous, hadPrevious = input.Store.Get(session, now)
		}
		d, err := decidePinned(deps, input, cfg, requestedRaw, requestedModel, requestID, session, now, resetOrder, standCache)
		if err == nil && input.Store != nil {
			state := SessionState{}
			if hadPrevious {
				state = previous
			}
			state.Provider, state.Model, state.Phase = d.Provider, d.Model, d.Phase
			state.UpdatedAt = now
			state.Turns++
			input.Store.Set(session, state)
		}
		return d, err
	}
	if cfg.Routing.Mode == "off" {
		return nil, &RouteError{
			Message: fmt.Sprintf("Model %q is a virtual model, but routing is off. Configure routing.mode = \"auto\" or request a real model.", requestedRaw),
		}
	}
	return decideVirtual(ctx, deps, input, cfg, requestedRaw, requestedModel, requestID, session, now, resetOrder, standCache)
}

// routingIDRe is the slug shape for routing ids. src/config.ts isRoutingId.
var routingIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// StateForBrain is the state one brain receives: the routing state plus the
// transcript when that brain asked for the full prompt. Compact-state
// channels (Kev) drop benchmark tables, the flat candidates mirror, and
// tool-result blobs. The input map is never mutated.
// src/routing.ts brainStateFor.
func StateForBrain(brain config.BrainConfig, ready map[string]any, transcript string) map[string]any {
	state := make(map[string]any, len(ready)+1)
	for k, v := range ready {
		state[k] = v
	}
	if brain.FullPrompt && transcript != "" {
		state["transcript"] = transcript
	}
	if !compactStateChannels[brain.Channel] {
		return state
	}
	delete(state, "candidates")
	delete(state, "recent_tool_results")
	delete(state, "benchmark_focus")
	delete(state, "benchmarks_coverage")
	if routings, ok := state["routings"].([]any); ok {
		trimmed := make([]any, 0, len(routings))
		for _, raw := range routings {
			routing, ok := raw.(map[string]any)
			if !ok {
				trimmed = append(trimmed, raw)
				continue
			}
			copyR := make(map[string]any, len(routing))
			for k, v := range routing {
				if k != "benchmark_focus" {
					copyR[k] = v
				}
			}
			if models, ok := routing["models"].([]any); ok {
				out := make([]any, 0, len(models))
				for _, rawM := range models {
					m, ok := rawM.(map[string]any)
					if !ok {
						out = append(out, rawM)
						continue
					}
					copyM := make(map[string]any, len(m))
					for k, v := range m {
						if k != "benchmarks" {
							copyM[k] = v
						}
					}
					out = append(out, copyM)
				}
				copyR["models"] = out
			}
			trimmed = append(trimmed, copyR)
		}
		state["routings"] = trimmed
	}
	return state
}

// compactStateChannels mirrors brain.Channel.CompactState without importing
// internal/brain (routing must not depend on the brain client).
// src/brain.ts JEV_CHANNELS compactState.
var compactStateChannels = map[string]bool{"kev": true}
