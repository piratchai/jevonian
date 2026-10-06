// Package quota answers provider spend health from ledger windows, config
// caps, upstream rate-limit header snapshots (quota.json), and an in-memory
// cooldown for quota/billing/rate-limit refusals.
//
// Host failures (transport errors, timeouts, 5xx) do NOT belong here; they
// trip internal/guard. Conflating the two makes the dashboard report a flaky
// host as "exhausted".
//
// Essential surface of src/quota.ts for routing and upstream failover.
// Live vendor probes are exposed through Service.
package quota

import (
	"math"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/ledger"
)

// Status is a provider's quota verdict for the guard.
type Status string

const (
	StatusOK        Status = "ok"
	StatusLow       Status = "low"
	StatusExhausted Status = "exhausted"
	StatusUnknown   Status = "unknown"
)

// DefaultLowPercent matches src/quota.ts DEFAULT_LOW_PERCENT.
const DefaultLowPercent float64 = 10

// SufficientRequestMultiple matches src/quota.ts SUFFICIENT_REQUEST_MULTIPLE.
const SufficientRequestMultiple float64 = 3

// ProviderCooldown is how long an unmarked rate-limit refusal benches a provider.
// Matches src/quota.ts PROVIDER_COOLDOWN_MS.
const ProviderCooldown = 60 * time.Second

// Health is the routing-facing quota verdict for one provider.
type Health struct {
	Provider         string
	Status           Status
	UsedPercent      float64
	RemainingPercent float64
	Window           string
	ResetsAt         string
	RemainingUSD     *float64
	AvgRequestUSD    *float64
	Note             string
}

// Spend is rolling-window provider spend (mirrors ProviderSpend in src/quota.ts).
type Spend struct {
	FiveHourUSD   float64 `json:"fiveHourUsd"`
	DayUSD        float64 `json:"dayUsd"`
	WeekUSD       float64 `json:"weekUsd"`
	MonthUSD      float64 `json:"monthUsd"`
	MonthRequests int64   `json:"monthRequests"`
}

// Window is one account (or model-scoped) allowance slice.
type Window struct {
	ID          string
	Label       string
	UsedPercent float64
	UsedUSD     *float64
	LimitUSD    *float64
	ResetsAt    string
	Status      string
	Model       string // set when the window meters a single model
}

// Tracker computes health from a ledger, in-memory spent cooldowns, and the
// header spend snapshots stored in quota.json.
type Tracker struct {
	db      *ledger.DB
	mu      sync.Mutex
	spent   map[string]spentEntry // key: provider or provider\0model
	clockMu sync.RWMutex
	clock   func() time.Time

	statePath string
	loaded    bool
	headers   HeaderStateFile
	versions  map[string]uint64
	balances  map[string]trackedBalance
}

type spentEntry struct {
	until time.Time
	label string
}

// New returns a Tracker backed by db. db may be nil (health then uses only cooldowns).
func New(db *ledger.DB) *Tracker {
	return &Tracker{
		db:    db,
		spent: make(map[string]spentEntry),
		clock: func() time.Time { return time.Now().UTC() },
	}
}

// SetClock replaces the clock used for windows and cooldowns. Pass nil to restore wall clock.
// For tests only.
func (t *Tracker) SetClock(now func() time.Time) {
	t.clockMu.Lock()
	defer t.clockMu.Unlock()
	if now == nil {
		t.clock = func() time.Time { return time.Now().UTC() }
		return
	}
	t.clock = now
}

// Reset clears in-memory spent cooldowns and the cached header snapshot
// (the file on disk is left alone).
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spent = make(map[string]spentEntry)
	t.headers = HeaderStateFile{}
	t.loaded = t.statePath == ""
	for provider := range t.versions {
		t.bumpLocked(provider)
	}
	t.balances = nil
}

// MarkSpentOptions configure an in-memory cooldown.
type MarkSpentOptions struct {
	Label    string
	ResetsAt time.Time // zero → ProviderCooldown from now
	Model    string    // non-empty → model-scoped; does not exhaust the account
}

// MarkSpent benches a provider (or one of its models) until ResetsAt or the default cooldown.
// Callers in transport/upstream use this on classified quota/billing refusals and rate limits.
func (t *Tracker) MarkSpent(provider string, opts MarkSpentOptions) {
	if provider == "" {
		return
	}
	now := t.now()
	until := opts.ResetsAt
	if until.IsZero() {
		until = now.Add(ProviderCooldown)
	}
	label := opts.Label
	if label == "" {
		label = "limit"
	}
	key := spentKey(provider, opts.Model)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spent[key] = spentEntry{until: until.UTC(), label: label}
	t.bumpLocked(provider)
}

// HealthOptions tune ProviderHealth. A nil LowPercent uses DefaultLowPercent;
// an explicit 0 disables the remaining-percent threshold (avg-request rules still apply).
type HealthOptions struct {
	LowPercent *float64
}

// ProviderHealth returns spent/low/ok/unknown from config quota caps vs ledger windows,
// overlaying any active in-memory MarkSpent cooldown.
func (t *Tracker) ProviderHealth(provider config.Provider, opts HealthOptions) Health {
	now := t.now()
	lowPercent := DefaultLowPercent
	if opts.LowPercent != nil {
		lowPercent = *opts.LowPercent
	}

	if until, label, ok := t.activeSpent(provider.Name, "", now); ok {
		return Health{
			Provider:         provider.Name,
			Status:           StatusExhausted,
			UsedPercent:      100,
			RemainingPercent: 0,
			Window:           label,
			ResetsAt:         until.UTC().Format(isoMillis),
			Note:             "in-memory cooldown",
		}
	}

	spend, err := t.spendOf(provider.Name, now)
	if err != nil {
		return Health{Provider: provider.Name, Status: StatusUnknown, Note: err.Error()}
	}

	balance := t.liveBalance(provider.Name, now)
	if balance != nil && balance.Amount <= 0 {
		zero := 0.0
		return Health{Provider: provider.Name, Status: StatusExhausted, UsedPercent: 100, Window: "balance", RemainingUSD: &zero, Note: "balance " + balance.Currency}
	}

	// Known windows, as src/quota.ts knownWindows picks them: an active account
	// window recorded from headers or a probe replaces the config-cap estimate
	// outright; a model-scoped refusal alone does not, so the ledger-based cap
	// stays visible beside it.
	var header []Window
	hasAccount := false
	for _, w := range t.HeaderWindows(provider.Name) {
		if activeWindow(w, now) {
			header = append(header, w)
			if w.Model == "" {
				hasAccount = true
			}
		}
	}
	var windows []Window
	note := ""
	if !hasAccount {
		windows = specWindows(provider.Quota, spend)
	}
	if len(windows) > 0 {
		note = "estimated from the local ledger"
	} else if len(header) > 0 {
		if entry, ok := t.headerEntry(provider.Name); ok {
			if stale := stalenessNote(entry.FetchedAt, now); stale != "" {
				note = "stale snapshot (" + stale + ")"
			}
		}
	}
	windows = append(windows, header...)
	if len(windows) == 0 {
		if balance != nil {
			return Health{Provider: provider.Name, Status: StatusOK, RemainingUSD: round6Ptr(&balance.Amount)}
		}
		return Health{Provider: provider.Name, Status: StatusUnknown}
	}

	var worst *Window
	for i := range windows {
		w := &windows[i]
		if w.Model != "" {
			continue
		}
		if worst == nil || windowSeverity(*w) > windowSeverity(*worst) {
			worst = w
		}
	}
	if worst == nil {
		return Health{Provider: provider.Name, Status: StatusOK}
	}

	usedPercent := clamp(worst.UsedPercent, 0, 100)
	remainingPercent := math.Max(0, 100-usedPercent)
	avgRequestUSD := averageRequestUSD(spend)

	var remainingUSD *float64
	if worst.LimitUSD != nil && worst.UsedUSD != nil {
		v := math.Max(0, *worst.LimitUSD-*worst.UsedUSD)
		remainingUSD = &v
	} else if balance != nil {
		remainingUSD = &balance.Amount
	}

	status := StatusOK
	if worst.Status == "rejected" || usedPercent >= 100 {
		status = StatusExhausted
	} else if remainingPercent < lowPercent {
		status = StatusLow
	}
	if remainingUSD != nil && avgRequestUSD != nil {
		if *remainingUSD < *avgRequestUSD {
			status = StatusExhausted
		} else if *remainingUSD < *avgRequestUSD*SufficientRequestMultiple && status == StatusOK {
			status = StatusLow
		}
	}

	h := Health{
		Provider:         provider.Name,
		Status:           status,
		UsedPercent:      round6(usedPercent),
		RemainingPercent: round6(remainingPercent),
		Window:           worst.Label,
		ResetsAt:         worst.ResetsAt,
		RemainingUSD:     round6Ptr(remainingUSD),
		AvgRequestUSD:    round6Ptr(avgRequestUSD),
		Note:             note,
	}
	return h
}

// windowSeverity ranks account windows for the worst-window pick. src/quota.ts
// compares usedPercent alone; a `rejected` status only decides the verdict
// when that window is the worst one, so it adds no rank of its own.
func windowSeverity(w Window) float64 {
	return w.UsedPercent
}

func windowsContain(ws []Window, id string) bool {
	for _, w := range ws {
		if w.ID == id {
			return true
		}
	}
	return false
}

// ModelHealth reports only model-scoped exhaustion. It never treats account
// exhaustion as a model-specific diagnostic.
type ModelHealth struct {
	Model    string
	Status   Status
	Reason   string
	ResetsAt time.Time
}

// ProviderModelHealth exposes active model cooldowns and exhausted model windows.
func (t *Tracker) ModelHealth(provider config.Provider, model string) ModelHealth {
	health := ModelHealth{Model: model, Status: StatusOK}
	if model == "" {
		return health
	}
	now := t.now()
	if until, reason, ok := t.activeSpent(provider.Name, model, now); ok {
		return ModelHealth{Model: model, Status: StatusExhausted, Reason: reason, ResetsAt: until}
	}
	for _, w := range t.HeaderWindows(provider.Name) {
		if w.Model == model && activeWindow(w, now) && (w.Status == "rejected" || w.UsedPercent >= 100) {
			reset, _ := parseISO(w.ResetsAt)
			reason := w.Status
			if reason == "" || reason == "allowed" {
				reason = "model quota window exhausted"
			}
			return ModelHealth{Model: model, Status: StatusExhausted, Reason: reason, ResetsAt: reset}
		}
	}
	return health
}

// ProviderModelExhausted is true when the account is exhausted, this model
// has an active cooldown, or a recorded model-scoped window is spent.
func (t *Tracker) ProviderModelExhausted(provider config.Provider, model string, opts HealthOptions) bool {
	if t.ProviderHealth(provider, opts).Status == StatusExhausted {
		return true
	}
	now := t.now()
	if _, _, ok := t.activeSpent(provider.Name, model, now); ok {
		return true
	}
	for _, w := range t.HeaderWindows(provider.Name) {
		if w.Model == model && activeWindow(w, now) && (w.Status == "rejected" || w.UsedPercent >= 100) {
			return true
		}
	}
	return false
}

// Standing is the routing-facing read: health, model exhaustion and account
// window renewals in one call. Renews lists reset epochs (ms), longest
// window first. src/routing.ts standingOf inputs.
func (t *Tracker) Standing(provider config.Provider, model string, now int64, lowPercent float64) (Health, bool, []int64) {
	opts := HealthOptions{LowPercent: &lowPercent}
	health := t.ProviderHealth(provider, opts)
	exhausted := t.ProviderModelExhausted(provider, model, opts)
	windows := t.AccountWindows(provider, now)
	renews := make([]int64, 0, len(windows))
	for _, w := range windows {
		renews = append(renews, w.ResetsAt)
	}
	return health, exhausted, renews
}

func (t *Tracker) activeSpent(provider, model string, now time.Time) (time.Time, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := spentKey(provider, model)
	entry, ok := t.spent[key]
	if !ok {
		return time.Time{}, "", false
	}
	if !entry.until.After(now) {
		delete(t.spent, key)
		return time.Time{}, "", false
	}
	return entry.until, entry.label, true
}

func spentKey(provider, model string) string {
	if model == "" {
		return provider
	}
	return provider + "\x00" + model
}

func (t *Tracker) spendOf(provider string, now time.Time) (Spend, error) {
	if t.db == nil {
		return Spend{}, nil
	}
	fiveH, err := t.db.ProviderWindow(provider, ledger.Window5h, now)
	if err != nil {
		return Spend{}, err
	}
	day, err := t.db.ProviderWindow(provider, ledger.Window1d, now)
	if err != nil {
		return Spend{}, err
	}
	week, err := t.db.ProviderWindow(provider, ledger.Window7d, now)
	if err != nil {
		return Spend{}, err
	}
	month, err := t.db.ProviderWindow(provider, ledger.Window30d, now)
	if err != nil {
		return Spend{}, err
	}
	return Spend{
		FiveHourUSD:   round6(fiveH.CostUSD),
		DayUSD:        round6(day.CostUSD),
		WeekUSD:       round6(week.CostUSD),
		MonthUSD:      round6(month.CostUSD),
		MonthRequests: month.Requests,
	}, nil
}

func specWindows(spec *config.ProviderQuotaSpec, spend Spend) []Window {
	if spec == nil {
		return nil
	}
	var out []Window
	push := func(id string, used float64, limit *float64) {
		if limit == nil || *limit <= 0 {
			return
		}
		u := round6(used)
		pct := clamp((used / *limit)*100, 0, 100)
		out = append(out, Window{
			ID:          id,
			Label:       id,
			UsedUSD:     &u,
			LimitUSD:    limit,
			UsedPercent: pct,
		})
	}
	push("5h", spend.FiveHourUSD, spec.FiveHourUSD)
	push("week", spend.WeekUSD, spec.WeeklyUSD)
	push("month", spend.MonthUSD, spec.MonthlyUSD)
	return out
}

func averageRequestUSD(spend Spend) *float64 {
	if spend.MonthRequests < 3 || spend.MonthUSD <= 0 {
		return nil
	}
	v := spend.MonthUSD / float64(spend.MonthRequests)
	return &v
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

func round6Ptr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := round6(*v)
	return &r
}
