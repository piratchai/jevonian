package ledger

import "fmt"

// Totals is the all-rows rollup behind the legacy GET /stats endpoint.
type Totals struct {
	Requests         int64
	CostUSD          float64
	CacheReadTokens  int64
	PromptTokens     int64
	CompletionTokens int64
}

// Totals sums every row (request and brain kinds alike), matching
// readRecords().reduce(...) in src/server.ts GET /stats.
func (d *DB) Totals() (Totals, error) {
	var t Totals
	err := d.read.QueryRow(`
SELECT
	COUNT(*),
	COALESCE(SUM(COALESCE(cost_usd, 0)), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(prompt_tokens), 0),
	COALESCE(SUM(completion_tokens), 0)
FROM records`).Scan(&t.Requests, &t.CostUSD, &t.CacheReadTokens, &t.PromptTokens, &t.CompletionTokens)
	if err != nil {
		return Totals{}, fmt.Errorf("ledger: totals: %w", err)
	}
	return t, nil
}
