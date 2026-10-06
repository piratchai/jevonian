package quota

import (
	"strings"
	"time"
)

type trackedBalance struct {
	balance Balance
	until   time.Time
}

func (t *Tracker) now() time.Time {
	t.clockMu.RLock()
	clock := t.clock
	t.clockMu.RUnlock()
	return clock().UTC()
}
func (t *Tracker) bumpLocked(provider string) {
	if t.versions == nil {
		t.versions = map[string]uint64{}
	}
	t.versions[provider]++
	delete(t.balances, provider)
}
func (t *Tracker) revision(provider string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.versions == nil {
		t.versions = map[string]uint64{}
	}
	if _, ok := t.versions[provider]; !ok {
		t.versions[provider] = 0
	}
	return t.versions[provider]
}
func (t *Tracker) headerEntry(provider string) (HeaderEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.loadHeadersLocked()[provider]
	e.Windows = cloneWindows(e.Windows)
	return e, ok
}
func (t *Tracker) liveBalance(provider string, now time.Time) *Balance {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.balances[provider]
	if !ok || !b.until.After(now) {
		return nil
	}
	v := b.balance
	return &v
}

// applyLive atomically overlays model refusals and commits only when no newer
// header/refusal arrived while the network request was in flight.
func (t *Tracker) applyLive(provider string, revision uint64, live liveQuota, now time.Time, ttl time.Duration) ([]Window, uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.versions[provider] != revision {
		return nil, t.versions[provider], false
	}
	current := t.loadHeadersLocked()
	snapshot := current[provider]
	var scoped []Window
	for _, w := range snapshot.Windows {
		if w.Model != "" && w.Status == "rejected" && activeWindow(w, now) {
			scoped = append(scoped, w)
		}
	}
	windows := make([]Window, 0, len(live.Windows)+len(scoped))
	for _, w := range live.Windows {
		drop := false
		for _, r := range scoped {
			if w.Model == r.Model {
				drop = true
				break
			}
		}
		if !drop && !(w.Model != "" && w.Status == "rejected") {
			windows = append(windows, w)
		}
	}
	windows = append(windows, scoped...)
	if len(live.Windows) > 0 {
		current[provider] = HeaderEntry{Windows: cloneWindows(windows), Plan: live.Plan, FetchedAt: now.Format(isoMillis)}
	} else if live.Balance != nil && live.Balance.Amount <= 0 {
		rejected := Window{ID: "balance", Label: "balance", UsedPercent: 100, Status: "rejected"}
		current[provider] = HeaderEntry{Windows: append([]Window{rejected}, scoped...), FetchedAt: now.Format(isoMillis)}
	} else {
		var remaining []Window
		for _, w := range snapshot.Windows {
			if w.Model != "" && activeWindow(w, now) {
				remaining = append(remaining, w)
			}
		}
		if len(remaining) > 0 {
			snapshot.Windows = remaining
			current[provider] = snapshot
		} else {
			delete(current, provider)
		}
	}
	// An account probe proves only account marks cleared, never model marks.
	delete(t.spent, provider)
	for key, entry := range t.spent {
		if strings.HasPrefix(key, provider+"\x00") && !entry.until.After(now) {
			delete(t.spent, key)
		}
	}
	t.bumpLocked(provider)
	if live.Balance != nil {
		if t.balances == nil {
			t.balances = map[string]trackedBalance{}
		}
		t.balances[provider] = trackedBalance{balance: *live.Balance, until: now.Add(ttl)}
	}
	t.saveHeadersLocked(current)
	return windows, t.versions[provider], true
}
