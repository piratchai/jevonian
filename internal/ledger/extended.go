package ledger

import (
	"database/sql"
	"fmt"
	"time"
)

// extendedColumns are the TS LedgerRecord fields the base schema does not hold.
// The admin log reader already understands these exact column names (tries,
// skipped and cache hold JSON text; cache_keep and brain_channel hold text).
var extendedColumns = []struct{ name, typ string }{
	{"tries", "TEXT"},
	{"skipped", "TEXT"},
	{"cache", "TEXT"},
	{"cache_keep", "TEXT"},
	{"brain_channel", "TEXT"},
	{"exclusive_input", "INTEGER"},
}

// ExtendSchema adds the extended TS-ledger columns (tries, skipped, cache,
// cache_keep, brain_channel) and makes Append write them. It is idempotent and
// safe on a ledger that already has some or all of the columns.
//
// It is opt-in rather than part of Open so existing callers and fixtures that
// add these columns themselves keep working; the serve path calls it once at
// startup.
func (d *DB) ExtendSchema() error {
	have, err := d.columnSet()
	if err != nil {
		return err
	}
	for _, c := range extendedColumns {
		if have[c.name] {
			continue
		}
		if _, err := d.db.Exec(fmt.Sprintf("ALTER TABLE records ADD COLUMN %s %s", c.name, c.typ)); err != nil {
			return fmt.Errorf("ledger: add column %s: %w", c.name, err)
		}
	}
	d.extended = true
	return nil
}

func (d *DB) columnSet() (map[string]bool, error) {
	rows, err := d.db.Query(`SELECT name FROM pragma_table_info('records')`)
	if err != nil {
		return nil, fmt.Errorf("ledger: table info: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

func nullJSON(raw []byte) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (d *DB) appendExtended(record Record, ts time.Time) error {
	_, err := insertExtended(d.db, record, ts, false)
	return err
}

// insertExtended writes one row including the extended columns. ignoreDup makes
// a repeated id a no-op (INSERT OR IGNORE), which keeps JSONL re-import idempotent.
func insertExtended(x execer, record Record, ts time.Time, ignoreDup bool) (sql.Result, error) {
	verb := "INSERT"
	if ignoreDup {
		verb = "INSERT OR IGNORE"
	}
	res, err := x.Exec(verb+` INTO records (
	id, request_id, ts_ms, ts, session, path, provider, model, stream, status, latency_ms,
	prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens,
	cost_usd, pricing_known, kind, billing, requested_model, phase, routed, reason,
	brain, confidence, canonical, effort, effort_note, saved_tokens, retries, failovers,
	ttft_ms, error, key_id, key_name, switch_penalty_usd,
	tries, skipped, cache, cache_keep, brain_channel, exclusive_input
) VALUES (
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?
)`,
		record.ID, record.RequestID, ts.UnixMilli(), ts.Format(time.RFC3339Nano), record.Session, record.Path,
		record.Provider, record.Model, boolToInt(record.Stream), record.Status, record.LatencyMs,
		record.PromptTokens, record.CompletionTokens, record.CacheReadTokens, record.CacheWriteTokens,
		nullFloat(record.CostUSD), boolToInt(record.PricingKnown), record.Kind, record.Billing,
		record.RequestedModel, record.Phase, nullBool(record.Routed), record.Reason,
		record.Brain, nullFloat(record.Confidence), record.Canonical, record.Effort, record.EffortNote,
		nullInt(record.SavedTokens), nullInt(record.Retries), nullInt(record.Failovers),
		nullInt(record.TTFTMs), record.Error, record.KeyID, record.KeyName, nullFloat(record.SwitchPenaltyUSD),
		nullJSON(record.Tries), nullJSON(record.Skipped), nullJSON(record.Cache),
		nullText(record.CacheKeep), nullText(record.BrainChannel), nullBool(record.ExclusiveInput),
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: append: %w", err)
	}
	return res, nil
}
