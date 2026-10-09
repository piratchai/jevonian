package ledger_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
	_ "modernc.org/sqlite"
)

// A row written before exclusive_input existed (NULL) must be labeled from the
// provider's serving wire, without touching rows that already recorded theirs.
func TestBackfillExclusiveInputLabelsLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	rows := []ledger.Record{
		{ID: "openai-1", TS: ts, Session: "s", Provider: "chatgpt-subscription", Model: "gpt-6.1-sol", Status: 200, PromptTokens: 1000, CacheReadTokens: 800},
		{ID: "openai-2", TS: ts, Session: "s", Provider: "chatgpt-subscription", Model: "gpt-6.1-sol", Status: 200, PromptTokens: 500, CacheReadTokens: 100},
		{ID: "anthropic-1", TS: ts, Session: "s", Provider: "claude-subscription", Model: "claude-opus-5-5", Status: 200, PromptTokens: 300, CacheReadTokens: 900},
		{ID: "brain", TS: ts, Session: "s", Provider: "chatgpt-subscription", Model: "gpt-6.1-sol", Status: 200, Kind: "brain", PromptTokens: 10},
	}
	for _, r := range rows {
		if err := db.Append(r); err != nil {
			t.Fatalf("Append(%s): %v", r.ID, err)
		}
	}
	// Pre-existing labels are authoritative: the openai-2 row already recorded
	// its own convention.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE records SET exclusive_input = 1 WHERE id = 'openai-2'`); err != nil {
		t.Fatalf("seed label: %v", err)
	}

	classify := func(provider, model string) (bool, bool) {
		switch provider {
		case "chatgpt-subscription":
			return false, true
		case "claude-subscription":
			return true, true
		default:
			return false, false
		}
	}
	updated, err := db.BackfillExclusiveInput(classify)
	if err != nil {
		t.Fatalf("BackfillExclusiveInput: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2 (openai-1 + anthropic-1)", updated)
	}

	want := map[string]*int{"openai-1": intPtr(0), "anthropic-1": intPtr(1), "openai-2": intPtr(1), "brain": nil}
	for id, label := range want {
		got := readExclusive(t, raw, id)
		if (label == nil) != (got == nil) || (label != nil && *got != *label) {
			t.Fatalf("row %s exclusive_input = %v, want %v", id, deref(got), deref(label))
		}
	}

	// Idempotent: a second pass changes nothing.
	again, err := db.BackfillExclusiveInput(classify)
	if err != nil {
		t.Fatalf("second BackfillExclusiveInput: %v", err)
	}
	if again != 0 {
		t.Fatalf("second pass updated = %d, want 0", again)
	}
}

func TestBackfillExclusiveInputNilClassifier(t *testing.T) {
	db := openTemp(t)
	n, err := db.BackfillExclusiveInput(nil)
	if err != nil || n != 0 {
		t.Fatalf("nil classifier = (%d,%v), want (0,nil)", n, err)
	}
}

func readExclusive(t *testing.T, raw *sql.DB, id string) *int {
	t.Helper()
	var v *int
	if err := raw.QueryRow(`SELECT exclusive_input FROM records WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return v
}

func intPtr(v int) *int { return &v }

func deref(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
