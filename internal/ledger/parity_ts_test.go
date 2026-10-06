package ledger_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
)

// Differential check against src/ledger-index.ts. The fixture is a generated
// golden file: a TS LedgerSpendIndex fed a ledger.jsonl sample, queried over
// several windows and "now" values. Set JEVONIAN_PARITY_LEDGER_SAMPLE (a
// ledger.jsonl copy) and JEVONIAN_PARITY_LEDGER_TS (the TS output JSON) to run.
// The test never touches the real data directory.
type tsSpend struct {
	CostUsd         float64 `json:"costUsd"`
	APIUsd          float64 `json:"apiUsd"`
	SubscriptionUsd float64 `json:"subscriptionUsd"`
	Requests        int64   `json:"requests"`
}

type tsCase struct {
	Kind string  `json:"kind"`
	ID   string  `json:"id"`
	Now  int64   `json:"now"`
	W    int64   `json:"w"`
	Got  tsSpend `json:"got"`
}

func TestLedgerIndexDifferentialAgainstTS(t *testing.T) {
	sample := os.Getenv("JEVONIAN_PARITY_LEDGER_SAMPLE")
	golden := os.Getenv("JEVONIAN_PARITY_LEDGER_TS")
	if sample == "" || golden == "" {
		t.Skip("set JEVONIAN_PARITY_LEDGER_SAMPLE and JEVONIAN_PARITY_LEDGER_TS to run")
	}
	raw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Newest int64    `json:"newest"`
		Out    []tsCase `json:"out"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ImportJSONL(sample); err != nil {
		t.Fatal(err)
	}
	// src/ledger-index.ts sums whole hourly buckets that overlap [now-w, now],
	// so a record in the boundary hour counts even when it predates the exact
	// window start. Go keeps exact timestamps (an intentional precision gain).
	// The assertion below proves this is the only difference: with the window
	// start snapped down to the hour, every case must match the TS totals.
	const hour = int64(3600000)
	var exactDiffs int
	for _, c := range fixture.Out {
		query := func(window int64) ledger.SpendTotal {
			var got ledger.SpendTotal
			now := time.UnixMilli(c.Now)
			d := time.Duration(window) * time.Millisecond
			switch c.Kind {
			case "provider":
				got, err = db.ProviderWindow(c.ID, d, now)
			case "key":
				got, err = db.KeyWindow(c.ID, d, now)
			case "providerAll":
				got, err = db.ProviderAllTime(c.ID)
			case "keyAll":
				got, err = db.KeyAllTime(c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		same := func(got ledger.SpendTotal) bool {
			return got.Requests == c.Got.Requests &&
				math.Abs(got.CostUSD-c.Got.CostUsd) <= 1e-9 &&
				math.Abs(got.APIUsd-c.Got.APIUsd) <= 1e-9 &&
				math.Abs(got.SubscriptionUsd-c.Got.SubscriptionUsd) <= 1e-9
		}
		window := c.W
		if c.Kind == "provider" || c.Kind == "key" {
			from := c.Now - c.W
			aligned := (from / hour) * hour
			if !same(query(c.W)) {
				exactDiffs++
			}
			window = c.Now - aligned
		}
		if got := query(window); !same(got) {
			t.Errorf("%s %q now=+%dms w=%dh: go=%+v ts=%+v", c.Kind, c.ID, c.Now-fixture.Newest, c.W/hour, got, c.Got)
		}
	}
	t.Logf("compared %d cases; %d differ only by hour-bucket granularity", len(fixture.Out), exactDiffs)
}
