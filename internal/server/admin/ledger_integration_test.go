package admin_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// Exercise the actual writer schema and its independent admin reader pool,
// rather than only the extended SQLite fixture used by query contract tests.
func TestAdminReadersDoNotLoseConcurrentRuntimeAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	writer, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := admin.OpenSQLiteLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const workers = 6
	const perWorker = 50
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(2)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := writer.Append(ledger.Record{
					ID: fmt.Sprintf("%d-%d", worker, i), Provider: "healthy", Model: "m",
					Status: 200, PromptTokens: 12, CompletionTokens: 3,
				}); err != nil {
					t.Error(err)
					return
				}
			}
		}(worker)
		go func() {
			defer wg.Done()
			cursor := int64(0)
			for i := 0; i < perWorker; i++ {
				page, err := reader.QueryLogs(ctx, admin.LogQuery{Limit: 20})
				if err != nil {
					t.Error(err)
					return
				}
				if len(page.Logs) > 20 {
					t.Error("page exceeded its bound")
				}
				batch, err := reader.ReadAfter(ctx, cursor, 16)
				if err != nil {
					t.Error(err)
					return
				}
				if len(batch.Logs) > 16 || batch.Cursor < cursor || batch.Reset {
					t.Error("invalid append-only cursor batch")
					return
				}
				cursor = batch.Cursor
			}
		}()
	}
	wg.Wait()
	page, err := reader.QueryLogs(ctx, admin.LogQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != workers*perWorker || len(page.Logs) != 20 {
		t.Fatalf("reader lost runtime records: total=%d page=%d", page.Total, len(page.Logs))
	}
	count, err := writer.Count()
	if err != nil || count != workers*perWorker {
		t.Fatalf("writer lost records: count=%d err=%v", count, err)
	}
}
