package admin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const logReadBatch = 256

func init() {
	// SQLite's built-in lower() only folds ASCII; use the existing Go matcher
	// semantics for non-ASCII provider, session and reason searches too.
	sqlite.MustRegisterDeterministicScalarFunction("jevonian_admin_lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		s, _ := args[0].(string)
		return strings.ToLower(s), nil
	})
}

func quoteColumn(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (s *SQLiteLogs) prepare() error {
	rows, err := s.db.Query("SELECT * FROM records LIMIT 0")
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	rows.Close()
	if err != nil {
		return err
	}
	s.columns = make(map[string]bool, len(columns))
	projection := make([]string, 0, len(columns))
	for _, col := range columns {
		s.columns[col] = true
		projection = append(projection, quoteColumn(col))
	}
	s.projection = strings.Join(projection, ", ")
	// These indexes complement the writer's provider/key indexes. No data is
	// copied and the read API remains compatible with extended TS-era schemas.
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_admin_records_ts ON records(ts_ms);
		CREATE INDEX IF NOT EXISTS idx_admin_records_request_kind ON records(request_id, kind);
		CREATE INDEX IF NOT EXISTS idx_admin_records_model ON records(model) WHERE kind != 'brain';
		CREATE INDEX IF NOT EXISTS idx_admin_records_phase ON records(COALESCE(NULLIF(phase, ''), '-')) WHERE kind != 'brain';`)
	return err
}

func (s *SQLiteLogs) filterSQL(f LogFilter) (string, []any) {
	clauses := []string{"kind != 'brain'"}
	args := []any{}
	if f.Phase != "" && f.Phase != "all" {
		clauses = append(clauses, "COALESCE(NULLIF(phase, ''), '-') = ?")
		args = append(args, f.Phase)
	}
	if f.Model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, f.Model)
	}
	if f.Query != "" {
		// Match the joined search text, not OR-ed per-field LIKE expressions:
		// phrases may cross fields, and '%' / '_' are literal substrings.
		search := "COALESCE(model, '') || ' ' || COALESCE(provider, '') || ' ' || COALESCE(phase, '') || ' ' || COALESCE(session, '') || ' ' || COALESCE(reason, '') || ' ' || COALESCE(effort, '')"
		search += " || CASE WHEN COALESCE(retries, 0) != 0 THEN ' retry' ELSE '' END"
		search += " || CASE WHEN COALESCE(failovers, 0) != 0 THEN ' failover' ELSE '' END"
		if s.columns["tries"] {
			search += ` || COALESCE((SELECT ' ' || group_concat(cause, ' ') FROM
				(SELECT CASE WHEN type = 'object' THEN json_extract(value, '$.cause') END AS cause
				 FROM json_each(CASE WHEN json_valid(tries) AND json_type(tries) = 'array' THEN tries ELSE '[]' END) ORDER BY key)
				WHERE cause IN ('retry', 'failover')), '')`
		}
		clauses = append(clauses, "instr(jevonian_admin_lower("+search+"), ?) > 0")
		args = append(args, strings.ToLower(f.Query))
	}
	return strings.Join(clauses, " AND "), args
}

func queryLogRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]LogRecord, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return decodeLogRows(rows)
}

func (s *SQLiteLogs) QueryLogs(ctx context.Context, q LogQuery) (LogPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	out := LogPage{Logs: []LogRecord{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	where, args := s.filterSQL(q.Filter)
	var first sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), MIN(rowid) FROM records WHERE "+where, args...).Scan(&out.Total, &first); err != nil {
		return out, err
	}
	if q.Before != nil {
		if *q.Before <= 0 {
			return out, tx.Commit()
		}
		// Public cursors rank *all* appended rows, including brain rows. OFFSET
		// translates the append offset even when SQLite rowids contain gaps.
		var boundary int64
		err := tx.QueryRowContext(ctx, "SELECT rowid FROM records ORDER BY rowid LIMIT 1 OFFSET ?", *q.Before).Scan(&boundary)
		if err != nil && err != sql.ErrNoRows {
			return out, err
		}
		if err == nil {
			where += " AND rowid < ?"
			args = append(args, boundary)
		}
	}
	rows, err := queryLogRows(ctx, tx, "SELECT "+s.projection+", rowid AS __admin_rowid FROM records WHERE "+where+" ORDER BY rowid DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return out, err
	}
	var oldest int64
	for _, rec := range rows {
		oldest = rec["__admin_rowid"].(int64)
		delete(rec, "__admin_rowid")
	}
	out.Logs = rows
	if len(rows) > 0 && first.Valid && first.Int64 < oldest {
		var offset int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM records WHERE rowid < ?", oldest).Scan(&offset); err != nil {
			return out, err
		}
		out.NextBefore = &offset
	}
	return out, tx.Commit()
}

func (s *SQLiteLogs) LogDetail(ctx context.Context, id string) (LogRecord, []LogRecord, error) {
	rows, err := queryLogRows(ctx, s.db, "SELECT "+s.projection+" FROM records WHERE id = ? LIMIT 1", id)
	if err != nil || len(rows) == 0 {
		return nil, []LogRecord{}, err
	}
	// Brain calls use an indexed request lookup and bounded cursor batches.
	// Capture the high-water mark so concurrent appends cannot prolong detail.
	upper, err := s.TailCursor(ctx)
	if err != nil {
		return nil, nil, err
	}
	calls := []LogRecord{}
	cursor := int64(-1)
	for {
		batch, err := queryLogRows(ctx, s.db, "SELECT "+s.projection+", rowid AS __admin_rowid FROM records INDEXED BY idx_admin_records_request_kind WHERE request_id = ? AND kind = 'brain' AND rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?", id, cursor, upper, logReadBatch)
		if err != nil {
			return nil, nil, err
		}
		for _, rec := range batch {
			cursor = rec["__admin_rowid"].(int64)
			delete(rec, "__admin_rowid")
			calls = append(calls, rec)
		}
		if len(batch) < logReadBatch {
			break
		}
	}
	return rows[0], calls, nil
}

func (s *SQLiteLogs) FirstLog(ctx context.Context) (LogRecord, error) {
	rows, err := queryLogRows(ctx, s.db, "SELECT "+s.projection+" FROM records ORDER BY rowid LIMIT 1")
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (s *SQLiteLogs) ScanLogRange(ctx context.Context, r LogRange, visit func(LogRecord) error) error {
	upper, err := s.TailCursor(ctx)
	if err != nil {
		return err
	}
	where := "rowid <= ?"
	args := []any{upper}
	if !r.Start.IsZero() {
		where += " AND ts_ms >= ?"
		args = append(args, r.Start.UnixMilli())
	}
	if !r.End.IsZero() {
		where += " AND ts_ms <= ?"
		args = append(args, r.End.UnixMilli())
	}
	if r.RequestsOnly {
		where += " AND kind != 'brain'"
	}
	// Range-index selection is explicit: ORDER BY rowid would otherwise let
	// SQLite choose a full append-order table scan. Sorting preserves first-seen
	// names, model variants and TS per-record rounding for backdated appends.
	index := ""
	if !r.Start.IsZero() || !r.End.IsZero() {
		index = " INDEXED BY idx_admin_records_ts"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+s.projection+" FROM records"+index+" WHERE "+where+" ORDER BY rowid", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := decodeLogRow(rows, columns)
		if err != nil {
			return err
		}
		if err := visit(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *SQLiteLogs) QueryLogSeries(ctx context.Context, filter LogFilter, start, end time.Time, count int) ([]LogBucket, error) {
	if count < 1 || count > 120 || !end.After(start) {
		return nil, fmt.Errorf("invalid log series range")
	}
	where, args := s.filterSQL(filter)
	width := float64(end.Sub(start).Milliseconds()) / float64(count)
	query := `SELECT MIN(?, CAST((ts_ms - ?) / ? AS INTEGER)) AS slot,
		COUNT(*), SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),
		COALESCE(SUM(cost_usd), 0), COALESCE(SUM(latency_ms), 0)
		FROM records INDEXED BY idx_admin_records_ts WHERE ` + where + ` AND ts_ms >= ? AND ts_ms <= ? GROUP BY slot`
	params := []any{count - 1, start.UnixMilli(), width}
	params = append(params, args...)
	params = append(params, start.UnixMilli(), end.UnixMilli())
	rows, err := s.db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LogBucket, count)
	for rows.Next() {
		var slot int
		var b LogBucket
		if err := rows.Scan(&slot, &b.Requests, &b.Errors, &b.CostUSD, &b.LatencyMS); err != nil {
			return nil, err
		}
		if slot >= 0 && slot < count {
			out[slot] = b
		}
	}
	return out, rows.Err()
}

func (s *SQLiteLogs) TailCursor(ctx context.Context) (int64, error) {
	var cursor int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid), 0) FROM records").Scan(&cursor)
	return cursor, err
}

func (s *SQLiteLogs) ReadAfter(ctx context.Context, cursor int64, limit int) (LogBatch, error) {
	out := LogBatch{Logs: []LogRecord{}, Cursor: cursor}
	if limit <= 0 || limit > logReadBatch {
		limit = logReadBatch
	}
	upper, err := s.TailCursor(ctx)
	if err != nil {
		return out, err
	}
	if upper < cursor {
		out.Reset = true
		out.Cursor = upper
		return out, nil
	}
	if upper == cursor {
		return out, nil
	}
	rows, err := queryLogRows(ctx, s.db, "SELECT "+s.projection+", rowid AS __admin_rowid FROM records WHERE rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?", cursor, upper, limit)
	if err != nil {
		return out, err
	}
	for _, rec := range rows {
		out.Cursor = rec["__admin_rowid"].(int64)
		delete(rec, "__admin_rowid")
	}
	out.Logs = rows
	return out, nil
}
