package admin_test

import (
	"math"
	"testing"

	"github.com/xinyao27/jevonian/internal/server/admin"
)

// The logs list carries no cache figure on its own: coverage is derived from a
// row's own accounting. This pins the definition the dashboard uses — cached
// reads over cached reads plus uncached input, the same as /stats cacheHitRate —
// and the "no accounting is not zero" rule.
func TestLogSeriesReportsCacheCoverage(t *testing.T) {
	source := &logs{rows: []admin.LogRecord{
		// Exclusive input (Anthropic/Connect-RPC): prompt is already uncached.
		{"id": "a", "ts": "2026-10-05T11:59:00.000Z", "kind": "request", "model": "m", "provider": "p", "status": 200, "promptTokens": 100, "cacheReadTokens": 300, "exclusiveInput": true},
		{"id": "b", "ts": "2026-10-05T11:59:30.000Z", "kind": "request", "model": "m", "provider": "p", "status": 200, "promptTokens": 100, "cacheReadTokens": 0, "exclusiveInput": true},
		// A turn with no token accounting must not read as a 0% miss.
		{"id": "c", "ts": "2026-10-05T11:59:45.000Z", "kind": "request", "model": "m", "provider": "p", "status": 200, "promptTokens": 0, "cacheReadTokens": 0},
	}}
	x := setup(t, source)
	code, out := request(t, x.h, "GET", "/logs/series?minutes=60&buckets=6", nil)
	checkStatus(t, code, 200, out)

	// 300 cached of 500 input across the window.
	window, ok := out["cacheCoverage"].(float64)
	if !ok || math.Abs(window-0.6) > 1e-6 {
		t.Fatalf("window coverage %#v", out["cacheCoverage"])
	}
	if out["cacheReadTokens"] != float64(300) || out["promptTokens"] != float64(200) {
		t.Fatalf("window totals %#v", out)
	}

	last := out["buckets"].([]any)[5].(map[string]any)
	if last["requests"] != float64(3) {
		t.Fatalf("bucket requests %#v", last)
	}
	got, ok := last["cacheCoverage"].(float64)
	if !ok || math.Abs(got-0.6) > 1e-6 {
		t.Fatalf("bucket coverage %#v", last["cacheCoverage"])
	}

	// A window whose rows carry no accounting reports null, never 0.
	code, out = request(t, x.h, "GET", "/logs/series?minutes=60&buckets=6&model=missing", nil)
	checkStatus(t, code, 200, out)
	if out["cacheCoverage"] != nil || out["cacheReadTokens"] != float64(0) || out["promptTokens"] != float64(0) {
		t.Fatalf("empty window %#v", out)
	}
	if out["buckets"].([]any)[5].(map[string]any)["cacheCoverage"] != nil {
		t.Fatalf("empty bucket %#v", out["buckets"].([]any)[5])
	}
}

// Inclusive input (OpenAI/Responses) counts cache reads inside promptTokens, so
// the uncached part is prompt minus cache. The same raw counts that read as
// 300/500 = 60% under exclusive convention read as 300/400 = 75% inclusive.
func TestLogSeriesHonorsInclusiveInput(t *testing.T) {
	source := &logs{rows: []admin.LogRecord{
		{"id": "a", "ts": "2026-10-05T11:59:00.000Z", "kind": "request", "model": "m", "provider": "p", "status": 200, "promptTokens": 400, "cacheReadTokens": 300, "exclusiveInput": false},
	}}
	x := setup(t, source)
	code, out := request(t, x.h, "GET", "/logs/series?minutes=60&buckets=6", nil)
	checkStatus(t, code, 200, out)

	window, ok := out["cacheCoverage"].(float64)
	if !ok || math.Abs(window-0.75) > 1e-6 {
		t.Fatalf("inclusive window coverage %#v", out["cacheCoverage"])
	}
}

// A row that predates the exclusive_input column keeps the historical reading:
// prompt_tokens is the uncached share, so 300 cached of 400 total input = 75%.
func TestLogSeriesDefaultsToExclusiveReading(t *testing.T) {
	source := &logs{rows: []admin.LogRecord{
		{"id": "a", "ts": "2026-10-05T11:59:00.000Z", "kind": "request", "model": "m", "provider": "p", "status": 200, "promptTokens": 100, "cacheReadTokens": 300},
	}}
	x := setup(t, source)
	code, out := request(t, x.h, "GET", "/logs/series?minutes=60&buckets=6", nil)
	checkStatus(t, code, 200, out)

	window, ok := out["cacheCoverage"].(float64)
	if !ok || math.Abs(window-0.75) > 1e-6 {
		t.Fatalf("default window coverage %#v", out["cacheCoverage"])
	}
}
