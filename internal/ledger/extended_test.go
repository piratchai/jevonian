package ledger_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
	_ "modernc.org/sqlite"
)

// ExtendSchema makes the ledger carry the TS-only row fields: the attempt
// waterfall (tries), withheld models (skipped), the cache estimate, cacheKeep
// and brainChannel. A clean first-try turn carries none of them.
func TestExtendSchemaStoresAttemptHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ExtendSchema(); err != nil {
		t.Fatal(err)
	}
	if err := db.ExtendSchema(); err != nil {
		t.Fatalf("ExtendSchema must be idempotent: %v", err)
	}
	ttft := 840
	if err := db.Append(ledger.Record{
		ID: "multi", TS: time.Now(), Provider: "p", Model: "m", Status: 200, Failovers: &one, Retries: &one, TTFTMs: &ttft,
		Tries:        json.RawMessage(`[{"provider":"a","model":"m","cause":"initial","status":429,"fail":"quota"},{"provider":"p","model":"m","cause":"failover","status":200}]`),
		Skipped:      json.RawMessage(`[{"model":"x","provider":"a","reason":"quota","detail":"spent"}]`),
		Cache:        json.RawMessage(`{"state":"hot"}`),
		CacheKeep:    "sticky",
		BrainChannel: "typesafe",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Append(ledger.Record{ID: "clean", TS: time.Now(), Provider: "p", Model: "m", Status: 200}); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var tries, skipped, cache, keep, channel sql.NullString
	if err := raw.QueryRow(`SELECT tries, skipped, cache, cache_keep, brain_channel FROM records WHERE id='multi'`).Scan(&tries, &skipped, &cache, &keep, &channel); err != nil {
		t.Fatal(err)
	}
	var waterfall []map[string]any
	if err := json.Unmarshal([]byte(tries.String), &waterfall); err != nil || len(waterfall) != 2 || waterfall[1]["cause"] != "failover" {
		t.Fatalf("tries = %q (%v)", tries.String, err)
	}
	if skipped.String == "" || cache.String != `{"state":"hot"}` || keep.String != "sticky" || channel.String != "typesafe" {
		t.Fatalf("extended columns = %v %v %v %v", skipped, cache, keep, channel)
	}
	var cleanTries, cleanKeep sql.NullString
	if err := raw.QueryRow(`SELECT tries, cache_keep FROM records WHERE id='clean'`).Scan(&cleanTries, &cleanKeep); err != nil {
		t.Fatal(err)
	}
	if cleanTries.Valid || cleanKeep.Valid {
		t.Fatalf("a clean first-try row must omit the fields: %v %v", cleanTries, cleanKeep)
	}
}

var one = 1

func TestExtendedImportKeepsTriesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "ledger.jsonl")
	line := `{"id":"r1","ts":"2026-10-04T11:30:00.000Z","session":"s","path":"/chat/completions","provider":"p1","model":"m","stream":true,"status":200,"latencyMs":10,"promptTokens":1,"completionTokens":2,"cacheReadTokens":0,"cacheWriteTokens":0,"costUsd":1.5,"pricingKnown":true,"failovers":1,"ttftMs":321,"tries":[{"provider":"a","cause":"initial","status":500},{"provider":"p1","cause":"failover","status":200}],"cacheKeep":"sticky","cache":{"state":"cold"}}` + "\n"
	if err := os.WriteFile(jsonl, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(dir, "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ExtendSchema(); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ImportJSONL(jsonl); err != nil || n != 1 {
		t.Fatalf("import = %d, %v", n, err)
	}
	if n, err := db.ImportJSONL(jsonl); err != nil || n != 0 {
		t.Fatalf("re-import = %d, %v", n, err)
	}
	raw, _ := sql.Open("sqlite", filepath.Join(dir, "l.db"))
	defer raw.Close()
	var tries, keep sql.NullString
	var ttft sql.NullInt64
	if err := raw.QueryRow(`SELECT tries, cache_keep, ttft_ms FROM records WHERE id='r1'`).Scan(&tries, &keep, &ttft); err != nil {
		t.Fatal(err)
	}
	if tries.String == "" || keep.String != "sticky" || ttft.Int64 != 321 {
		t.Fatalf("imported = %v %v %v", tries, keep, ttft)
	}
}
