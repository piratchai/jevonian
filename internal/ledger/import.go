package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// jsonlRecord mirrors the TypeScript LedgerRecord JSON shape for migration.
type jsonlRecord struct {
	ID               string   `json:"id"`
	RequestID        string   `json:"requestId"`
	TS               string   `json:"ts"`
	Session          string   `json:"session"`
	Path             string   `json:"path"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	Stream           bool     `json:"stream"`
	Status           int      `json:"status"`
	LatencyMs        int      `json:"latencyMs"`
	PromptTokens     int      `json:"promptTokens"`
	CompletionTokens int      `json:"completionTokens"`
	CacheReadTokens  int      `json:"cacheReadTokens"`
	CacheWriteTokens int      `json:"cacheWriteTokens"`
	CostUSD          *float64 `json:"costUsd"`
	PricingKnown     bool     `json:"pricingKnown"`
	Kind             string   `json:"kind"`
	Billing          string   `json:"billing"`
	RequestedModel   string   `json:"requestedModel"`
	Phase            string   `json:"phase"`
	Routed           *bool    `json:"routed"`
	Reason           string   `json:"reason"`
	Brain            string   `json:"brain"`
	Confidence       *float64 `json:"confidence"`
	Canonical        string   `json:"canonical"`
	Effort           string   `json:"effort"`
	EffortNote       string   `json:"effortNote"`
	SavedTokens      *int     `json:"savedTokens"`
	Retries          *int     `json:"retries"`
	Failovers        *int     `json:"failovers"`
	TTFTMs           *int     `json:"ttftMs"`
	Error            string   `json:"error"`
	KeyID            string   `json:"keyId"`
	KeyName          string   `json:"keyName"`
	SwitchPenaltyUSD *float64 `json:"switchPenaltyUsd"`

	// Extended TS fields; stored only after ExtendSchema.
	Tries        json.RawMessage `json:"tries"`
	Skipped      json.RawMessage `json:"skipped"`
	Cache        json.RawMessage `json:"cache"`
	CacheKeep    string          `json:"cacheKeep"`
	BrainChannel string          `json:"brainChannel"`
}

// ImportJSONL migrates an existing ledger.jsonl into the SQLite ledger.
// Malformed lines are skipped (same as the TypeScript parser). Returns the
// number of rows successfully inserted.
func (d *DB) ImportJSONL(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("ledger: import open: %w", err)
	}
	defer f.Close()

	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("ledger: import begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
INSERT OR IGNORE INTO records (
	id, request_id, ts_ms, ts, session, path, provider, model, stream, status, latency_ms,
	prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens,
	cost_usd, pricing_known, kind, billing, requested_model, phase, routed, reason,
	brain, confidence, canonical, effort, effort_note, saved_tokens, retries, failovers,
	ttft_ms, error, key_id, key_name, switch_penalty_usd
) VALUES (
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?
)`)
	if err != nil {
		return 0, fmt.Errorf("ledger: import prepare: %w", err)
	}
	defer stmt.Close()

	inserted := 0
	scanner := bufio.NewScanner(f)
	// Large ledger lines can exceed the default 64 KiB token.
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw jsonlRecord
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		rec, ok := raw.toRecord()
		if !ok {
			continue
		}
		if d.extended {
			res, err := insertExtended(tx, rec, rec.TS, true)
			if err != nil {
				return inserted, fmt.Errorf("ledger: import insert: %w", err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted++
			}
			continue
		}
		res, err := stmt.Exec(
			rec.ID,
			rec.RequestID,
			rec.TS.UnixMilli(),
			rec.TS.Format(time.RFC3339Nano),
			rec.Session,
			rec.Path,
			rec.Provider,
			rec.Model,
			boolToInt(rec.Stream),
			rec.Status,
			rec.LatencyMs,
			rec.PromptTokens,
			rec.CompletionTokens,
			rec.CacheReadTokens,
			rec.CacheWriteTokens,
			nullFloat(rec.CostUSD),
			boolToInt(rec.PricingKnown),
			rec.Kind,
			rec.Billing,
			rec.RequestedModel,
			rec.Phase,
			nullBool(rec.Routed),
			rec.Reason,
			rec.Brain,
			nullFloat(rec.Confidence),
			rec.Canonical,
			rec.Effort,
			rec.EffortNote,
			nullInt(rec.SavedTokens),
			nullInt(rec.Retries),
			nullInt(rec.Failovers),
			nullInt(rec.TTFTMs),
			rec.Error,
			rec.KeyID,
			rec.KeyName,
			nullFloat(rec.SwitchPenaltyUSD),
		)
		if err != nil {
			return inserted, fmt.Errorf("ledger: import insert: %w", err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			inserted++
		}
	}
	if err := scanner.Err(); err != nil {
		return inserted, fmt.Errorf("ledger: import scan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("ledger: import commit: %w", err)
	}
	return inserted, nil
}

func (raw jsonlRecord) toRecord() (Record, bool) {
	ts, err := time.Parse(time.RFC3339Nano, raw.TS)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, raw.TS)
		if err != nil {
			return Record{}, false
		}
	}
	id := raw.ID
	if id == "" {
		id = uuidFromImport(raw)
	}
	return Record{
		ID:               id,
		RequestID:        raw.RequestID,
		TS:               ts.UTC(),
		Session:          raw.Session,
		Path:             raw.Path,
		Provider:         raw.Provider,
		Model:            raw.Model,
		Stream:           raw.Stream,
		Status:           raw.Status,
		LatencyMs:        raw.LatencyMs,
		PromptTokens:     raw.PromptTokens,
		CompletionTokens: raw.CompletionTokens,
		CacheReadTokens:  raw.CacheReadTokens,
		CacheWriteTokens: raw.CacheWriteTokens,
		CostUSD:          raw.CostUSD,
		PricingKnown:     raw.PricingKnown,
		Kind:             raw.Kind,
		Billing:          raw.Billing,
		RequestedModel:   raw.RequestedModel,
		Phase:            raw.Phase,
		Routed:           raw.Routed,
		Reason:           raw.Reason,
		Brain:            raw.Brain,
		Confidence:       raw.Confidence,
		Canonical:        raw.Canonical,
		Effort:           raw.Effort,
		EffortNote:       raw.EffortNote,
		SavedTokens:      raw.SavedTokens,
		Retries:          raw.Retries,
		Failovers:        raw.Failovers,
		TTFTMs:           raw.TTFTMs,
		Error:            raw.Error,
		KeyID:            raw.KeyID,
		KeyName:          raw.KeyName,
		SwitchPenaltyUSD: raw.SwitchPenaltyUSD,
		Tries:            raw.Tries,
		Skipped:          raw.Skipped,
		Cache:            raw.Cache,
		CacheKeep:        raw.CacheKeep,
		BrainChannel:     raw.BrainChannel,
	}, true
}

// uuidFromImport generates a stable-enough ID when the JSONL row has none.
func uuidFromImport(raw jsonlRecord) string {
	// Prefer requestId when present so re-import is idempotent via INSERT OR IGNORE.
	if raw.RequestID != "" {
		return "req:" + raw.RequestID
	}
	return fmt.Sprintf("import:%s:%s:%s", raw.TS, raw.Provider, raw.Session)
}
