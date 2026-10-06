package quota

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// Differential fixtures from src/quota.ts: providerSpendSignal / captureUsageLimit
// (including reset parsing), messageSpendSignal, isProviderRefusal,
// isRateLimitRefusal and the Anthropic / Codex header windows, all run with a
// pinned quota clock.
type quotaFuzz struct {
	Now   int64 `json:"now"`
	Cases []struct {
		Fn       string            `json:"fn"`
		Status   int               `json:"status"`
		Body     string            `json:"body"`
		Signal   *string           `json:"signal"`
		Captured bool              `json:"captured"`
		Window   *tsWindow         `json:"window"`
		Message  string            `json:"message"`
		Refusal  bool              `json:"refusal"`
		Rate     bool              `json:"rate"`
		Headers  map[string]string `json:"headers"`
		Anth     []tsHeaderWindow  `json:"anthropic"`
		Codex    []tsHeaderWindow  `json:"codex"`
	} `json:"cases"`
}

type tsWindow struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	ResetsAt *string `json:"resetsAt"`
}

type tsHeaderWindow struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    string  `json:"resetsAt"`
	Status      string  `json:"status"`
}

func signalString(s SpendSignal) string { return string(s) }

func TestQuotaSignalsAndHeadersMatchTypeScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_quota_fuzz.json")
	if err != nil {
		t.Skip("no TS quota fixture")
	}
	var fx quotaFuzz
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(fx.Now).UTC()
	for i, c := range fx.Cases {
		switch c.Fn {
		case "capture":
			want := ""
			if c.Signal != nil {
				want = *c.Signal
			}
			if got := signalString(ProviderSpendSignal(c.Status, c.Body)); got != want {
				t.Errorf("case %d signal(%d, %q) = %q want %q", i, c.Status, c.Body, got, want)
				continue
			}
			tracker := New(nil)
			tracker.SetClock(func() time.Time { return now })
			tracker.SetStatePath("")
			if got := tracker.CaptureUsageLimit("p", c.Status, c.Body); got != c.Captured {
				t.Errorf("case %d captured = %v want %v", i, got, c.Captured)
				continue
			}
			if !c.Captured {
				continue
			}
			windows := tracker.HeaderWindows("p")
			_ = windows
			// Go keeps the cooldown in memory; the label and reset live in the spent entry.
			// A reset already in the past is benched for nothing (TS: an inactive window).
			entry, present := tracker.spent["p"]
			if !present {
				t.Errorf("case %d: no spent entry after capture", i)
				continue
			}
			spent := entry.until
			label := entry.label
			if c.Window == nil || label != c.Window.Label {
				t.Errorf("case %d (%d %q): label = %q want %+v", i, c.Status, c.Body, label, c.Window)
			}
			wantReset := ""
			if c.Window != nil && c.Window.ResetsAt != nil {
				wantReset = *c.Window.ResetsAt
			}
			if wantReset == "" {
				if spent.Sub(now) != ProviderCooldown {
					t.Errorf("case %d (%q): no stated reset must use the %v cooldown, got %v", i, c.Body, ProviderCooldown, spent.Sub(now))
				}
				continue
			}
			wantAt, _ := time.Parse(time.RFC3339Nano, wantReset)
			if !spent.Equal(wantAt) {
				t.Errorf("case %d (%q): reset = %v want %v", i, c.Body, spent, wantAt)
			}
		case "message":
			want := ""
			if c.Signal != nil {
				want = *c.Signal
			}
			if got := signalString(MessageSpendSignal(c.Message)); got != want {
				t.Errorf("case %d message(%q) = %q want %q", i, c.Message, got, want)
			}
		case "status":
			if IsProviderRefusal(c.Status) != c.Refusal || IsRateLimitRefusal(c.Status) != c.Rate {
				t.Errorf("case %d status %d refusal/rate mismatch", i, c.Status)
			}
		case "headers":
			h := http.Header{}
			for k, v := range c.Headers {
				h.Set(k, v)
			}
			compareWindows(t, i, "anthropic", anthropicWindowsFromHeaders(h), c.Anth)
			compareWindows(t, i, "codex", codexWindowsFromHeaders(h), c.Codex)
		}
	}
}

func compareWindows(t *testing.T, i int, kind string, got []Window, want []tsHeaderWindow) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("case %d %s: %d windows want %d (%+v)", i, kind, len(got), len(want), got)
		return
	}
	for j, w := range want {
		g := got[j]
		// Header windows are keyed "5h"/"7d" (anthropic) and codex-primary/secondary in TS.
		if g.Label != w.Label || g.UsedPercent != w.UsedPercent || g.ResetsAt != w.ResetsAt || g.Status != w.Status {
			t.Errorf("case %d %s[%d]: go=%+v ts=%+v", i, kind, j, g, w)
		}
		if g.ID != w.ID {
			t.Errorf("case %d %s[%d]: id go=%q ts=%q", i, kind, j, g.ID, w.ID)
		}
	}
}
