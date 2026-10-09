package ledger

import (
	"fmt"
	"time"
)

// ConventionClassifier decides whether one ledger row's stored prompt tokens
// already exclude cache reads, given its provider, model, client path, stream
// flag, and the instant it was written. ok=false leaves the row's convention
// unset. It is a function rather than a config lookup so the ledger package
// stays free of routing knowledge.
type ConventionClassifier func(provider, model, path string, stream bool, at time.Time) (exclusive bool, ok bool)

// ReconcileExclusiveInput labels every row with the cache convention it was
// actually served with, so cache coverage divides cached reads by the right
// denominator. Coverage = cacheRead / (cacheRead + uncachedInput), and the
// uncached half depends on the convention.
//
// Two kinds of row need work, so this runs once per startup and is idempotent:
//
//   - Rows written before `exclusive_input` existed carry NULL. Their
//     convention comes from the pre-cutover provider behavior.
//   - Connect-RPC rows written after the Go cutover were labeled exclusive
//     while the Go egress actually renders an OpenAI-shaped inclusive prompt
//     count. Reading them as exclusive understates coverage roughly by half.
//
// cutover is the instant the Go engine replaced the legacy runtime; a zero
// value treats every row as Go-era. Each era is one bulk pass keyed by
// (provider, model, path, stream), so the work is a few set-based updates, not
// one statement per row. Only rows whose stored label disagrees with the
// derived one are touched. Returns how many rows were updated.
func (d *DB) ReconcileExclusiveInput(cutover time.Time, classify ConventionClassifier) (int, error) {
	if classify == nil {
		return 0, nil
	}
	updated := 0
	for _, era := range conventionEras(cutover) {
		keys, err := d.conventionKeys(era.where)
		if err != nil {
			return updated, err
		}
		stmt, err := d.db.Prepare(`UPDATE records SET exclusive_input = ?
			WHERE provider = ? AND model = ? AND path = ? AND stream = ? AND kind != 'brain'
			AND ` + era.where + ` AND (prompt_tokens > 0 OR cache_read_tokens > 0)
			AND (exclusive_input IS NULL OR exclusive_input != ?)`)
		if err != nil {
			return updated, fmt.Errorf("ledger: reconcile prepare: %w", err)
		}
		for _, k := range keys {
			exclusive, ok := classify(k.provider, k.model, k.path, k.stream != 0, era.at)
			if !ok {
				continue
			}
			res, err := stmt.Exec(boolToInt(exclusive), k.provider, k.model, k.path, k.stream, boolToInt(exclusive))
			if err != nil {
				stmt.Close()
				return updated, fmt.Errorf("ledger: reconcile exclusive_input: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil {
				updated += int(n)
			}
		}
		stmt.Close()
	}
	return updated, nil
}

// conventionEra bounds one side of the cutover so a pass selects exactly its
// rows, and samples an instant the classifier can answer for.
type conventionEra struct {
	where string
	at    time.Time
}

func conventionEras(cutover time.Time) []conventionEra {
	if cutover.IsZero() {
		return []conventionEra{{where: `1 = 1`, at: time.Now().UTC()}}
	}
	ms := cutover.UTC().UnixMilli()
	return []conventionEra{
		{where: fmt.Sprintf(`ts_ms < %d`, ms), at: cutover.UTC().Add(-time.Nanosecond)},
		{where: fmt.Sprintf(`ts_ms >= %d`, ms), at: cutover.UTC()},
	}
}

// conventionKey is one distinct row shape whose convention is uniform.
type conventionKey struct {
	provider string
	model    string
	path     string
	stream   int
}

func (d *DB) conventionKeys(where string) ([]conventionKey, error) {
	rows, err := d.db.Query(`SELECT provider, model, path, stream FROM records
		WHERE kind != 'brain' AND ` + where + `
		AND (prompt_tokens > 0 OR cache_read_tokens > 0)
		GROUP BY provider, model, path, stream`)
	if err != nil {
		return nil, fmt.Errorf("ledger: convention keys: %w", err)
	}
	defer rows.Close()
	var out []conventionKey
	for rows.Next() {
		var k conventionKey
		if err := rows.Scan(&k.provider, &k.model, &k.path, &k.stream); err != nil {
			return nil, fmt.Errorf("ledger: convention key scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
