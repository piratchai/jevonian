package admin_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

func seededSQLite(t testing.TB, count int) (*admin.SQLiteLogs, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	writer, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Sparse rowids catch accidental use of SQLite IDs as public append offsets.
	if _, err := db.Exec(`ALTER TABLE records ADD COLUMN tries TEXT;
		ALTER TABLE records ADD COLUMN skipped TEXT;
		ALTER TABLE records ADD COLUMN cache TEXT;
		ALTER TABLE records ADD COLUMN brain_channel TEXT;
		ALTER TABLE records ADD COLUMN cache_keep INTEGER;`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO records(rowid, id, request_id, ts_ms, ts, model, provider, kind, phase, session, reason, effort, retries, failovers, status, latency_ms, cost_usd, tries, skipped, cache, brain_channel, cache_keep)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 5, 11, 55, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		kind, phase, model, requestID := "request", "execute", "m", ""
		if i%7 == 0 {
			kind, requestID = "brain", "row-1"
		}
		if i%3 == 0 {
			phase = ""
		}
		if i%5 == 0 {
			model = "other"
		}
		// Status variety exercises the ok/error facets; every fourth row errors.
		status := 200
		if i%4 == 0 {
			status = 500
		}
		ts := at
		if i < count-20 {
			ts = at.Add(-48 * time.Hour)
		}
		if _, err := stmt.Exec(i*3+10, fmt.Sprintf("row-%d", i), requestID, ts.UnixMilli(), ts.Format(time.RFC3339Nano), model, "Provider", kind, phase, "session", "reason", "high", 0, 0, status, 120, 0.25, `[{"cause":"retry","marker":"preserved"}]`, `[{"provider":"skipped"}]`, `{"hit":true}`, "typesafe", 1); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	source, err := admin.OpenSQLiteLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	return source, db
}

func TestSQLiteQueriesMatchAppendOffsetsAndFilters(t *testing.T) {
	source, _ := seededSQLite(t, 3000)
	all, err := source.Records()
	if err != nil {
		t.Fatal(err)
	}
	fast := setup(t, source)
	slow := setup(t, &logs{rows: all})
	for _, query := range []string{
		"?limit=1", "?limit=7&before=2001", "?before=0", "?before=-2", "?before=9000",
		"?phase=-&model=m&limit=9", "?phase=all&q=retry&limit=3", "?q=PROVIDER%20execute", "?q=retry%20retry", "?q=retry%20failover", "?q=%25", "?q=preserved", "?q=reason%20high%20retry",
		"?session=session", "?session=missing", "?session=session&model=m&limit=9",
		"?session=session&phase=execute", "?session=session&limit=7&before=2001",
		"?session=session&q=retry", "?session=%20session%20",
		// Repeatable, multi-value filters. Within a group values are OR-ed.
		"?model=m&model=other", "?model=m&model=missing", "?phase=execute&phase=-",
		"?phase=-&phase=execute&model=other", "?provider=Provider", "?provider=Provider&provider=none",
		"?status=ok", "?status=error", "?status=ok&phase=plan", "?status=ok&phase=execute",
		"?provider=Provider&provider=none&q=retry", "?status=ok&status=error", "?status=bogus",
		"?status=error&model=m&model=other&session=session", "?status=&status=all&status=ok",
	} {
		_, got := request(t, fast.h, "GET", "/logs"+query, nil)
		_, want := request(t, slow.h, "GET", "/logs"+query, nil)
		if string(mustJSON(got)) != string(mustJSON(want)) {
			t.Fatalf("%s\ngot %s\nwant %s", query, mustJSON(got), mustJSON(want))
		}
	}
	// Facets must agree between the GROUP BY path and the in-memory path,
	// with and without filters and with the optional minutes window.
	for _, query := range []string{
		"", "?status=error", "?status=ok&phase=plan", "?model=m&model=other",
		"?provider=Provider&provider=none&q=retry", "?session=session&status=error",
		"?minutes=60", "?minutes=60&status=error", "?minutes=1", "?minutes=0", "?minutes=99999",
	} {
		_, got := request(t, fast.h, "GET", "/logs/facets"+query, nil)
		_, want := request(t, slow.h, "GET", "/logs/facets"+query, nil)
		if string(mustJSON(got)) != string(mustJSON(want)) {
			t.Fatalf("facets %s\ngot %s\nwant %s", query, mustJSON(got), mustJSON(want))
		}
	}
	for _, path := range []string{"/logs/row-1", "/logs/missing", "/logs/series?minutes=60&buckets=6&q=retry", "/logs/series?minutes=60&buckets=6&session=session", "/logs/series?minutes=60&buckets=6&session=missing", "/activity?range=today", "/activity?range=all"} {
		gotCode, got := request(t, fast.h, "GET", path, nil)
		wantCode, want := request(t, slow.h, "GET", path, nil)
		if gotCode != wantCode || string(mustJSON(got)) != string(mustJSON(want)) {
			t.Fatalf("%s\ngot %s\nwant %s", path, mustJSON(got), mustJSON(want))
		}
	}
	// The exact session filter keeps non-brain rows only; brain rows share the
	// same session value and must stay excluded on both paths.
	session, err := source.QueryLogs(context.Background(), admin.LogQuery{Filter: admin.LogFilter{Session: "session"}, Limit: 5})
	if err != nil || session.Total != 2571 || len(session.Logs) != 5 {
		t.Fatalf("session page %#v: %v", session, err)
	}
	for _, rec := range session.Logs {
		if rec["kind"] == "brain" {
			t.Fatalf("brain row leaked into session filter: %#v", rec)
		}
	}
	missing, err := source.QueryLogs(context.Background(), admin.LogQuery{Filter: admin.LogFilter{Session: "session-other"}})
	if err != nil || missing.Total != 0 || len(missing.Logs) != 0 {
		t.Fatalf("session miss %#v: %v", missing, err)
	}
	page, err := source.QueryLogs(context.Background(), admin.LogQuery{Limit: 1})
	if err != nil || len(page.Logs) != 1 || page.NextBefore == nil || *page.NextBefore != 2999 {
		t.Fatalf("page %#v: %v", page, err)
	}
	row := page.Logs[0]
	if row["brainChannel"] != "typesafe" || row["cacheKeep"] != int64(1) || row["cache"].(map[string]any)["hit"] != true || row["skipped"].([]any)[0].(map[string]any)["provider"] != "skipped" || row["tries"].([]any)[0].(map[string]any)["marker"] != "preserved" {
		t.Fatalf("extended fields lost: %#v", row)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.QueryLogs(ctx, admin.LogQuery{Limit: 1}); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

// guardedSQLite proves every scalable HTTP endpoint chooses the optional seam.
// Calling Records is a failure, not a hidden compatibility fallback.
type guardedSQLite struct {
	*admin.SQLiteLogs
	fullReads atomic.Int64
	maxBatch  atomic.Int64
}

func (s *guardedSQLite) Records() ([]admin.LogRecord, error) {
	s.fullReads.Add(1)
	return nil, errors.New("unexpected full ledger read")
}
func (s *guardedSQLite) ReadAfter(ctx context.Context, cursor int64, limit int) (admin.LogBatch, error) {
	batch, err := s.SQLiteLogs.ReadAfter(ctx, cursor, limit)
	for old := s.maxBatch.Load(); int64(len(batch.Logs)) > old; old = s.maxBatch.Load() {
		if s.maxBatch.CompareAndSwap(old, int64(len(batch.Logs))) {
			break
		}
	}
	return batch, err
}

func TestSQLiteIncrementalStreamsMultipleClientsAndCancellation(t *testing.T) {
	sqlite, db := seededSQLite(t, 10000)
	source := &guardedSQLite{SQLiteLogs: sqlite}
	x := setup(t, source)
	for _, path := range []string{"/logs?limit=3", "/logs/row-1", "/logs/series", "/activity?range=today", "/stats"} {
		code, out := request(t, x.h, "GET", path, nil)
		checkStatus(t, code, 200, out)
	}
	const clients = 6
	exited := make(chan struct{}, clients)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { exited <- struct{}{} }()
		x.h.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	responses := make([]*http.Response, clients)
	readers := make([]*bufio.Reader, clients)
	cancels := make([]context.CancelFunc, clients)
	for i := range responses {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/logs/stream?model=live", nil)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responses[i] = response
		t.Cleanup(func() { cancel(); response.Body.Close() })
		readers[i] = bufio.NewReader(response.Body)
		if event, _, err := readSSE(readers[i]); err != nil || event != "ready" {
			t.Fatalf("ready %q: %v", event, err)
		}
	}
	// More than one cursor batch, filtered brain rows, and a backdated append.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		kind := "request"
		if i%10 == 0 {
			kind = "brain"
		}
		if _, err := tx.Exec(`INSERT INTO records(id, ts_ms, ts, model, kind) VALUES (?, 0, '1970-01-01T00:00:00Z', 'live', ?)`, fmt.Sprintf("live-%d", i), kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := range readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 600; n++ {
				if n%10 == 0 {
					continue
				}
				event, data, err := readSSE(readers[i])
				want := fmt.Sprintf(`"id":"live-%d"`, n)
				if err != nil || event != "log" || !strings.Contains(data, want) {
					errs <- fmt.Errorf("client %d expected %s got %s %s: %v", i, want, event, data, err)
					return
				}
			}
			cancels[i]()
			responses[i].Body.Close()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := 0; i < clients; i++ {
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled stream handler leaked")
		}
	}
	if source.fullReads.Load() != 0 || source.maxBatch.Load() > 256 {
		t.Fatalf("full reads %d, max batch %d", source.fullReads.Load(), source.maxBatch.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.ReadAfter(ctx, 0, 256); err == nil {
		t.Fatal("cancelled tail succeeded")
	}
}

func readSSE(reader *bufio.Reader) (string, string, error) {
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return event, data, err
		}
		if line == "\n" {
			return event, data, nil
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
}

// A blocked HTTP writer models a slow client without blocking the producer.
type blockedWriter struct {
	header  http.Header
	body    bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Header() http.Header { return w.header }
func (w *blockedWriter) WriteHeader(int)     {}
func (w *blockedWriter) Flush()              {}
func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.body.Write(p)
}

func TestSubscriberOverflowResyncDoesNotBlockProducer(t *testing.T) {
	source := &logs{}
	x := setup(t, source)
	w := &blockedWriter{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("GET", "/logs/stream", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	done := make(chan struct{})
	go func() { x.h.ServeHTTP(w, r); close(done) }()
	<-w.entered
	produced := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			source.append(admin.LogRecord{"id": fmt.Sprintf("r-%d", i)})
		}
		close(produced)
	}()
	select {
	case <-produced:
	case <-time.After(2 * time.Second):
		t.Error("slow client blocked producer")
	}
	close(w.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("overflow failed to close stream")
	}
	if !strings.Contains(w.body.String(), "event: resync") || !strings.Contains(w.body.String(), `"reason":"overflow"`) || !strings.Contains(w.body.String(), `"resync":true`) {
		t.Fatalf("missing explicit resync: %s", w.body.String())
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.listener != nil {
		t.Fatal("subscription leaked")
	}
}

func TestSQLiteRangeUsesIndexesAndCancellation(t *testing.T) {
	source, db := seededSQLite(t, 10000)
	queries := []struct{ sql, index string }{
		{`SELECT id FROM records WHERE rowid > 30000 ORDER BY rowid LIMIT 256`, "INTEGER PRIMARY KEY"},
		{`SELECT id FROM records WHERE id = 'row-1' LIMIT 1`, "sqlite_autoindex_records_1"},
		{`SELECT id FROM records INDEXED BY idx_admin_records_request_kind WHERE request_id = 'row-1' AND kind = 'brain' AND rowid > 0 ORDER BY rowid LIMIT 256`, "idx_admin_records_request_kind"},
		{`SELECT COUNT(*) FROM records WHERE ts_ms >= 1791198000000 AND ts_ms <= 1791201600000 AND kind != 'brain'`, "idx_admin_records_ts"},
		{`SELECT id FROM records INDEXED BY idx_admin_records_ts WHERE ts_ms >= 1791198000000 AND ts_ms <= 1791201600000 ORDER BY rowid`, "idx_admin_records_ts"},
		{`SELECT id FROM records WHERE session = 'session' AND kind != 'brain' ORDER BY rowid DESC LIMIT 5`, "idx_admin_records_session"},
	}
	for _, q := range queries {
		rows, err := db.Query("EXPLAIN QUERY PLAN " + q.sql)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "; ")
		}
		rows.Close()
		t.Log(plan.String())
		if !strings.Contains(plan.String(), q.index) {
			t.Fatalf("missing %s: %s", q.index, plan.String())
		}
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	n := 0
	if err := source.ScanLogRange(context.Background(), admin.LogRange{Start: now.Add(-time.Hour), End: now}, func(admin.LogRecord) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("range visited %d want 20", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n = 0
	err := source.ScanLogRange(ctx, admin.LogRange{}, func(admin.LogRecord) error { n++; cancel(); return nil })
	if err == nil || n != 1 {
		t.Fatalf("cancel range visited %d: %v", n, err)
	}
}

func BenchmarkSQLiteBoundedPaths(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			source, _ := seededSQLite(b, count)
			ctx := context.Background()
			cursor, err := source.TailCursor(ctx)
			if err != nil {
				b.Fatal(err)
			}
			b.Run("IdleTail", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := source.ReadAfter(ctx, cursor, 256); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Page20", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := source.QueryLogs(ctx, admin.LogQuery{Limit: 20}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Series", func(b *testing.B) {
				b.ReportAllocs()
				now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
				for i := 0; i < b.N; i++ {
					if _, err := source.QueryLogSeries(ctx, admin.LogFilter{}, now.Add(-time.Hour), now, 30); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func TestSQLiteCursorStableAcrossBackdatedAppendAndLargeRowIDs(t *testing.T) {
	source, db := seededSQLite(t, 1000)
	ctx := context.Background()
	page, err := source.QueryLogs(ctx, admin.LogQuery{Limit: 10})
	if err != nil || page.NextBefore == nil {
		t.Fatalf("first page: %#v %v", page, err)
	}
	old, err := source.QueryLogs(ctx, admin.LogQuery{Limit: 10, Before: page.NextBefore})
	if err != nil {
		t.Fatal(err)
	}
	const large int64 = 9007199254740993
	if _, err := db.Exec(`INSERT INTO records(rowid, id, ts_ms, ts, model) VALUES (?, 'backdated', 0, '1970-01-01T00:00:00Z', 'm')`, large); err != nil {
		t.Fatal(err)
	}
	again, err := source.QueryLogs(ctx, admin.LogQuery{Limit: 10, Before: page.NextBefore})
	if err != nil || string(mustJSON(old.Logs)) != string(mustJSON(again.Logs)) {
		t.Fatalf("append shifted cursor: %v", err)
	}
	latest, err := source.QueryLogs(ctx, admin.LogQuery{Limit: 1})
	if err != nil || latest.Logs[0]["id"] != "backdated" || latest.NextBefore == nil || *latest.NextBefore != 1000 {
		t.Fatalf("append cursor: %#v %v", latest, err)
	}
	batch, err := source.ReadAfter(ctx, large-1, 256)
	if err != nil || batch.Cursor != large || len(batch.Logs) != 1 {
		t.Fatalf("large rowid tail: %#v %v", batch, err)
	}
	if _, err := db.Exec(`DELETE FROM records WHERE id='backdated'`); err != nil {
		t.Fatal(err)
	}
	reset, err := source.ReadAfter(ctx, batch.Cursor, 256)
	if err != nil || !reset.Reset {
		t.Fatalf("reset %#v %v", reset, err)
	}
}

func TestSQLiteStreamResetRequestsBackfill(t *testing.T) {
	source, db := seededSQLite(t, 1000)
	x := setup(t, source)
	server := httptest.NewServer(x.h)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if event, _, err := readSSE(reader); err != nil || event != "ready" {
		t.Fatalf("ready %q: %v", event, err)
	}
	if _, err := db.Exec(`DELETE FROM records`); err != nil {
		t.Fatal(err)
	}
	if event, data, err := readSSE(reader); err != nil || event != "resync" || !strings.Contains(data, "ledger-reset") {
		t.Fatalf("reset %q %s: %v", event, data, err)
	}
	if event, data, err := readSSE(reader); err != nil || event != "ready" || !strings.Contains(data, `"resync":true`) {
		t.Fatalf("backfill %q %s: %v", event, data, err)
	}
}

func TestSQLiteSearchUnicodeAndMalformedTries(t *testing.T) {
	source, db := seededSQLite(t, 100)
	if _, err := db.Exec(`UPDATE records SET provider='ÄProvider', tries='broken' WHERE id='row-1';
		UPDATE records SET tries='["retry", null, {"cause":"retry"}, {"cause":"failover"}]' WHERE id='row-2'`); err != nil {
		t.Fatal(err)
	}
	all, err := source.Records()
	if err != nil {
		t.Fatal(err)
	}
	fast, slow := setup(t, source), setup(t, &logs{rows: all})
	for _, query := range []string{"äprovider", "retry failover", "provider  session"} {
		path := "/logs?q=" + url.QueryEscape(query)
		_, got := request(t, fast.h, "GET", path, nil)
		_, want := request(t, slow.h, "GET", path, nil)
		if string(mustJSON(got)) != string(mustJSON(want)) {
			t.Fatalf("%s got %s want %s", query, mustJSON(got), mustJSON(want))
		}
	}
}
