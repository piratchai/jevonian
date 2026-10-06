package quota_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/quota"
)

// Differential: src/quota.ts providerQuotaHealth / providerModelExhausted /
// accountWindows over random provider specs, ledger spend, quota.json
// snapshots, thresholds and a pinned clock.
type healthFuzz struct {
	Now   int64 `json:"now"`
	Cases []struct {
		Provider struct {
			Quota *struct {
				FiveHourUSD *float64 `json:"fiveHourUsd"`
				WeeklyUSD   *float64 `json:"weeklyUsd"`
				MonthlyUSD  *float64 `json:"monthlyUsd"`
			} `json:"quota"`
		} `json:"provider"`
		Records []struct {
			TS      string   `json:"ts"`
			Cost    *float64 `json:"costUsd"`
			Kind    string   `json:"kind"`
			Billing string   `json:"billing"`
		} `json:"records"`
		Windows    []json.RawMessage `json:"windows"`
		FetchedAt  string            `json:"fetchedAt"`
		LowPercent *float64          `json:"lowPercent"`
		Want       struct {
			Status           string   `json:"status"`
			UsedPercent      *float64 `json:"usedPercent"`
			RemainingPercent *float64 `json:"remainingPercent"`
			Window           string   `json:"window"`
			ResetsAt         string   `json:"resetsAt"`
			RemainingUSD     *float64 `json:"remainingUsd"`
			AvgRequestUSD    *float64 `json:"avgRequestUsd"`
			Note             string   `json:"note"`
		} `json:"want"`
		ModelExhausted bool `json:"modelExhausted"`
		Acct           []struct {
			SpanMinutes float64 `json:"spanMinutes"`
			UsedPercent float64 `json:"usedPercent"`
			ResetsAt    int64   `json:"resetsAt"`
		} `json:"acct"`
	} `json:"cases"`
}

func near(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 1e-6
}

func TestProviderHealthMatchesTypeScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_health_fuzz.json")
	if err != nil {
		t.Skip("no TS health fixture")
	}
	var fx healthFuzz
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(fx.Now).UTC()
	failures := 0
	for i, c := range fx.Cases {
		dir := t.TempDir()
		db, err := ledger.Open(filepath.Join(dir, "l.db"))
		if err != nil {
			t.Fatal(err)
		}
		for j, r := range c.Records {
			ts, _ := time.Parse(time.RFC3339Nano, r.TS)
			if err := db.Append(ledger.Record{ID: string(rune('a'+j)) + r.TS, TS: ts, Provider: "p", Model: "m", Status: 200, CostUSD: r.Cost, Kind: r.Kind, Billing: r.Billing}); err != nil {
				t.Fatal(err)
			}
		}
		state := filepath.Join(dir, "quota.json")
		if len(c.Windows) > 0 {
			body, _ := json.Marshal(map[string]any{"p": map[string]any{"windows": c.Windows, "fetchedAt": c.FetchedAt}})
			_ = os.WriteFile(state, body, 0o600)
		}
		tracker := quota.New(db)
		tracker.SetStatePath(state)
		tracker.SetClock(func() time.Time { return now })
		p := config.Provider{Name: "p", Billing: config.BillingSubscription}
		if c.Provider.Quota != nil {
			p.Quota = &config.ProviderQuotaSpec{FiveHourUSD: c.Provider.Quota.FiveHourUSD, WeeklyUSD: c.Provider.Quota.WeeklyUSD, MonthlyUSD: c.Provider.Quota.MonthlyUSD}
		}
		opts := quota.HealthOptions{LowPercent: c.LowPercent}
		h := tracker.ProviderHealth(p, opts)
		bad := func(field string, got, want any) {
			failures++
			t.Errorf("case %d %s: go=%v ts=%v (provider quota=%+v windows=%s fetchedAt=%s low=%v)", i, field, got, want, c.Provider.Quota, mustJSON(c.Windows), c.FetchedAt, c.LowPercent)
		}
		if string(h.Status) != c.Want.Status {
			bad("status", h.Status, c.Want.Status)
			db.Close()
			continue
		}
		if c.Want.Status != "unknown" && c.Want.Window != "" && h.Window != c.Want.Window {
			bad("window", h.Window, c.Want.Window)
		}
		if c.Want.UsedPercent != nil && math.Abs(h.UsedPercent-*c.Want.UsedPercent) > 1e-6 {
			bad("usedPercent", h.UsedPercent, *c.Want.UsedPercent)
		}
		if c.Want.ResetsAt != "" && h.ResetsAt != c.Want.ResetsAt {
			bad("resetsAt", h.ResetsAt, c.Want.ResetsAt)
		}
		if !near(h.RemainingUSD, c.Want.RemainingUSD) {
			bad("remainingUsd", h.RemainingUSD, c.Want.RemainingUSD)
		}
		if !near(h.AvgRequestUSD, c.Want.AvgRequestUSD) {
			bad("avgRequestUsd", h.AvgRequestUSD, c.Want.AvgRequestUSD)
		}
		if h.Note != c.Want.Note {
			bad("note", h.Note, c.Want.Note)
		}
		if got := tracker.ProviderModelExhausted(p, "Fable", opts); got != c.ModelExhausted {
			bad("modelExhausted", got, c.ModelExhausted)
		}
		got := tracker.AccountWindows(p, fx.Now)
		if len(got) != len(c.Acct) {
			bad("accountWindows len", len(got), len(c.Acct))
		} else {
			for j, w := range c.Acct {
				if got[j].SpanMinutes != w.SpanMinutes || math.Abs(got[j].UsedPercent-w.UsedPercent) > 1e-6 || got[j].ResetsAt != w.ResetsAt {
					bad("accountWindows", got[j], w)
				}
			}
		}
		db.Close()
	}
	t.Logf("%d cases, %d field mismatches", len(fx.Cases), failures)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
