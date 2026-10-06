package keys_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/ledger"
)

func floatPtr(v float64) *float64 { return &v }

func TestAllowKeyOpenModeAndVerify(t *testing.T) {
	dir := t.TempDir()
	db, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := keys.Open(dir, db)
	if store.HasKeys() {
		t.Fatal("expected empty store")
	}

	d, err := store.AllowKey("")
	if err != nil {
		t.Fatalf("AllowKey open: %v", err)
	}
	if !d.Allowed || !d.Open {
		t.Fatalf("open mode decision = %+v", d)
	}

	created, err := store.Create(keys.CreateOptions{Name: "agent", LimitUSD: floatPtr(1)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(created.Key, "sk-jev-") {
		t.Fatalf("key prefix = %q", created.Key)
	}
	if !store.HasKeys() {
		t.Fatal("expected HasKeys after create")
	}

	d, err = store.AllowKey("")
	if err != nil {
		t.Fatalf("AllowKey missing: %v", err)
	}
	if d.Allowed || d.Reason != keys.DenyMissing {
		t.Fatalf("missing token decision = %+v", d)
	}

	d, err = store.AllowKey("sk-jev-not-a-real-key")
	if err != nil {
		t.Fatalf("AllowKey invalid: %v", err)
	}
	if d.Allowed || d.Reason != keys.DenyInvalid {
		t.Fatalf("invalid token decision = %+v", d)
	}

	auth := "Bearer " + created.Key
	token := keys.TokenFromHeaders(auth, "")
	d, err = store.AllowKey(token)
	if err != nil {
		t.Fatalf("AllowKey valid: %v", err)
	}
	if !d.Allowed || d.Key == nil || d.Key.ID != created.Record.ID {
		t.Fatalf("valid decision = %+v", d)
	}
	if d.Key.Requests != 1 {
		t.Fatalf("requests = %d, want 1", d.Key.Requests)
	}
}

func TestKeySpendAndCreditLimit(t *testing.T) {
	dir := t.TempDir()
	db, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := keys.Open(dir, db)
	created, err := store.Create(keys.CreateOptions{Name: "agent-a", LimitUSD: floatPtr(0.10)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now().UTC()

	if err := db.Append(ledger.Record{
		TS: now, Session: "s1", Path: "/v1/chat/completions",
		Provider: "openrouter", Model: "gpt-4o", Status: 200,
		CostUSD: floatPtr(0.05), Billing: "api",
		KeyID: created.Record.ID, KeyName: created.Record.Name,
	}); err != nil {
		t.Fatalf("Append api: %v", err)
	}
	if err := db.Append(ledger.Record{
		TS: now, Session: "s2", Path: "/v1/chat/completions",
		Provider: "chatgpt", Model: "o1", Status: 200,
		CostUSD: floatPtr(0.12), Billing: "subscription",
		KeyID: created.Record.ID, KeyName: created.Record.Name,
	}); err != nil {
		t.Fatalf("Append sub: %v", err)
	}

	spend, err := store.KeySpendUSD(created.Record.ID)
	if err != nil {
		t.Fatalf("KeySpendUSD: %v", err)
	}
	if spend != 0.05 {
		t.Fatalf("api spend = %v, want 0.05", spend)
	}

	withUsage, err := store.ListWithUsage()
	if err != nil {
		t.Fatalf("ListWithUsage: %v", err)
	}
	if len(withUsage) != 1 {
		t.Fatalf("len = %d", len(withUsage))
	}
	if withUsage[0].SpendUSD == nil || *withUsage[0].SpendUSD != 0.05 {
		t.Fatalf("spendUsd = %v", withUsage[0].SpendUSD)
	}
	if withUsage[0].SubscriptionUSD == nil || *withUsage[0].SubscriptionUSD != 0.12 {
		t.Fatalf("subscriptionUsd = %v", withUsage[0].SubscriptionUSD)
	}

	// Still under limit (0.05 < 0.10).
	d, err := store.AllowKey(created.Key)
	if err != nil {
		t.Fatalf("AllowKey under limit: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("under limit decision = %+v", d)
	}

	if err := db.Append(ledger.Record{
		TS: now, Session: "s3", Path: "/v1/chat/completions",
		Provider: "openrouter", Model: "gpt-4o", Status: 200,
		CostUSD: floatPtr(0.06), Billing: "api",
		KeyID: created.Record.ID, KeyName: created.Record.Name,
	}); err != nil {
		t.Fatalf("Append over: %v", err)
	}

	d, err = store.AllowKey(created.Key)
	if err != nil {
		t.Fatalf("AllowKey over limit: %v", err)
	}
	if d.Allowed || d.Reason != keys.DenyCreditLimit {
		t.Fatalf("credit limit decision = %+v", d)
	}
}

// The auth hot path must not rewrite keys.json per request (src/keys.ts did).
// Usage is batched in memory and persisted by Flush.
func TestAllowKeyBatchesUsageWrites(t *testing.T) {
	dir := t.TempDir()
	store := keys.Open(dir, nil)
	created, err := store.Create(keys.CreateOptions{Name: "hot"})
	if err != nil {
		t.Fatal(err)
	}
	path := keys.Path(dir)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		d, err := store.AllowKey(created.Key)
		if err != nil || !d.Allowed {
			t.Fatalf("AllowKey %d: %+v %v", i, d, err)
		}
		if d.Key.Requests != i+1 {
			t.Fatalf("decision requests = %d, want %d", d.Key.Requests, i+1)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("keys.json was rewritten on the request path")
	}
	// Pending usage is visible to the dashboard before it hits disk.
	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].Requests != 50 {
		t.Fatalf("List before flush = %+v %v", list, err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh := keys.Open(dir, nil)
	list, err = fresh.List()
	if err != nil || len(list) != 1 || list[0].Requests != 50 || list[0].LastUsedAt == "" {
		t.Fatalf("persisted usage = %+v %v", list, err)
	}
}

// A key created by another process (the CLI) is picked up without a restart,
// and a flush does not clobber it.
func TestStorePicksUpExternalWrites(t *testing.T) {
	dir := t.TempDir()
	server := keys.Open(dir, nil)
	first, err := server.Create(keys.CreateOptions{Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.AllowKey(first.Key); err != nil {
		t.Fatal(err)
	}
	// Ensure the external write gets a distinct mtime on coarse filesystems.
	time.Sleep(20 * time.Millisecond)
	cli := keys.Open(dir, nil)
	second, err := cli.Create(keys.CreateOptions{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := server.AllowKey(second.Key)
	if err != nil || !d.Allowed {
		t.Fatalf("external key not visible: %+v %v", d, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	list, err := keys.Open(dir, nil).List()
	if err != nil || len(list) != 2 {
		t.Fatalf("after flush = %+v %v", list, err)
	}
	for _, k := range list {
		if k.Requests != 1 {
			t.Fatalf("key %s requests = %d, want 1", k.Name, k.Requests)
		}
	}
}

func TestAllowKeyConcurrent(t *testing.T) {
	dir := t.TempDir()
	store := keys.Open(dir, nil)
	created, err := store.Create(keys.CreateOptions{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := store.AllowKey(created.Key); err != nil || !d.Allowed {
				t.Errorf("AllowKey: %+v %v", d, err)
			}
		}()
	}
	wg.Wait()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	list, _ := keys.Open(dir, nil).List()
	if len(list) != 1 || list[0].Requests != 100 {
		t.Fatalf("requests = %+v", list)
	}
}

func TestTokenFromHeaders(t *testing.T) {
	if got := keys.TokenFromHeaders("Bearer abc", ""); got != "abc" {
		t.Fatalf("bearer = %q", got)
	}
	if got := keys.TokenFromHeaders("bearer abc", ""); got != "abc" {
		t.Fatalf("bearer lower = %q", got)
	}
	if got := keys.TokenFromHeaders("", "xyz"); got != "xyz" {
		t.Fatalf("x-api-key = %q", got)
	}
}

func TestListCreate(t *testing.T) {
	dir := t.TempDir()
	store := keys.Open(dir, nil)
	a, err := store.Create(keys.CreateOptions{Name: "a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := store.Create(keys.CreateOptions{Name: "b", LimitUSD: floatPtr(5)})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].ID != a.Record.ID || list[1].ID != b.Record.ID {
		t.Fatalf("order/ids = %+v", list)
	}
	if b.Record.LimitUSD == nil || *b.Record.LimitUSD != 5 {
		t.Fatalf("limit = %v", b.Record.LimitUSD)
	}
}
