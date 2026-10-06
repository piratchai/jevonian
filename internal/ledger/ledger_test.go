package ledger_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
)

func floatPtr(v float64) *float64 { return &v }

func openTemp(t *testing.T) *ledger.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAppendAndProviderWindows(t *testing.T) {
	db := openTemp(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	records := []ledger.Record{
		{
			ID: "a", TS: time.Date(2026, 10, 4, 11, 30, 0, 0, time.UTC),
			Session: "s", Path: "/v1/chat/completions", Provider: "p1", Model: "m",
			Status: 200, CostUSD: floatPtr(1.5),
		},
		{
			ID: "b", TS: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
			Session: "s", Path: "/v1/chat/completions", Provider: "p1", Model: "m",
			Status: 200, CostUSD: floatPtr(2),
		},
		{
			ID: "c", TS: time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC),
			Session: "s", Path: "/v1/chat/completions", Provider: "p2", Model: "m",
			Status: 200, CostUSD: floatPtr(9),
		},
		{
			ID: "brain", TS: now, Session: "s", Path: "/brain", Provider: "brain:typesafe",
			Model: "m", Status: 200, Kind: "brain", CostUSD: floatPtr(99),
		},
	}
	for _, r := range records {
		if err := db.Append(r); err != nil {
			t.Fatalf("Append(%s): %v", r.ID, err)
		}
	}

	fiveH, err := db.ProviderWindow("p1", ledger.Window5h, now)
	if err != nil {
		t.Fatalf("ProviderWindow 5h: %v", err)
	}
	if fiveH.CostUSD != 1.5 {
		t.Fatalf("p1 5h cost = %v, want 1.5", fiveH.CostUSD)
	}

	day, err := db.ProviderWindow("p1", ledger.Window1d, now)
	if err != nil {
		t.Fatalf("ProviderWindow 1d: %v", err)
	}
	if day.CostUSD != 3.5 {
		t.Fatalf("p1 1d cost = %v, want 3.5", day.CostUSD)
	}

	p2, err := db.ProviderWindow("p2", ledger.Window5h, now)
	if err != nil {
		t.Fatalf("ProviderWindow p2: %v", err)
	}
	if p2.CostUSD != 9 {
		t.Fatalf("p2 5h cost = %v, want 9", p2.CostUSD)
	}

	brain, err := db.ProviderAllTime("brain:typesafe")
	if err != nil {
		t.Fatalf("ProviderAllTime brain: %v", err)
	}
	if brain.CostUSD != 0 || brain.Requests != 0 {
		t.Fatalf("brain rows must be excluded: %+v", brain)
	}

	// Wider windows still include the day-old row.
	week, err := db.ProviderWindow("p1", ledger.Window7d, now)
	if err != nil {
		t.Fatalf("ProviderWindow 7d: %v", err)
	}
	if week.CostUSD != 3.5 {
		t.Fatalf("p1 7d cost = %v, want 3.5", week.CostUSD)
	}
	month, err := db.ProviderWindow("p1", ledger.Window30d, now)
	if err != nil {
		t.Fatalf("ProviderWindow 30d: %v", err)
	}
	if month.CostUSD != 3.5 {
		t.Fatalf("p1 30d cost = %v, want 3.5", month.CostUSD)
	}
}

func TestKeyAllTimeAPIVsSubscription(t *testing.T) {
	db := openTemp(t)
	now := time.Now().UTC()

	if err := db.Append(ledger.Record{
		ID: "a", TS: now, Session: "s", Path: "/v1/chat/completions",
		Provider: "p1", Model: "m", Status: 200, KeyID: "k1",
		CostUSD: floatPtr(3), Billing: "api",
	}); err != nil {
		t.Fatalf("Append api: %v", err)
	}
	if err := db.Append(ledger.Record{
		ID: "b", TS: now, Session: "s", Path: "/v1/chat/completions",
		Provider: "p1", Model: "m", Status: 200, KeyID: "k1",
		CostUSD: floatPtr(4), Billing: "subscription",
	}); err != nil {
		t.Fatalf("Append subscription: %v", err)
	}

	all, err := db.KeyAllTime("k1")
	if err != nil {
		t.Fatalf("KeyAllTime: %v", err)
	}
	if all.APIUsd != 3 {
		t.Fatalf("apiUsd = %v, want 3", all.APIUsd)
	}
	if all.SubscriptionUsd != 4 {
		t.Fatalf("subscriptionUsd = %v, want 4", all.SubscriptionUsd)
	}
	if all.CostUSD != 7 {
		t.Fatalf("costUsd = %v, want 7", all.CostUSD)
	}

	win, err := db.KeyWindow("k1", ledger.Window5h, now)
	if err != nil {
		t.Fatalf("KeyWindow: %v", err)
	}
	if win.CostUSD != 7 || win.Requests != 2 {
		t.Fatalf("KeyWindow = %+v, want cost 7 requests 2", win)
	}
}

func TestImportJSONL(t *testing.T) {
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "ledger.jsonl")
	content := `{"id":"r1","ts":"2026-10-04T11:30:00.000Z","session":"s","path":"/v1/chat/completions","provider":"p1","model":"m","stream":false,"status":200,"latencyMs":10,"promptTokens":1,"completionTokens":2,"cacheReadTokens":0,"cacheWriteTokens":0,"costUsd":1.5,"pricingKnown":true,"billing":"api","keyId":"k1"}
{"id":"r2","ts":"2026-10-04T11:00:00.000Z","session":"s","path":"/v1/chat/completions","provider":"p1","model":"m","stream":false,"status":200,"latencyMs":10,"promptTokens":1,"completionTokens":2,"cacheReadTokens":0,"cacheWriteTokens":0,"costUsd":2,"pricingKnown":true,"billing":"subscription","keyId":"k1"}
{"id":"brain1","ts":"2026-10-04T11:15:00.000Z","session":"s","path":"/brain","provider":"brain:x","model":"m","stream":false,"status":200,"latencyMs":1,"promptTokens":0,"completionTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0,"costUsd":50,"pricingKnown":false,"kind":"brain"}
not-json
`
	if err := os.WriteFile(jsonlPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	db := openTemp(t)
	n, err := db.ImportJSONL(jsonlPath)
	if err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}
	if n != 3 {
		t.Fatalf("imported = %d, want 3 (malformed line skipped)", n)
	}

	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	p1, err := db.ProviderWindow("p1", ledger.Window5h, now)
	if err != nil {
		t.Fatalf("ProviderWindow: %v", err)
	}
	if p1.CostUSD != 3.5 {
		t.Fatalf("imported p1 5h = %v, want 3.5", p1.CostUSD)
	}

	key, err := db.KeyAllTime("k1")
	if err != nil {
		t.Fatalf("KeyAllTime: %v", err)
	}
	if key.APIUsd != 1.5 || key.SubscriptionUsd != 2 || key.CostUSD != 3.5 {
		t.Fatalf("key totals = %+v, want api 1.5 sub 2 cost 3.5", key)
	}

	// Re-import is idempotent (INSERT OR IGNORE on primary key).
	n2, err := db.ImportJSONL(jsonlPath)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("re-import inserted %d, want 0", n2)
	}
}
