package ledger

import "fmt"

// ConventionClassifier decides whether one ledger row's usage counted prompt
// tokens as already excluding cache reads (Anthropic and the Connect-RPC
// hosts), or as including them (OpenAI and Responses). ok=false leaves the
// row's convention unset so it keeps the historical reading.
// It is a function rather than a config lookup so the ledger package stays
// free of routing knowledge.
type ConventionClassifier func(provider, model string) (exclusive bool, ok bool)

// BackfillExclusiveInput fills exclusive_input on rows written before the
// column existed. Cache coverage divides cached reads by cached plus uncached
// input, and the uncached half depends on the serving wire's convention: an
// OpenAI or Responses row counts cache reads inside prompt_tokens, so reading
// its prompt_tokens as already-uncached understates coverage roughly by half.
//
// Only NULL rows change, so the call is idempotent and a row that recorded its
// own convention is never overridden. Returns how many rows were updated.
func (d *DB) BackfillExclusiveInput(classify ConventionClassifier) (int, error) {
	if classify == nil {
		return 0, nil
	}
	targets, err := d.rowsMissingConvention()
	if err != nil {
		return 0, err
	}
	updated := 0
	stmt, err := d.db.Prepare(`UPDATE records SET exclusive_input = ?
		WHERE exclusive_input IS NULL AND kind != 'brain' AND provider = ? AND model = ?
		AND (prompt_tokens > 0 OR cache_read_tokens > 0)`)
	if err != nil {
		return 0, fmt.Errorf("ledger: backfill prepare: %w", err)
	}
	defer stmt.Close()
	for _, t := range targets {
		exclusive, ok := classify(t.provider, t.model)
		if !ok {
			continue
		}
		res, err := stmt.Exec(boolToInt(exclusive), t.provider, t.model)
		if err != nil {
			return updated, fmt.Errorf("ledger: backfill exclusive_input: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			updated += int(n)
		}
	}
	return updated, nil
}

type conventionTarget struct {
	provider string
	model    string
}

func (d *DB) rowsMissingConvention() ([]conventionTarget, error) {
	rows, err := d.db.Query(`SELECT provider, model FROM records
		WHERE exclusive_input IS NULL AND kind != 'brain'
		AND (prompt_tokens > 0 OR cache_read_tokens > 0)
		GROUP BY provider, model`)
	if err != nil {
		return nil, fmt.Errorf("ledger: convention targets: %w", err)
	}
	defer rows.Close()
	var out []conventionTarget
	for rows.Next() {
		var t conventionTarget
		if err := rows.Scan(&t.provider, &t.model); err != nil {
			return nil, fmt.Errorf("ledger: convention target scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
