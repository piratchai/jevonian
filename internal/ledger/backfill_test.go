package ledger_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
	_ "modernc.org/sqlite"
)

// Rows written before exclusive_input existed (NULL) must be labeled from the
// provider's serving wire, and rows the Go cutover mislabeled must be corrected,
// without touching rows that are already right.
func TestReconcileExclusiveInputLabelsAndCorrectsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cutover := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	legacy := cutover.Add(-time.Hour)
	modern := cutover.Add(time.Hour)

	rows := []ledger.Record{
		// OpenAI wire counts cache reads inside prompt_tokens: inclusive.
		{ID: "openai-legacy", TS: legacy, Session: "s", Provider: "chatgpt-subscription", Model: "gpt-6.1-sol", Path: "/chat/completions", Stream: true, Status: 200, PromptTokens: 1000, CacheReadTokens: 800},
		// Anthropic Messages input_tokens is the uncached share: exclusive.
		{ID: "anthropic-legacy", TS: legacy, Session: "s", Provider: "claude-subscription", Model: "claude-opus-5-5", Path: "/messages", Stream: true, Status: 200, PromptTokens: 300, CacheReadTokens: 900},
		// A brain row carries no usage convention.
		{ID: "brain", TS: legacy, Session: "s", Provider: "chatgpt-subscription", Model: "gpt-6.1-sol", Path: "/responses", Status: 200, Kind: "brain", PromptTokens: 10},
		// The Go engine labeled this Connect-RPC row exclusive, but its egress
		// renders an inclusive prompt count: it must be corrected to inclusive.
		{ID: "devin-go", TS: modern, Session: "s", Provider: "devin-subscription", Model: "swe-2-max", Path: "/chat/completions", Stream: true, Status: 200, PromptTokens: 36223, CacheReadTokens: 24128},
		// A legacy Connect-RPC row was exclusive and must stay exclusive.
		{ID: "devin-legacy", TS: legacy, Session: "s", Provider: "devin-subscription", Model: "swe-2-max", Path: "/chat/completions", Stream: true, Status: 200, PromptTokens: 685, CacheReadTokens: 118497},
	}
	for _, r := range rows {
		if err := db.Append(r); err != nil {
			t.Fatalf("Append(%s): %v", r.ID, err)
		}
	}
	// Seed the wrong Go-era label the old code wrote, and an already-correct row.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE records SET exclusive_input = 1 WHERE id = 'devin-go'`); err != nil {
		t.Fatalf("seed devin-go: %v", err)
	}
	if _, err := raw.Exec(`UPDATE records SET exclusive_input = 0 WHERE id = 'openai-legacy'`); err != nil {
		t.Fatalf("seed openai-legacy: %v", err)
	}

	classify := func(provider, model, path string, stream bool, at time.Time) (bool, bool) {
		switch provider {
		case "chatgpt-subscription":
			return false, true
		case "claude-subscription":
			return true, true
		case "devin-subscription":
			return at.Before(cutover), true
		default:
			return false, false
		}
	}
	updated, err := db.ReconcileExclusiveInput(cutover, classify)
	if err != nil {
		t.Fatalf("ReconcileExclusiveInput: %v", err)
	}
	// anthropic-legacy (NULL->1), devin-go (1->0), devin-legacy (NULL->1).
	if updated != 3 {
		t.Fatalf("updated = %d, want 3", updated)
	}

	want := map[string]*int{
		"openai-legacy":    intPtr(0),
		"anthropic-legacy": intPtr(1),
		"devin-go":         intPtr(0),
		"devin-legacy":     intPtr(1),
		"brain":            nil,
	}
	for id, label := range want {
		got := readExclusive(t, raw, id)
		if (label == nil) != (got == nil) || (label != nil && *got != *label) {
			t.Fatalf("row %s exclusive_input = %v, want %v", id, deref(got), deref(label))
		}
	}

	// Idempotent: a second pass changes nothing.
	again, err := db.ReconcileExclusiveInput(cutover, classify)
	if err != nil {
		t.Fatalf("second ReconcileExclusiveInput: %v", err)
	}
	if again != 0 {
		t.Fatalf("second pass updated = %d, want 0", again)
	}
}

func TestReconcileExclusiveInputNilClassifier(t *testing.T) {
	db := openTemp(t)
	n, err := db.ReconcileExclusiveInput(time.Now(), nil)
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
