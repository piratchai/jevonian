package quota

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// HeaderStateFile is the provider spend snapshot format shared with the TS
// build's quota.json (src/quota.ts HeaderQuotaFile). The Go server keeps its
// snapshot in memory and persists it for the dashboard and the TS process.
type HeaderStateFile map[string]HeaderEntry

type HeaderEntry struct {
	Windows   []Window `json:"windows"`
	Plan      string   `json:"plan,omitempty"`
	FetchedAt string   `json:"fetchedAt"`
}

type windowJSON struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	UsedPercent *float64 `json:"usedPercent,omitempty"`
	UsedUSD     *float64 `json:"usedUsd,omitempty"`
	LimitUSD    *float64 `json:"limitUsd,omitempty"`
	ResetsAt    string   `json:"resetsAt,omitempty"`
	Status      string   `json:"status,omitempty"`
	Model       string   `json:"model,omitempty"`
}

func windowToJSON(w Window) windowJSON {
	used := w.UsedPercent
	return windowJSON{
		ID:          w.ID,
		Label:       w.Label,
		UsedPercent: &used,
		UsedUSD:     w.UsedUSD,
		LimitUSD:    w.LimitUSD,
		ResetsAt:    w.ResetsAt,
		Status:      w.Status,
		Model:       w.Model,
	}
}

func windowFromJSON(j windowJSON) Window {
	w := Window{
		ID:       j.ID,
		Label:    j.Label,
		UsedUSD:  j.UsedUSD,
		LimitUSD: j.LimitUSD,
		ResetsAt: j.ResetsAt,
		Status:   j.Status,
		Model:    j.Model,
	}
	if j.UsedPercent != nil {
		w.UsedPercent = *j.UsedPercent
	}
	if w.Label == "" {
		w.Label = w.ID
	}
	return w
}

// MarshalJSON keeps the quota.json shape identical to the TS file.
func (e HeaderEntry) MarshalJSON() ([]byte, error) {
	windows := make([]windowJSON, 0, len(e.Windows))
	for _, w := range e.Windows {
		windows = append(windows, windowToJSON(w))
	}
	return json.Marshal(struct {
		Windows   []windowJSON `json:"windows"`
		Plan      string       `json:"plan,omitempty"`
		FetchedAt string       `json:"fetchedAt"`
	}{Windows: windows, Plan: e.Plan, FetchedAt: e.FetchedAt})
}

func (e *HeaderEntry) UnmarshalJSON(data []byte) error {
	var j struct {
		Windows   []windowJSON `json:"windows"`
		Plan      string       `json:"plan,omitempty"`
		FetchedAt string       `json:"fetchedAt"`
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	e.Plan, e.FetchedAt = j.Plan, j.FetchedAt
	e.Windows = e.Windows[:0]
	for _, w := range j.Windows {
		e.Windows = append(e.Windows, windowFromJSON(w))
	}
	return nil
}

// SetStatePath makes the Tracker read and write header snapshots at path
// (dataDir/quota.json in production). Tests use "" to stay in memory.
func (t *Tracker) SetStatePath(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statePath = path
	t.loaded = false
	for provider := range t.versions {
		t.bumpLocked(provider)
	}
	t.balances = nil
}

// loadHeaders reads quota.json (once) into memory. Missing file → empty.
func (t *Tracker) loadHeadersLocked() HeaderStateFile {
	if t.loaded {
		return t.headers
	}
	t.loaded = true
	t.headers = HeaderStateFile{}
	if t.statePath == "" {
		return t.headers
	}
	raw, err := os.ReadFile(t.statePath)
	if err != nil {
		return t.headers
	}
	var parsed HeaderStateFile
	if json.Unmarshal(raw, &parsed) == nil {
		t.headers = parsed
	}
	return t.headers
}

// saveHeadersLocked persists the snapshot like src/quota.ts saveHeaderQuotas.
func (t *Tracker) saveHeadersLocked(next HeaderStateFile) {
	t.headers = next
	if t.statePath == "" {
		return
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(t.statePath), 0o755)
	_ = os.WriteFile(t.statePath, append(data, '\n'), 0o644)
}

// isoMillis is JavaScript's Date.toISOString layout: always three fractional
// digits and a literal Z. quota.json is shared with the TS build, so the
// timestamps Go writes keep the same shape.
const isoMillis = "2006-01-02T15:04:05.000Z"

func activeWindow(w Window, now time.Time) bool {
	if w.ResetsAt == "" {
		return true
	}
	resets, ok := parseISO(w.ResetsAt)
	return !ok || resets.After(now)
}

// MarkProviderSpent records a provider as spent from an already-classified
// refusal, writing the same synthetic `rejected` snapshot a header probe
// stores. Model-scoped refusals keep the account windows and other models'
// refusals. src/quota.ts markProviderSpent.
func (t *Tracker) MarkProviderSpent(provider string, opts MarkSpentOptions) {
	if provider == "" {
		return
	}
	now := t.now()
	label := opts.Label
	if label == "" {
		label = "limit"
	}
	resetsAt := ""
	if !opts.ResetsAt.IsZero() {
		resetsAt = opts.ResetsAt.UTC().Format(isoMillis)
	}
	model := strings.TrimSpace(opts.Model)
	id := label
	if model != "" {
		id = label + ":" + model
	}
	pct := 100.0
	window := Window{
		ID:          id,
		Label:       label,
		UsedPercent: pct,
		ResetsAt:    resetsAt,
		Status:      "rejected",
		Model:       model,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.loadHeadersLocked()
	previous, havePrev := current[provider]
	var windows []Window
	if model != "" {
		// Scoped refusals keep account windows and other models' refusals.
		if havePrev {
			for _, w := range previous.Windows {
				if w.Model != model && activeWindow(w, now) {
					windows = append(windows, w)
				}
			}
		}
		windows = append(windows, window)
	} else {
		windows = []Window{window}
	}
	entry := HeaderEntry{Windows: windows, FetchedAt: now.Format(isoMillis)}
	if havePrev {
		entry.Plan = previous.Plan
	}
	current[provider] = entry
	t.bumpLocked(provider)
	t.saveHeadersLocked(current)
}

// ClearRejection forgets a snapshot recorded by a refusal once a live probe
// succeeds. Only a *successful* probe clears it.
// src/quota.ts clearRejection.
func (t *Tracker) ClearRejection(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.loadHeadersLocked()
	snapshot, ok := current[provider]
	if !ok {
		return
	}
	hasRejected := false
	for _, w := range snapshot.Windows {
		if w.Status == "rejected" {
			hasRejected = true
			break
		}
	}
	if !hasRejected {
		return
	}
	now := t.now()
	var remaining []Window
	for _, w := range snapshot.Windows {
		if w.Model != "" && activeWindow(w, now) {
			remaining = append(remaining, w)
		}
	}
	next := HeaderStateFile{}
	for k, v := range current {
		next[k] = v
	}
	if len(remaining) > 0 {
		snapshot.Windows = remaining
		next[provider] = snapshot
	} else {
		delete(next, provider)
	}
	t.bumpLocked(provider)
	t.saveHeadersLocked(next)
}

// HeaderWindows returns the recorded account windows for a provider.
// Used for account probes and reset-aware ordering.
func (t *Tracker) HeaderWindows(provider string) []Window {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.loadHeadersLocked()[provider]
	if !ok {
		return nil
	}
	return cloneWindows(entry.Windows)
}

// AccountWindow is one account-wide allowance window as the vendor last said
// it, for reset-aware ordering. src/quota.ts AccountWindow.
type AccountWindow struct {
	SpanMinutes float64
	UsedPercent float64
	ResetsAt    int64 // epoch ms, 0 when not stated
}

var spanLabel = regexp.MustCompile(`^(\d+)([hd])$`)

// "5h"/"7d" → minutes; another label does not say, so 0.
func spanMinutesOf(label string) float64 {
	m := spanLabel.FindStringSubmatch(strings.TrimSpace(label))
	if m == nil {
		return 0
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	if m[2] == "d" {
		return float64(v) * 1440
	}
	return float64(v) * 60
}

// AccountWindows lists a provider's account-wide windows, longest first.
// Model-scoped and expired windows are left out.
// src/quota.ts accountWindows (header snapshots plus config-cap windows).
func (t *Tracker) AccountWindows(provider config.Provider, now int64) []AccountWindow {
	nowTime := time.UnixMilli(now).UTC()
	// Same selection as src/quota.ts knownWindows: an active account-wide window
	// from headers or a probe replaces the config-cap estimate; a model-scoped
	// refusal alone does not.
	var windows []Window
	hasAccount := false
	var recorded []Window
	for _, w := range t.HeaderWindows(provider.Name) {
		if activeWindow(w, nowTime) {
			recorded = append(recorded, w)
			if w.Model == "" {
				hasAccount = true
			}
		}
	}
	if !hasAccount {
		spend, _ := t.spendOf(provider.Name, nowTime)
		windows = specWindows(provider.Quota, spend)
	}
	windows = append(windows, recorded...)
	// An in-memory spend cooldown acts like a `rejected` account window with a
	// known reset so ordering can compare renewal times.
	t.mu.Lock()
	if e, ok := t.spent[provider.Name]; ok && e.until.After(nowTime) {
		windows = append(windows, Window{
			ID:          e.label,
			Label:       e.label,
			UsedPercent: 100,
			ResetsAt:    e.until.UTC().Format(isoMillis),
			Status:      "rejected",
		})
	}
	t.mu.Unlock()

	var out []AccountWindow
	for _, w := range windows {
		if w.Model != "" || !activeWindow(w, nowTime) {
			continue
		}
		resets := int64(0)
		if w.ResetsAt != "" {
			if ts, ok := parseISO(w.ResetsAt); ok {
				resets = ts.UnixMilli()
			}
		}
		out = append(out, AccountWindow{
			SpanMinutes: spanMinutesOf(w.Label),
			UsedPercent: math.Min(100, math.Max(0, w.UsedPercent)),
			ResetsAt:    resets,
		})
	}
	// The longest window leads: a week's allowance outlives the five hours
	// inside it, so it is the week that decides when an account can take a
	// turn again. src/quota.ts accountWindows ordering.
	sort.SliceStable(out, func(i, j int) bool { return out[i].SpanMinutes > out[j].SpanMinutes })
	return out
}

// CaptureRateLimitHeaders stores the window utilization upstream reported on a
// response (Anthropic unified + Codex headers). src/quota.ts
// refreshHeaderQuota.
func (t *Tracker) CaptureRateLimitHeaders(provider string, headers http.Header) {
	windows := append(anthropicWindowsFromHeaders(headers), codexWindowsFromHeaders(headers)...)
	if len(windows) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	windows = t.withModelRejectionsLocked(provider, windows)
	current := t.loadHeadersLocked()
	previous, havePrevious := current[provider]
	// A window's usedPercent and resetsAt can stay identical for a whole reset
	// period, so equality alone does not prove the snapshot is current: skip the
	// write only while the stored one is still inside the TTL. Skipping keeps the
	// live cache warm and avoids rewriting quota.json on every response
	// (src/quota.ts captureQuotaHeaders).
	if havePrevious && !t.snapshotStale(previous.FetchedAt) && sameWindows(previous.Windows, windows) {
		return
	}
	current[provider] = HeaderEntry{
		Windows:   windows,
		FetchedAt: t.now().Format(isoMillis),
		Plan:      previous.Plan,
	}
	t.bumpLocked(provider)
	t.saveHeadersLocked(current)
}

// snapshotStale reports a snapshot older than HeaderSnapshotTTL, or with an
// unreadable timestamp. src/quota.ts snapshotStale.
func (t *Tracker) snapshotStale(fetchedAt string) bool {
	at, ok := parseISO(fetchedAt)
	return !ok || t.now().Sub(at) >= HeaderSnapshotTTL
}

func sameWindows(a, b []Window) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

// withModelRejections merges in live model-scoped rejections.
// src/quota.ts withModelRejections.
func (t *Tracker) withModelRejections(provider string, windows []Window) []Window {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.withModelRejectionsLocked(provider, windows)
}

func (t *Tracker) withModelRejectionsLocked(provider string, windows []Window) []Window {
	now := t.now()
	var rejections []Window
	for _, w := range t.loadHeadersLocked()[provider].Windows {
		if w.Model != "" && w.Status == "rejected" && activeWindow(w, now) {
			rejections = append(rejections, w)
		}
	}
	out := windows[:0:0]
	for _, w := range windows {
		if w.Model != "" && w.Status == "rejected" {
			continue
		}
		drop := false
		for _, r := range rejections {
			if r.Model == w.Model {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, w)
		}
	}
	return append(out, rejections...)
}

// anthropicWindowsFromHeaders reads anthropic-ratelimit-unified-* headers.
// src/quota.ts anthropicWindowsFromHeaders.
func anthropicWindowsFromHeaders(h http.Header) []Window {
	var windows []Window
	parse := func(suffix string) (float64, bool) {
		v := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-" + suffix))
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	read := func(suffix string) string {
		return h.Get("anthropic-ratelimit-unified-" + suffix)
	}
	for _, id := range []string{"5h", "7d"} {
		used, ok := parse(id + "-utilization")
		if !ok {
			continue
		}
		w := Window{ID: id, Label: id, UsedPercent: fractionPercent(used)}
		if r, ok2 := parse(id + "-reset"); ok2 {
			w.ResetsAt = epochISO(r)
		}
		w.Status = read(id + "-status")
		windows = append(windows, w)
	}
	return windows
}

// windowMinutesLabel mirrors src/quota.ts windowMinutesLabel.
func windowMinutesLabel(minutes float64) string {
	if minutes <= 0 {
		return "window"
	}
	hours := minutes / 60
	if hours <= 6 {
		return strconv.Itoa(int(math.Round(hours))) + "h"
	}
	return strconv.Itoa(int(math.Round(hours/24))) + "d"
}

// codexWindowsFromHeaders reads x-codex-* window headers.
// src/quota.ts codexWindowsFromHeaders.
func codexWindowsFromHeaders(h http.Header) []Window {
	var windows []Window
	parse := func(suffix string) (float64, bool) {
		v := strings.TrimSpace(h.Get("x-codex-" + suffix))
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	win := func(id, prefix string) *Window {
		used, ok := parse(prefix + "-used-percent")
		if !ok {
			return nil
		}
		minutes, _ := parse(prefix + "-window-minutes")
		w := Window{ID: id, Label: windowMinutesLabel(minutes), UsedPercent: clampPercent(used)}
		if r, ok2 := parse(prefix + "-reset-at"); ok2 {
			w.ResetsAt = epochISO(r)
		}
		return &w
	}
	if w := win("codex-primary", "primary"); w != nil {
		windows = append(windows, *w)
	}
	if w := win("codex-secondary", "secondary"); w != nil {
		windows = append(windows, *w)
	}
	return windows
}

// epochISO mirrors src/quota.ts toIso for numbers: values above 1e12 are ms,
// otherwise seconds.
func epochISO(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	ms := v
	if v <= 1e12 {
		ms = v * 1000
	}
	return time.UnixMilli(int64(ms)).UTC().Format(isoMillis)
}

// fractionPercent mirrors src/quota.ts percent: <= 1 is a 0-1 fraction.
func fractionPercent(v float64) float64 {
	if v <= 1 {
		v *= 100
	}
	return clampPercent(v)
}

// clampPercent mirrors src/quota.ts percentPoints (already 0-100).
func clampPercent(v float64) float64 { return math.Min(100, math.Max(0, v)) }
