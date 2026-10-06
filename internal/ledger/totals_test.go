package ledger_test

import (
	"math"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
)

// Totals backs GET /stats: it sums every row, brain rows included, exactly like
// the readRecords().reduce(...) in src/server.ts. A null cost counts as zero.
func TestTotalsSumsEveryRowKind(t *testing.T) {
	db := openTemp(t)
	now := time.Now().UTC()
	rows := []ledger.Record{
		{ID: "a", TS: now, Provider: "p", CostUSD: floatPtr(1.25), CacheReadTokens: 10, PromptTokens: 100},
		{ID: "b", TS: now, Provider: "p", CostUSD: nil, CacheReadTokens: 5, PromptTokens: 50},
		{ID: "c", TS: now, Provider: "brain:typesafe", Kind: "brain", CostUSD: floatPtr(0.5), PromptTokens: 7},
	}
	for _, r := range rows {
		if err := db.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.Totals()
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 3 || got.CacheReadTokens != 15 || got.PromptTokens != 157 || math.Abs(got.CostUSD-1.75) > 1e-9 {
		t.Fatalf("totals = %+v", got)
	}
}

func TestTotalsEmptyLedger(t *testing.T) {
	got, err := openTemp(t).Totals()
	if err != nil || got != (ledger.Totals{}) {
		t.Fatalf("empty totals = %+v, %v", got, err)
	}
}
