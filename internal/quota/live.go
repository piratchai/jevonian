package quota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

const LiveProbeTimeout = 4 * time.Second
const HeaderSnapshotTTL = time.Minute

// Balance is the remaining pay-as-you-go credit reported by the vendor.
type Balance struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// ProviderQuota mirrors src/quota.ts and the dashboard's ProviderQuota payload.
type ProviderQuota struct {
	Provider  string                 `json:"provider"`
	Billing   config.ProviderBilling `json:"billing"`
	Auth      string                 `json:"auth"`
	Source    string                 `json:"source"`
	Plan      string                 `json:"plan,omitempty"`
	Note      string                 `json:"note,omitempty"`
	Balance   *Balance               `json:"balance,omitempty"`
	Windows   []Window               `json:"windows"`
	Spend     Spend                  `json:"spend"`
	FetchedAt string                 `json:"fetchedAt"`
	Error     string                 `json:"error,omitempty"`
}

// LiveOptions injects every remote/credential dependency. Configure before use.
type LiveOptions struct {
	HTTP  *http.Client
	OAuth *oauth.Resolver
	// ResolveAuth overrides credential reads/refreshes, including WorkBuddy and Codex.
	ResolveAuth func(context.Context, config.Provider) (oauth.AuthResolution, error)
	// AuthSource is a non-network credential presence label. Optional; defaults to
	// inline/credentials/env:/oauth: labels using OAuth.HasCredential.
	AuthSource func(config.Provider) string
	// BackgroundContext owns asynchronous refreshes. Use the server lifecycle
	// context, NOT context.Background(), so shutdown cancels probes. If nil, the
	// caller's context owns refreshes (an HTTP request will cancel on return).
	BackgroundContext  context.Context
	Timeout            time.Duration
	ProviderTimeout    func(config.Provider) time.Duration
	Concurrency        int
	CodexUsageURL      string
	CommandCodeBaseURL string
}

type liveEntry struct {
	quota    ProviderQuota
	at       time.Time
	revision uint64
}
type liveCall struct{ done chan struct{} }

// Service performs live probes and assembles snapshot-first dashboard reads.
// Tracker remains the sole owner of routing health, marks and persisted windows.
// Cache/single-flight locks are never held across auth resolution or network IO.
type Service struct {
	tracker *Tracker
	opts    LiveOptions
	auth    *oauth.Resolver
	slots   chan struct{}
	mu      sync.Mutex
	cache   map[string]liveEntry
	pending map[string]*liveCall
}

func NewService(tracker *Tracker, opts LiveOptions) *Service {
	if tracker == nil {
		tracker = New(nil)
	}
	n := opts.Concurrency
	if n <= 0 {
		n = 4
	}
	auth := opts.OAuth
	if auth == nil {
		auth = &oauth.Resolver{HTTP: opts.HTTP}
	}
	return &Service{tracker: tracker, opts: opts, auth: auth, slots: make(chan struct{}, n), cache: map[string]liveEntry{}, pending: map[string]*liveCall{}}
}

// Quotas has the exact admin.Deps.Quotas signature. refresh=false serves cached
// snapshots immediately and schedules context-bound work. refresh=true always
// probes, even inside the normal TTL, and waits (CLI quota --refresh).
// Vendor errors live on individual rows; cancellation/ledger errors are returned.
// A missing-credential probe reports its sign-in message through `error`, which
// the CLI prints as a note; it is never replaced by "no quota source".
// src/quota.ts buildQuota -> storedQuota(provider, spend, liveError).
func (s *Service) Quotas(ctx context.Context, cfg *config.Config, refresh bool) ([]map[string]any, error) {
	quotas, err := s.ProviderQuotas(ctx, cfg, refresh)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(quotas))
	for _, q := range quotas {
		raw, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		var row map[string]any
		if err = json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// ProviderQuotas is the typed CLI-facing counterpart of Quotas.
func (s *Service) ProviderQuotas(ctx context.Context, cfg *config.Config, refresh bool) ([]ProviderQuota, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("quota: nil config")
	}
	providers := make([]config.Provider, len(cfg.Providers))
	for i, p := range cfg.Providers {
		providers[i] = copyProvider(p)
	}
	spends := make([]Spend, len(providers))
	rows := make([]ProviderQuota, len(providers))
	type job struct {
		p    config.Provider
		key  string
		call *liveCall
	}
	jobs := make([]job, 0, len(providers))
	waiting := make([]*liveCall, 0, len(providers))
	now := s.tracker.now()
	for i, p := range providers {
		spend, err := s.tracker.spendOf(p.Name, now)
		if err != nil {
			return nil, err
		}
		spends[i] = spend
		key := providerKey(p)
		revision := s.tracker.revision(p.Name)
		s.mu.Lock()
		entry, ok := s.cache[key]
		s.mu.Unlock()
		fresh := ok && entry.revision == revision && now.Sub(entry.at) < liveTTL(p)
		if fresh {
			rows[i] = cloneQuota(entry.quota)
			rows[i].Windows = s.tracker.withModelRejections(p.Name, rows[i].Windows)
		} else {
			rows[i] = s.storedQuota(p, spend, "")
		}
		if probeKind(p) == "" || (!refresh && fresh) {
			continue
		}
		s.mu.Lock()
		call, exists := s.pending[key]
		if !exists {
			call = &liveCall{done: make(chan struct{})}
			s.pending[key] = call
			jobs = append(jobs, job{p, key, call})
		}
		s.mu.Unlock()
		waiting = append(waiting, call)
	}
	workCtx := ctx
	if !refresh && s.opts.BackgroundContext != nil {
		workCtx = s.opts.BackgroundContext
	}
	// Bounded workers and a service-wide semaphore bound probes even across
	// simultaneous reads with disjoint provider sets.
	if len(jobs) > 0 {
		go func() {
			queue := make(chan job)
			var wg sync.WaitGroup
			for i := 0; i < min(len(jobs), cap(s.slots)); i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := range queue {
						s.refresh(workCtx, j.p, j.key)
						s.mu.Lock()
						delete(s.pending, j.key)
						close(j.call.done)
						s.mu.Unlock()
					}
				}()
			}
			for _, j := range jobs {
				queue <- j
			}
			close(queue)
			wg.Wait()
		}()
	}
	if refresh {
		for _, call := range waiting {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-call.done:
			}
		}
		for i, p := range providers {
			s.mu.Lock()
			entry, ok := s.cache[providerKey(p)]
			s.mu.Unlock()
			if ok && entry.revision == s.tracker.revision(p.Name) {
				rows[i] = cloneQuota(entry.quota)
				rows[i].Windows = s.tracker.withModelRejections(p.Name, rows[i].Windows)
			} else {
				rows[i] = s.storedQuota(p, spends[i], "")
			}
		}
	}
	for i := range rows {
		rows[i].Spend = spends[i]
	}
	return rows, nil
}

func (s *Service) refresh(ctx context.Context, p config.Provider, key string) {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.slots }()
	timeout := s.opts.Timeout
	if s.opts.ProviderTimeout != nil {
		timeout = s.opts.ProviderTimeout(p)
	}
	if timeout <= 0 {
		timeout = LiveProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	revision := s.tracker.revision(p.Name)
	live, err := s.fetchLive(probeCtx, p)
	if ctx.Err() != nil {
		return
	} // Cancelled lifecycles must not overwrite snapshots.
	if probeCtx.Err() != nil {
		err = fmt.Errorf("%s usage probe timed out", p.Name)
	}
	now := s.tracker.now()
	spend, spendErr := s.tracker.spendOf(p.Name, now)
	if spendErr != nil {
		return
	}
	q := s.storedQuota(p, spend, "")
	if err != nil {
		q.Error = err.Error()
	} else if len(live.Windows) > 0 || live.Balance != nil {
		var committed bool
		live.Windows, revision, committed = s.tracker.applyLive(p.Name, revision, live, now, liveTTL(p))
		if !committed {
			return
		}
		q = s.baseQuota(p, now)
		q.Source = "live"
		q.Windows = live.Windows
		q.Plan = live.Plan
		q.Note = live.Note
		q.Balance = live.Balance
	}
	// A newer refusal/header observation wins, including one arriving while the
	// failed probe was assembling its fallback.
	if revision != s.tracker.revision(p.Name) {
		return
	}
	s.mu.Lock()
	s.cache[key] = liveEntry{quota: cloneQuota(q), at: now, revision: revision}
	s.mu.Unlock()
}

func (s *Service) baseQuota(p config.Provider, now time.Time) ProviderQuota {
	source := s.authSource(p)
	return ProviderQuota{Provider: p.Name, Billing: p.Billing, Auth: source, Source: "none", Windows: []Window{}, FetchedAt: now.UTC().Format(isoMillis)}
}
func (s *Service) storedQuota(p config.Provider, spend Spend, liveError string) ProviderQuota {
	now := s.tracker.now()
	q := s.baseQuota(p, now)
	q.Error = liveError
	entry, ok := s.tracker.headerEntry(p.Name)
	if ok && len(entry.Windows) > 0 {
		for _, w := range entry.Windows {
			if activeWindow(w, now) {
				q.Windows = append(q.Windows, w)
			}
		}
		hasAccount := false
		for _, w := range q.Windows {
			if w.Model == "" {
				hasAccount = true
			}
		}
		if !hasAccount {
			estimated := specWindows(p.Quota, spend)
			if len(estimated) > 0 {
				q.Windows = append(estimated, q.Windows...)
				q.Source = "ledger"
			}
		}
		if q.Source != "ledger" {
			q.Source = "headers"
			q.Note = stalenessNote(entry.FetchedAt, now)
		}
		q.FetchedAt = entry.FetchedAt
		q.Plan = entry.Plan
		return q
	}
	if windows := specWindows(p.Quota, spend); len(windows) > 0 {
		q.Source = "ledger"
		q.Windows = windows
	}
	return q
}
func (s *Service) authSource(p config.Provider) string {
	if s.opts.AuthSource != nil {
		return s.opts.AuthSource(p)
	}
	if p.Auth == config.AuthOAuth && p.OAuthSource != "" && p.OAuthSource != config.OAuthStatic {
		if s.auth.HasCredential(string(p.OAuthSource), p.Login) {
			return "oauth:" + string(p.OAuthSource)
		}
		return "none"
	}
	if p.APIKey != "" {
		return "inline"
	}
	store := s.auth.Credentials
	if store == nil {
		store = multiacct.DefaultStore()
	}
	if store.Get(p.Name) != "" {
		if p.Auth == config.AuthOAuth {
			return "oauth:static"
		}
		return "credentials"
	}
	if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "" {
		return "env:" + p.APIKeyEnv
	}
	return "none"
}
func (s *Service) resolveAuth(ctx context.Context, p config.Provider) (oauth.AuthResolution, error) {
	if s.opts.ResolveAuth != nil {
		return s.opts.ResolveAuth(ctx, p)
	}
	if p.OAuthSource == config.OAuthCodex {
		p.Type = config.ProviderTypeResponses
	}
	return s.auth.ResolveProviderAuth(ctx, p, oauth.WireKindOpenAI, "")
}
func liveTTL(p config.Provider) time.Duration {
	if p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthClaudeCode {
		return 5 * time.Minute
	}
	return time.Minute
}
func providerKey(p config.Provider) string {
	raw, _ := json.Marshal(p)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func copyProvider(p config.Provider) config.Provider {
	raw, _ := json.Marshal(p)
	var out config.Provider
	_ = json.Unmarshal(raw, &out)
	return out
}
func cloneQuota(q ProviderQuota) ProviderQuota {
	q.Windows = cloneWindows(q.Windows)
	if q.Balance != nil {
		b := *q.Balance
		q.Balance = &b
	}
	return q
}
func cloneWindows(windows []Window) []Window {
	out := make([]Window, len(windows))
	for i, w := range windows {
		out[i] = w
		if w.UsedUSD != nil {
			v := *w.UsedUSD
			out[i].UsedUSD = &v
		}
		if w.LimitUSD != nil {
			v := *w.LimitUSD
			out[i].LimitUSD = &v
		}
	}
	return out
}
func stalenessNote(fetched string, now time.Time) string {
	at, ok := parseISO(fetched)
	if !ok {
		return "snapshot timestamp is unreadable"
	}
	age := now.Sub(at)
	if age < HeaderSnapshotTTL {
		return ""
	}
	minutes := int(age / time.Minute)
	label := fmt.Sprintf("%dm", minutes)
	if minutes >= 1440 {
		label = fmt.Sprintf("%dd", minutes/1440)
	} else if minutes >= 60 {
		label = fmt.Sprintf("%dh", minutes/60)
	}
	return "measured " + label + " ago"
}

// MarshalJSON makes Window usable in both live payloads and quota.json.
func (w Window) MarshalJSON() ([]byte, error) { return json.Marshal(windowToJSON(w)) }
func (w *Window) UnmarshalJSON(raw []byte) error {
	var j windowJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return err
	}
	*w = windowFromJSON(j)
	return nil
}
