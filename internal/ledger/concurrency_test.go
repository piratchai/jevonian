package ledger_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
)

func TestConcurrentAppendsAndSpendReadsLoseNoRecords(t *testing.T) {
	db, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const writers = 24
	const each = 20
	var wg sync.WaitGroup
	for worker := 0; worker < writers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				cost := 0.01
				if err := db.Append(ledger.Record{ID: fmt.Sprintf("%d-%d", worker, i), Provider: "p", KeyID: "k", CostUSD: &cost}); err != nil {
					t.Error(err)
				}
				if _, err := db.KeyAllTime("k"); err != nil {
					t.Error(err)
				}
				if _, err := db.ProviderWindow("p", time.Hour, time.Now()); err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	wg.Wait()
	n, err := db.Count()
	if err != nil || n != writers*each {
		t.Fatalf("lost records: %d %v", n, err)
	}
	total, err := db.KeyAllTime("k")
	if err != nil || total.Requests != writers*each {
		t.Fatalf("rollup: %+v %v", total, err)
	}
}

func BenchmarkConcurrentLedgerAppend(b *testing.B) {
	db, err := ledger.Open(filepath.Join(b.TempDir(), "ledger.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := db.Append(ledger.Record{Provider: "p", KeyID: "k"}); err != nil {
				b.Error(err)
			}
		}
	})
}
